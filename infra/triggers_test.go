package main

import (
	"reflect"
	"testing"
)

func TestBuildEdgeTriggersHaveOnlyEdgeInputsInStableOrder(t *testing.T) {
	got := BuildEdgeTriggers(EdgeTriggerInput{
		TraefikImage: "traefik:v3.3.3", ACMEEmail: "ops@example.com",
		SingBoxConfig: `{"server":"www.cloudflare.com","target":"host.docker.internal:8443"}`,
		EdgeChecksum:  "edge-v1",
	})
	want := []string{"edge-reconcile-v1", "traefik:v3.3.3", "ops@example.com", `{"server":"www.cloudflare.com","target":"host.docker.internal:8443"}`, "edge-v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edge triggers = %#v, want %#v", got, want)
	}
}

func TestBuildSiteTriggersAreStableAndExcludeEdgeInputs(t *testing.T) {
	got := BuildSiteReconcileTriggers(SiteTriggerInput{
		SiteID: "code2", Domain: "code2.contextid.cn", RuntimeRoot: "runtime/sites/code2",
		ComposeProject: "sub2api-code2", RoutePath: "runtime/edge/dynamic/site-code2.yml",
		SiteChecksum: "site-v1",
	})
	want := []string{"site-reconcile-v1", "code2", "code2.contextid.cn", "runtime/sites/code2", "sub2api-code2", "runtime/edge/dynamic/site-code2.yml", "site-v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("site triggers = %#v, want %#v", got, want)
	}
	for _, forbidden := range []string{"traefik", "ops@example.com", "host.docker.internal", "edge-v1"} {
		for _, value := range got {
			if value == forbidden {
				t.Fatalf("site trigger includes edge input %q", value)
			}
		}
	}
}

func TestBuildSiteReleaseTriggersChangeOnlyForThatSitesImage(t *testing.T) {
	code2Old := BuildSiteReleaseTriggers("code2", "weishaw/sub2api@sha256:old")
	code2New := BuildSiteReleaseTriggers("code2", "weishaw/sub2api@sha256:new")
	code3 := BuildSiteReleaseTriggers("code3", "weishaw/sub2api@sha256:old")
	if reflect.DeepEqual(code2Old, code2New) {
		t.Fatal("code2 release triggers ignore its image")
	}
	if !reflect.DeepEqual(code3, []string{"site-release-v1", "code3", "weishaw/sub2api@sha256:old"}) {
		t.Fatalf("code3 release triggers = %#v", code3)
	}
}

func TestHostDesiredStateDigestTracksOrdinarySideEffectsOnly(t *testing.T) {
	host, layouts, err := ValidateHostSpec(validHostSpec())
	if err != nil {
		t.Fatal(err)
	}
	base, err := hostDesiredStateDigest(host, layouts)
	if err != nil {
		t.Fatal(err)
	}
	secretChanged := HostSpec{Edge: host.Edge, Sites: host.Sites, EdgeSecrets: host.EdgeSecrets, SiteSecrets: host.SiteSecrets}
	secretChanged.EdgeSecrets.CloudflareAPIToken = "different-secret"
	secretDigest, err := hostDesiredStateDigest(secretChanged, layouts)
	if err != nil {
		t.Fatal(err)
	}
	if base != secretDigest {
		t.Fatal("host desired-state digest includes secrets")
	}
	imageChanged := host
	code2 := imageChanged.Sites["code2"]
	code2.Image = "weishaw/sub2api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	imageChanged.Sites["code2"] = code2
	imageDigest, err := hostDesiredStateDigest(imageChanged, layouts)
	if err != nil {
		t.Fatal(err)
	}
	if base == imageDigest {
		t.Fatal("host desired-state digest ignores Site image")
	}
	edgeChanged := host
	edgeChanged.Edge.ACMEEmail = "new-ops@example.com"
	edgeDigest, err := hostDesiredStateDigest(edgeChanged, layouts)
	if err != nil {
		t.Fatal(err)
	}
	if base == edgeDigest {
		t.Fatal("host desired-state digest ignores Edge config")
	}
	siteChanged := host
	code3 := siteChanged.Sites["code3"]
	code3.Redis.Endpoint = "cache.changed.upstash.io"
	siteChanged.Sites["code3"] = code3
	siteDigest, err := hostDesiredStateDigest(siteChanged, layouts)
	if err != nil {
		t.Fatal(err)
	}
	if base == siteDigest {
		t.Fatal("host desired-state digest ignores Site data config")
	}
}

