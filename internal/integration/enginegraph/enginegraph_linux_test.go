//go:build linux

package enginegraph_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/program"
	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/backend/diy"
	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	"github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy/deploytest"
	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/pkg/v3/resource/stack"
	"github.com/pulumi/pulumi/pkg/v3/secrets/b64"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/testing/diagtest"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const release = "ghcr.io/example/sub2api-deploy@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const (
	scriptedAlphaFailure = "scripted Host alpha create failure"
	alphaFailureTrace    = "host:alpha:create:fail"
)

type traceFixture struct {
	mu                sync.Mutex
	events            []string
	publicationEvents []string
	hostReadiness     map[string]bool
}

func (f *traceFixture) append(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *traceFixture) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *traceFixture) recordPublication(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	f.publicationEvents = append(f.publicationEvents, event)
}

func (f *traceFixture) publicationSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.publicationEvents...)
}

func TestEngineGraphFailureStopsPublication(t *testing.T) {
	trace, snapshot, err := runEngineGraphUpdate(t, "external-two-host-cloudflare.yaml", "external-two-host-cloudflare-secrets.yaml", nil)
	if err == nil {
		t.Fatal("update unexpectedly succeeded")
	}

	if snapshot == nil {
		t.Fatal("partial checkpoint is nil")
	}
	assertFailureCheckpoint(t, snapshot)
	if !strings.Contains(err.Error(), scriptedAlphaFailure) {
		t.Fatalf("update error = %q, want sanitized scripted alpha failure %q", err, scriptedAlphaFailure)
	}
	if got := trace.snapshot(); len(got) != 1 || got[0] != alphaFailureTrace {
		t.Fatalf("scripted trace = %v, want [%s]; bravo Host and Cloudflare DNS publication must not run after alpha fails", got, alphaFailureTrace)
	}
	if got := trace.publicationSnapshot(); len(got) != 0 {
		t.Fatalf("Cloudflare publication events = %v, want none after alpha fails", got)
	}
}

func TestEngineGraphReadyPublishesAfterOrderedHosts(t *testing.T) {
	trace, _, err := runEngineGraphUpdate(t, "external-two-host-cloudflare.yaml", "external-two-host-cloudflare-secrets.yaml", map[string]bool{
		"alpha": true,
		"bravo": true,
	})
	if err != nil {
		if !strings.Contains(err.Error(), scriptedAlphaFailure) {
			t.Fatalf("ready-success matrix failed before the expected alpha readiness failure: %v", err)
		}
		t.Fatalf("ready-success matrix update = %q, want success after ready alpha and bravo Hosts", err)
	}

	want := []string{
		"host:alpha:create:ok",
		"host:bravo:create:ok",
		"cloudflare:dns:create",
		"cloudflare:dns:create",
	}
	if got := trace.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("ready-success trace = %v, want %v", got, want)
	}
	if got := trace.publicationSnapshot(); len(got) != 2 {
		t.Fatalf("Cloudflare publication events = %v, want two events after both Hosts", got)
	}
}

