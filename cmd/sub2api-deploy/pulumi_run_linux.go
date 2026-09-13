//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/debug"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optrefresh"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"gopkg.in/yaml.v3"
)

var errInvalidPulumiInputs = errors.New("invalid Pulumi inputs")

const maxPulumiSecretsSize = 16 << 20

func preparePulumiStackValues(configYAML, secretsYAML []byte) (string, stackConfigValues, error) {
	if !validPulumiYAMLBytes(configYAML) || len(secretsYAML) > maxPulumiSecretsSize || !validPulumiYAMLBytes(secretsYAML) {
		return "", stackConfigValues{}, errInvalidPulumiInputs
	}
	decoder := yaml.NewDecoder(bytes.NewReader(secretsYAML))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || !validPulumiYAMLNode(&document) {
		return "", stackConfigValues{}, errInvalidPulumiInputs
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", stackConfigValues{}, errInvalidPulumiInputs
	}

	root := document.Content[0]
	passphrase := ""
	passphraseIndex := -1
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Value != "pulumiPassphrase" {
			continue
		}
		if passphraseIndex >= 0 || value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value == "" || len(value.Value) > 4096 || !validPulumiYAMLBytes([]byte(value.Value)) {
			return "", stackConfigValues{}, errInvalidPulumiInputs
		}
		passphrase, passphraseIndex = value.Value, i
	}
	if passphraseIndex < 0 {
		return "", stackConfigValues{}, errInvalidPulumiInputs
	}
	root.Content = append(root.Content[:passphraseIndex], root.Content[passphraseIndex+2:]...)
	var sanitized bytes.Buffer
	encoder := yaml.NewEncoder(&sanitized)
	if err := encoder.Encode(&document); err != nil || encoder.Close() != nil {
		return "", stackConfigValues{}, errInvalidPulumiInputs
	}
	secrets, err := environment.ParseSecrets(sanitized.Bytes())
	if err != nil || secrets.PulumiPassphrase != "" || secrets.RevisionKey == "" {
		return "", stackConfigValues{}, errInvalidPulumiInputs
	}
	return passphrase, stackConfigValues{environmentConfig: string(configYAML), environmentSecrets: sanitized.String(), revisionKey: secrets.RevisionKey}, nil
}

func validPulumiYAMLBytes(value []byte) bool {
	return utf8.Valid(value) && !bytes.Contains(value, []byte{0})
}

func validPulumiYAMLNode(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode || (node.Kind == yaml.MappingNode && len(node.Content)%2 != 0) {
		return false
	}
	if node.Kind == yaml.MappingNode {
		keys := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Value == "<<" {
				return false
			}
			identity := key.Tag + "\x00" + key.Value
			if _, exists := keys[identity]; exists {
				return false
			}
			keys[identity] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if !validPulumiYAMLNode(child) {
			return false
		}
	}
	return true
}

func runPulumiPlan(ctx context.Context, plan pulumiPlan, workdir, cliPath string, env []string, stdout, stderr io.Writer, decide func(context.Context, hostcontract.ApprovalSubject) bool) error {
	if ctx == nil || stdout == nil || stderr == nil || decide == nil {
		return errInvalidPulumiInputs
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !environment.ValidID(plan.environment) || !pulumiOperation(plan.operation) {
		return errInvalidPulumiInputs
	}
	resolvedWorkdir, err := resolvePulumiWorkdir(workdir)
	if err != nil {
		return errInvalidPulumiInputs
	}
	paths, err := environment.ResolveEnvironment(resolvedWorkdir, plan.environment)
	if err != nil {
		return errInvalidPulumiInputs
	}
	configYAML, err := readBoundedPulumiFile(paths.Config)
	if err != nil {
		return errInvalidPulumiInputs
	}
	sopsEnv, childEnv := pulumiProcessEnvs(env)
	sopsPath, err := resolveSOPS(sopsEnv)
	if err != nil {
		return errInvalidPulumiInputs
	}
	secretsYAML, err := decryptPulumiSecrets(ctx, sopsPath, resolvedWorkdir, paths.Secrets, sopsEnv)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errInvalidPulumiInputs
	}
	passphrase, values, err := preparePulumiStackValues(configYAML, secretsYAML)
	if err != nil {
		return errInvalidPulumiInputs
	}
	values.hostImportTarget = plan.importTarget
	project, err := workspace.LoadProject(filepath.Join(resolvedWorkdir, "Pulumi.yaml"))
	if err != nil || string(project.Name) != "sub2api-environment" {
		return errInvalidPulumiInputs
	}
	executables, err := resolveAttachedExecutables(cliPath)
	if err != nil {
		return errInvalidPulumiInputs
	}
	projectYAML, err := readBoundedPulumiFile(filepath.Join(resolvedWorkdir, "Pulumi.yaml"))
	if err != nil {
		return errInvalidPulumiInputs
	}
	projectYAML, err = stagedProjectYAML(projectYAML, resolvedWorkdir)
	if err != nil {
		return errInvalidPulumiInputs
	}
	err = withNamedStagedStack(ctx, project, filepath.Join(resolvedWorkdir, "Pulumi."+plan.environment+".yaml"), "Pulumi."+plan.environment+".yaml", passphrase, values, func(stagedPath string) error {
		pulumiEnv := append(childEnv, "PULUMI_CONFIG_PASSPHRASE_FILE="+filepath.Join(filepath.Dir(stagedPath), "passphrase"))
		command, err := newAttachedPulumiCommand(ctx, executables, pulumiEnv, decide)
		if err != nil {
			return errInvalidStagedStack
		}
		workspace, err := newStagedPulumiWorkspace(ctx, stagedPath, plan.environment, projectYAML, command)
		if err != nil {
			return errInvalidStagedStack
		}
		return runPulumiStack(ctx, workspace, plan, stagedPath, stdout, stderr)
	})
	if err != nil && errors.Is(err, errInvalidStagedStack) {
		return errInvalidPulumiInputs
	}
	return err
}

