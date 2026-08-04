package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	edgeComponentToken = "sub2api:host:Edge"
	siteComponentToken = "sub2api:host:Site"
)

type graphMocks struct { mu sync.Mutex; resources []pulumi.MockResourceArgs }
type legacyFixtureResource struct { URN string `json:"urn"`; Type string `json:"type"`; Name string `json:"name"`; Parent string `json:"parent"`; Provider string `json:"provider"`; ID string `json:"id"`; Protect bool `json:"protect"`; RetainOnDelete bool `json:"retainOnDelete"`; Inputs map[string]interface{} `json:"inputs"` }

func (m *graphMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, args)
	m.mu.Unlock()
	state := args.Inputs.Copy()
	switch args.TypeToken {
	case "neon:provider:Project":
		state["connection_uri"] = resource.NewStringProperty("postgresql://user:pass@ep.generated.neon.tech/db?sslmode=require")
	case "upstash:index/redisDatabase:RedisDatabase":
		state["endpoint"] = resource.NewStringProperty("redis.generated.upstash.io")
		state["port"] = resource.NewNumberProperty(6379)
		state["password"] = resource.NewStringProperty("redis-secret")
	}
	return args.Name + "-id", state, nil
}

func (m *graphMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) { return args.Args, nil }

func TestHostGraphOwnsEdgeAndIsolatedSites(t *testing.T) {
	mocks, exports := deployTwoSiteHost(t, "weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	resources := resourcesByName(mocks.resources)

	edge := requireResource(t, resources, "edge")
	if edge.TypeToken != edgeComponentToken {
		t.Fatalf("edge type = %q, want %q", edge.TypeToken, edgeComponentToken)
	}
	if countResources(mocks.resources, edgeComponentToken) != 1 || countResources(mocks.resources, siteComponentToken) != 2 {
		t.Fatalf("component counts = edge %d, site %d", countResources(mocks.resources, edgeComponentToken), countResources(mocks.resources, siteComponentToken))
	}
	if countResources(mocks.resources, "pulumi:providers:cloudflare") != 1 || countResources(mocks.resources, "cloudflare:index/zoneSetting:ZoneSetting") != 1 {
		t.Fatalf("shared Cloudflare resources = provider %d, ssl %d", countResources(mocks.resources, "pulumi:providers:cloudflare"), countResources(mocks.resources, "cloudflare:index/zoneSetting:ZoneSetting"))
	}
	preflight := requireResource(t, resources, "host-preflight")
	if preflight.RegisterRPC == nil || !strings.Contains(preflight.RegisterRPC.GetParent(), "pulumi:pulumi:Stack") { t.Fatalf("host preflight must be Stack-owned: %+v", preflight.RegisterRPC) }
	preflightEnvironment := preflight.Inputs["environment"].ObjectValue()
	expectedModes := preflightEnvironment["EXPECTED_SITE_MODES"]
	if expectedModes.IsSecret() { t.Fatal("expected Site modes must remain non-secret derived host data") }
	if got := expectedModes.StringValue(); got != `{"code2":{"postgresMode":"neon","redisMode":"upstash"},"code3":{"postgresMode":"neon","redisMode":"upstash"}}` {
		t.Fatalf("EXPECTED_SITE_MODES = %q", got)
	}
	if !strings.Contains(preflight.Inputs["create"].StringValue(), `"$EXPECTED_SITE_MODES"`) {
		t.Fatal("Stack-level host preflight command does not consume expected Site modes")
	}
	for _, name := range []string{"cloudflare", "cloudflare-full-strict", "edge-reconcile", "site-code2-origin", "site-code2-neon-project", "site-code2-upstash-redis", "site-code3-origin", "site-code3-neon-project", "site-code3-upstash-redis", "site-code2-reconcile", "site-code2-release", "site-code2-strict-public-readiness", "site-code2-rollback-preparation", "site-code3-reconcile", "site-code3-release", "site-code3-strict-public-readiness", "site-code3-rollback-preparation", "host-finalize-state"} {
		assertDependsOn(t, requireResource(t, resources, name), preflight.Name, "must not run before host preflight")
	}
	finalize := requireResource(t, resources, "host-finalize-state")
	if finalize.RegisterRPC == nil || !strings.Contains(finalize.RegisterRPC.GetParent(), "pulumi:pulumi:Stack") { t.Fatalf("host finalization must be Stack-owned: %+v", finalize.RegisterRPC) }

	for _, siteID := range []string{"code2", "code3"} {
		site := requireResource(t, resources, "site-"+siteID)
		if site.TypeToken != siteComponentToken {
			t.Fatalf("%s type = %q, want %q", siteID, site.TypeToken, siteComponentToken)
		}
		if site.RegisterRPC == nil || !strings.Contains(site.RegisterRPC.GetParent(), "pulumi:pulumi:Stack") {
			t.Fatalf("%s parent = %q, want Stack-owned Site component", siteID, site.RegisterRPC.GetParent())
		}
		children := siteChildren(mocks.resources, siteID)
		if len(children) == 0 { t.Fatalf("%s has no Site-owned children", siteID) }
		parent := children[0].RegisterRPC.GetParent()
		for _, child := range children {
			assertParent(t, child, parent, "site children must share their Site component parent")
		}
		dns := requireResource(t, resources, "site-"+siteID+"-origin")
		if dns.TypeToken != "cloudflare:index/dnsRecord:DnsRecord" || !dns.Inputs["proxied"].BoolValue() {
			t.Fatalf("%s DNS = %+v", siteID, dns)
		}
		assertParent(t, dns, parent, "DNS must be Site-parented")
		for _, provider := range []string{"neon", "upstash"} {
			item := requireSiteProvider(t, mocks.resources, siteID, provider)
			assertParent(t, item, parent, "selected provider must be Site-parented")
			assertDependsOn(t, item, preflight.Name, "selected provider must not run before host preflight")
		}
	}
	if countResources(mocks.resources, "cloudflare:index/dnsRecord:DnsRecord") != 2 {
		t.Fatalf("DNS record count = %d, want 2", countResources(mocks.resources, "cloudflare:index/dnsRecord:DnsRecord"))
	}

	assertSiteCommands(t, resources, "code2", "")
	assertSiteCommands(t, resources, "code3", "code2")
	assertSiteRuntimeIsolation(t, resources, "code2", "code3")
	assertPublicSiteExports(t, exports)
}

func TestHostGraphManagedDataIsProtectedAndSiteOwned(t *testing.T) {
	mocks, _ := deployTwoSiteHost(t, "weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	resources := resourcesByName(mocks.resources)
	for _, siteID := range []string{"code2", "code3"} {
		siteOwnedChildren := siteChildren(mocks.resources, siteID)
		if len(siteOwnedChildren) == 0 { t.Fatalf("%s has no Site-owned children", siteID) }
		parent := siteOwnedChildren[0].RegisterRPC.GetParent()
		for _, suffix := range []string{"neon-project", "upstash-redis"} {
			item := requireResource(t, resources, "site-"+siteID+"-"+suffix)
			assertParent(t, item, parent, "managed provider/data resource must be Site-parented")
		}
		for _, suffix := range []string{"neon-project", "upstash-redis"} {
			item := requireResource(t, resources, "site-"+siteID+"-"+suffix)
			if item.RegisterRPC == nil || !item.RegisterRPC.GetProtect() || !item.RegisterRPC.GetRetainOnDelete() {
				t.Fatalf("%s options must include protect and retain-on-delete: %+v", item.Name, item.RegisterRPC)
			}
		}
	}
}

func TestCode2PersistedResourcesHaveExactNoParentAliases(t *testing.T) {
	spec, layouts, err := ValidateHostSpec(validHostSpec()); if err != nil { t.Fatal(err) }
	for index := range layouts { if layouts[index].SiteID == "code2" { layouts[index] = legacyCode2Layout(layouts[index]) } }
	mocks := &graphMocks{}
	err = pulumi.RunErr(func(ctx *pulumi.Context) error { _, err := deployHostGraph(ctx, spec, layouts, secretHostSpec(spec), "edge-v1", "site-v1", "host-v1", true); return err }, pulumi.WithMocks("sub2api-vps-deploy", "test", mocks))
	if err != nil { t.Fatal(err) }
	resources := resourcesByName(mocks.resources)
	for name, oldName := range map[string]string{
		"cloudflare": "cloudflare", "cloudflare-full-strict": "cloudflare-full-strict", "site-code2-origin": "sub2api-origin",
		"site-code2-neon": "neon", "site-code2-neon-project": "contextid-us-neon-project",
		"site-code2-upstash": "upstash", "site-code2-upstash-redis": "contextid-us-upstash-redis",
		"site-code2-reconcile": "infra-reconcile", "site-code2-strict-public-readiness": "post-strict-public-readiness",
		"site-code2-release": "application-release",
	} { assertNoParentAlias(t, requireResource(t, resources, name), oldName) }
	for _, name := range []string{"site-code3-origin", "site-code3-neon", "site-code3-neon-project", "site-code3-upstash-redis", "site-code3-reconcile", "site-code3-release"} {
		if hasNoParentLegacyAlias(requireResource(t, resources, name), "") { t.Fatalf("%s unexpectedly has legacy aliases", name) }
	}
	if hasNoParentLegacyAlias(requireSiteProvider(t, mocks.resources, "code3", "upstash"), "") { t.Fatal("code3 Upstash provider unexpectedly has a legacy alias") }
	for _, name := range []string{"site-code2-neon-project", "site-code2-upstash"} {
		if len(requireResource(t, resources, name).RegisterRPC.GetIgnoreChanges()) != 1 { t.Fatalf("%s must preserve its legacy-only optional provider input", name) }
	}
}

func TestCleanCode2DoesNotReceiveLegacyAliases(t *testing.T) {
	mocks, _ := deployTwoSiteHost(t, "weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	for name, oldName := range map[string]string{"cloudflare":"cloudflare", "cloudflare-full-strict":"cloudflare-full-strict", "site-code2-origin":"sub2api-origin", "site-code2-neon":"neon", "site-code2-neon-project":"contextid-us-neon-project", "site-code2-upstash":"upstash", "site-code2-upstash-redis":"contextid-us-upstash-redis", "site-code2-reconcile":"infra-reconcile", "site-code2-release":"application-release", "site-code2-strict-public-readiness":"post-strict-public-readiness"} {
		if hasNoParentLegacyAlias(requireResource(t, resourcesByName(mocks.resources), name), oldName) { t.Fatalf("clean %s unexpectedly has legacy alias %q", name, oldName) }
	}
}

func TestSanitizedLegacyStackShapeMatchesAdoptedGraph(t *testing.T) {
	contents, err := os.ReadFile("../test/fixtures/legacy-code2-stack-shape.json"); if err != nil { t.Fatal(err) }
	var fixture struct { Deployment struct { Resources []legacyFixtureResource `json:"resources"` } `json:"deployment"` }
	if err := json.Unmarshal(contents, &fixture); err != nil { t.Fatal(err) }
	spec, layouts, err := ValidateHostSpec(validHostSpec()); if err != nil { t.Fatal(err) }
	for index := range layouts { if layouts[index].SiteID == "code2" { layouts[index] = legacyCode2Layout(layouts[index]) } }
	mocks := &graphMocks{}
	err = pulumi.RunErr(func(ctx *pulumi.Context) error { _, err := deployHostGraph(ctx, spec, layouts, secretHostSpec(spec), "edge-v1", "site-v1", "host-v1", true); return err }, pulumi.WithMocks("sub2api-vps-deploy", "test", mocks)); if err != nil { t.Fatal(err) }
	current := resourcesByName(mocks.resources)
	mapping := map[string]string{"cloudflare":"cloudflare", "cloudflare-full-strict":"cloudflare-full-strict", "sub2api-origin":"site-code2-origin", "neon":"site-code2-neon", "contextid-us-neon-project":"site-code2-neon-project", "upstash":"site-code2-upstash", "contextid-us-upstash-redis":"site-code2-upstash-redis", "infra-reconcile":"site-code2-reconcile", "post-strict-public-readiness":"site-code2-strict-public-readiness", "application-release":"site-code2-release"}
	fixtureByURN := map[string]legacyFixtureResource{}
	for _, resource := range fixture.Deployment.Resources { fixtureByURN[resource.URN] = resource }
	if len(fixture.Deployment.Resources) != len(mapping) { t.Fatal("sanitized fixture must cover every persisted resource") }
	for _, old := range fixture.Deployment.Resources {
		name := mapping[old.Name]; item := requireResource(t, current, name)
		if item.TypeToken != old.Type { t.Fatalf("%s type %q, want fixture %q", name, item.TypeToken, old.Type) }
		assertNoParentAlias(t, item, old.Name)
		if old.URN == "" || old.ID == "" || old.Parent != "" { t.Fatalf("fixture %s must model a persisted top-level resource", old.Name) }
		if old.Provider != "" {
			providerURN := strings.TrimSuffix(old.Provider, "::"+providerIDFromFixture(t, fixtureByURN, old.Provider))
			provider, ok := fixtureByURN[providerURN]
			if !ok || provider.ID == "" || old.Provider != provider.URN+"::"+provider.ID { t.Fatalf("fixture %s provider reference does not resolve", old.Name) }
		}
		if item.RegisterRPC.GetParent() == "" { t.Fatalf("%s must have a new component parent", name) }
		if (old.Name == "contextid-us-neon-project" || old.Name == "contextid-us-upstash-redis") && (!item.RegisterRPC.GetProtect() || !item.RegisterRPC.GetRetainOnDelete()) { t.Fatalf("%s must gain protect/retain during adoption", name) }
		if old.Name == "contextid-us-neon-project" && !strings.Contains(strings.Join(item.RegisterRPC.GetIgnoreChanges(), ","), "org_id") { t.Fatal("Neon org_id must be preserved") }
		if old.Name == "upstash" && !strings.Contains(strings.Join(item.RegisterRPC.GetIgnoreChanges(), ","), "email") { t.Fatal("Upstash email must be preserved") }
		if old.Name == "cloudflare-full-strict" && item.Inputs["settingId"].StringValue() != old.Inputs["settingId"] { t.Fatal("SSL input drift") }
		if old.Provider != "" && item.RegisterRPC.GetProvider() == "" { t.Fatalf("%s lost provider relationship", name) }
		if old.Name == "sub2api-origin" && !strings.Contains(item.RegisterRPC.GetProvider(), "cloudflare") { t.Fatal("DNS must use shared Cloudflare provider") }
		if old.Name == "contextid-us-neon-project" && !strings.Contains(item.RegisterRPC.GetProvider(), "site-code2-neon") { t.Fatal("Neon project must use Site Neon provider") }
		if old.Name == "contextid-us-upstash-redis" && !strings.Contains(item.RegisterRPC.GetProvider(), "site-code2-upstash") { t.Fatal("Upstash database must use Site Upstash provider") }
		if old.Name == "sub2api-origin" { assertFixtureInputs(t, item, old.Inputs, "zoneId", "name", "type", "content", "proxied", "ttl") }
		if old.Name == "contextid-us-neon-project" { assertFixtureInputs(t, item, old.Inputs, "name"); if old.Inputs["org_id"] == nil || !strings.Contains(strings.Join(item.RegisterRPC.GetIgnoreChanges(), ","), "org_id") { t.Fatal("Neon historical org_id projection drift") } }
		if old.Name == "contextid-us-upstash-redis" { assertFixtureInputs(t, item, old.Inputs, "databaseName", "region", "tls") }
	}
}

func providerIDFromFixture(t *testing.T, resources map[string]legacyFixtureResource, reference string) string { t.Helper(); for urn, resource := range resources { if reference == urn+"::"+resource.ID { return resource.ID } }; t.Fatalf("fixture provider reference %q has no exact provider URN+ID", reference); return "" }

func assertFixtureInputs(t *testing.T, item pulumi.MockResourceArgs, inputs map[string]interface{}, keys ...string) { t.Helper(); for _, key := range keys { want, ok := inputs[key]; if !ok { t.Fatalf("fixture omits stable input %s", key) }; got, ok := item.Inputs[resource.PropertyKey(key)]; if !ok { t.Fatalf("%s omits stable input %s", item.Name, key) }; switch expected := want.(type) { case string: if got.StringValue() != expected { t.Fatalf("%s %s = %q, want %q", item.Name, key, got.StringValue(), expected) }; case bool: if got.BoolValue() != expected { t.Fatalf("%s %s = %t, want %t", item.Name, key, got.BoolValue(), expected) }; case float64: if got.NumberValue() != expected { t.Fatalf("%s %s = %v, want %v", item.Name, key, got.NumberValue(), expected) }; default: t.Fatalf("unsupported fixture input %s", key) } } }

func TestCode2ImageChangeOnlyChangesCode2Release(t *testing.T) {
	before, _ := deployTwoSiteHost(t, "weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	after, _ := deployTwoSiteHost(t, "weishaw/sub2api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	beforeResources, afterResources := resourcesByName(before.resources), resourcesByName(after.resources)
	for name, beforeCommand := range beforeResources {
		if name == "site-code2-release" || name == "host-preflight" { continue }
		afterCommand, ok := afterResources[name]
		if !ok { t.Fatalf("%s disappeared after a code2 image update", name) }
		if beforeCommand.TypeToken == "command:local:Command" && !reflect.DeepEqual(beforeCommand.Inputs, afterCommand.Inputs) {
			t.Fatalf("%s inputs changed after a code2 image update", name)
		}
	}
	if reflect.DeepEqual(beforeResources["site-code2-release"].Inputs, afterResources["site-code2-release"].Inputs) {
		t.Fatal("code2 release triggers did not change with code2 image")
	}
	if reflect.DeepEqual(beforeResources["host-preflight"].Inputs, afterResources["host-preflight"].Inputs) {
		t.Fatal("host preflight did not change with code2 image")
	}
}

func TestHostGraphExportsPublicSiteStatusOnly(t *testing.T) {
	spec, layouts, err := ValidateHostSpec(validHostSpec())
	if err != nil { t.Fatal(err) }
	mocks := &graphMocks{}
	var exported map[string]interface{}
	err = pulumi.RunErr(func(ctx *pulumi.Context) error {
		exports, err := deployHostGraph(ctx, spec, layouts, secretHostSpec(spec), "edge-v1", "site-v1", "host-v1")
		if err != nil { return err }
		exports.Sites.ApplyT(func(value map[string]interface{}) map[string]interface{} { exported = value; return value })
		return nil
	}, pulumi.WithMocks("sub2api-vps-deploy", "test", mocks))
	if err != nil { t.Fatal(err) }
	for _, siteID := range []string{"code2", "code3"} {
		status, ok := exported[siteID].(map[string]interface{})
		if !ok { t.Fatalf("%s public status = %#v", siteID, exported[siteID]) }
		for _, key := range []string{"domain", "dnsRecordId", "readinessId", "deploymentId"} { if _, ok := status[key]; !ok { t.Fatalf("%s status missing %s: %#v", siteID, key, status) } }
		encoded := status["domain"].(string)
		for _, forbidden := range []string{"secret", "postgresql://", "redis"} { if strings.Contains(encoded, forbidden) { t.Fatalf("%s export leaks %q", siteID, forbidden) } }
	}
}

func TestExistingUpstashEndpointIsInOnlyItsSiteRuntime(t *testing.T) {
	spec := validHostSpec()
	code2 := spec.Sites["code2"]
	code2.Database = DatabaseSpec{Mode: "docker", ResourceMode: "existing"}
	code2.Redis = RedisSpec{Mode: "upstash", ResourceMode: "existing", Endpoint: "cache.code2.upstash.io"}
	spec.Sites["code2"] = code2
	code2Secrets := spec.SiteSecrets["code2"]
	code2Secrets.Database = DatabaseSecrets{Password: "code2-postgres-secret"}
	code2Secrets.Redis = RedisSecrets{Password: "code2-upstash-secret"}
	spec.SiteSecrets["code2"] = code2Secrets
	resolved, layouts, err := ValidateHostSpec(spec)
	if err != nil { t.Fatal(err) }
	mocks := &graphMocks{}
	err = pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := deployHostGraph(ctx, resolved, layouts, secretHostSpec(resolved), "edge-v1", "site-v1", "host-v1")
		return err
	}, pulumi.WithMocks("sub2api-vps-deploy", "test", mocks))
	if err != nil { t.Fatal(err) }
	runtime := requireResource(t, resourcesByName(mocks.resources), "site-code2-reconcile").Inputs["environment"].ObjectValue()["RUNTIME_JSON"]
	if !strings.Contains(runtime.SecretValue().Element.StringValue(), "cache.code2.upstash.io") { t.Fatalf("code2 runtime omits configured Upstash endpoint") }
}

func TestDockerPostgresRuntimeIncludesLocalServiceInputs(t *testing.T) {
	spec := validHostSpec()
	code2 := spec.Sites["code2"]
	code2.Database, code2.Redis = DatabaseSpec{Mode: "docker", ResourceMode: "existing"}, RedisSpec{Mode: "docker", ResourceMode: "existing"}
	spec.Sites["code2"] = code2
	code2Secrets := spec.SiteSecrets["code2"]
	code2Secrets.Database, code2Secrets.Redis = DatabaseSecrets{Password: "code2-postgres-secret"}, RedisSecrets{Password: "code2-redis-secret"}
	spec.SiteSecrets["code2"] = code2Secrets
	resolved, layouts, err := ValidateHostSpec(spec)
	if err != nil { t.Fatal(err) }
	mocks := &graphMocks{}
	err = pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := deployHostGraph(ctx, resolved, layouts, secretHostSpec(resolved), "edge-v1", "site-v1", "host-v1")
		return err
	}, pulumi.WithMocks("sub2api-vps-deploy", "test", mocks))
	if err != nil { t.Fatal(err) }
	runtime := requireResource(t, resourcesByName(mocks.resources), "site-code2-reconcile").Inputs["environment"].ObjectValue()["RUNTIME_JSON"]
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(runtime.SecretValue().Element.StringValue()), &payload); err != nil { t.Fatal(err) }
	for key, want := range map[string]interface{}{"POSTGRES_PASSWORD": "code2-postgres-secret", "POSTGRES_USER": "sub2api", "POSTGRES_DB": "sub2api", "DATABASE_HOST": "postgres"} {
		if got := payload[key]; got != want { t.Fatalf("%s = %#v, want %#v", key, got, want) }
	}
}

