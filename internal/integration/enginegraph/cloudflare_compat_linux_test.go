//go:build linux

package enginegraph_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/cloudflareresource"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/integration/automationtest"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const cloudflareCompatVersion = "6.18.0"

// TestCloudflareOldCheckpointPreviewWithTypedRegistration proves the Engine
// accepts a checkpoint made by the fixed SDK wire contract when the program
// changes to the small typed registration package. This is intentionally not
// an import of the generated Cloudflare SDK. It proves same-type checkpoint
// compatibility; legacy alias migration shape is checked by the RPC tests.
func TestCloudflareOldCheckpointPreviewWithTypedRegistration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cliPath := requireEngineGraphPulumiCLI(t)
	command, err := automationtest.NewCommand(ctx, cliPath, t.TempDir())
	if err != nil {
		t.Fatalf("create official Pulumi command: %v", err)
	}
	trace := &traceFixture{}
	fakeProvider := cloudflareProvider(trace)
	fakeProvider.DiffConfig = func(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
		return p.DiffResponse{HasChanges: !req.OldInputs.Equals(req.Inputs)}, nil
	}
	baseSchema := fakeProvider.GetSchema
	fakeProvider.GetSchema = func(ctx context.Context, req p.GetSchemaRequest) (p.GetSchemaResponse, error) {
		response, err := baseSchema(ctx, req)
		if err != nil {
			return response, err
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(response.Schema), &schema); err != nil {
			return p.GetSchemaResponse{}, err
		}
		credentials := map[string]any{}
		for _, key := range []string{"apiKey", "apiToken", "apiUserServiceKey"} {
			credentials[key] = map[string]any{"type": "string", "secret": true}
		}
		schema["config"] = map[string]any{"variables": credentials}
		schema["provider"] = map[string]any{"inputProperties": credentials}
		encoded, err := json.Marshal(schema)
		return p.GetSchemaResponse{Schema: string(encoded)}, err
	}
	fakeProvider.Diff = func(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
		return p.DiffResponse{HasChanges: !req.OldInputs.Equals(req.Inputs)}, nil
	}
	provider, err := automationtest.StartProvider(ctx, string(cloudflareProviderPackage), cloudflareCompatVersion, fakeProvider)
	if err != nil {
		t.Fatalf("start fake Cloudflare provider: %v", err)
	}
	t.Cleanup(provider.Close)

	workspace, err := auto.NewLocalWorkspace(ctx,
		auto.WorkDir(t.TempDir()),
		auto.Program(registerOldCloudflareSDKContract),
		auto.Project(workspace.Project{
			Name:    "cloudflare-checkpoint-compat",
			Runtime: workspace.NewProjectRuntimeInfo("go", nil),
		}),
		auto.Pulumi(command),
		auto.PulumiHome(engineGraphPulumiHome(t)),
		auto.SecretsProvider("passphrase"),
		auto.EnvVars(map[string]string{
			"PULUMI_BACKEND_URL":       "file://" + t.TempDir(),
			"PULUMI_CONFIG_PASSPHRASE": "cloudflare-checkpoint-compat-passphrase",
			"PULUMI_DEBUG_PROVIDERS":   automationtest.DebugProviders(map[string]int{string(cloudflareProviderPackage): provider.Port()}),
		}),
	)
	if err != nil {
		t.Fatalf("create compatibility workspace: %v", err)
	}
	stack, err := auto.NewStack(ctx, "canary", workspace)
	if err != nil {
		t.Fatalf("create compatibility stack: %v", err)
	}

	upCtx, upCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer upCancel()
	if _, err := stack.Up(upCtx, optup.SuppressProgress(), optup.SuppressOutputs(), optup.Color("never")); err != nil {
		t.Fatal("create old Cloudflare SDK-contract checkpoint failed")
	}
	before := exportCloudflareCompatCheckpoint(t, stack)
	assertOldCloudflareCompatIdentity(t, before)

	workspace.SetProgram(registerTypedCloudflareResource)
	previewEvents := make(chan events.EngineEvent)
	previewDone := make(chan []string, 1)
	go func() {
		var operations []string
		for event := range previewEvents {
			if event.Error != nil {
				operations = append(operations, "event-error")
			}
			if event.ResourcePreEvent != nil {
				operations = append(operations, string(event.ResourcePreEvent.Metadata.Op))
			}
		}
		previewDone <- operations
	}()
	previewCtx, previewCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer previewCancel()
	preview, previewErr := stack.Preview(previewCtx, optpreview.SuppressProgress(), optpreview.SuppressOutputs(), optpreview.Color("never"), optpreview.EventStreams(previewEvents))
	if previewErr != nil {
		t.Fatal("preview typed Cloudflare registration against old checkpoint failed")
	}
	if preview.ChangeSummary["same"] == 0 {
		t.Fatal("typed registration preview did not report unchanged resources")
	}
	for operation, count := range preview.ChangeSummary {
		if operation != "same" && count != 0 {
			t.Fatal("typed registration preview reported resource changes")
		}
	}
	for _, operation := range <-previewDone {
		if operation != "same" {
			t.Fatalf("typed registration preview planned unexpected %q operation; want no diff, replace, or delete", operation)
		}
	}
	after := exportCloudflareCompatCheckpoint(t, stack)
	if !snapshotsEqual(before, after) {
		t.Fatal("typed registration preview mutated the old Cloudflare checkpoint")
	}
}

