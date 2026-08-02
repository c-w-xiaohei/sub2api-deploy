package main

import (
	"encoding/json"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type graphMocks struct {
	resources []pulumi.MockResourceArgs
}

func (m *graphMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.resources = append(m.resources, args)
	state := args.Inputs.Copy()
	if args.TypeToken == "neon:resource:Project" {
		state["connection_uri"] = resource.NewStringProperty("postgresql://user:pass@ep.generated.neon.tech/db?sslmode=require")
	}
	if args.TypeToken == "upstash:index/redisDatabase:RedisDatabase" {
		state["endpoint"] = resource.NewStringProperty("redis.generated.upstash.io")
		state["port"] = resource.NewNumberProperty(6379)
		state["password"] = resource.NewStringProperty("redis-secret")
	}
	return args.Name + "-id", state, nil
}

func (m *graphMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

func mockSecrets() map[string]pulumi.StringOutput {
	secrets := map[string]pulumi.StringOutput{}
	for _, key := range []string{
		"postgresPassword", "redisPassword", "neonPassword", "neonDsn", "neonApiToken",
		"upstashPassword", "upstashApiKey", "cloudflareApiToken", "adminPassword", "jwtSecret", "totpEncryptionKey",
	} {
		secrets[key] = pulumi.ToSecret(pulumi.String(key + "-value")).(pulumi.StringOutput)
	}
	return secrets
}

func TestResourceGraphPreservesTypesNamesInputsAndEdges(t *testing.T) {
	mocks := &graphMocks{}
	var exports DeploymentExports
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		input := validDeploymentInput()
		input.DrainSeconds = intPointer(10)
		config, err := ValidateDeploymentConfig(input)
		if err != nil {
			return err
		}
		exports, err = deployResourceGraph(ctx, ProgramConfig{DeploymentConfig: config, Secrets: mockSecrets()}, "compose-v1")
		return err
	}, pulumi.WithMocks("sub2api-vps-deploy", "test", mocks))
	if err != nil {
		t.Fatalf("resource graph failed: %v", err)
	}
	if exports.DomainName.OutputState == nil || exports.DNSRecordID.OutputState == nil || exports.StrictReadinessID.OutputState == nil || exports.DeploymentID.OutputState == nil {
		t.Fatalf("deployment exports = %+v", exports)
	}

	byName := make(map[string]pulumi.MockResourceArgs)
	for _, args := range mocks.resources {
		byName[args.Name] = args
	}
	if byName["sub2api-origin"].TypeToken != "cloudflare:index/dnsRecord:DnsRecord" {
		t.Fatalf("DNS token = %q", byName["sub2api-origin"].TypeToken)
	}
	if byName["cloudflare"].RegisterRPC.GetVersion() != "6.18.0" || byName["sub2api-origin"].RegisterRPC.GetVersion() != "6.18.0" {
		t.Fatalf("Cloudflare versions = %q / %q", byName["cloudflare"].RegisterRPC.GetVersion(), byName["sub2api-origin"].RegisterRPC.GetVersion())
	}
	if byName["cloudflare-full-strict"].TypeToken != "cloudflare:index/zoneSetting:ZoneSetting" {
		t.Fatalf("SSL token = %q", byName["cloudflare-full-strict"].TypeToken)
	}
	if byName["sub2api-origin"].Inputs["type"].StringValue() != "A" || !byName["sub2api-origin"].Inputs["proxied"].BoolValue() {
		t.Fatalf("DNS inputs = %v", byName["sub2api-origin"].Inputs)
	}
	if byName["cloudflare-full-strict"].Inputs["settingId"].StringValue() != "ssl" || byName["cloudflare-full-strict"].Inputs["value"].StringValue() != "strict" {
		t.Fatalf("SSL inputs = %v", byName["cloudflare-full-strict"].Inputs)
	}

	infra := byName["infra-reconcile"]
	if infra.RegisterRPC == nil || !contains(infra.RegisterRPC.GetIgnoreChanges(), "environment.SUB2API_IMAGE") {
		t.Fatalf("infra options = %v", infra.RegisterRPC)
	}
	if !sameStrings(infra.RegisterRPC.GetAdditionalSecretOutputs(), []string{"stdout", "stderr"}) {
		t.Fatalf("infra secret outputs = %v", infra.RegisterRPC.GetAdditionalSecretOutputs())
	}
	if infra.Inputs["logging"].StringValue() != "none" || infra.Inputs["create"].StringValue() != "bash scripts/infra-reconcile.sh" {
		t.Fatalf("infra inputs = %v", infra.Inputs)
	}
	environment := infra.Inputs["environment"].ObjectValue()
	if !environment[resource.PropertyKey("RUNTIME_JSON")].IsSecret() {
		t.Fatal("runtime payload is not secret")
	}
	var runtimeFields map[string]interface{}
	if err := json.Unmarshal([]byte(environment[resource.PropertyKey("RUNTIME_JSON")].SecretValue().Element.StringValue()), &runtimeFields); err != nil {
		t.Fatalf("runtime payload JSON = %v", err)
	}
	for _, key := range []string{
		"DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER", "DATABASE_PASSWORD", "POSTGRES_PASSWORD", "POSTGRES_USER", "POSTGRES_DB", "DATABASE_DBNAME", "DATABASE_SSLMODE",
		"REDIS_HOST", "REDIS_PORT", "REDIS_USERNAME", "REDIS_PASSWORD", "REDIS_DB", "REDIS_ENABLE_TLS", "POSTGRES_MODE", "REDIS_MODE", "TRAEFIK_IMAGE",
		"SLOT", "SLOT_DATA_DIR", "BLUE_CONTAINER_NAME", "GREEN_CONTAINER_NAME", "POSTGRES_CONTAINER_NAME", "REDIS_CONTAINER_NAME", "AUTO_SETUP", "DOMAIN", "CLOUDFLARE_DNS_API_TOKEN",
		"ACME_EMAIL", "ORIGIN_IP", "APP_PROBE_PATH", "DRAIN_SECONDS", "ADMIN_EMAIL", "ADMIN_PASSWORD", "JWT_SECRET", "TOTP_ENCRYPTION_KEY",
	} {
		if _, ok := runtimeFields[key]; !ok {
			t.Errorf("runtime payload is missing %s", key)
		}
	}
	if len(infra.Inputs["triggers"].ArrayValue()) != 12 {
		t.Fatalf("infra triggers = %v", infra.Inputs["triggers"])
	}
	if infra.Inputs["triggers"].ArrayValue()[11].StringValue() != `{"postgresResourceMode":"existing","redisResourceMode":"existing"}` {
		t.Fatalf("resource mode trigger = %v", infra.Inputs["triggers"])
	}
	if len(infra.RegisterRPC.GetDependencies()) == 0 {
		t.Fatal("infra command has no DNS dependency")
	}

	readiness := byName["post-strict-public-readiness"]
	release := byName["application-release"]
	if len(readiness.RegisterRPC.GetDependencies()) == 0 || len(release.RegisterRPC.GetDependencies()) != 2 {
		t.Fatalf("readiness/release dependencies = %v / %v", readiness.RegisterRPC.GetDependencies(), release.RegisterRPC.GetDependencies())
	}
	if readiness.Inputs["create"].StringValue() != `bash scripts/probe-origin.sh "$DOMAIN" "/health"` {
		t.Fatalf("readiness command = %q", readiness.Inputs["create"].StringValue())
	}
	if release.Inputs["triggers"].ArrayValue()[0].StringValue() != "weishaw/sub2api@sha256:abcdef1234567890" {
		t.Fatalf("release triggers = %v", release.Inputs["triggers"])
	}
	for _, name := range []string{"infra-reconcile", "post-strict-public-readiness", "application-release"} {
		if byName[name].RegisterRPC.GetVersion() != "1.2.1" {
			t.Fatalf("Command %s version = %q", name, byName[name].RegisterRPC.GetVersion())
		}
	}
}

