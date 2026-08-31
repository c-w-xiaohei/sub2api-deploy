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
	"github.com/pulumi/pulumi/pkg/v3/secrets"
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

func TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication(t *testing.T) {
	harness := newEngineGraphHarness(t, map[string]bool{
		"alpha": true,
		"bravo": true,
	})

	first, err := harness.update(t, "external-two-host-cloudflare.yaml", "external-two-host-cloudflare-secrets.yaml")
	if err != nil {
		t.Fatalf("ready fixture update = %q, want success before maintenance transition", err)
	}
	assertReadyCheckpoint(t, first)
	if got := harness.trace.snapshot(); !slices.Equal(got, []string{
		"host:alpha:create:ok",
		"host:bravo:create:ok",
		"cloudflare:dns:create",
		"cloudflare:dns:create",
	}) {
		t.Fatalf("ready fixture trace = %v, want two Hosts followed by two DNS publications", got)
	}

	final, err := harness.update(t, "external-two-host-maintenance.yaml", "external-two-host-maintenance-secrets.yaml")
	if err != nil {
		t.Fatalf("maintenance update = %q, want success with configured Hosts retained", err)
	}
	assertMaintenanceCheckpoint(t, final)
	if got := harness.trace.publicationSnapshot(); len(got) != 2 {
		t.Fatalf("Cloudflare publication events = %v, want no new publication during maintenance", got)
	}

	// This is intentionally RED until the test provider exposes its Update lifecycle callback.
	// The engine has reached the real Host update steps if the only missing evidence is these entries.
	got := harness.trace.snapshot()
	for _, serverKey := range []string{"alpha", "bravo"} {
		want := "host:" + serverKey + ":update:ok"
		if countEvent(got, want) != 1 {
			t.Fatalf("maintenance Host lifecycle trace = %v, want one %q; current test provider does not expose Update", got, want)
		}
	}
}

func runEngineGraphUpdate(t *testing.T, configName, secretsName string, hostReadiness map[string]bool) (*traceFixture, *deploy.Snapshot, error) {
	t.Helper()
	harness := newEngineGraphHarness(t, hostReadiness)
	snapshot, updateErr := harness.update(t, configName, secretsName)
	return harness.trace, snapshot, updateErr
}

type engineGraphHarness struct {
	backend       backend.Backend
	stack         backend.Stack
	project       *workspace.Project
	programRoot   string
	trace          *traceFixture
	secretsManager secrets.Manager
	revisionKey    string
}

func newEngineGraphHarness(t *testing.T, hostReadiness map[string]bool) *engineGraphHarness {
	t.Helper()
	ctx := context.Background()
	project := &workspace.Project{
		Name:    tokens.PackageName("sub2api-environment"),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
	}
	localBackend, err := diy.New(ctx, diagtest.LogSink(t), "file://"+filepath.ToSlash(t.TempDir()), project)
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

	secretsManager := b64.NewBase64SecretsManager()
	revisionKey, err := secretsManager.Encrypter().EncryptValue(ctx, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatalf("encrypt Host revision key: %v", err)
	}
	return &engineGraphHarness{
		backend:        localBackend,
		stack:          stackState,
		project:        project,
		programRoot:    t.TempDir(),
		trace:          &traceFixture{hostReadiness: hostReadiness},
		secretsManager: secretsManager,
		revisionKey:   revisionKey,
	}
}

