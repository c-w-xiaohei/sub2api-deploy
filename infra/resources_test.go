package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	edgeComponentToken = "sub2api:host:Edge"
	siteComponentToken = "sub2api:host:Site"
)

type graphMocks struct{ resources []pulumi.MockResourceArgs }

func (m *graphMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.resources = append(m.resources, args)
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
	mocks, exports := deployTwoSiteHost(t, "weishaw/sub2api@sha256:abcdef1234567890")
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

	for _, siteID := range []string{"code2", "code3"} {
		site := requireResource(t, resources, "site-"+siteID)
		if site.TypeToken != siteComponentToken {
			t.Fatalf("%s type = %q, want %q", siteID, site.TypeToken, siteComponentToken)
		}
		assertParent(t, site, "", "site component must be host-owned")
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
	mocks, _ := deployTwoSiteHost(t, "weishaw/sub2api@sha256:abcdef1234567890")
	resources := resourcesByName(mocks.resources)
	for _, siteID := range []string{"code2", "code3"} {
		siteOwnedChildren := siteChildren(mocks.resources, siteID)
		if len(siteOwnedChildren) == 0 { t.Fatalf("%s has no Site-owned children", siteID) }
		parent := siteOwnedChildren[0].RegisterRPC.GetParent()
		for _, suffix := range []string{"neon", "neon-project", "upstash", "upstash-redis"} {
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

func TestCode2ImageChangeOnlyChangesCode2Release(t *testing.T) {
	before, _ := deployTwoSiteHost(t, "weishaw/sub2api@sha256:abcdef1234567890")
	after, _ := deployTwoSiteHost(t, "weishaw/sub2api@sha256:fedcba0987654321")
	beforeResources, afterResources := resourcesByName(before.resources), resourcesByName(after.resources)
	for _, name := range []string{"edge-reconcile", "site-code3-reconcile", "site-code3-strict-public-readiness", "site-code3-release", "site-code3-rollback-preparation"} {
		if !reflect.DeepEqual(commandTriggers(beforeResources[name]), commandTriggers(afterResources[name])) {
			t.Fatalf("%s triggers changed after a code2 image update", name)
		}
	}
	if reflect.DeepEqual(commandTriggers(beforeResources["site-code2-release"]), commandTriggers(afterResources["site-code2-release"])) {
		t.Fatal("code2 release triggers did not change with code2 image")
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
		exports, err = deployHostGraph(ctx, resolved, layouts, secretHostSpec(resolved), "site-compose-v1")
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
		for _, key := range []string{"SITE_ID", "SITE_RUNTIME_ROOT", "COMPOSE_PROJECT_NAME", "SITE_ROUTE_PATH", "EDGE_NETWORK_NAME", "RUNTIME_JSON"} {
			if _, ok := environment[resource.PropertyKey(key)]; !ok { t.Fatalf("%s missing %s", command.Name, key) }
		}
		if command.Inputs["logging"].StringValue() != "none" || !environment["RUNTIME_JSON"].IsSecret() { t.Fatalf("%s command secrecy/logging = %v", command.Name, command.Inputs) }
		if previousSiteID != "" {
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
	for _, forbidden := range []string{otherSiteID, "code3.contextid.cn", "cloudflare-secret", "TRAEFIK_IMAGE", "ACME_EMAIL", "SING_BOX"} {
		if strings.Contains(encoded, forbidden) { t.Fatalf("%s runtime payload leaks %q: %s", siteID, forbidden, encoded) }
	}
}

func assertPublicSiteExports(t *testing.T, exports HostGraphExports) {
	t.Helper()
	if exports.Sites.OutputState == nil { t.Fatalf("site exports are missing: %+v", exports) }
}

func resourcesByName(items []pulumi.MockResourceArgs) map[string]pulumi.MockResourceArgs { result := map[string]pulumi.MockResourceArgs{}; for _, item := range items { result[item.Name] = item }; return result }
func requireResource(t *testing.T, resources map[string]pulumi.MockResourceArgs, name string) pulumi.MockResourceArgs { t.Helper(); item, ok := resources[name]; if !ok { t.Fatalf("missing resource %q", name) }; return item }
func countResources(items []pulumi.MockResourceArgs, token string) int { count := 0; for _, item := range items { if item.TypeToken == token { count++ } }; return count }
func assertParent(t *testing.T, item pulumi.MockResourceArgs, want, message string) { t.Helper(); if item.RegisterRPC == nil || item.RegisterRPC.GetParent() != want { t.Fatalf("%s parent = %q, want %q: %s", item.Name, item.RegisterRPC.GetParent(), want, message) } }
func siteChildren(items []pulumi.MockResourceArgs, siteID string) []pulumi.MockResourceArgs { result := []pulumi.MockResourceArgs{}; for _, item := range items { if strings.HasPrefix(item.Name, "site-"+siteID+"-") { result = append(result, item) } }; return result }
func commandTriggers(item pulumi.MockResourceArgs) []resource.PropertyValue { return item.Inputs["triggers"].ArrayValue() }
