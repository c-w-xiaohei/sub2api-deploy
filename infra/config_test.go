package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestProgramConfigModelHasNoFlatDeploymentFallback(t *testing.T) {
	if _, _, err := ValidateHostSpec(validHostSpec()); err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
}

func TestDefaultIntPreservesExplicitZero(t *testing.T) {
	zero := 0
	if got := defaultInt(&zero, 10); got != 0 { t.Fatalf("defaultInt(explicit zero) = %d, want 0", got) }
}

func TestResolveHostConfigDecodesOnlyStructuredObjects(t *testing.T) {
	var edge EdgeSpec
	var sites map[string]SiteSpec
	var edgeSecrets EdgeSecrets
	var siteSecrets map[string]SiteSecrets
	if err := json.Unmarshal([]byte(`{"originIp":"203.0.113.10","cloudflareZoneId":"zone-id","acmeEmail":"ops@example.com","traefikImage":"traefik:v3.3.3","singBox":{"serverName":"www.cloudflare.com","target":"host.docker.internal:8443"}}`), &edge); err != nil { t.Fatal(err) }
	if err := json.Unmarshal([]byte(`{"code2":{"domain":"code2.contextid.cn","image":"weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","adminEmail":"code2-admin@example.com","appProbePath":"/api/ready","database":{"mode":"docker"},"redis":{"mode":"docker"}}}`), &sites); err != nil { t.Fatal(err) }
	if err := json.Unmarshal([]byte(`{"cloudflareApiToken":"edge-token"}`), &edgeSecrets); err != nil { t.Fatal(err) }
	if err := json.Unmarshal([]byte(`{"code2":{"adminPassword":"code2-admin-password","jwtSecret":"code2-jwt","totpEncryptionKey":"code2-totp","database":{"password":"code2-postgres-password"},"redis":{"password":"code2-redis-password"}}}`), &siteSecrets); err != nil { t.Fatal(err) }

	host, layouts, err := resolveHostConfig(edge, sites, edgeSecrets, siteSecrets)
	if err != nil { t.Fatalf("resolveHostConfig() error = %v", err) }
	if len(layouts) != 1 || layouts[0].SiteID != "code2" { t.Fatalf("layouts = %#v", layouts) }
	if host.EdgeSecrets.CloudflareAPIToken != "edge-token" { t.Fatalf("edge secret lost before wrapping") }
	if got := host.SiteSecrets["code2"].AdminPassword; got != "code2-admin-password" { t.Fatalf("site secret identity lost before wrapping: %q", got) }
	if host.EdgeSecrets.CloudflareAPIToken == host.SiteSecrets["code2"].AdminPassword { t.Fatal("edge and Site secret objects were conflated") }
}

func TestWrapHostSecretsKeepsStructuredSecretsIndependentAndTainted(t *testing.T) {
	input := validHostSpec()
	ordinary, err := json.Marshal(struct {
		Edge  EdgeSpec            `json:"edge"`
		Sites map[string]SiteSpec `json:"sites"`
	}{Edge: input.Edge, Sites: input.Sites})
	if err != nil { t.Fatal(err) }
	secretObjects, err := json.Marshal(struct {
		EdgeSecrets EdgeSecrets            `json:"edgeSecrets"`
		SiteSecrets map[string]SiteSecrets `json:"siteSecrets"`
	}{EdgeSecrets: input.EdgeSecrets, SiteSecrets: input.SiteSecrets})
	if err != nil { t.Fatal(err) }
	var decoded struct { Edge EdgeSpec `json:"edge"`; Sites map[string]SiteSpec `json:"sites"` }
	if err := json.Unmarshal(ordinary, &decoded); err != nil { t.Fatal(err) }
	var decodedSecrets struct { EdgeSecrets EdgeSecrets `json:"edgeSecrets"`; SiteSecrets map[string]SiteSecrets `json:"siteSecrets"` }
	if err := json.Unmarshal(secretObjects, &decodedSecrets); err != nil { t.Fatal(err) }
	host, layouts, err := resolveHostConfig(decoded.Edge, decoded.Sites, decodedSecrets.EdgeSecrets, decodedSecrets.SiteSecrets)
	if err != nil { t.Fatal(err) }

	if err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		secrets := wrapHostSecrets(host, layouts)
		for name, output := range map[string]pulumi.StringOutput{"edge": secrets.Edge, "code2": secrets.Sites["code2"], "code3": secrets.Sites["code3"]} {
			if !pulumi.IsSecret(output) { t.Errorf("%s secret output lost Pulumi taint", name) }
		}
		secrets.Edge.ApplyT(func(value string) string { if value != "cloudflare-secret" { t.Errorf("edge secret = %q", value) }; return value })
		secrets.Sites["code2"].ApplyT(func(value string) string { if !strings.Contains(value, "code2-admin-secret") || strings.Contains(value, "code3-admin-secret") { t.Errorf("code2 payload is not isolated: %q", value) }; return value })
		secrets.Sites["code3"].ApplyT(func(value string) string { if !strings.Contains(value, "code3-admin-secret") || strings.Contains(value, "code2-admin-secret") { t.Errorf("code3 payload is not isolated: %q", value) }; return value })
		return nil
	}, pulumi.WithMocks("sub2api-vps-deploy", "test", &graphMocks{})); err != nil { t.Fatal(err) }
}