func deployTwoSiteHost(t *testing.T, code2Image string) (*graphMocks, HostGraphExports) {
	t.Helper()
	spec := validHostSpec()
	code2 := spec.Sites["code2"]
	code2.Image = code2Image
	spec.Sites["code2"] = code2
	resolved, layouts, err := ValidateHostSpec(spec)
	if err != nil { t.Fatalf("ValidateHostSpec() error = %v", err) }
	mocks := &graphMocks{}
	var exports HostGraphExports
	err = pulumi.RunErr(func(ctx *pulumi.Context) error {
		var err error
		exports, err = deployHostGraph(ctx, resolved, layouts, secretHostSpec(resolved), "edge-v1", "site-v1", "host-v1")
		return err
	}, pulumi.WithMocks("sub2api-vps-deploy", "test", mocks))
	if err != nil { t.Fatalf("host graph failed: %v", err) }
	return mocks, exports
}

func assertSiteCommands(t *testing.T, resources map[string]pulumi.MockResourceArgs, siteID, previousSiteID string) {
	t.Helper()
	for _, suffix := range []string{"reconcile", "strict-public-readiness", "release", "rollback-preparation"} {
		command := requireResource(t, resources, "site-"+siteID+"-"+suffix)
		if command.TypeToken != "command:local:Command" { t.Fatalf("%s type = %q", command.Name, command.TypeToken) }
		environment := command.Inputs["environment"].ObjectValue()
		for _, key := range []string{"SITE_ID", "SITE_RUNTIME_ROOT", "COMPOSE_PROJECT_NAME", "SITE_ROUTE_PATH", "SITE_RUNTIME_ENV_PATH", "SITE_DEPLOY_STATE_PATH", "SITE_BOOTSTRAP_MARKER_PATH", "BLUE_DATA_PATH", "GREEN_DATA_PATH", "BLUE_EDGE_ALIAS", "GREEN_EDGE_ALIAS", "ACTIVE_EDGE_ALIAS", "EDGE_NETWORK_NAME", "DOMAIN", "ORIGIN_IP", "APP_PROBE_PATH", "DRAIN_SECONDS", "RUNTIME_JSON"} {
			if _, ok := environment[resource.PropertyKey(key)]; !ok { t.Fatalf("%s missing %s", command.Name, key) }
		}
		if suffix == "release" { if _, ok := environment["SUB2API_IMAGE"]; !ok { t.Fatalf("%s missing SUB2API_IMAGE", command.Name) } } else if _, ok := environment["SUB2API_IMAGE"]; ok { t.Fatalf("%s unexpectedly owns SUB2API_IMAGE", command.Name) }
		if command.Inputs["logging"].StringValue() != "none" || !environment["RUNTIME_JSON"].IsSecret() { t.Fatalf("%s command secrecy/logging = %v", command.Name, command.Inputs) }
		if suffix == "rollback-preparation" && strings.Contains(command.Inputs["create"].StringValue(), "rollback-slot.sh") { t.Fatalf("%s invokes deferred rollback orchestration", command.Name) }
		if suffix == "rollback-preparation" { assertDependsOn(t, command, "site-"+siteID+"-strict-public-readiness", "must wait for public readiness") }
		if previousSiteID != "" && suffix == "reconcile" {
			previous := requireResource(t, resources, "site-"+previousSiteID+"-rollback-preparation")
			if len(command.RegisterRPC.GetDependencies()) == 0 || !strings.Contains(strings.Join(command.RegisterRPC.GetDependencies(), " "), previous.Name) {
				t.Fatalf("%s is not serialized after %s: %v", command.Name, previousSiteID, command.RegisterRPC.GetDependencies())
			}
		}
	}
}

