package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"gopkg.in/yaml.v3"
)

func TestProgramConfigModelHasNoFlatDeploymentFallback(t *testing.T) {
	if _, _, err := ValidateHostSpec(validHostSpec()); err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
}

func TestDefaultIntPreservesExplicitZero(t *testing.T) {
	zero := 0
	if got := defaultInt(&zero, 10); got != 0 {
		t.Fatalf("defaultInt(explicit zero) = %d, want 0", got)
	}
}

func TestNeonComputeDefaultsAndValidation(t *testing.T) {
	spec := validHostSpec()
	resolved, _, err := ValidateHostSpec(spec)
	if err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
	compute := resolved.Sites["code2"].Database.Compute
	if *compute.MinCU != 0.25 || *compute.MaxCU != 0.25 || *compute.SuspendTimeoutSeconds != 300 {
		t.Fatalf("Neon compute defaults = %#v", compute)
	}
	if got := resolved.Sites["code2"].Database.Region; got != "aws-us-east-1" {
		t.Fatalf("Neon region default = %q, want aws-us-east-1", got)
	}

	invalid := validHostSpec()
	zero := 0.0
	code2 := invalid.Sites["code2"]
	code2.Database.Compute.MinCU = &zero
	invalid.Sites["code2"] = code2
	if _, _, err := ValidateHostSpec(invalid); err == nil {
		t.Fatal("zero Neon autoscaling minimum was accepted")
	}
}

func TestNeonComputeAndRegionPreserveExplicitValues(t *testing.T) {
	spec := validHostSpec()
	minCU, maxCU, timeout := 0.5, 2.0, 900
	code2 := spec.Sites["code2"]
	code2.Database.Compute = NeonComputeSpec{MinCU: &minCU, MaxCU: &maxCU, SuspendTimeoutSeconds: &timeout}
	code2.Database.Region = "aws-eu-central-1"
	spec.Sites["code2"] = code2

	resolved, _, err := ValidateHostSpec(spec)
	if err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
	compute := resolved.Sites["code2"].Database.Compute
	if *compute.MinCU != minCU || *compute.MaxCU != maxCU || *compute.SuspendTimeoutSeconds != timeout {
		t.Fatalf("Neon compute overrides = %#v", compute)
	}
	if got := resolved.Sites["code2"].Database.Region; got != "aws-eu-central-1" {
		t.Fatalf("Neon region = %q, want aws-eu-central-1", got)
	}
}

func TestResolveHostConfigDecodesOnlyStructuredObjects(t *testing.T) {
	var edge EdgeSpec
	var sites map[string]SiteSpec
	var edgeSecrets EdgeSecrets
	var siteSecrets map[string]SiteSecrets
	if err := json.Unmarshal([]byte(`{"originIp":"203.0.113.10","cloudflareZoneId":"zone-id","acmeEmail":"ops@example.com","traefikImage":"traefik:v3.3.3","singBox":{"serverName":"www.cloudflare.com","target":"host.docker.internal:8443"}}`), &edge); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"code2":{"domain":"code2.contextid.cn","image":"weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","adminEmail":"code2-admin@example.com","appProbePath":"/api/ready","database":{"mode":"docker"},"redis":{"mode":"docker"}}}`), &sites); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"cloudflareApiToken":"edge-token"}`), &edgeSecrets); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"code2":{"adminPassword":"code2-admin-password","jwtSecret":"code2-jwt","totpEncryptionKey":"code2-totp","database":{"password":"code2-postgres-password"},"redis":{"password":"code2-redis-password"}}}`), &siteSecrets); err != nil {
		t.Fatal(err)
	}

	host, layouts, err := resolveHostConfig(edge, sites, edgeSecrets, siteSecrets)
	if err != nil {
		t.Fatalf("resolveHostConfig() error = %v", err)
	}
	if len(layouts) != 1 || layouts[0].SiteID != "code2" {
		t.Fatalf("layouts = %#v", layouts)
	}
	if host.EdgeSecrets.CloudflareAPIToken != "edge-token" {
		t.Fatalf("edge secret lost before wrapping")
	}
	if got := host.SiteSecrets["code2"].AdminPassword; got != "code2-admin-password" {
		t.Fatalf("site secret identity lost before wrapping: %q", got)
	}
	if host.EdgeSecrets.CloudflareAPIToken == host.SiteSecrets["code2"].AdminPassword {
		t.Fatal("edge and Site secret objects were conflated")
	}
}

func TestProductionExampleUsesEnvironmentControllerConfig(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "Pulumi.production.example.yaml"))
	if err != nil {
		t.Fatalf("read production example: %v", err)
	}

	var document struct {
		Config map[string]yaml.Node `yaml:"config"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode production example YAML: %v", err)
	}

	expectedKeys := map[string]bool{
		"sub2api-environment:environmentConfig":  true,
		"sub2api-environment:environmentSecrets": true,
	}
	if len(document.Config) != len(expectedKeys) {
		t.Fatalf("project configuration keys = %v, want only environmentConfig and environmentSecrets", configKeys(document.Config))
	}
	for key := range document.Config {
		if !expectedKeys[key] {
			t.Fatalf("unexpected project configuration key %q", key)
		}
	}

	configNode := document.Config["sub2api-environment:environmentConfig"]
	if configNode.Kind != yaml.ScalarNode {
		t.Fatalf("environmentConfig node kind = %v, want scalar", configNode.Kind)
	}
	config, err := environment.ParseConfig([]byte(configNode.Value))
	if err != nil {
		t.Fatalf("parse environmentConfig: %v", err)
	}

	app, ok := config.Apps["api"]
	if !ok || len(config.Apps) != 1 {
		t.Fatalf("apps = %#v, want only api", config.Apps)
	}
	if postgres := config.Postgres["app"]; len(config.Postgres) != 1 || postgres.Type != "docker" || postgres.Server != "data-one" {
		t.Fatalf("postgres app = %#v, want Docker on data-one", postgres)
	}
	if redis := config.Redis["app"]; len(config.Redis) != 1 || redis.Type != "docker" || redis.Server != "data-one" {
		t.Fatalf("redis app = %#v, want Docker on data-one", redis)
	}
	if strings.Join(app.Servers, ",") != "api-one,api-two" || strings.Join(app.PublicAccess.Servers, ",") != "api-one,api-two" {
		t.Fatalf("app servers = %v, public servers = %v, want api-one and api-two", app.Servers, app.PublicAccess.Servers)
	}
	if app.ReadinessPath != "/healthz" {
		t.Fatalf("readinessPath = %q, want /healthz", app.ReadinessPath)
	}
	if strings.Contains(configNode.Value, "/api/ready") {
		t.Fatal("environmentConfig still recommends deprecated /api/ready")
	}
}

