package main

import "testing"

func TestBuildInfraTriggersPreservesOrder(t *testing.T) {
	got := BuildInfraTriggers(InfraTriggerInput{
		ResourceNamespace: "sub2api",
		Domain:            "sub2api.example.com",
		OriginIP:          "203.0.113.10",
		PostgresMode:      "docker",
		RedisMode:         "docker",
		TraefikImage:      "traefik:v3.3.3",
		ACMEEmail:         "ops@example.com",
		AppProbePath:      "/api/ready",
		DrainSeconds:      10,
		ComposeChecksum:   "compose-v1",
		ResourceModes:     "existing/existing",
	})
	want := []string{
		"infra-reconcile-v1", "sub2api", "sub2api.example.com", "203.0.113.10",
		"docker", "docker", "traefik:v3.3.3", "ops@example.com", "/api/ready",
		"10", "compose-v1", "existing/existing",
	}
	if len(got) != len(want) {
		t.Fatalf("trigger count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trigger[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildReleaseTriggersAreImageOnly(t *testing.T) {
	got := BuildReleaseTriggers("image@sha256:new")
	if len(got) != 1 || got[0] != "image@sha256:new" {
		t.Fatalf("release triggers = %#v", got)
	}
}

func TestBuildResourceModesPreservesSerializedConfig(t *testing.T) {
	config := DeploymentConfig{DeploymentInput: DeploymentInput{
		NeonResourceMode:    "existing",
		UpstashResourceMode: "create",
	}}
	if got := BuildResourceModes(config); got != `{"postgresResourceMode":"existing","redisResourceMode":"create"}` {
		t.Fatalf("resource modes = %q", got)
	}
}