func runEngineGraphUpdate(t *testing.T, configName, secretsName string, hostReadiness map[string]bool) (*traceFixture, *deploy.Snapshot, error) {
	t.Helper()
	ctx := context.Background()
	configYAML := readFixture(t, configName)
	secretsYAML := readFixture(t, secretsName)
	project := &workspace.Project{
		Name:    tokens.PackageName("sub2api-environment"),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
	}

	stateDir := t.TempDir()
	programRoot := t.TempDir()
	localBackend, err := diy.New(ctx, diagtest.LogSink(t), "file://"+filepath.ToSlash(stateDir), project)
	if err != nil {
		t.Fatalf("create local backend: %v", err)
	}
	ref, err := localBackend.ParseStackReference("canary")
	if err != nil {
		t.Fatalf("parse stack reference: %v", err)
	}
	stackState, err := localBackend.CreateStack(ctx, ref, "", nil, nil)
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}

	trace := &traceFixture{hostReadiness: hostReadiness}
	languageRuntime := newContextAwareLanguageRuntime(func(ctx *pulumi.Context) error {
		return program.Register(ctx, release, configYAML, secretsYAML)
	})
	hostFactory := deploytest.NewPluginHostF(nil, nil, languageRuntime, nil, nil, engineProviderLoaders(trace)...)
	host := hostFactory()
	secretsManager := b64.NewBase64SecretsManager()
	revisionKey, err := secretsManager.Encrypter().EncryptValue(ctx, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatalf("encrypt Host revision key: %v", err)
	}
	updateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, updateErr := localBackend.Update(updateCtx, stackState, backend.UpdateOperation{
		Proj: project,
		M:    &backend.UpdateMetadata{},
		Root: programRoot,
		Opts: backend.UpdateOptions{
			AutoApprove: true,
			SkipPreview: true,
			Display: display.Options{
				Color:            colors.Never,
				Stdout:           io.Discard,
				Stderr:           io.Discard,
				SuppressProgress: true,
			},
			Engine: engineOptions(host),
		},
		SecretsManager:  secretsManager,
		SecretsProvider: stack.Base64SecretsProvider{},
		StackConfiguration: backend.StackConfiguration{
			Config: config.Map{
				config.MustMakeKey("sub2api-host", "revisionKey"): config.NewSecureValue(revisionKey),
			},
			Decrypter: secretsManager.Decrypter(),
		},
		Scopes: backend.CancellationScopes,
	}, nil)
	snapshot, snapshotErr := stackState.Snapshot(ctx, stack.Base64SecretsProvider{})
	if snapshotErr != nil {
		t.Fatalf("load partial checkpoint: %v", snapshotErr)
	}
	return trace, snapshot, updateErr
}

func engineOptions(host plugin.Host) engine.UpdateOptions {
	return engine.UpdateOptions{
		Parallel: 4,
		HostFactory: func(context.Context, diag.Sink, diag.Sink, plugin.DebugContext) (plugin.Host, error) {
			return host, nil
		},
		SkipPluginPreInstall: true,
	}
}

type contextAwareLanguageRuntime struct {
	plugin.LanguageRuntime
	program func(*pulumi.Context) error
}

func newContextAwareLanguageRuntime(program func(*pulumi.Context) error) deploytest.LanguageRuntimeFactory {
	return func() plugin.LanguageRuntime {
		base := deploytest.NewLanguageRuntime(func(_ plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
			return nil
		})
		return &contextAwareLanguageRuntime{LanguageRuntime: base, program: program}
	}
}

func (r *contextAwareLanguageRuntime) Run(ctx context.Context, info plugin.RunInfo) (string, bool, error) {
	pulumiCtx, err := pulumi.NewContext(ctx, pulumi.RunInfo{
		Project:     info.Project,
		Stack:       info.Stack,
		Parallel:    info.Parallel,
		DryRun:      info.DryRun,
		MonitorAddr: info.MonitorAddress,
	})
	if err != nil {
		return "", false, err
	}
	runErr := pulumi.RunWithContext(pulumiCtx, r.program)
	closeErr := pulumiCtx.Close()
	if joinedErr := errors.Join(runErr, closeErr); joinedErr != nil {
		return joinedErr.Error(), false, nil
	}
	return "", false, nil
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return contents
}

func assertFailureCheckpoint(t *testing.T, snapshot *deploy.Snapshot) {
	t.Helper()
	if err := snapshot.VerifyIntegrity(); err != nil {
		t.Fatalf("partial checkpoint is invalid: %v", err)
	}
	stackResources := 0
	for _, resource := range snapshot.Resources {
		if resource.Type == "pulumi:pulumi:Stack" {
			stackResources++
			continue
		}
		if resource.Type == "sub2api-host:index:Host" || resource.Type == "cloudflare:index/dnsRecord:DnsRecord" {
			t.Fatalf("partial checkpoint must not contain Host or DNS resources: %s", resource.URN)
		}
		if !strings.HasPrefix(string(resource.Type), "pulumi:providers:") {
			t.Fatalf("partial checkpoint contains unexpected resource type %q: %s", resource.Type, resource.URN)
		}
	}
	if stackResources != 1 {
		t.Fatalf("partial checkpoint stack resources = %d, want 1", stackResources)
	}
}