func (h *engineGraphHarness) update(t *testing.T, configName, secretsName string) (*deploy.Snapshot, error) {
	t.Helper()
	ctx := context.Background()
	configYAML := readFixture(t, configName)
	secretsYAML := readFixture(t, secretsName)
	languageRuntime := newContextAwareLanguageRuntime(func(ctx *pulumi.Context) error {
		return program.Register(ctx, release, configYAML, secretsYAML)
	})
	hostFactory := deploytest.NewPluginHostF(nil, nil, languageRuntime, nil, nil, engineProviderLoaders(h.trace)...)
	updateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, updateErr := h.backend.Update(updateCtx, h.stack, backend.UpdateOperation{
		Proj: h.project,
		M:    &backend.UpdateMetadata{},
		Root: h.programRoot,
		Opts: backend.UpdateOptions{
			AutoApprove: true,
			SkipPreview: true,
			Display: display.Options{
				Color:            colors.Never,
				Stdout:           io.Discard,
				Stderr:           io.Discard,
				SuppressProgress: true,
			},
			Engine: engineOptions(hostFactory),
		},
		SecretsManager:  h.secretsManager,
		SecretsProvider: stack.Base64SecretsProvider{},
		StackConfiguration: backend.StackConfiguration{
			Config: config.Map{
				config.MustMakeKey("sub2api-host", "revisionKey"): config.NewSecureValue(h.revisionKey),
			},
			Decrypter: h.secretsManager.Decrypter(),
		},
		Scopes: backend.CancellationScopes,
	}, nil)
	snapshot, snapshotErr := h.stack.Snapshot(ctx, stack.Base64SecretsProvider{})
	if snapshotErr != nil {
		t.Fatalf("load partial checkpoint: %v", snapshotErr)
	}
	return snapshot, updateErr
}

func engineOptions(hostFactory deploytest.PluginHostFactory) engine.UpdateOptions {
	return engine.UpdateOptions{
		Parallel: 4,
		HostFactory: func(context.Context, diag.Sink, diag.Sink, plugin.DebugContext) (plugin.Host, error) {
			return hostFactory(), nil
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

func assertReadyCheckpoint(t *testing.T, snapshot *deploy.Snapshot) {
	t.Helper()
	if err := snapshot.VerifyIntegrity(); err != nil {
		t.Fatalf("ready checkpoint is invalid: %v", err)
	}
	stackResources := 0
	hosts := map[string]bool{}
	dnsRecords := 0
	for _, resource := range snapshot.Resources {
		switch resource.Type {
		case "pulumi:pulumi:Stack":
			stackResources++
		case "sub2api-host:index:Host":
			hosts[string(resource.ID)] = true
		case "cloudflare:index/dnsRecord:DnsRecord":
			dnsRecords++
		}
	}
	if stackResources != 1 || len(hosts) != 2 || !hosts["host-alpha"] || !hosts["host-bravo"] || dnsRecords != 2 {
		t.Fatalf("ready checkpoint resources = stacks:%d hosts:%d DNS:%d, want one stack, both Hosts, and two DNS records", stackResources, len(hosts), dnsRecords)
	}
}

func assertMaintenanceCheckpoint(t *testing.T, snapshot *deploy.Snapshot) {
	t.Helper()
	if err := snapshot.VerifyIntegrity(); err != nil {
		t.Fatalf("maintenance checkpoint is invalid: %v", err)
	}
	stackResources := 0
	hostResources := 0
	hosts := map[string]bool{}
	dnsRecords := 0
	for _, resource := range snapshot.Resources {
		switch resource.Type {
		case "pulumi:pulumi:Stack":
			stackResources++
		case "sub2api-host:index:Host":
			hostResources++
			name := resource.URN.Name()
			if name != "host-alpha" && name != "host-bravo" {
				t.Fatalf("maintenance checkpoint contains Host with unstable identity: %s", resource.URN)
			}
			if string(resource.ID) != name {
				t.Fatalf("maintenance checkpoint Host %s has ID %q, want %q", resource.URN, resource.ID, name)
			}
			target, ok := resource.Inputs["target"]
			if !ok || !target.IsObject() {
				t.Fatalf("maintenance checkpoint Host %s has no target object", resource.URN)
			}
			apps, ok := target.ObjectValue()["apps"]
			if !ok || !apps.IsArray() || len(apps.ArrayValue()) != 0 {
				t.Fatalf("maintenance checkpoint Host %s still projects App runtime: %v", resource.URN, target)
			}
			hosts[string(resource.ID)] = true
		case "cloudflare:index/dnsRecord:DnsRecord":
			dnsRecords++
		}
	}
	if stackResources != 1 || hostResources != 2 || len(hosts) != 2 || !hosts["host-alpha"] || !hosts["host-bravo"] || dnsRecords != 0 {
		t.Fatalf("maintenance checkpoint resources = stacks:%d hosts:%d DNS:%d, want one stack, both Hosts, and no DNS records", stackResources, hostResources, dnsRecords)
	}
}

func countEvent(events []string, want string) int {
	count := 0
	for _, event := range events {
		if event == want {
			count++
		}
	}
	return count
}
