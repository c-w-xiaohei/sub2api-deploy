//go:build linux && sub2api_ci

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"gopkg.in/yaml.v3"
)

func TestRunPulumiStackUsesAutomationLifecycle(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "Pulumi.yaml"), []byte("name: sub2api-environment\nruntime: go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "Pulumi.production.yaml"), []byte("config: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &recordingPulumiCommand{}
	workspace, err := auto.NewLocalWorkspace(t.Context(), auto.WorkDir(directory), auto.Pulumi(command))
	if err != nil {
		t.Fatal(err)
	}

	plan := pulumiPlan{operation: "up", environment: "production", options: pulumiOptions{approve: true}}
	if err := runPulumiStack(t.Context(), workspace, plan, "", io.Discard, io.Discard); err != nil {
		t.Fatalf("runPulumiStack() error = %v", err)
	}

	if len(command.calls) != 5 || command.calls[0][0] != "stack" || command.calls[0][1] != "select" {
		t.Fatalf("Automation API did not select the requested stack: %#v", command.calls)
	}
	if command.calls[1][0] != "up" || !hasCallArgument(command.calls[1], "--yes") || !hasCallArgument(command.calls[1], "--skip-preview") || !hasCallArgument(command.calls[1], "--exec-kind=auto.local") {
		t.Fatalf("Automation API did not perform an approved update: %#v", command.calls[1])
	}
	if command.calls[2][0] != "stack" || command.calls[2][1] != "output" || command.calls[3][0] != "stack" || command.calls[3][1] != "output" || command.calls[4][0] != "stack" || command.calls[4][1] != "history" {
		t.Fatalf("Automation API did not complete the update lifecycle: %#v", command.calls)
	}
}

func TestStagedProjectYAMLUpdatesOnlyTheRuntimeBinaryNode(t *testing.T) {
	project := []byte("# project comment\nname: sub2api-environment\nruntime:\n  name: go\n  options:\n    binary: ./bin/pulumi-program\n    note: 'binary: ./bin/pulumi-program'\n")
	updated, err := stagedProjectYAML(project, "/private/workspace")
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(updated, &document); err != nil {
		t.Fatal(err)
	}
	runtime := yamlMappingValue(document.Content[0], "runtime")
	options := yamlMappingValue(runtime, "options")
	if binary := yamlMappingValue(options, "binary"); binary == nil || binary.Value != "/private/workspace/bin/pulumi-program" {
		t.Fatalf("runtime binary = %#v", binary)
	}
	if note := yamlMappingValue(options, "note"); note == nil || note.Value != "binary: ./bin/pulumi-program" {
		t.Fatalf("unrelated note changed = %#v", note)
	}
}

func hasCallArgument(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

type recordingPulumiCommand struct {
	calls [][]string
}

func (c *recordingPulumiCommand) Version() semver.Version { return semver.MustParse("3.256.0") }

func (c *recordingPulumiCommand) Run(_ context.Context, _ string, _ io.Reader, _, _ []io.Writer, _ []string, args ...string) (string, string, int, error) {
	c.calls = append(c.calls, append([]string(nil), args...))
	switch {
	case len(args) > 1 && args[0] == "stack" && args[1] == "output":
		return "{}", "", 0, nil
	case len(args) > 1 && args[0] == "stack" && args[1] == "history":
		return "[]", "", 0, nil
	default:
		return "", "", 0, nil
	}
}