func TestProductionExampleUsesProtectedEnvironmentSecrets(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "Pulumi.production.example.yaml"))
	if err != nil {
		t.Fatalf("read production example: %v", err)
	}

	var document struct {
		Config map[string]yaml.Node `yaml:"config"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode production example YAML: %v", err)
	}

	secrets := document.Config["sub2api-environment:environmentSecrets"]
	if secrets.Kind != yaml.MappingNode || len(secrets.Content) != 2 || secrets.Content[0].Value != "secure" || secrets.Content[1].Value == "" {
		t.Fatalf("environmentSecrets = %#v, want nonempty protected secure value", secrets)
	}
}

func configKeys(config map[string]yaml.Node) []string {
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	return keys
}

func TestWrapHostSecretsKeepsStructuredSecretsIndependentAndTainted(t *testing.T) {
	input := validHostSpec()
	ordinary, err := json.Marshal(struct {
		Edge  EdgeSpec            `json:"edge"`
		Sites map[string]SiteSpec `json:"sites"`
	}{Edge: input.Edge, Sites: input.Sites})
	if err != nil {
		t.Fatal(err)
	}
	secretObjects, err := json.Marshal(struct {
		EdgeSecrets EdgeSecrets            `json:"edgeSecrets"`
		SiteSecrets map[string]SiteSecrets `json:"siteSecrets"`
	}{EdgeSecrets: input.EdgeSecrets, SiteSecrets: input.SiteSecrets})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Edge  EdgeSpec            `json:"edge"`
		Sites map[string]SiteSpec `json:"sites"`
	}
	if err := json.Unmarshal(ordinary, &decoded); err != nil {
		t.Fatal(err)
	}
	var decodedSecrets struct {
		EdgeSecrets EdgeSecrets            `json:"edgeSecrets"`
		SiteSecrets map[string]SiteSecrets `json:"siteSecrets"`
	}
	if err := json.Unmarshal(secretObjects, &decodedSecrets); err != nil {
		t.Fatal(err)
	}
	host, layouts, err := resolveHostConfig(decoded.Edge, decoded.Sites, decodedSecrets.EdgeSecrets, decodedSecrets.SiteSecrets)
	if err != nil {
		t.Fatal(err)
	}

	if err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		secrets := wrapHostSecrets(host, layouts)
		for name, output := range map[string]pulumi.StringOutput{"edge": secrets.Edge, "code2": secrets.Sites["code2"], "code3": secrets.Sites["code3"]} {
			if !pulumi.IsSecret(output) {
				t.Errorf("%s secret output lost Pulumi taint", name)
			}
		}
		secrets.Edge.ApplyT(func(value string) string {
			if value != "cloudflare-secret" {
				t.Errorf("edge secret = %q", value)
			}
			return value
		})
		secrets.Sites["code2"].ApplyT(func(value string) string {
			if !strings.Contains(value, "code2-admin-secret") || strings.Contains(value, "code3-admin-secret") {
				t.Errorf("code2 payload is not isolated: %q", value)
			}
			return value
		})
		secrets.Sites["code3"].ApplyT(func(value string) string {
			if !strings.Contains(value, "code3-admin-secret") || strings.Contains(value, "code2-admin-secret") {
				t.Errorf("code3 payload is not isolated: %q", value)
			}
			return value
		})
		return nil
	}, pulumi.WithMocks("sub2api-vps-deploy", "test", &graphMocks{})); err != nil {
		t.Fatal(err)
	}
}