var errPulumiConfirmationRequired = errors.New("Pulumi operation was not confirmed")

func runPulumiStack(ctx context.Context, workspace auto.Workspace, plan pulumiPlan, configFile string, stdout, stderr io.Writer) error {
	if ctx == nil || workspace == nil || stdout == nil || stderr == nil {
		return errInvalidPulumiInputs
	}
	if !environment.ValidID(plan.environment) || !pulumiOperation(plan.operation) || !validPulumiOptions(plan.operation, plan.options) {
		return errInvalidPulumiInputs
	}
	stack, err := auto.SelectStack(ctx, plan.environment, workspace)
	if err != nil {
		return publicPulumiError(ctx, workspace, err)
	}
	options := plan.options
	options.configFile = configFile
	if (plan.operation == "up" || plan.operation == "destroy") && !options.approve {
		if err := runPulumiPreview(ctx, &stack, plan.operation, options, stdout, stderr); err != nil {
			return publicPulumiError(ctx, workspace, err)
		}
		if !confirmPulumiOperation(ctx, plan.operation) {
			return errPulumiConfirmationRequired
		}
	}

	switch plan.operation {
	case "preview":
		_, err = stack.Preview(ctx, pulumiPreviewOptions(options, stdout, stderr)...)
	case "up":
		_, err = stack.Up(ctx, pulumiUpOptions(options, stdout, stderr)...)
	case "refresh":
		_, err = stack.Refresh(ctx, pulumiRefreshOptions(options, stdout, stderr)...)
	case "destroy":
		_, err = stack.Destroy(ctx, pulumiDestroyOptions(options, stdout, stderr)...)
	default:
		return errInvalidPulumiInputs
	}
	return publicPulumiError(ctx, workspace, err)
}

func publicPulumiError(ctx context.Context, workspace auto.Workspace, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if workspace != nil {
		if _, attached := workspace.PulumiCommand().(attachedPulumiCommand); attached {
			return errors.New("pulumi failed")
		}
	}
	return err
}

func runPulumiPreview(ctx context.Context, stack *auto.Stack, operation string, options pulumiOptions, stdout, stderr io.Writer) error {
	var err error
	switch operation {
	case "up":
		_, err = stack.Preview(ctx, pulumiPreviewOptions(options, stdout, stderr)...)
	case "destroy":
		_, err = stack.PreviewDestroy(ctx, pulumiDestroyOptions(options, stdout, stderr)...)
	default:
		return errInvalidPulumiInputs
	}
	return err
}

