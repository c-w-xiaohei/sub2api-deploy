package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/cloudflareresource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
)

type cloudflareCompatMocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
}

func (m *cloudflareCompatMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, args)
	m.mu.Unlock()
	return args.Name + "-id", args.Inputs.Copy(), nil
}

func (m *cloudflareCompatMocks) Call(pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func TestCloudflareRegistrationCompatibility(t *testing.T) {
	mocks := &cloudflareCompatMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		var parent pulumi.ResourceState
		if err := ctx.RegisterComponentResource("test:index:Parent", "parent", &parent); err != nil {
			return err
		}
		provider, err := cloudflareresource.NewProvider(ctx, "cloudflare", &cloudflareresource.ProviderArgs{
			ApiKey: pulumi.StringPtr("key"), ApiToken: pulumi.StringPtr("token"), ApiUserServiceKey: pulumi.StringPtr("service-key"),
		}, pulumi.Parent(&parent))
		if err != nil {
			return err
		}
		setting, err := cloudflareresource.NewZoneSetting(ctx, "ssl", &cloudflareresource.ZoneSettingArgs{
			ZoneId: pulumi.String("zone"), SettingId: pulumi.String("ssl"), Value: pulumi.String("strict"),
		}, pulumi.Parent(&parent), pulumi.Provider(provider))
		if err != nil {
			return err
		}
		_, err = cloudflareresource.NewDnsRecord(ctx, "origin", &cloudflareresource.DnsRecordArgs{
			ZoneId: pulumi.String("zone"), Name: pulumi.String("app.example.test"), Content: pulumi.StringPtr("198.51.100.10"),
			Proxied: pulumi.BoolPtr(true), Ttl: pulumi.Float64(1), Type: pulumi.String("A"),
		}, pulumi.Parent(&parent), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{setting}))
		return err
	}, pulumi.WithMocks("compat", "test", mocks))
	if err != nil {
		t.Fatalf("register Cloudflare resources: %v", err)
	}

	provider := requireCloudflareCompatResource(t, mocks.resources, "cloudflare")
	if provider.TypeToken != "pulumi:providers:cloudflare" || provider.RegisterRPC.GetVersion() != "6.18.0" {
		t.Fatalf("provider registration = type %q version %q", provider.TypeToken, provider.RegisterRPC.GetVersion())
	}
	for _, key := range []resource.PropertyKey{"apiKey", "apiToken", "apiUserServiceKey"} {
		if !provider.Inputs[key].IsSecret() {
			t.Fatalf("provider input %q is not secret", key)
		}
	}
	if got := provider.RegisterRPC.GetAdditionalSecretOutputs(); len(got) != 3 || got[0] != "apiKey" || got[1] != "apiToken" || got[2] != "apiUserServiceKey" {
		t.Fatalf("provider additional secret outputs = %v", got)
	}

	setting := requireCloudflareCompatResource(t, mocks.resources, "ssl")
	if setting.TypeToken != "cloudflare:index/zoneSetting:ZoneSetting" || setting.RegisterRPC.GetVersion() != "6.18.0" || setting.RegisterRPC.GetProvider() != cloudflareCompatProviderRef(provider) {
		t.Fatalf("zone setting registration = %+v", setting.RegisterRPC)
	}
	if setting.RegisterRPC.GetParent() == "" || setting.Inputs["zoneId"].StringValue() != "zone" || setting.Inputs["settingId"].StringValue() != "ssl" || setting.Inputs["value"].StringValue() != "strict" {
		t.Fatalf("zone setting inputs/parent = %v / %+v", setting.Inputs, setting.RegisterRPC)
	}

	record := requireCloudflareCompatResource(t, mocks.resources, "origin")
	if record.TypeToken != "cloudflare:index/dnsRecord:DnsRecord" || record.RegisterRPC.GetVersion() != "6.18.0" || record.RegisterRPC.GetProvider() != cloudflareCompatProviderRef(provider) {
		t.Fatalf("DNS registration = %+v", record.RegisterRPC)
	}
	if len(record.RegisterRPC.GetDependencies()) != 1 || len(record.RegisterRPC.GetAliases()) != 1 || record.RegisterRPC.GetAliases()[0].GetSpec().GetType() != "cloudflare:index/record:Record" {
		t.Fatalf("DNS dependencies/aliases = %v / %+v", record.RegisterRPC.GetDependencies(), record.RegisterRPC.GetAliases())
	}
	for key, want := range map[resource.PropertyKey]string{"zoneId": "zone", "name": "app.example.test", "content": "198.51.100.10", "type": "A"} {
		if record.Inputs[key].StringValue() != want {
			t.Fatalf("DNS input %q = %q, want %q", key, record.Inputs[key].StringValue(), want)
		}
	}
	if !record.Inputs["proxied"].BoolValue() || record.Inputs["ttl"].NumberValue() != 1 {
		t.Fatalf("DNS proxy/TTL = %v", record.Inputs)
	}
}