func assertSiteRuntimeIsolation(t *testing.T, resources map[string]pulumi.MockResourceArgs, siteID, otherSiteID string) {
	t.Helper()
	runtime := requireResource(t, resources, "site-"+siteID+"-reconcile").Inputs["environment"].ObjectValue()["RUNTIME_JSON"]
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(runtime.SecretValue().Element.StringValue()), &payload); err != nil { t.Fatalf("%s runtime JSON = %v", siteID, err) }
	if payload["SITE_ID"] != siteID { t.Fatalf("runtime SITE_ID = %v, want %q", payload["SITE_ID"], siteID) }
	encoded := runtime.SecretValue().Element.StringValue()
	for _, forbidden := range []string{otherSiteID, "code3.contextid.cn", "cloudflare-secret", "code3-admin-secret", "code3-jwt-secret", "code3-totp-secret", "TRAEFIK_IMAGE", "ACME_EMAIL", "SING_BOX", "SUB2API_IMAGE"} {
		if strings.Contains(encoded, forbidden) { t.Fatalf("%s runtime payload leaks %q: %s", siteID, forbidden, encoded) }
	}
}

func assertPublicSiteExports(t *testing.T, exports HostGraphExports) {
	t.Helper()
	if exports.Sites.OutputState == nil { t.Fatalf("site exports are missing: %+v", exports) }
}