func pulumiPreviewOptions(options pulumiOptions, stdout, stderr io.Writer) []optpreview.Option {
	result := []optpreview.Option{optpreview.ProgressStreams(stdout), optpreview.ErrorProgressStreams(stderr)}
	if options.debugLevel != nil { result = append(result, optpreview.DebugLogging(debug.LoggingOptions{LogLevel: options.debugLevel})) }
	if options.message != "" { result = append(result, optpreview.Message(options.message)) }
	if options.parallel > 0 { result = append(result, optpreview.Parallel(options.parallel)) }
	if len(options.targets) > 0 { result = append(result, optpreview.Target(options.targets)) }
	if len(options.replaces) > 0 { result = append(result, optpreview.Replace(options.replaces)) }
	if len(options.excludes) > 0 { result = append(result, optpreview.Exclude(options.excludes)) }
	if len(options.policyPacks) > 0 { result = append(result, optpreview.PolicyPacks(options.policyPacks...)) }
	if len(options.policyPackConfig) > 0 { result = append(result, optpreview.PolicyPackConfigs(options.policyPackConfig...)) }
	if options.plan != "" { result = append(result, optpreview.Plan(options.plan)) }
	if options.color != "" { result = append(result, optpreview.Color(options.color)) }
	if options.diff { result = append(result, optpreview.Diff()) }
	if options.expectNoChanges { result = append(result, optpreview.ExpectNoChanges()) }
	if options.targetDependents { result = append(result, optpreview.TargetDependents()) }
	if options.excludeDependents { result = append(result, optpreview.ExcludeDependents()) }
	if options.refresh { result = append(result, optpreview.Refresh()) }
	if options.suppressProgress { result = append(result, optpreview.SuppressProgress()) }
	if options.suppressOutputs { result = append(result, optpreview.SuppressOutputs()) }
	if options.configFile != "" { result = append(result, optpreview.ConfigFile(options.configFile)) }
	return result
}

func pulumiUpOptions(options pulumiOptions, stdout, stderr io.Writer) []optup.Option {
	result := []optup.Option{optup.ProgressStreams(stdout), optup.ErrorProgressStreams(stderr)}
	if options.debugLevel != nil { result = append(result, optup.DebugLogging(debug.LoggingOptions{LogLevel: options.debugLevel})) }
	if options.message != "" { result = append(result, optup.Message(options.message)) }
	if options.parallel > 0 { result = append(result, optup.Parallel(options.parallel)) }
	if len(options.targets) > 0 { result = append(result, optup.Target(options.targets)) }
	if len(options.replaces) > 0 { result = append(result, optup.Replace(options.replaces)) }
	if len(options.excludes) > 0 { result = append(result, optup.Exclude(options.excludes)) }
	if len(options.policyPacks) > 0 { result = append(result, optup.PolicyPacks(options.policyPacks...)) }
	if len(options.policyPackConfig) > 0 { result = append(result, optup.PolicyPackConfigs(options.policyPackConfig...)) }
	if options.plan != "" { result = append(result, optup.Plan(options.plan)) }
	if options.color != "" { result = append(result, optup.Color(options.color)) }
	if options.diff { result = append(result, optup.Diff()) }
	if options.expectNoChanges { result = append(result, optup.ExpectNoChanges()) }
	if options.targetDependents { result = append(result, optup.TargetDependents()) }
	if options.excludeDependents { result = append(result, optup.ExcludeDependents()) }
	if options.refresh { result = append(result, optup.Refresh()) }
	if options.suppressProgress { result = append(result, optup.SuppressProgress()) }
	if options.suppressOutputs { result = append(result, optup.SuppressOutputs()) }
	if options.continueOnError { result = append(result, optup.ContinueOnError()) }
	if options.configFile != "" { result = append(result, optup.ConfigFile(options.configFile)) }
	return result
}

func pulumiRefreshOptions(options pulumiOptions, stdout, stderr io.Writer) []optrefresh.Option {
	result := []optrefresh.Option{optrefresh.ProgressStreams(stdout), optrefresh.ErrorProgressStreams(stderr)}
	result = append(result, optrefresh.ShowSecrets(false))
	if options.debugLevel != nil { result = append(result, optrefresh.DebugLogging(debug.LoggingOptions{LogLevel: options.debugLevel})) }
	if options.message != "" { result = append(result, optrefresh.Message(options.message)) }
	if options.parallel > 0 { result = append(result, optrefresh.Parallel(options.parallel)) }
	if len(options.targets) > 0 { result = append(result, optrefresh.Target(options.targets)) }
	if len(options.excludes) > 0 { result = append(result, optrefresh.Exclude(options.excludes)) }
	if options.color != "" { result = append(result, optrefresh.Color(options.color)) }
	if options.expectNoChanges { result = append(result, optrefresh.ExpectNoChanges()) }
	if options.diff { result = append(result, optrefresh.Diff()) }
	if options.targetDependents { result = append(result, optrefresh.TargetDependents()) }
	if options.excludeDependents { result = append(result, optrefresh.ExcludeDependents()) }
	if options.suppressProgress { result = append(result, optrefresh.SuppressProgress()) }
	if options.suppressOutputs { result = append(result, optrefresh.SuppressOutputs()) }
	if options.configFile != "" { result = append(result, optrefresh.ConfigFile(options.configFile)) }
	return result
}