func TestCloudflareExplicitVersionOverridesPackageDefault(t *testing.T) {
	mocks := &cloudflareCompatMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := cloudflareresource.NewProvider(ctx, "provider-override", &cloudflareresource.ProviderArgs{}, pulumi.Version("9.9.9"))
		if err != nil {
			return err
		}
		_, err = cloudflareresource.NewDnsRecord(ctx, "dns-override", &cloudflareresource.DnsRecordArgs{
			ZoneId: pulumi.String("zone"), Name: pulumi.String("app.example.test"), Content: pulumi.StringPtr("198.51.100.10"),
			Proxied: pulumi.BoolPtr(true), Ttl: pulumi.Float64(1), Type: pulumi.String("A"),
		}, pulumi.Version("9.9.9"))
		if err != nil {
			return err
		}
		_, err = cloudflareresource.NewZoneSetting(ctx, "setting-override", &cloudflareresource.ZoneSettingArgs{
			ZoneId: pulumi.String("zone"), SettingId: pulumi.String("ssl"), Value: pulumi.String("strict"),
		}, pulumi.Version("9.9.9"))
		return err
	}, pulumi.WithMocks("compat", "test", mocks))
	if err != nil {
		t.Fatalf("register version override: %v", err)
	}
	for _, name := range []string{"provider-override", "dns-override", "setting-override"} {
		if got := requireCloudflareCompatResource(t, mocks.resources, name).RegisterRPC.GetVersion(); got != "9.9.9" {
			t.Fatalf("%s explicit version = %q, want caller override", name, got)
		}
	}
}

func TestCloudflareInputsPreserveUnknownAndSecretRPCValues(t *testing.T) {
	t.Setenv(pulumi.EnvDryRun, "true")
	mocks := &cloudflareCompatMocks{}
	unknownString := pulumi.UnsafeUnknownOutput(nil).ApplyT(func(any) string { return "" }).(pulumi.StringOutput)
	unknownValue := pulumi.UnsafeUnknownOutput(nil)
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := cloudflareresource.NewDnsRecord(ctx, "unknown", &cloudflareresource.DnsRecordArgs{
			ZoneId: pulumi.String("zone"), Name: unknownString, Content: pulumi.ToSecret(pulumi.StringPtr("secret-content")).(pulumi.StringPtrInput),
			Proxied: pulumi.BoolPtr(true), Ttl: pulumi.Float64(1), Type: pulumi.String("A"),
		})
		if err != nil {
			return err
		}
		_, err = cloudflareresource.NewZoneSetting(ctx, "unknown-setting", &cloudflareresource.ZoneSettingArgs{
			ZoneId: pulumi.String("zone"), SettingId: pulumi.String("ssl"), Value: unknownValue,
		})
		return err
	}, pulumi.WithMocks("compat", "test", mocks))
	if err != nil {
		t.Fatalf("register unknown and secret inputs: %v", err)
	}
	record := requireCloudflareCompatResource(t, mocks.resources, "unknown")
	if !record.Inputs["name"].IsComputed() || !record.Inputs["content"].IsSecret() {
		t.Fatalf("DNS unknown/secret propagation = %v", record.Inputs)
	}
	setting := requireCloudflareCompatResource(t, mocks.resources, "unknown-setting")
	if !setting.Inputs["value"].IsComputed() {
		t.Fatalf("zone-setting unknown propagation = %v", setting.Inputs)
	}
}

