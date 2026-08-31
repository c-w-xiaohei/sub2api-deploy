//go:build linux

package enginegraph_test

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
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
	managedUpstashConfig = "managed-upstash-preview.yaml"
	managedUpstashSecrets = "managed-upstash-preview-secrets.yaml"
)

type traceFixture struct {
	mu                sync.Mutex
	events            []string
	publicationEvents []string
	hostChecks        []hostCheckObservation
	hostReadiness     map[string]bool
}

type hostCheckObservation struct {
	URN           resource.URN
	Type          tokens.Type
	Name          string
	AllowUnknowns bool
	News          resource.PropertyMap
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

func (f *traceFixture) recordHostCheck(req plugin.CheckRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostChecks = append(f.hostChecks, hostCheckObservation{
		URN:           req.URN,
		Type:          req.Type,
		Name:          req.Name,
		AllowUnknowns: req.AllowUnknowns,
		News:          clonePropertyMap(req.News),
	})
}

func (f *traceFixture) hostCheckSnapshot() []hostCheckObservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	observations := make([]hostCheckObservation, len(f.hostChecks))
	for index, observation := range f.hostChecks {
		observations[index] = observation
		observations[index].News = clonePropertyMap(observation.News)
	}
	return observations
}

func clonePropertyMap(values resource.PropertyMap) resource.PropertyMap {
	clone := make(resource.PropertyMap, len(values))
	for key, value := range values {
		clone[key] = clonePropertyValue(value)
	}
	return clone
}