func pulumiDestroyOptions(options pulumiOptions, stdout, stderr io.Writer) []optdestroy.Option {
	result := []optdestroy.Option{optdestroy.ProgressStreams(stdout), optdestroy.ErrorProgressStreams(stderr)}
	result = append(result, optdestroy.ShowSecrets(false))
	if options.debugLevel != nil { result = append(result, optdestroy.DebugLogging(debug.LoggingOptions{LogLevel: options.debugLevel})) }
	if options.message != "" { result = append(result, optdestroy.Message(options.message)) }
	if options.parallel > 0 { result = append(result, optdestroy.Parallel(options.parallel)) }
	if len(options.targets) > 0 { result = append(result, optdestroy.Target(options.targets)) }
	if len(options.excludes) > 0 { result = append(result, optdestroy.Exclude(options.excludes)) }
	if options.color != "" { result = append(result, optdestroy.Color(options.color)) }
	if options.targetDependents { result = append(result, optdestroy.TargetDependents()) }
	if options.excludeDependents { result = append(result, optdestroy.ExcludeDependents()) }
	if options.refresh { result = append(result, optdestroy.Refresh()) }
	if options.suppressProgress { result = append(result, optdestroy.SuppressProgress()) }
	if options.suppressOutputs { result = append(result, optdestroy.SuppressOutputs()) }
	if options.continueOnError { result = append(result, optdestroy.ContinueOnError()) }
	if options.diff { result = append(result, optdestroy.Diff()) }
	if options.configFile != "" { result = append(result, optdestroy.ConfigFile(options.configFile)) }
	return result
}

func confirmPulumiOperation(ctx context.Context, operation string) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	file, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	stop := context.AfterFunc(ctx, func() { _ = file.Close() })
	defer stop()
	if _, err := fmt.Fprintf(file, "Preview complete for %s. Type APPLY to continue: ", operation); err != nil {
		return false
	}
	line, err := bufio.NewReaderSize(file, 32).ReadString('\n')
	return err == nil && strings.TrimSuffix(line, "\n") == "APPLY" && ctx.Err() == nil
}

func resolvePulumiWorkdir(workdir string) (string, error) {
	absolute, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("invalid workdir")
	}
	return resolved, nil
}

func readBoundedPulumiFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxPulumiSecretsSize+1))
	if err != nil || len(contents) > maxPulumiSecretsSize {
		return nil, errors.New("invalid input file")
	}
	return contents, nil
}

func pulumiProcessEnvs(env []string) ([]string, []string) {
	sopsEnv := make([]string, 0, len(env))
	childEnv := make([]string, 0, len(env))
	for _, value := range env {
		if strings.HasPrefix(value, "PULUMI_CONFIG_PASSPHRASE=") || strings.HasPrefix(value, "PULUMI_CONFIG_PASSPHRASE_FILE=") {
			continue
		}
		sopsEnv = append(sopsEnv, value)
		if !strings.HasPrefix(value, "SOPS_") {
			childEnv = append(childEnv, value)
		}
	}
	return sopsEnv, childEnv
}

func resolveSOPS(env []string) (string, error) {
	path := ""
	for _, value := range env {
		if strings.HasPrefix(value, "PATH=") {
			path = strings.TrimPrefix(value, "PATH=")
		}
	}
	if path == "" {
		return "", errors.New("sops unavailable")
	}
	for _, directory := range filepath.SplitList(path) {
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, "sops")
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("sops unavailable")
}

func decryptPulumiSecrets(ctx context.Context, sopsPath, workdir, secretsPath string, env []string) ([]byte, error) {
	command := exec.CommandContext(ctx, sopsPath, "--decrypt", secretsPath)
	command.Dir, command.Env, command.Stderr = workdir, env, io.Discard
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	contents, readErr := io.ReadAll(io.LimitReader(output, maxPulumiSecretsSize+1))
	if len(contents) > maxPulumiSecretsSize {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if readErr != nil || len(contents) > maxPulumiSecretsSize || waitErr != nil {
		return nil, errors.New("sops failed")
	}
	return contents, nil
}