func TestManagedProviderResourcesPreserveLogicalNamesAndTokens(t *testing.T) {
	mocks := &graphMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		input := validDeploymentInput()
		input.ResourceNamespace = "tenant-a"
		input.PostgresMode = "neon"
		input.PostgresPassword = ""
		input.NeonResourceMode = "create"
		input.NeonAPIToken = "neon-token"
		input.NeonOrgID = "org-id"
		input.RedisMode = "upstash"
		input.RedisPassword = ""
		input.UpstashResourceMode = "create"
		input.UpstashAPIKey = "upstash-token"
		input.UpstashEmail = "ops@example.com"
		input.UpstashDatabaseName = "tenant-a-redis"
		input.UpstashRegion = "us-east-1"
		config, err := ValidateDeploymentConfig(input)
		if err != nil {
			return err
		}
		_, err = deployResourceGraph(ctx, ProgramConfig{DeploymentConfig: config, Secrets: mockSecrets()}, "compose-v1")
		return err
	}, pulumi.WithMocks("sub2api-vps-deploy", "managed", mocks))
	if err != nil {
		t.Fatalf("managed resource graph failed: %v", err)
	}
	byName := make(map[string]pulumi.MockResourceArgs)
	for _, args := range mocks.resources {
		byName[args.Name] = args
	}
	if byName["tenant-a-neon-project"].TypeToken != "neon:resource:Project" {
		t.Fatalf("Neon token = %q", byName["tenant-a-neon-project"].TypeToken)
	}
	if byName["neon"].RegisterRPC.GetVersion() != "0.0.1-alpha.1" || byName["tenant-a-neon-project"].RegisterRPC.GetVersion() != "0.0.1-alpha.1" {
		t.Fatalf("Neon versions = %q / %q", byName["neon"].RegisterRPC.GetVersion(), byName["tenant-a-neon-project"].RegisterRPC.GetVersion())
	}
	if byName["tenant-a-upstash-redis"].TypeToken != "upstash:index/redisDatabase:RedisDatabase" {
		t.Fatalf("Upstash token = %q", byName["tenant-a-upstash-redis"].TypeToken)
	}
	if byName["upstash"].RegisterRPC.GetVersion() != "0.5.0" || byName["tenant-a-upstash-redis"].RegisterRPC.GetVersion() != "0.5.0" {
		t.Fatalf("Upstash versions = %q / %q", byName["upstash"].RegisterRPC.GetVersion(), byName["tenant-a-upstash-redis"].RegisterRPC.GetVersion())
	}
	if byName["tenant-a-neon-project"].Inputs["name"].StringValue() != "tenant-a-postgres" || byName["tenant-a-neon-project"].Inputs["org_id"].StringValue() != "org-id" {
		t.Fatalf("Neon inputs = %v", byName["tenant-a-neon-project"].Inputs)
	}
	if byName["tenant-a-upstash-redis"].Inputs["databaseName"].StringValue() != "tenant-a-redis" || byName["tenant-a-upstash-redis"].Inputs["region"].StringValue() != "us-east-1" || !byName["tenant-a-upstash-redis"].Inputs["tls"].BoolValue() {
		t.Fatalf("Upstash inputs = %v", byName["tenant-a-upstash-redis"].Inputs)
	}
	neonResourceCount := 0
	for _, args := range mocks.resources {
		if args.TypeToken == "neon:resource:Project" {
			neonResourceCount++
		}
	}
	if neonResourceCount != 1 {
		t.Fatalf("native Neon project count = %d, want 1", neonResourceCount)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
