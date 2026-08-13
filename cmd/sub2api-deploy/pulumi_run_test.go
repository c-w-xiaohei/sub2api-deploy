//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/sdk/v3/go/common/encoding"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"gopkg.in/yaml.v3"
)

const (
	pulumiRunPassphrase = stagePassphrase
	pulumiRunRuntime    = "PULUMI_RUNTIME_SECRET_CANARY"
	pulumiRunSOPSKey    = "PULUMI_SOPS_KEY_CANARY"
)

func TestPreparePulumiStackValuesSeparatesPassphraseFromProgramSecrets(t *testing.T) {
	config := []byte("# preserve this configuration exactly\napps: {}\n")
	secrets := pulumiRunSecrets()

	passphrase, values, err := preparePulumiStackValues(config, secrets)
	assertPulumiRunRedacted(t, err, pulumiRunPassphrase, pulumiRunRuntime)
	if err != nil {
		t.Fatal("preparePulumiStackValues unexpectedly failed")
	}
	if passphrase != pulumiRunPassphrase {
		t.Fatalf("passphrase = %q, want canary", passphrase)
	}
	if values.environmentConfig != string(config) {
		t.Fatal("environmentConfig was not preserved byte-for-byte")
	}
	if values.revisionKey != "PULUMI_REVISION_KEY_CANARY" {
		t.Fatalf("revisionKey = %q", values.revisionKey)
	}
	parsed, err := environment.ParseSecrets([]byte(values.environmentSecrets))
	if err != nil {
		t.Fatalf("environmentSecrets does not parse: %v", err)
	}
	if parsed.PulumiPassphrase != "" || parsed.Cloudflare == nil || parsed.Cloudflare.APIToken != pulumiRunRuntime {
		t.Fatal("environmentSecrets did not retain runtime secrets while omitting passphrase")
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(values.environmentSecrets), &document); err != nil {
		t.Fatalf("decode sanitized secrets AST: %v", err)
	}
	if pulumiRunYAMLField(&document, "pulumiPassphrase") {
		t.Fatal("sanitized secrets YAML retained pulumiPassphrase field")
	}
}

func TestPreparePulumiStackValuesRejectsUnsafeSecretsRedacted(t *testing.T) {
	for _, test := range []struct {
		name    string
		secrets []byte
	}{
		{"missing", []byte("revisionKey: PULUMI_REVISION_KEY_CANARY\ncloudflare:\n  apiToken: " + pulumiRunRuntime + "\n")},
		{"empty", []byte("pulumiPassphrase: ''\nrevisionKey: PULUMI_REVISION_KEY_CANARY\n")},
		{"null", []byte("pulumiPassphrase: null\nrevisionKey: PULUMI_REVISION_KEY_CANARY\n")},
		{"non-string", []byte("pulumiPassphrase: 1\nrevisionKey: PULUMI_REVISION_KEY_CANARY\n")},
		{"duplicate", []byte("pulumiPassphrase: " + pulumiRunPassphrase + "\npulumiPassphrase: " + pulumiRunRuntime + "\nrevisionKey: PULUMI_REVISION_KEY_CANARY\n")},
		{"merge", []byte("defaults: &defaults\n  pulumiPassphrase: " + pulumiRunPassphrase + "\n<<: *defaults\nrevisionKey: PULUMI_REVISION_KEY_CANARY\n")},
		{"alias", []byte("passphrase: &passphrase " + pulumiRunPassphrase + "\npulumiPassphrase: *passphrase\nrevisionKey: PULUMI_REVISION_KEY_CANARY\n")},
		{"multiple documents", append(pulumiRunSecrets(), []byte("---\nrevisionKey: PULUMI_RUNTIME_SECRET_CANARY\n")...)},
		{"malformed", []byte("pulumiPassphrase: " + pulumiRunPassphrase + "\n  malformed\n")},
		{"NUL", []byte("pulumiPassphrase: " + pulumiRunPassphrase + "\x00\nrevisionKey: x\n")},
		{"invalid UTF-8", append([]byte("pulumiPassphrase: "), append([]byte{0xff}, []byte("\nrevisionKey: x\n")...)...)},
		{"oversize", []byte("pulumiPassphrase: " + strings.Repeat("x", 4097) + "\nrevisionKey: PULUMI_REVISION_KEY_CANARY\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			passphrase, values, err := preparePulumiStackValues([]byte("apps: {}\n"), test.secrets)
			assertPulumiRunRedacted(t, err, pulumiRunPassphrase, pulumiRunRuntime, "PULUMI_REVISION_KEY_CANARY")
			if !errors.Is(err, errInvalidPulumiInputs) {
				t.Fatal("preparePulumiStackValues returned the wrong error class")
			}
			if passphrase != "" || values != (stackConfigValues{}) {
				t.Fatalf("failure returned values: passphrase present=%t values=%#v", passphrase != "", values)
			}
		})
	}
}

func TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi(t *testing.T) {
	workdir, cli, source, sourceBefore, logs := pulumiRunFixture(t, pulumiRunSecrets(), "success")
	plan, err := parsePulumiPlan([]string{"pulumi", "production", "up", "--yes", "--message=release"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	decided := false
	err = runPulumiPlan(t.Context(), plan, workdir, cli, os.Environ(), &stdout, &stderr, func(context.Context, hostcontract.ApprovalSubject) bool {
		decided = true
		return true
	})
	assertPulumiRunRedacted(t, err, pulumiRunPassphrase, pulumiRunRuntime)
	assertPulumiRunTextRedacted(t, stdout.String(), stderr.String())
	if err != nil {
		t.Fatal("runPulumiPlan unexpectedly failed")
	}
	if decided { // This fake does not request approval, but the callback must be wired by the runner.
		t.Fatal("unexpected fake approval request")
	}
	data := pulumiRunRead(t, logs.pulumi)
	for _, want := range []string{
		"cwd=" + workdir,
		"args=<up><--stack=production><--config-file=", "<--yes><--message=release>",
		"fd3=no", "approval=", "passphrase=absent", "runtime=absent", "sops=absent",
	} {
		if !strings.Contains(data, want) {
			t.Fatalf("Pulumi observation missing %q", want)
		}
	}
	staged := pulumiRunRead(t, logs.staged)
	if bytes.Contains([]byte(staged), []byte(pulumiRunPassphrase)) || bytes.Contains([]byte(staged), []byte(pulumiRunRuntime)) {
		t.Fatal("staged stack leaked a protected value")
	}
	if !strings.Contains(staged, "sub2api-environment:environmentConfig") || !strings.Contains(staged, "secure:") {
		t.Fatal("staged stack did not contain ordinary config and encrypted protected values")
	}
	if got := pulumiRunProgramSecrets(t, staged, sourceBefore.manager); pulumiRunYAMLFieldMustNot(t, got, "pulumiPassphrase") {
		t.Fatal("Program secrets retained pulumiPassphrase")
	}
	assertPulumiRunSourceUnchanged(t, source, sourceBefore)
	stagedParent := strings.TrimSpace(pulumiRunRead(t, logs.stagedParent))
	if _, err := os.Stat(stagedParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged stack parent remains: %v", err)
	}
	if got := pulumiRunRead(t, logs.cleanup); got != "closed\n" {
		t.Fatalf("provider cleanup = %q", got)
	}
}

func TestRunPulumiPlanRejectsInvalidInputsBeforeStartingProcesses(t *testing.T) {
	for _, test := range []struct {
		name       string
		secrets    []byte
		makeSource func(t *testing.T, source string)
	}{
		{"invalid SOPS payload", []byte("pulumiPassphrase: " + pulumiRunPassphrase + "\n malformed\n"), nil},
		{"symlink stack baseline", pulumiRunSecrets(), func(t *testing.T, source string) {
			t.Helper()
			target := source + ".target"
			if err := os.Rename(source, target); err != nil { t.Fatal(err) }
			if err := os.Symlink(target, source); err != nil { t.Fatal(err) }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workdir, cli, source, _, logs := pulumiRunFixture(t, test.secrets, "success")
			if test.makeSource != nil { test.makeSource(t, source) }
			plan, err := parsePulumiPlan([]string{"pulumi", "production", "preview"})
			if err != nil { t.Fatal(err) }
			err = runPulumiPlan(t.Context(), plan, workdir, cli, os.Environ(), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return true })
			if err == nil { t.Fatal("runPulumiPlan unexpectedly succeeded") }
			assertPulumiRunRedacted(t, err, pulumiRunPassphrase, pulumiRunRuntime)
			for _, path := range []string{logs.providerStarted, logs.pulumi} {
				if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) { t.Fatalf("process started after %s: %v", test.name, statErr) }
			}
		})
	}
}

