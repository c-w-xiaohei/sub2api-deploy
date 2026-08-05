package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

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

	invalid := validHostSpec()
	zero := 0.0
	code2 := invalid.Sites["code2"]
	code2.Database.Compute.MinCU = &zero
	invalid.Sites["code2"] = code2
	if _, _, err := ValidateHostSpec(invalid); err == nil {
		t.Fatal("zero Neon autoscaling minimum was accepted")
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

func TestProductionExampleUsesStructuredPublicConfig(t *testing.T) {
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

	const prefix = "sub2api-vps-deploy:"
	expectedKeys := map[string]bool{
		prefix + "edge":  true,
		prefix + "sites": true,
	}
	if len(document.Config) != len(expectedKeys) {
		t.Fatalf("project configuration keys = %v, want only edge and sites", configKeys(document.Config))
	}
	for key := range document.Config {
		if !expectedKeys[key] {
			t.Fatalf("unexpected project configuration key %q", key)
		}
	}

	var edge EdgeSpec
	decodeYAMLObject(t, document.Config[prefix+"edge"], &edge)
	var sites map[string]SiteSpec
	decodeYAMLObject(t, document.Config[prefix+"sites"], &sites)

	siteSecrets := make(map[string]SiteSecrets, len(sites))
	for siteID, site := range sites {
		siteSecrets[siteID] = fakeExampleSiteSecrets(siteID, site)
	}
	host, layouts, err := resolveHostConfig(edge, sites, EdgeSecrets{CloudflareAPIToken: "example-cloudflare-token"}, siteSecrets)
	if err != nil {
		t.Fatalf("resolveHostConfig() from production example: %v", err)
	}

	if !reflect.DeepEqual(sortedSiteIDs(sites), []string{"code2", "code3"}) {
		t.Fatalf("sites = %v, want code2 and code3", sortedSiteIDs(sites))
	}
	wantLayouts := []SiteLayout{
		DeriveSiteLayout("code2", defaultString(sites["code2"].ResourcePrefix, "code2")),
		DeriveSiteLayout("code3", defaultString(sites["code3"].ResourcePrefix, "code3")),
	}
	if !reflect.DeepEqual(layouts, wantLayouts) {
		t.Fatalf("resolved layouts = %#v, want %#v", layouts, wantLayouts)
	}
	for siteID, site := range sites {
		resolvedSite := host.Sites[siteID]
		if resolvedSite.Database.Mode != defaultString(site.Database.Mode, "docker") {
			t.Fatalf("%s database mode = %q, want %q", siteID, resolvedSite.Database.Mode, site.Database.Mode)
		}
		if resolvedSite.Redis.Mode != defaultString(site.Redis.Mode, "docker") {
			t.Fatalf("%s redis mode = %q, want %q", siteID, resolvedSite.Redis.Mode, site.Redis.Mode)
		}
	}
}

func TestProductionExampleUsesRootProbeAndDoesNotMentionDeprecatedProbe(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "Pulumi.production.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "/api/ready") {
		t.Fatal("production example still recommends /api/ready")
	}
	if strings.Count(string(contents), "appProbePath: /") != 2 {
		t.Fatalf("production example must use / for both Sites")
	}
}

func decodeYAMLObject(t *testing.T, node yaml.Node, target interface{}) {
	t.Helper()
	encoded, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatalf("marshal YAML object: %v", err)
	}
	var object map[string]interface{}
	if err := yaml.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("decode YAML object: %v", err)
	}
	jsonValue, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal decoded YAML object: %v", err)
	}
	if err := json.Unmarshal(jsonValue, target); err != nil {
		t.Fatalf("decode object into %T: %v", target, err)
	}
}

func configKeys(config map[string]yaml.Node) []string {
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	return keys
}

func sortedSiteIDs(sites map[string]SiteSpec) []string {
	ids := make([]string, 0, len(sites))
	for siteID := range sites {
		ids = append(ids, siteID)
	}
	sort.Strings(ids)
	return ids
}

func fakeExampleSiteSecrets(siteID string, site SiteSpec) SiteSecrets {
	secrets := SiteSecrets{
		AdminPassword:     siteID + "-admin-password",
		JWTSecret:         siteID + "-jwt-secret",
		TOTPEncryptionKey: siteID + "-totp-key",
	}

	databaseMode := defaultString(site.Database.Mode, "docker")
	databaseResourceMode := defaultString(site.Database.ResourceMode, "existing")
	if databaseMode == "docker" {
		secrets.Database.Password = siteID + "-database-password"
	} else if databaseResourceMode == "create" {
		secrets.Database.APIToken = siteID + "-neon-api-token"
	} else {
		secrets.Database.DSN = "postgresql://sub2api:secret@" + siteID + ".neon.tech/sub2api?sslmode=require"
	}

	redisMode := defaultString(site.Redis.Mode, "docker")
	redisResourceMode := defaultString(site.Redis.ResourceMode, "existing")
	if redisMode == "docker" {
		secrets.Redis.Password = siteID + "-redis-password"
	} else if redisResourceMode == "create" {
		secrets.Redis.APIKey = siteID + "-upstash-api-key"
	} else {
		secrets.Redis.Password = siteID + "-upstash-password"
	}
	return secrets
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