func requireEngineGraphPulumiCLI(t *testing.T) string {
	t.Helper()
	cliPath := os.Getenv("ENGINE_GRAPH_PULUMI_CLI")
	if cliPath == "" {
		t.Fatal("ENGINE_GRAPH_PULUMI_CLI is required for external Cloudflare checkpoint compatibility tests")
	}
	return cliPath
}

func engineGraphPulumiHome(t *testing.T) string {
	t.Helper()
	if home := os.Getenv("PULUMI_HOME"); home != "" {
		return home
	}
	return t.TempDir()
}

func exportCloudflareCompatCheckpoint(t *testing.T, stack auto.Stack) *automationtest.Checkpoint {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	export, err := automationtest.ValidatedExport(ctx, stack)
	if err != nil {
		t.Fatal("export compatibility checkpoint failed")
	}
	checkpoint, err := automationtest.DecodeCheckpoint(export)
	if err != nil {
		t.Fatalf("decode compatibility checkpoint: %v", err)
	}
	return checkpoint
}

func assertOldCloudflareCompatIdentity(t *testing.T, checkpoint *automationtest.Checkpoint) {
	t.Helper()
	const dnsURN = "urn:pulumi:canary::cloudflare-checkpoint-compat::cloudflare:index/dnsRecord:DnsRecord::dns-api-A"
	for _, state := range checkpoint.Resources {
		if string(state.URN) != dnsURN {
			continue
		}
		if state.Type != "cloudflare:index/dnsRecord:DnsRecord" || state.ID != "cloudflare-dns-api-A" {
			t.Fatalf("old SDK-contract DNS identity = type:%q id:%q, want fixed DnsRecord/%q", state.Type, state.ID, "cloudflare-dns-api-A")
		}
		// Pulumi v3.256.0 consumes aliases during registration and clears them
		// before persisting state. The RPC compatibility tests cover alias shape.
		return
	}
	t.Fatalf("old SDK-contract checkpoint lacks fixed DNS resource %s", dnsURN)
}

type oldCloudflareProvider struct{ pulumi.ProviderResourceState }

type oldCloudflareProviderArgs struct {
	ApiToken pulumi.StringPtrInput `pulumi:"apiToken"`
}

type oldCloudflareProviderArgsElement struct {
	ApiToken *string `pulumi:"apiToken"`
}