func TestRunPulumiPlanPropagatesCancellationAndCleansUpAfterOperationFailure(t *testing.T) {
	for _, test := range []struct { name, mode string; cancelled bool }{
		{"cancelled", "success", true}, {"operation failure", "failure", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			workdir, cli, _, _, logs := pulumiRunFixture(t, pulumiRunSecrets(), test.mode)
			plan, err := parsePulumiPlan([]string{"pulumi", "production", "preview"})
			if err != nil { t.Fatal(err) }
			ctx := t.Context()
			if test.cancelled { var cancel context.CancelFunc; ctx, cancel = context.WithCancel(ctx); cancel() }
			err = runPulumiPlan(ctx, plan, workdir, cli, os.Environ(), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return true })
			if test.cancelled {
				if !errors.Is(err, context.Canceled) { t.Fatalf("cancelled run error = %v", err) }
				if _, statErr := os.Stat(logs.providerStarted); !errors.Is(statErr, os.ErrNotExist) { t.Fatalf("provider started after cancellation: %v", statErr) }
			} else if err == nil {
				t.Fatal("operation failure was accepted")
			}
			assertPulumiRunRedacted(t, err, pulumiRunPassphrase, pulumiRunRuntime)
			if !test.cancelled && pulumiRunRead(t, logs.cleanup) != "closed\n" { t.Fatal("provider was not cleaned after operation failure") }
			if !test.cancelled {
				stagedParent := strings.TrimSpace(pulumiRunRead(t, logs.stagedParent))
				if _, statErr := os.Stat(stagedParent); !errors.Is(statErr, os.ErrNotExist) { t.Fatalf("staged parent remains: %v", statErr) }
			}
		})
	}
}

func pulumiRunSecrets() []byte {
	return []byte("pulumiPassphrase: " + pulumiRunPassphrase + "\nrevisionKey: PULUMI_REVISION_KEY_CANARY\ncloudflare:\n  apiToken: " + pulumiRunRuntime + "\n")
}

func pulumiRunYAMLField(node *yaml.Node, name string) bool {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 { if node.Content[i].Value == name || pulumiRunYAMLField(node.Content[i+1], name) { return true } }
		return false
	}
	for _, child := range node.Content { if pulumiRunYAMLField(child, name) { return true } }
	return false
}

func pulumiRunYAMLFieldMustNot(t *testing.T, contents, field string) bool {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &document); err != nil { t.Fatalf("invalid captured program secrets: %v", err) }
	return pulumiRunYAMLField(&document, field)
}

func assertPulumiRunRedacted(t *testing.T, err error, canaries ...string) {
	t.Helper()
	if err == nil { return }
	for _, canary := range canaries { if strings.Contains(err.Error(), canary) { t.Fatalf("error exposed secret") } }
}

func assertPulumiRunTextRedacted(t *testing.T, values ...string) {
	t.Helper()
	for _, value := range values {
		for _, canary := range []string{pulumiRunPassphrase, pulumiRunRuntime, pulumiRunSOPSKey, "PULUMI_REVISION_KEY_CANARY"} {
			if strings.Contains(value, canary) { t.Fatal("output exposed secret") }
		}
	}
}

type pulumiRunLogs struct { pulumi, staged, cleanup, providerStarted, stagedParent string }

type pulumiRunSource struct {
	bytes   []byte
	info    os.FileInfo
	manager secrets.Manager
}

