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
	"strings"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
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
	if _, err := parsePulumiPlan(append([]string{"pulumi", plan.environment, plan.operation}, plan.userArgs...)); err != nil {
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
	project, err := workspace.LoadProject(filepath.Join(resolvedWorkdir, "Pulumi.yaml"))
	if err != nil || string(project.Name) != "sub2api-environment" {
		return errInvalidPulumiInputs
	}
	executables, err := resolveAttachedExecutables(cliPath)
	if err != nil {
		return errInvalidPulumiInputs
	}
	err = withStagedStack(ctx, project, filepath.Join(resolvedWorkdir, "Pulumi."+plan.environment+".yaml"), passphrase, values, func(stagedPath string) error {
		return runAttachedIn(ctx, executables, resolvedWorkdir, plan.arguments(stagedPath), childEnv, stdout, stderr, decide)
	})
	if err != nil && errors.Is(err, errInvalidStagedStack) {
		return errInvalidPulumiInputs
	}
	return err
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