func (oldCloudflareProviderArgs) ElementType() reflect.Type {
	return reflect.TypeOf((*oldCloudflareProviderArgsElement)(nil)).Elem()
}

type oldCloudflareDNSRecord struct{ pulumi.CustomResourceState }

type oldCloudflareDNSRecordArgs struct {
	Content pulumi.StringPtrInput `pulumi:"content"`
	Name    pulumi.StringInput    `pulumi:"name"`
	Proxied pulumi.BoolPtrInput   `pulumi:"proxied"`
	Ttl     pulumi.Float64Input   `pulumi:"ttl"`
	Type    pulumi.StringInput    `pulumi:"type"`
	ZoneId  pulumi.StringInput    `pulumi:"zoneId"`
}

type oldCloudflareDNSRecordArgsElement struct {
	Content *string `pulumi:"content"`
	Name    string  `pulumi:"name"`
	Proxied *bool   `pulumi:"proxied"`
	Ttl     float64 `pulumi:"ttl"`
	Type    string  `pulumi:"type"`
	ZoneId  string  `pulumi:"zoneId"`
}

func (oldCloudflareDNSRecordArgs) ElementType() reflect.Type {
	return reflect.TypeOf((*oldCloudflareDNSRecordArgsElement)(nil)).Elem()
}

func registerOldCloudflareSDKContract(ctx *pulumi.Context) error {
	provider := &oldCloudflareProvider{}
	providerArgs := &oldCloudflareProviderArgs{
		ApiToken: pulumi.ToSecret(pulumi.String("cloudflare-api-token-canary").ToStringPtrOutput()).(pulumi.StringPtrInput),
	}
	if err := ctx.RegisterResource("pulumi:providers:cloudflare", "cloudflare", providerArgs, provider,
		pulumi.Version(cloudflareCompatVersion),
		pulumi.AdditionalSecretOutputs([]string{"apiKey", "apiToken", "apiUserServiceKey"}),
	); err != nil {
		return err
	}
	return registerCloudflareCompatDNS(ctx, provider, &oldCloudflareDNSRecord{}, &oldCloudflareDNSRecordArgs{
		Content: pulumi.String("203.0.113.10").ToStringPtrOutput(),
		Name:    pulumi.String("api.example.test"),
		Proxied: pulumi.Bool(true).ToBoolPtrOutput(),
		Ttl:     pulumi.Float64(1),
		Type:    pulumi.String("A"),
		ZoneId:  pulumi.String("zone-canary"),
	})
}

func registerTypedCloudflareResource(ctx *pulumi.Context) error {
	provider, err := cloudflareresource.NewProvider(ctx, "cloudflare", &cloudflareresource.ProviderArgs{
		ApiToken: pulumi.String("cloudflare-api-token-canary").ToStringPtrOutput(),
	})
	if err != nil {
		return err
	}
	_, err = cloudflareresource.NewDnsRecord(ctx, "dns-api-A", &cloudflareresource.DnsRecordArgs{
		Content: pulumi.String("203.0.113.10").ToStringPtrOutput(),
		Name:    pulumi.String("api.example.test"),
		Proxied: pulumi.Bool(true).ToBoolPtrOutput(),
		Ttl:     pulumi.Float64(1),
		Type:    pulumi.String("A"),
		ZoneId:  pulumi.String("zone-canary"),
	}, pulumi.Provider(provider))
	return err
}

func registerCloudflareCompatDNS(ctx *pulumi.Context, provider pulumi.ProviderResource, record *oldCloudflareDNSRecord, args *oldCloudflareDNSRecordArgs) error {
	return ctx.RegisterResource("cloudflare:index/dnsRecord:DnsRecord", "dns-api-A", args, record,
		pulumi.Provider(provider),
		pulumi.Version(cloudflareCompatVersion),
		pulumi.Aliases([]pulumi.Alias{{Type: pulumi.String("cloudflare:index/record:Record")}}),
	)
}
