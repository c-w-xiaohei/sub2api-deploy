package main

import (
	"reflect"
	"testing"
)

func TestBuildEdgeTriggersHaveOnlyEdgeInputsInStableOrder(t *testing.T) {
	got := BuildEdgeTriggers(EdgeTriggerInput{
		TraefikImage: "traefik:v3.3.3", ACMEEmail: "ops@example.com",
		SingBoxConfig: `{"server":"www.cloudflare.com","target":"host.docker.internal:8443"}`,
		EdgeChecksum: "edge-v1",
	})
	want := []string{"edge-reconcile-v1", "traefik:v3.3.3", "ops@example.com", `{"server":"www.cloudflare.com","target":"host.docker.internal:8443"}`, "edge-v1"}
	if !reflect.DeepEqual(got, want) { t.Fatalf("edge triggers = %#v, want %#v", got, want) }
}

func TestBuildSiteTriggersAreStableAndExcludeEdgeInputs(t *testing.T) {
	got := BuildSiteReconcileTriggers(SiteTriggerInput{
		SiteID: "code2", Domain: "code2.contextid.cn", RuntimeRoot: "runtime/sites/code2",
		ComposeProject: "sub2api-code2", RoutePath: "runtime/edge/dynamic/site-code2.yml",
		Image: "weishaw/sub2api@sha256:abcdef", SiteChecksum: "site-v1",
	})
	want := []string{"site-reconcile-v1", "code2", "code2.contextid.cn", "runtime/sites/code2", "sub2api-code2", "runtime/edge/dynamic/site-code2.yml", "weishaw/sub2api@sha256:abcdef", "site-v1"}
	if !reflect.DeepEqual(got, want) { t.Fatalf("site triggers = %#v, want %#v", got, want) }
	for _, forbidden := range []string{"traefik", "ops@example.com", "host.docker.internal", "edge-v1"} { for _, value := range got { if value == forbidden { t.Fatalf("site trigger includes edge input %q", value) } } }
}

func TestBuildSiteReleaseTriggersChangeOnlyForThatSitesImage(t *testing.T) {
	code2Old := BuildSiteReleaseTriggers("code2", "weishaw/sub2api@sha256:old")
	code2New := BuildSiteReleaseTriggers("code2", "weishaw/sub2api@sha256:new")
	code3 := BuildSiteReleaseTriggers("code3", "weishaw/sub2api@sha256:old")
	if reflect.DeepEqual(code2Old, code2New) { t.Fatal("code2 release triggers ignore its image") }
	if !reflect.DeepEqual(code3, []string{"site-release-v1", "code3", "weishaw/sub2api@sha256:old"}) { t.Fatalf("code3 release triggers = %#v", code3) }
}