func TestChecksumBoundariesKeepOwnerSpecificFilesIsolated(t *testing.T) {
	edgePaths := map[string]bool{}
	for _, path := range edgeChecksumPaths {
		edgePaths[path] = true
	}
	for _, required := range []string{"compose/edge.yml", "scripts/edge-compose-common.sh", "scripts/reconcile-edge.sh", "scripts/render-edge-config.ts", "scripts/render-runtime-env.ts", "traefik/dynamic/sing-box.yml"} {
		if !edgePaths[required] {
			t.Fatalf("edge checksum omits %q", required)
		}
	}
	sitePaths := map[string]bool{}
	for _, path := range siteChecksumPaths {
		sitePaths[path] = true
	}
	for _, required := range []string{"compose/site.yml", "compose/upstream.yml", "scripts/site-compose-common.sh", "scripts/read-runtime-env.cjs", "scripts/reconcile-site.sh", "scripts/bootstrap-site.sh", "scripts/application-release.sh", "scripts/switch-slot.sh", "scripts/rollback-slot.sh", "scripts/render-site-route.ts", "scripts/render-runtime-env.ts", "scripts/verify-legacy-app-env.ts", "scripts/deployment-mode.ts", "scripts/write-deploy-state.ts", "scripts/write-bootstrap-marker.ts", "src/deployment-preflight.ts", "traefik/dynamic/site.yml"} {
		if !sitePaths[required] {
			t.Fatalf("site checksum omits %q", required)
		}
	}
	for _, edgeOnly := range []string{"compose/edge.yml", "scripts/edge-compose-common.sh", "scripts/reconcile-edge.sh", "scripts/render-edge-config.ts", "traefik/dynamic/sing-box.yml"} {
		if sitePaths[edgeOnly] {
			t.Fatalf("Site checksum includes Edge-only path %q", edgeOnly)
		}
	}
	for _, siteOnly := range []string{"compose/site.yml", "compose/upstream.yml", "scripts/site-compose-common.sh", "scripts/read-runtime-env.cjs", "scripts/reconcile-site.sh", "scripts/bootstrap-site.sh", "scripts/application-release.sh", "scripts/switch-slot.sh", "scripts/rollback-slot.sh", "scripts/render-site-route.ts", "scripts/deployment-mode.ts", "scripts/write-deploy-state.ts", "scripts/write-bootstrap-marker.ts", "src/deployment-preflight.ts", "traefik/dynamic/site.yml"} {
		if edgePaths[siteOnly] {
			t.Fatalf("Edge checksum includes Site-only path %q", siteOnly)
		}
	}
	hostPaths := map[string]bool{}
	for _, path := range hostChecksumPaths {
		hostPaths[path] = true
	}
	endpointPaths := map[string]bool{}
	for _, path := range neonEndpointChecksumPaths {
		endpointPaths[path] = true
	}
	for _, required := range []string{"scripts/node-env.sh", "scripts/create-neon-project.ts", "scripts/fetch-neon-connection.ts", "scripts/reconcile-neon-endpoint.ts", "scripts/validate-neon-region.ts"} {
		if !endpointPaths[required] {
			t.Fatalf("Neon endpoint checksum omits %q", required)
		}
		if edgePaths[required] || sitePaths[required] || hostPaths[required] {
			t.Fatalf("Neon endpoint-only path %q is owned by another checksum", required)
		}
	}
	for _, path := range hostChecksumPaths {
		if edgePaths[path] || sitePaths[path] {
			t.Fatalf("host checksum path %q overlaps Edge or Site", path)
		}
	}
	for _, required := range []string{"scripts/host-preflight.ts", "scripts/finalize-host-state.sh", "scripts/write-host-state.cjs"} {
		if !hostPaths[required] {
			t.Fatalf("host checksum omits %q", required)
		}
	}
}