func clonePropertyValue(value resource.PropertyValue) resource.PropertyValue {
	switch typed := value.V.(type) {
	case resource.PropertyMap:
		return resource.NewObjectProperty(clonePropertyMap(typed))
	case []resource.PropertyValue:
		clone := make([]resource.PropertyValue, len(typed))
		for index, child := range typed {
			clone[index] = clonePropertyValue(child)
		}
		return resource.NewArrayProperty(clone)
	case resource.Computed:
		return resource.MakeComputed(clonePropertyValue(typed.Element))
	case resource.Output:
		return resource.NewOutputProperty(resource.Output{
			Element:      clonePropertyValue(typed.Element),
			Known:        typed.Known,
			Secret:       typed.Secret,
			Dependencies: append([]resource.URN(nil), typed.Dependencies...),
		})
	case *resource.Secret:
		if typed == nil {
			return value
		}
		return resource.MakeSecret(clonePropertyValue(typed.Element))
	case resource.ResourceReference:
		typed.ID = clonePropertyValue(typed.ID)
		return resource.NewResourceReferenceProperty(typed)
	default:
		return value
	}
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

func TestEngineManagedUpstashPreviewPreservesComputedSecretProjection(t *testing.T) {
	// Re-anchors TR-SEC-01..05 and TR-PROG-03/04 at the real Engine preview boundary.
	harness := newEngineGraphHarness(t, map[string]bool{"alpha": true})
	before, after, err := harness.preview(t, managedUpstashConfig, managedUpstashSecrets)
	if err != nil {
		t.Fatalf("managed Upstash preview failed: %v", err)
	}
	if !snapshotsEqual(before, after) {
		t.Fatal("managed Upstash preview mutated the checkpoint")
	}

	checks := harness.trace.hostCheckSnapshot()
	if len(checks) == 0 {
		t.Fatal("invalid RED path: preview produced no Host Check observation")
	}
	var host hostCheckObservation
	for _, check := range checks {
		if check.Type == hostProviderType && check.Name == "host-alpha" {
			host = check
			break
		}
	}
	if host.Name == "" {
		t.Fatal("invalid RED path: preview did not Check Host alpha")
	}

	for _, field := range []string{"providerId", "endpoint", "port"} {
		value, ok := propertyAt(host.News, "target", "apps", "0", "dataLinks", "1", "identity", field)
		if !ok || !value.ContainsUnknowns() {
			t.Fatalf("managed Redis %s projection lacks unknown/computed semantics", field)
		}
	}

	for _, path := range [][]string{
		{"target", "releaseArtifact"},
		{"target", "apps", "0", "id"},
		{"target", "apps", "0", "image"},
		{"target", "apps", "0", "hostname"},
		{"target", "apps", "0", "dataLinks", "0", "identity", "endpoint"},
	} {
		value, ok := propertyAt(host.News, path...)
		if !ok || !value.IsString() || value.ContainsUnknowns() || value.ContainsSecrets() {
			t.Fatalf("ordinary Host field %s is not known and non-secret", strings.Join(path, "."))
		}
	}

	secretProjection, ok := host.News["secrets"]
	if !ok || !secretProjection.ContainsSecrets() {
		t.Fatal("Host secrets projection lost its secret semantic class")
	}
	redisPassword, ok := propertyAt(host.News, "secrets", "apps", "app", "redis", "password")
	if !ok || !redisPassword.ContainsUnknowns() || !redisPassword.ContainsSecrets() {
		t.Fatal("generated Redis password lacks unknown+secret semantic classes")
	}
	if containsString(resource.NewObjectProperty(host.News), "upstash-api-key-canary") {
		t.Fatal("Upstash API key canary reached Host Check input")
	}

	if got := harness.trace.snapshot(); len(got) != 0 {
		t.Fatalf("preview lifecycle events = %v, want no create/update/delete events", got)
	}
	if got := harness.trace.publicationSnapshot(); len(got) != 0 {
		t.Fatalf("preview publication events = %v, want none", got)
	}
}

func TestEngineGraphReadyPublishesAfterOrderedHosts(t *testing.T) {
	trace, _, err := runEngineGraphUpdate(t, "external-two-host-cloudflare.yaml", "external-two-host-cloudflare-secrets.yaml", map[string]bool{
		"alpha": true,
		"bravo": true,
	})
	if err != nil {
		t.Fatalf("ready-success matrix update failed unexpectedly: %v", err)
	}

	got := trace.snapshot()
	alphaReady := "host:alpha:create:ok"
	bravoReady := "host:bravo:create:ok"
	alphaDNS := "cloudflare:dns:dns-app-alpha-A:create:ok"
	bravoDNS := "cloudflare:dns:dns-app-bravo-A:create:ok"
	assertEventCount(t, got, alphaReady, 1)
	assertEventCount(t, got, bravoReady, 1)
	assertEventCount(t, got, alphaDNS, 1)
	assertEventCount(t, got, bravoDNS, 1)
	if len(got) != 4 {
		t.Fatalf("ready-success trace = %v, want exactly two Host successes and two DNS creates", got)
	}
	assertEventAfter(t, got, bravoReady, alphaReady)
	for _, dnsEvent := range []string{alphaDNS, bravoDNS} {
		assertEventAfter(t, got, dnsEvent, alphaReady)
		assertEventAfter(t, got, dnsEvent, bravoReady)
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
	got := harness.trace.snapshot()
	alphaReady := "host:alpha:create:ok"
	bravoReady := "host:bravo:create:ok"
	alphaDNS := "cloudflare:dns:dns-app-alpha-A:create:ok"
	bravoDNS := "cloudflare:dns:dns-app-bravo-A:create:ok"
	assertEventCount(t, got, alphaReady, 1)
	assertEventCount(t, got, bravoReady, 1)
	assertEventCount(t, got, alphaDNS, 1)
	assertEventCount(t, got, bravoDNS, 1)
	if len(got) != 4 {
		t.Fatalf("ready fixture trace = %v, want exactly two Host successes and two DNS creates", got)
	}
	assertEventAfter(t, got, bravoReady, alphaReady)
	for _, dnsEvent := range []string{alphaDNS, bravoDNS} {
		assertEventAfter(t, got, dnsEvent, alphaReady)
		assertEventAfter(t, got, dnsEvent, bravoReady)
	}

	final, err := harness.update(t, "external-two-host-maintenance.yaml", "external-two-host-maintenance-secrets.yaml")
	if err != nil {
		t.Fatalf("maintenance update = %q, want success with configured Hosts retained", err)
	}
	assertMaintenanceCheckpoint(t, final)
	if got := harness.trace.publicationSnapshot(); len(got) != 2 {
		t.Fatalf("Cloudflare publication events = %v, want no new publication during maintenance", got)
	}

	got = harness.trace.snapshot()
	for _, serverKey := range []string{"alpha", "bravo"} {
		want := "host:" + serverKey + ":update:ok"
		if countEvent(got, want) != 1 {
			t.Fatalf("maintenance Host lifecycle trace = %v, want one %q", got, want)
		}
	}
}

func TestEngineConfiguredServerCountOneTwo(t *testing.T) {
	harness := newEngineGraphHarness(t, map[string]bool{
		"alpha": true,
		"bravo": true,
	})

	ready, err := harness.update(t, "external-two-host-cloudflare.yaml", "external-two-host-cloudflare-secrets.yaml")
	if err != nil {
		t.Fatalf("ready fixture update = %q, want success before server-count boundary", err)
	}
	assertConfiguredHostCheckpoint(t, ready, []string{"alpha", "bravo"}, []string{"dns-app-alpha-A", "dns-app-bravo-A"})

	one, err := harness.update(t, "external-one-host-empty.yaml", "external-two-host-maintenance-secrets.yaml")
	if err != nil {
		t.Fatalf("one-server fixture update = %q, want success after bravo removal", err)
	}
	assertConfiguredHostCheckpoint(t, one, []string{"alpha"}, nil)
	if got := harness.trace.publicationSnapshot(); !slices.Equal(got, []string{"cloudflare:dns:create", "cloudflare:dns:create"}) {
		t.Fatalf("one-server publication trace = %v, want no publication after DNS removal", got)
	}
	events := harness.trace.snapshot()
	assertHostDeletionTrace(t, events, []string{"host:bravo:delete:ok"})
	assertDNSDeletionOrdering(t, events, []string{
		"cloudflare:dns:dns-app-bravo-A:delete:ok",
	}, "host:bravo:delete:ok")
}

func TestEngineConfiguredServerCountZero(t *testing.T) {
	trace, snapshot, err := runEngineGraphUpdate(t, "external-zero-server.yaml", "external-zero-server-secrets.yaml", nil)
	if err != nil {
		if !strings.Contains(err.Error(), "servers, postgres, redis, and apps are required") {
			t.Fatalf("zero-server update error = %q, want the known environment.Validate empty-server error", err)
		}
		t.Fatalf("zero-server update = %q, want success with an empty configured-server graph", err)
	}

	assertConfiguredHostCheckpoint(t, snapshot, nil, nil)
	if got := trace.snapshot(); len(got) != 0 {
		t.Fatalf("zero-server lifecycle trace = %v, want none", got)
	}
	if got := trace.publicationSnapshot(); len(got) != 0 {
		t.Fatalf("zero-server publication trace = %v, want none", got)
	}
}

func TestEngineAppPlacementOneReadyFailure(t *testing.T) {
	t.Run("alpha failure blocks publication", func(t *testing.T) {
		trace, snapshot, err := runEngineGraphUpdate(t, "external-two-host-cloudflare-app-alpha.yaml", "external-two-host-cloudflare-secrets.yaml", map[string]bool{
			"alpha": false,
			"bravo": true,
		})
		if err == nil {
			t.Fatal("placement-one failure update unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), scriptedAlphaFailure) {
			t.Fatalf("placement-one update error = %q, want scripted alpha failure %q", err, scriptedAlphaFailure)
		}
		assertPlacementFailureCheckpoint(t, snapshot)
		got := trace.snapshot()
		assertEventCount(t, got, alphaFailureTrace, 1)
		assertEventCount(t, got, "host:bravo:create:ok", 1)
		if len(got) != 2 {
			t.Fatalf("placement-one failure trace = %v, want only alpha failure and bravo Host success", got)
		}
		if got := trace.publicationSnapshot(); len(got) != 0 {
			t.Fatalf("placement-one failure publication events = %v, want none", got)
		}
	})

	t.Run("alpha ready publishes once", func(t *testing.T) {
		trace, snapshot, err := runEngineGraphUpdate(t, "external-two-host-cloudflare-app-alpha.yaml", "external-two-host-cloudflare-secrets.yaml", map[string]bool{
			"alpha": true,
			"bravo": true,
		})
		if err != nil {
			t.Fatalf("placement-one ready update failed unexpectedly: %v", err)
		}
		assertAppPlacementCheckpoint(t, snapshot)

		got := trace.snapshot()
		alphaReady := "host:alpha:create:ok"
		bravoReady := "host:bravo:create:ok"
		alphaDNS := "cloudflare:dns:dns-app-alpha-A:create:ok"
		assertEventCount(t, got, alphaReady, 1)
		assertEventCount(t, got, bravoReady, 1)
		assertEventCount(t, got, alphaDNS, 1)
		if len(got) != 3 {
			t.Fatalf("placement-one ready lifecycle trace = %v, want exactly two Host successes and one Alpha DNS create", got)
		}
		assertEventAfter(t, got, alphaDNS, alphaReady)
	})
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
	currentStack, getStackErr := h.backend.GetStack(ctx, h.stack.Ref())
	if getStackErr != nil {
		t.Fatalf("load current checkpoint stack: %v", getStackErr)
	}
	if currentStack == nil {
		t.Fatalf("load current checkpoint stack: backend returned nil stack")
	}
	snapshot, snapshotErr := currentStack.Snapshot(ctx, stack.Base64SecretsProvider{})
	if snapshotErr != nil {
		t.Fatalf("load partial checkpoint: %v", snapshotErr)
	}
	return snapshot, updateErr
}

func (h *engineGraphHarness) preview(t *testing.T, configName, secretsName string) (*deploy.Snapshot, *deploy.Snapshot, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	before, err := h.reloadSnapshot(ctx)
	if err != nil {
		t.Fatalf("load checkpoint before preview: %v", err)
	}
	configYAML := readFixture(t, configName)
	secretsYAML := readFixture(t, secretsName)
	languageRuntime := newContextAwareLanguageRuntime(func(ctx *pulumi.Context) error {
		return program.Register(ctx, release, configYAML, secretsYAML)
	})
	hostFactory := deploytest.NewPluginHostF(nil, nil, languageRuntime, nil, nil, engineProviderLoaders(h.trace)...)
	engineEvents := make(chan engine.Event)
	eventsDone := make(chan struct{})
	go func() {
		for range engineEvents {
		}
		close(eventsDone)
	}()

	_, _, previewErr := backend.PreviewStack(ctx, h.stack, backend.UpdateOperation{
		Proj: h.project,
		M:    &backend.UpdateMetadata{},
		Root: h.programRoot,
		Opts: backend.UpdateOptions{
			AutoApprove: true,
			PreviewOnly: true,
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
	}, engineEvents)
	<-eventsDone

	after, err := h.reloadSnapshot(ctx)
	if err != nil {
		t.Fatalf("load checkpoint after preview: %v", err)
	}
	return before, after, previewErr
}

func (h *engineGraphHarness) reloadSnapshot(ctx context.Context) (*deploy.Snapshot, error) {
	currentStack, err := h.backend.GetStack(ctx, h.stack.Ref())
	if err != nil {
		return nil, err
	}
	if currentStack == nil {
		return nil, errors.New("backend returned nil stack")
	}
	return currentStack.Snapshot(ctx, stack.Base64SecretsProvider{})
}

func snapshotsEqual(before, after *deploy.Snapshot) bool {
	if before == nil || after == nil {
		return before == after
	}
	return reflect.DeepEqual(before.Manifest, after.Manifest) &&
		reflect.DeepEqual(before.Resources, after.Resources) &&
		reflect.DeepEqual(before.PendingOperations, after.PendingOperations) &&
		reflect.DeepEqual(before.Metadata, after.Metadata) &&
		reflect.DeepEqual(before.Snippets, after.Snippets) &&
		reflect.DeepEqual(before.Extensions, after.Extensions)
}

func propertyAt(root resource.PropertyMap, path ...string) (resource.PropertyValue, bool) {
	value := resource.NewObjectProperty(root)
	inheritedUnknown := false
	inheritedSecret := false
	for _, part := range path {
		for value.IsSecret() || value.IsComputed() || value.IsOutput() {
			switch {
			case value.IsSecret():
				inheritedSecret = true
				value = value.SecretValue().Element
			case value.IsComputed():
				inheritedUnknown = true
				value = value.Input().Element
			case value.IsOutput():
				inheritedUnknown = inheritedUnknown || !value.OutputValue().Known
				inheritedSecret = inheritedSecret || value.OutputValue().Secret
				value = value.OutputValue().Element
			}
		}
		if value.IsObject() {
			child, ok := value.ObjectValue()[resource.PropertyKey(part)]
			if !ok {
				return resource.PropertyValue{}, false
			}
			value = child
			continue
		}
		if value.IsArray() {
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(value.ArrayValue()) {
				return resource.PropertyValue{}, false
			}
			value = value.ArrayValue()[index]
			continue
		}
		return resource.PropertyValue{}, false
	}
	if inheritedUnknown && !value.ContainsUnknowns() {
		value = resource.MakeComputed(value)
	}
	if inheritedSecret && !value.ContainsSecrets() {
		value = resource.MakeSecret(value)
	}
	return value, true
}

func containsString(value resource.PropertyValue, want string) bool {
	if value.IsString() {
		return value.StringValue() == want
	}
	if value.IsSecret() {
		return containsString(value.SecretValue().Element, want)
	}
	if value.IsComputed() {
		return containsString(value.Input().Element, want)
	}
	if value.IsOutput() {
		return containsString(value.OutputValue().Element, want)
	}
	if value.IsArray() {
		for _, child := range value.ArrayValue() {
			if containsString(child, want) {
				return true
			}
		}
	}
	if value.IsObject() {
		for _, child := range value.ObjectValue() {
			if containsString(child, want) {
				return true
			}
		}
	}
	return false
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

func assertConfiguredHostCheckpoint(t *testing.T, snapshot *deploy.Snapshot, serverKeys, wantDNSNames []string) {
	t.Helper()
	if snapshot == nil {
		t.Fatal("configured-server checkpoint is nil")
	}
	if err := snapshot.VerifyIntegrity(); err != nil {
		t.Fatalf("configured-server checkpoint is invalid: %v", err)
	}

	wantHosts := make(map[string]resource.ID, len(serverKeys))
	for _, serverKey := range serverKeys {
		wantHosts["urn:pulumi:canary::sub2api-environment::sub2api-host:index:Host::host-"+serverKey] = resource.ID("host-" + serverKey)
	}
	gotHosts := make(map[string]resource.ID)
	gotDNS := make(map[string]resource.ID)
	stackResources := 0
	for _, state := range snapshot.Resources {
		switch state.Type {
		case "pulumi:pulumi:Stack":
			stackResources++
		case "sub2api-host:index:Host":
			gotHosts[string(state.URN)] = state.ID
		case "cloudflare:index/dnsRecord:DnsRecord":
			gotDNS[string(state.URN)] = state.ID
		default:
			if !strings.HasPrefix(string(state.Type), "pulumi:providers:") {
				t.Fatalf("configured-server checkpoint contains unexpected resource type %q: %s", state.Type, state.URN)
			}
		}
	}
	if !maps.Equal(gotHosts, wantHosts) {
		t.Fatalf("configured Host URN/ID set = %v, want %v", gotHosts, wantHosts)
	}
	wantDNS := make(map[string]resource.ID, len(wantDNSNames))
	for _, name := range wantDNSNames {
		wantDNS["urn:pulumi:canary::sub2api-environment::cloudflare:index/dnsRecord:DnsRecord::"+name] = resource.ID("cloudflare-" + name)
	}
	if stackResources != 1 {
		t.Fatalf("configured-server checkpoint stack resources = %d, want 1 (provider states may remain independently)", stackResources)
	}
	if !maps.Equal(gotDNS, wantDNS) {
		t.Fatalf("configured-server checkpoint DNS URN set = %v, want %v", gotDNS, wantDNS)
	}
}

func assertAppPlacementCheckpoint(t *testing.T, snapshot *deploy.Snapshot) {
	t.Helper()
	assertConfiguredHostCheckpoint(t, snapshot, []string{"alpha", "bravo"}, []string{"dns-app-alpha-A"})

	appsByHost := make(map[string][]resource.PropertyMap)
	for _, state := range snapshot.Resources {
		if state.Type != "sub2api-host:index:Host" {
			continue
		}
		target, ok := state.Inputs["target"]
		if !ok || !target.IsObject() {
			t.Fatalf("configured Host %s has no target object", state.URN)
		}
		apps, ok := target.ObjectValue()["apps"]
		if !ok || !apps.IsArray() {
			t.Fatalf("configured Host %s has no target apps array", state.URN)
		}
		appInputs := make([]resource.PropertyMap, 0, len(apps.ArrayValue()))
		for _, app := range apps.ArrayValue() {
			if !app.IsObject() {
				t.Fatalf("configured Host %s has non-object App target: %v", state.URN, app)
			}
			appInputs = append(appInputs, app.ObjectValue())
		}
		appsByHost[string(state.ID)] = appInputs
	}

	alphaApps, ok := appsByHost["host-alpha"]
	if !ok || len(alphaApps) != 1 {
		t.Fatalf("alpha target Apps = %v, want exactly one App", alphaApps)
	}
	if appID, ok := alphaApps[0]["id"]; !ok || !appID.IsString() || appID.StringValue() != "app" {
		t.Fatalf("alpha target App = %v, want App id %q", alphaApps[0], "app")
	}
	if bravoApps := appsByHost["host-bravo"]; len(bravoApps) != 0 {
		t.Fatalf("bravo target Apps = %v, want none", bravoApps)
	}
}

func assertPlacementFailureCheckpoint(t *testing.T, snapshot *deploy.Snapshot) {
	t.Helper()
	assertConfiguredHostCheckpoint(t, snapshot, []string{"bravo"}, nil)

	for _, state := range snapshot.Resources {
		if state.Type != "sub2api-host:index:Host" || state.ID != "host-bravo" {
			continue
		}
		target, ok := state.Inputs["target"]
		if !ok || !target.IsObject() {
			t.Fatalf("partial bravo Host %s has no target object", state.URN)
		}
		apps, ok := target.ObjectValue()["apps"]
		if !ok || !apps.IsArray() || len(apps.ArrayValue()) != 0 {
			t.Fatalf("partial bravo Host %s target Apps = %v, want none", state.URN, apps)
		}
		return
	}
	t.Fatal("partial checkpoint has no stable bravo Host")
}

func assertHostDeletionTrace(t *testing.T, events, want []string) {
	t.Helper()
	deletions := []string{}
	for _, event := range events {
		if strings.HasPrefix(event, "host:") && strings.HasSuffix(event, ":delete:ok") {
			deletions = append(deletions, event)
		}
	}
	if !slices.Equal(deletions, want) {
		t.Fatalf("Host deletion trace = %v, want %v", deletions, want)
	}
}

func assertDNSDeletionOrdering(t *testing.T, events, dnsDeletes []string, hostDelete string) {
	t.Helper()
	hostIndex := eventIndex(events, hostDelete)
	if hostIndex == -1 {
		t.Fatalf("lifecycle trace = %v, want %q", events, hostDelete)
	}
	for _, dnsDelete := range dnsDeletes {
		dnsIndex := eventIndex(events, dnsDelete)
		if dnsIndex == -1 {
			t.Fatalf("lifecycle trace = %v, want %q", events, dnsDelete)
		}
		if dnsIndex >= hostIndex {
			t.Fatalf("lifecycle trace = %v, want %q before %q", events, dnsDelete, hostDelete)
		}
	}
}

func eventIndex(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

func assertEventCount(t *testing.T, events []string, want string, count int) {
	t.Helper()
	if got := countEvent(events, want); got != count {
		t.Fatalf("lifecycle trace = %v, want %d occurrences of %q, got %d", events, count, want, got)
	}
}

func assertEventAfter(t *testing.T, events []string, event, predecessor string) {
	t.Helper()
	eventAt := eventIndex(events, event)
	predecessorAt := eventIndex(events, predecessor)
	if eventAt == -1 || predecessorAt == -1 || eventAt <= predecessorAt {
		t.Fatalf("lifecycle trace = %v, want %q after %q", events, event, predecessor)
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