func TestLegacyCloudflareCallersPreservePersistedIdentities(t *testing.T) {
	mocks := &cloudflareCompatMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		var edge, site, preflight pulumi.ResourceState
		if err := ctx.RegisterComponentResource("sub2api:host:Edge", "edge", &edge); err != nil {
			return err
		}
		if err := ctx.RegisterComponentResource("sub2api:host:Site", "site-code2", &site); err != nil {
			return err
		}
		if err := ctx.RegisterComponentResource("test:index:Preflight", "preflight", &preflight); err != nil {
			return err
		}
		provider, err := createCloudflareProvider(ctx, &edge, &preflight, pulumi.ToSecret(pulumi.String("token")).(pulumi.StringOutput), true)
		if err != nil {
			return err
		}
		if _, err = createStrictSSLSetting(ctx, &edge, &preflight, provider, "zone", true); err != nil {
			return err
		}
		_, err = createSiteDNSRecord(ctx, &site, &preflight, provider, legacyCode2Layout(DeriveSiteLayout("code2", "contextid-us")), SiteSpec{Domain: "code2.example.test"}, EdgeSpec{CloudflareZoneID: "zone", OriginIP: "2001:db8::1"})
		return err
	}, pulumi.WithMocks("compat", "test", mocks))
	if err != nil {
		t.Fatalf("register legacy Cloudflare callers: %v", err)
	}

	edgeURN := cloudflareCompatURN("", "sub2api:host:Edge", "edge")
	siteURN := cloudflareCompatURN("", "sub2api:host:Site", "site-code2")
	preflightURN := cloudflareCompatURN("", "test:index:Preflight", "preflight")
	provider := requireCloudflareCompatResource(t, mocks.resources, "cloudflare")
	if provider.TypeToken != "pulumi:providers:cloudflare" || provider.RegisterRPC.GetParent() != edgeURN || provider.RegisterRPC.GetVersion() != "6.18.0" {
		t.Fatalf("legacy provider registration = %+v", provider.RegisterRPC)
	}
	assertExactAliases(t, provider.RegisterRPC.GetAliases(), []cloudflareAlias{{name: "cloudflare", noParent: true}})
	if got := provider.RegisterRPC.GetDependencies(); len(got) != 1 || got[0] != preflightURN {
		t.Fatalf("legacy provider dependencies = %v", got)
	}

	setting := requireCloudflareCompatResource(t, mocks.resources, "cloudflare-full-strict")
	if setting.TypeToken != "cloudflare:index/zoneSetting:ZoneSetting" || setting.RegisterRPC.GetParent() != edgeURN || setting.RegisterRPC.GetProvider() != cloudflareCompatProviderRef(provider) {
		t.Fatalf("legacy zone setting registration = %+v", setting.RegisterRPC)
	}
	assertExactAliases(t, setting.RegisterRPC.GetAliases(), []cloudflareAlias{{name: "cloudflare-full-strict", noParent: true}})
	if got := setting.RegisterRPC.GetDependencies(); len(got) != 1 || got[0] != preflightURN {
		t.Fatalf("legacy zone setting dependencies = %v", got)
	}

	record := requireCloudflareCompatResource(t, mocks.resources, "site-code2-origin")
	if record.TypeToken != "cloudflare:index/dnsRecord:DnsRecord" || record.RegisterRPC.GetParent() != siteURN || record.RegisterRPC.GetProvider() != cloudflareCompatProviderRef(provider) {
		t.Fatalf("legacy DNS registration = %+v", record.RegisterRPC)
	}
	assertExactAliases(t, record.RegisterRPC.GetAliases(), []cloudflareAlias{
		{name: "sub2api-origin", noParent: true},
		{typ: "cloudflare:index/record:Record", noParent: false},
	})
	if got := record.RegisterRPC.GetDependencies(); len(got) != 1 || got[0] != preflightURN {
		t.Fatalf("legacy DNS dependencies = %v", got)
	}
	if record.Inputs["name"].StringValue() != "code2.example.test" || record.Inputs["type"].StringValue() != "AAAA" || record.Inputs["zoneId"].StringValue() != "zone" {
		t.Fatalf("legacy DNS inputs = %v", record.Inputs)
	}
}

func requireCloudflareCompatResource(t *testing.T, resources []pulumi.MockResourceArgs, name string) pulumi.MockResourceArgs {
	t.Helper()
	for _, item := range resources {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("missing resource %q", name)
	return pulumi.MockResourceArgs{}
}

func cloudflareCompatProviderRef(provider pulumi.MockResourceArgs) string {
	return cloudflareCompatURN(provider.RegisterRPC.GetParent(), provider.TypeToken, provider.Name) + "::" + provider.Name + "-id"
}

func cloudflareCompatURN(parent, typ, name string) string {
	parentType := ""
	if parent == "" {
		return "urn:pulumi:test::compat::" + typ + "::" + name
	}
	parts := strings.Split(parent, "::")
	parentType = parts[len(parts)-2]
	if parentType != "pulumi:pulumi:Stack" {
		typ = parentType + "$" + typ
	}
	return "urn:pulumi:test::compat::" + typ + "::" + name
}

type cloudflareAlias struct {
	name     string
	typ      string
	noParent bool
}

func assertExactAliases(t *testing.T, actual []*pulumirpc.Alias, want []cloudflareAlias) {
	t.Helper()
	if len(actual) != len(want) {
		t.Fatalf("alias count = %d, want %d: %+v", len(actual), len(want), actual)
	}
	for index, expected := range want {
		spec := actual[index].GetSpec()
		if spec == nil {
			t.Fatalf("alias %d is not a spec: %+v", index, actual[index])
		}
		parent, ok := spec.Parent.(*pulumirpc.Alias_Spec_NoParent)
		if !ok || spec.GetName() != expected.name || spec.GetType() != expected.typ || parent.NoParent != expected.noParent {
			t.Fatalf("alias %d = %+v, want name=%q type=%q noParent=%t", index, actual[index], expected.name, expected.typ, expected.noParent)
		}
	}
}