func resourcesByName(items []pulumi.MockResourceArgs) map[string]pulumi.MockResourceArgs { result := map[string]pulumi.MockResourceArgs{}; for _, item := range items { result[item.Name] = item }; return result }
func legacyCode2Layout(layout SiteLayout) SiteLayout { layout.ComposeProject, layout.RuntimeRoot, layout.RuntimeEnvPath, layout.DeployStatePath, layout.BootstrapMarkerPath, layout.BlueDataPath, layout.GreenDataPath, layout.BlueAlias, layout.GreenAlias = "sub2api", "runtime", "runtime/runtime.env", "runtime/deploy-state.json", "runtime/bootstrap.marker", "runtime/data/blue", "runtime/data/green", "sub2api-blue", "sub2api-green"; return layout }
func requireResource(t *testing.T, resources map[string]pulumi.MockResourceArgs, name string) pulumi.MockResourceArgs { t.Helper(); item, ok := resources[name]; if !ok { t.Fatalf("missing resource %q", name) }; return item }
func requireSiteProvider(t *testing.T, items []pulumi.MockResourceArgs, siteID, provider string) pulumi.MockResourceArgs { t.Helper(); prefix := "site-" + siteID + "-"; for _, item := range items { if strings.HasPrefix(item.Name, prefix) && strings.Contains(item.TypeToken, "providers:"+provider) { return item } }; t.Fatalf("missing Site-qualified %s provider for %s", provider, siteID); return pulumi.MockResourceArgs{} }
func countResources(items []pulumi.MockResourceArgs, token string) int { count := 0; for _, item := range items { if item.TypeToken == token { count++ } }; return count }
func assertParent(t *testing.T, item pulumi.MockResourceArgs, want, message string) { t.Helper(); if item.RegisterRPC == nil || item.RegisterRPC.GetParent() != want { t.Fatalf("%s parent = %q, want %q: %s", item.Name, item.RegisterRPC.GetParent(), want, message) } }
func assertDependsOn(t *testing.T, item pulumi.MockResourceArgs, name, message string) { t.Helper(); if item.RegisterRPC == nil || !strings.Contains(strings.Join(item.RegisterRPC.GetDependencies(), " "), name) { t.Fatalf("%s dependencies = %v: %s", item.Name, item.RegisterRPC.GetDependencies(), message) } }
func hasNoParentLegacyAlias(item pulumi.MockResourceArgs, oldName string) bool { if item.RegisterRPC == nil { return false }; for _, alias := range item.RegisterRPC.GetAliases() { spec := alias.GetSpec(); if spec != nil && spec.GetNoParent() && (oldName == "" || spec.GetName() == oldName) { return true } }; return false }
func assertNoParentAlias(t *testing.T, item pulumi.MockResourceArgs, oldName string) { t.Helper(); if !hasNoParentLegacyAlias(item, oldName) { t.Fatalf("%s aliases = %+v, want old no-parent name %q", item.Name, item.RegisterRPC.GetAliases(), oldName) } }
func siteChildren(items []pulumi.MockResourceArgs, siteID string) []pulumi.MockResourceArgs { result := []pulumi.MockResourceArgs{}; for _, item := range items { if item.TypeToken != siteComponentToken && strings.HasPrefix(item.Name, "site-"+siteID+"-") { result = append(result, item) } }; return result }
func commandTriggers(item pulumi.MockResourceArgs) []resource.PropertyValue { return item.Inputs["triggers"].ArrayValue() }

func secretHostSpec(host HostSpec) SecretHostSpec {
	secrets := SecretHostSpec{Edge: pulumi.ToSecret(pulumi.String(host.EdgeSecrets.CloudflareAPIToken)).(pulumi.StringOutput), Sites: map[string]pulumi.StringOutput{}}
	for siteID, siteSecrets := range host.SiteSecrets { secrets.Sites[siteID] = pulumi.ToSecret(pulumi.String(marshalRuntimeSecrets(siteSecrets))).(pulumi.StringOutput) }
	return secrets
}
