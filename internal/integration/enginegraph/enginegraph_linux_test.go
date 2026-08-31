//go:build linux

package enginegraph_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
	mu     sync.Mutex
	events []string
}

func (f *traceFixture) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func TestEngineGraphFailureStopsPublication(t *testing.T) {
	ctx := context.Background()
	configYAML := readFixture(t, "external-two-host-cloudflare.yaml")
	secretsYAML := readFixture(t, "external-two-host-cloudflare-secrets.yaml")
	project := &workspace.Project{
		Name:    tokens.PackageName("sub2api-environment"),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
	}

	stateDir := t.TempDir()
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

	trace := &traceFixture{}
	// The isolated host has no loaders yet, so it cannot discover or download a provider.
	languageRuntime := deploytest.NewLanguageRuntimeF(func(info plugin.RunInfo, _ *deploytest.ResourceMonitor) error {
		pulumiCtx, err := pulumi.NewContext(context.Background(), pulumi.RunInfo{
			Project:     info.Project,
			Stack:       info.Stack,
			Parallel:    info.Parallel,
			DryRun:      info.DryRun,
			MonitorAddr: info.MonitorAddress,
		})
		if err != nil {
			return err
		}
		return pulumi.RunWithContext(pulumiCtx, func(pulumiCtx *pulumi.Context) error {
			return program.Register(pulumiCtx, release, configYAML, secretsYAML)
		})
	})
	hostFactory := deploytest.NewPluginHostF(nil, nil, languageRuntime, nil, nil)
	host := hostFactory()
	secretsManager := b64.NewBase64SecretsManager()
	revisionKey, err := secretsManager.Encrypter().EncryptValue(ctx, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatalf("encrypt Host revision key: %v", err)
	}
	updateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = localBackend.Update(updateCtx, stackState, backend.UpdateOperation{
		Proj: project,
		M:    &backend.UpdateMetadata{},
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
	if err == nil {
		t.Fatal("update unexpectedly succeeded")
	}

	snapshot, snapshotErr := stackState.Snapshot(ctx, stack.Base64SecretsProvider{})
	if snapshotErr != nil {
		t.Fatalf("load partial checkpoint: %v", snapshotErr)
	}
	if snapshot == nil {
		t.Fatal("partial checkpoint is nil")
	}
	if err := snapshot.VerifyIntegrity(); err != nil {
		t.Fatalf("partial checkpoint is invalid: %v", err)
	}
	if strings.Contains(err.Error(), "Could not find plugin for (") {
		assertInitialMissingProviderCheckpoint(t, snapshot)
	}

	if !strings.Contains(err.Error(), scriptedAlphaFailure) {
		t.Fatalf("update error = %q, want sanitized scripted alpha failure %q; missing-provider diagnostics mean Task5 provider loaders are still absent", err, scriptedAlphaFailure)
	}
	if got := trace.snapshot(); len(got) != 1 || got[0] != alphaFailureTrace {
		t.Fatalf("scripted trace = %v, want [%s]; bravo Host and Cloudflare DNS publication must not run after alpha fails", got, alphaFailureTrace)
	}
}

func engineOptions(host plugin.Host) engine.UpdateOptions {
	return engine.UpdateOptions{
		HostFactory: func(context.Context, diag.Sink, diag.Sink, plugin.DebugContext) (plugin.Host, error) {
			return host, nil
		},
		SkipPluginPreInstall: true,
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return contents
}

func assertInitialMissingProviderCheckpoint(t *testing.T, snapshot *deploy.Snapshot) {
	t.Helper()
	if len(snapshot.Resources) != 1 {
		t.Fatalf("missing-provider partial checkpoint resources = %d, want only stack resource", len(snapshot.Resources))
	}
	stackResource := snapshot.Resources[0]
	if stackResource.Type != "pulumi:pulumi:Stack" {
		t.Fatalf("missing-provider partial checkpoint resource = %q, want stack resource", stackResource.Type)
	}
	for _, resource := range snapshot.Resources {
		if resource.Type == "sub2api-host:index:Host" || resource.Type == "cloudflare:index/dnsRecord:DnsRecord" {
			t.Fatalf("missing-provider partial checkpoint must not contain Host or DNS resources: %s", resource.URN)
		}
	}
}