func pulumiRunFixture(t *testing.T, sopsOutput []byte, pulumiMode string) (string, string, string, pulumiRunSource, pulumiRunLogs) {
	t.Helper()
	workdir, bin, logsDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(workdir, "environments", "production"), 0o700); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(workdir, "environments", "production", "config.yaml"), []byte("apps: {}\n"), 0o600); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(workdir, "environments", "production", "secrets.yaml"), []byte("encrypted placeholder\n"), 0o600); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(workdir, "Pulumi.yaml"), []byte("name: sub2api-environment\nruntime: go\n"), 0o600); err != nil { t.Fatal(err) }
	_, sourceBytes, manager, _ := stagedStackFixture(t)
	source := filepath.Join(workdir, "Pulumi.production.yaml")
	if err := os.WriteFile(source, sourceBytes, 0o600); err != nil { t.Fatal(err) }
	beforeInfo, err := os.Lstat(source); if err != nil { t.Fatal(err) }
	before := pulumiRunSource{bytes: append([]byte(nil), sourceBytes...), info: beforeInfo, manager: manager}
	logs := pulumiRunLogs{pulumi: filepath.Join(logsDir, "pulumi"), staged: filepath.Join(logsDir, "staged"), cleanup: filepath.Join(logsDir, "cleanup"), providerStarted: filepath.Join(logsDir, "provider-started"), stagedParent: filepath.Join(logsDir, "staged-parent")}
	writeAttachedExecutable(t, filepath.Join(bin, "sub2api-deploy"), "#!/bin/sh\nexit 0\n")
	writeAttachedExecutable(t, filepath.Join(bin, "sops"), "#!/bin/sh\n[ \"$SOPS_AGE_KEY\" = '"+pulumiRunSOPSKey+"' ] || exit 24\nprintf '%s' '"+strings.ReplaceAll(string(sopsOutput), "'", "'\\''")+"'\n")
	writeAttachedExecutable(t, filepath.Join(bin, "pulumi-resource-sub2api-host"), "#!/bin/sh\nif env | grep -q '"+pulumiRunSOPSKey+"'; then exit 25; fi\nprintf x > '"+logs.providerStarted+"'\nprintf '%s\\n' 43123\ncat <&3 >/dev/null\nprintf '%s\\n' closed > '"+logs.cleanup+"'\n")
	writeAttachedExecutable(t, filepath.Join(bin, "pulumi"), "#!/bin/sh\nprintf 'cwd=%s\\nargs=' \"$PWD\" > '"+logs.pulumi+"'\nfor arg; do printf '<%s>' \"$arg\" >> '"+logs.pulumi+"'; case \"$arg\" in --config-file=*) config=${arg#--config-file=};; esac; done\nprintf '\\nfd3=' >> '"+logs.pulumi+"'; if [ -e /proc/self/fd/3 ]; then printf yes >> '"+logs.pulumi+"'; else printf no >> '"+logs.pulumi+"'; fi\nprintf '\\napproval=%s\\n' \"$SUB2API_HOST_APPROVAL_FD\" >> '"+logs.pulumi+"'\nprintf 'passphrase=' >> '"+logs.pulumi+"'; if env | grep -q '"+pulumiRunPassphrase+"'; then printf leaked >> '"+logs.pulumi+"'; else printf absent >> '"+logs.pulumi+"'; fi\nprintf '\\nruntime=' >> '"+logs.pulumi+"'; if env | grep -q '"+pulumiRunRuntime+"'; then printf leaked >> '"+logs.pulumi+"'; else printf absent >> '"+logs.pulumi+"'; fi\nprintf '\\nsops=' >> '"+logs.pulumi+"'; if env | grep -q '"+pulumiRunSOPSKey+"'; then printf leaked >> '"+logs.pulumi+"'; else printf absent >> '"+logs.pulumi+"'; fi\ncat \"$config\" > '"+logs.staged+"'\ndirname \"$config\" > '"+logs.stagedParent+"'\nif [ '"+pulumiMode+"' = failure ]; then exit 23; fi\nexit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOPS_AGE_KEY", pulumiRunSOPSKey)
	return workdir, filepath.Join(bin, "sub2api-deploy"), source, before, logs
}

func pulumiRunRead(t *testing.T, path string) string { t.Helper(); data, err := os.ReadFile(path); if err != nil { t.Fatal(err) }; return string(data) }

func assertPulumiRunSourceUnchanged(t *testing.T, path string, before pulumiRunSource) {
	t.Helper()
	info, err := os.Lstat(path); if err != nil { t.Fatal(err) }
	after, err := os.ReadFile(path); if err != nil { t.Fatal(err) }
	if info.Mode().Perm() != 0o600 || !os.SameFile(before.info, info) || !bytes.Equal(after, before.bytes) { t.Fatal("source stack identity, bytes, or mode changed") }
}

func pulumiRunProgramSecrets(t *testing.T, staged string, manager secrets.Manager) string {
	t.Helper()
	project := &workspace.Project{Name: "sub2api-environment", Runtime: workspace.NewProjectRuntimeInfo("go", nil)}
	stack, err := workspace.LoadProjectStackBytes(nil, project, []byte(staged), "captured-staged.yaml", encoding.YAML)
	if err != nil { t.Fatalf("load captured staged stack: %v", err) }
	value, ok := stack.Config[config.MustMakeKey("sub2api-environment", "environmentSecrets")]
	if !ok { t.Fatal("captured staged stack has no environmentSecrets") }
	plain, err := value.Value(manager.Decrypter())
	if err != nil { t.Fatalf("decrypt captured Program secrets: %v", err) }
	return plain
}
