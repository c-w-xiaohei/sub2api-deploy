//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is a source contract: the CI suite exercises behavior; this guards the
// required Automation boundary without starting a Pulumi process locally.
func TestAutomationSeamSourceContract(t *testing.T) {
	path := filepath.Join("automation_linux.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"github.com/pulumi/pulumi/sdk/v3/go/auto",
		"var _ auto.PulumiCommand = attachedPulumiCommand{}",
		"runAttachedCommand(ctx",
		"if attachedLifecycle(args)",
		"args = attachedNonInteractiveArgs(args)",
		"return append([]string{\"--non-interactive\"}, args...)",
		"PULUMI_AUTOMATION_API=true",
		"attachedPulumiVersion(ctx, paths.pulumi, env)",
		"command.Env, command.Stderr = mergePulumiEnv(env, nil), io.Discard",
		"context.WithTimeout(ctx, attachedVersionTimeout)",
		"os.Pipe()",
		"io.LimitReader(output, 257)",
		"case <-done.done:",
		"case <-versionCtx.Done():",
		"Sweep the isolated group even after the direct probe child exits.",
		"stopAttachedProcess(command, done)",
		"auto.NewLocalWorkspace(ctx, auto.WorkDir(workspaceDir), auto.Pulumi(command))",
		"Pulumi.\"+stackName+\".yaml",
		"stagedProjectYAML",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("Automation seam missing %q", want)
		}
	}
}

func TestAttachedExecutionSourceContract(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("attached_linux.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"if err := ctx.Err(); err != nil {",
		"pulumi.Env, pulumi.Stdin, pulumi.Stdout, pulumi.Stderr",
		"SysProcAttr{Setpgid: true}",
		"syscall.Kill(-command.Process.Pid, syscall.SIGINT)",
		"syscall.Kill(-command.Process.Pid, syscall.SIGKILL)",
		"descendants remain in its group",
		"stopAttachedProcess(pulumi, pulumiDone)",
		"stopAttachedProcess(provider, providerDone)",
		"_ = output.Close()",
		"!strings.HasPrefix(value, \"SOPS_\")",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("attached execution seam missing %q", want)
		}
	}
}
