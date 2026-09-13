//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/blang/semver"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

// attachedPulumiCommand is the Automation API boundary. The Engine remains in
// the bundled Pulumi CLI; this adapter only owns the attached Host Provider.
type attachedPulumiCommand struct {
	paths   attachedExecutables
	env     []string
	decide  func(context.Context, hostcontract.ApprovalSubject) bool
	version semver.Version
	lifecycleSucceeded *atomic.Bool
}

var _ auto.PulumiCommand = attachedPulumiCommand{}

const attachedVersionTimeout = 5 * time.Second

func newAttachedPulumiCommand(ctx context.Context, paths attachedExecutables, env []string, decide func(context.Context, hostcontract.ApprovalSubject) bool) (attachedPulumiCommand, error) {
	version, err := attachedPulumiVersion(ctx, paths.pulumi, env)
	if err != nil {
		return attachedPulumiCommand{}, errInvalidPulumiInputs
	}
	return attachedPulumiCommand{paths: paths, env: append([]string(nil), env...), decide: decide, version: version, lifecycleSucceeded: &atomic.Bool{}}, nil
}

func (c attachedPulumiCommand) Version() semver.Version { return c.version }

func (c attachedPulumiCommand) Run(ctx context.Context, workdir string, stdin io.Reader, additionalOutput, additionalErrorOutput []io.Writer, additionalEnv []string, args ...string) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	outputs := append(append([]io.Writer(nil), additionalOutput...), &stdout)
	errorsOut := append(append([]io.Writer(nil), additionalErrorOutput...), &stderr)
	env := mergePulumiEnv(c.env, additionalEnv)
	args = attachedNonInteractiveArgs(args)

	var err error
	lifecycle := attachedLifecycle(args)
	if lifecycle {
		err = runAttachedCommand(ctx, c.paths, workdir, stdin, args, env, io.MultiWriter(outputs...), io.MultiWriter(errorsOut...), c.decide)
	} else {
		err = runPulumiCommand(ctx, c.paths.pulumi, workdir, stdin, args, env, io.MultiWriter(outputs...), io.MultiWriter(errorsOut...))
	}
	if lifecycle && err == nil && c.lifecycleSucceeded != nil {
		c.lifecycleSucceeded.Store(true)
	}
	return stdout.String(), stderr.String(), pulumiExitCode(err), err
}

func (c attachedPulumiCommand) beginLifecycle() {
	if c.lifecycleSucceeded != nil {
		c.lifecycleSucceeded.Store(false)
	}
}

func (c attachedPulumiCommand) completedLifecycle() bool {
	return c.lifecycleSucceeded != nil && c.lifecycleSucceeded.Load()
}

func attachedNonInteractiveArgs(args []string) []string {
	pulumiArgs := args
	if separator := slices.Index(args, "--"); separator >= 0 {
		pulumiArgs = args[:separator]
	}
	if slices.Contains(pulumiArgs, "--non-interactive") {
		return args
	}
	return append([]string{"--non-interactive"}, args...)
}

func attachedLifecycle(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "up", "preview", "refresh", "destroy", "import":
			return true
		default:
			return false
		}
	}
	return false
}

func mergePulumiEnv(base, additional []string) []string {
	result := make([]string, 0, len(base)+len(additional)+1)
	for _, value := range append(append([]string(nil), base...), additional...) {
		name, _, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		for i := len(result) - 1; i >= 0; i-- {
			if existing, _, _ := strings.Cut(result[i], "="); existing == name {
				result = append(result[:i], result[i+1:]...)
			}
		}
		result = append(result, value)
	}
	return mergePulumiEnvValue(result, "PULUMI_AUTOMATION_API=true")
}

func mergePulumiEnvValue(env []string, value string) []string {
	name, _, _ := strings.Cut(value, "=")
	for i := len(env) - 1; i >= 0; i-- {
		if existing, _, _ := strings.Cut(env[i], "="); existing == name {
			env = append(env[:i], env[i+1:]...)
		}
	}
	return append(env, value)
}

func runPulumiCommand(ctx context.Context, path, workdir string, stdin io.Reader, args, env []string, stdout, stderr io.Writer) error {
	if ctx == nil {
		return errInvalidPulumiInputs
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	command := exec.Command(path, args...)
	command.Dir, command.Env, command.Stdin, command.Stdout, command.Stderr = workdir, env, stdin, stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return err
	}
	done := waitAttached(command)
	select {
	case <-done.done:
		return done.err
	case <-ctx.Done():
		stopAttachedProcess(command, done)
		return context.Cause(ctx)
	}
}

func attachedPulumiVersion(ctx context.Context, path string, env []string) (semver.Version, error) {
	if ctx == nil || ctx.Err() != nil {
		return semver.Version{}, errInvalidPulumiInputs
	}
	versionCtx, cancel := context.WithTimeout(ctx, attachedVersionTimeout)
	defer cancel()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		return semver.Version{}, errInvalidPulumiInputs
	}
	defer output.Close()
	command := exec.Command(path, "version")
	command.Env, command.Stderr = mergePulumiEnv(env, nil), io.Discard
	command.Stdout = outputWriter
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = outputWriter.Close()
		return semver.Version{}, errInvalidPulumiInputs
	}
	_ = outputWriter.Close()
	done := waitAttached(command)
	type versionOutput struct {
		contents []byte
		err      error
	}
	readDone := make(chan versionOutput, 1)
	go func() {
		contents, err := io.ReadAll(io.LimitReader(output, 257))
		readDone <- versionOutput{contents, err}
	}()
	var result versionOutput
	select {
	case result = <-readDone:
		if result.err != nil || len(result.contents) > 256 {
			stopAttachedProcess(command, done)
			return semver.Version{}, errInvalidPulumiInputs
		}
		select {
		case <-done.done:
		case <-versionCtx.Done():
			_ = output.Close()
			stopAttachedProcess(command, done)
			return semver.Version{}, errInvalidPulumiInputs
		}
	case <-versionCtx.Done():
		_ = output.Close()
		stopAttachedProcess(command, done)
		return semver.Version{}, errInvalidPulumiInputs
	}
	// Sweep the isolated group even after the direct probe child exits.
	stopAttachedProcess(command, done)
	if done.err != nil || versionCtx.Err() != nil {
		return semver.Version{}, errInvalidPulumiInputs
	}
	version, err := semver.ParseTolerant(strings.TrimSpace(string(result.contents)))
	if err != nil {
		return semver.Version{}, errInvalidPulumiInputs
	}
	return version, nil
}

func pulumiExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -2 // Automation API's unknownErrorCode sentinel.
}

func newStagedPulumiWorkspace(ctx context.Context, workdir, stagedPath, stackName string, command attachedPulumiCommand) (auto.Workspace, error) {
	if ctx == nil || !filepath.IsAbs(workdir) || stackName == "" || filepath.Base(stagedPath) != "Pulumi."+stackName+".yaml" {
		return nil, errInvalidStagedStack
	}
	return auto.NewLocalWorkspace(ctx, auto.WorkDir(workdir), auto.Pulumi(command))
}
