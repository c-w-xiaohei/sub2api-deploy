package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func validHostSpec() HostSpec {
	return HostSpec{
		Edge: EdgeSpec{
			OriginIP:         "203.0.113.10",
			CloudflareZoneID: "zone-id",
			ACMEEmail:        "ops@example.com",
			TraefikImage:     "traefik:v3.3.3",
			SingBox: SingBoxSpec{
				ServerName: "www.cloudflare.com",
				Target:     "host.docker.internal:8443",
			},
		},
		Sites: map[string]SiteSpec{
			"code2": {
				Domain:         "code2.contextid.cn",
				Image:          "weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				AdminEmail:     "code2-admin@example.com",
				AppProbePath:   "/api/ready",
				ResourcePrefix: "contextid-us",
				Database:       DatabaseSpec{Mode: "neon", ResourceMode: "create"},
				Redis:          RedisSpec{Mode: "upstash", ResourceMode: "create"},
			},
			"code3": {
				Domain:       "code3.contextid.cn",
				Image:        "weishaw/sub2api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				AdminEmail:   "code3-admin@example.com",
				AppProbePath: "/api/ready",
				Database:     DatabaseSpec{Mode: "neon", ResourceMode: "create"},
				Redis:        RedisSpec{Mode: "upstash", ResourceMode: "create"},
			},
		},
		EdgeSecrets: EdgeSecrets{CloudflareAPIToken: "cloudflare-secret"},
		SiteSecrets: map[string]SiteSecrets{
			"code2": {
				AdminPassword:     "code2-admin-secret",
				JWTSecret:         "code2-jwt-secret",
				TOTPEncryptionKey: "code2-totp-secret",
				Database:          DatabaseSecrets{APIToken: "code2-neon-api-token"},
				Redis:             RedisSecrets{APIKey: "code2-upstash-api-key"},
			},
			"code3": {
				AdminPassword:     "code3-admin-secret",
				JWTSecret:         "code3-jwt-secret",
				TOTPEncryptionKey: "code3-totp-secret",
				Database:          DatabaseSecrets{APIToken: "code3-neon-api-token"},
				Redis:             RedisSecrets{APIKey: "code3-upstash-api-key"},
			},
		},
	}
}

func TestValidateHostSpecValidCode2Code3(t *testing.T) {
	resolved, layouts, err := ValidateHostSpec(validHostSpec())
	if err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
	if len(layouts) != 2 {
		t.Fatalf("layouts = %d, want 2", len(layouts))
	}
	if layouts[0].SiteID != "code2" || layouts[1].SiteID != "code3" {
		t.Fatalf("layouts not in sorted site ID order: %q, %q", layouts[0].SiteID, layouts[1].SiteID)
	}
	code2 := resolved.Sites["code2"]
	if code2.DrainSeconds == nil || *code2.DrainSeconds != 10 {
		t.Fatalf("code2 drainSeconds = %v, want default 10", code2.DrainSeconds)
	}
	if code2.ResourcePrefix != "contextid-us" {
		t.Fatalf("code2 resourcePrefix = %q, want explicit contextid-us", code2.ResourcePrefix)
	}
	if code2.Redis.Region != "us-east-1" {
		t.Fatalf("code2 redis region = %q, want default us-east-1", code2.Redis.Region)
	}
	code3 := resolved.Sites["code3"]
	if code3.ResourcePrefix != "code3" {
		t.Fatalf("code3 resourcePrefix = %q, want default code3", code3.ResourcePrefix)
	}
	if code3.Database.ResourceMode != "create" || code3.Redis.ResourceMode != "create" {
		t.Fatalf("code3 resource modes = %+v, want create", code3)
	}
	if resolved.Edge.SingBox.Target != "host.docker.internal:8443" {
		t.Fatalf("edge sing-box target = %q", resolved.Edge.SingBox.Target)
	}
}

func TestStructuredConfigShapesDecodeIntoHostSpec(t *testing.T) {
	var edge EdgeSpec
	if err := json.Unmarshal([]byte(`{"originIp":"203.0.113.10","cloudflareZoneId":"zone","acmeEmail":"ops@example.com","traefikImage":"traefik:v3.3.3","singBox":{"serverName":"www.cloudflare.com","target":"host.docker.internal:8443"}}`), &edge); err != nil { t.Fatal(err) }
	var sites map[string]SiteSpec
	if err := json.Unmarshal([]byte(`{"code2":{"domain":"code2.contextid.cn","image":"weishaw/sub2api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","adminEmail":"ops@example.com","appProbePath":"/api/ready","database":{"mode":"docker"},"redis":{"mode":"upstash","resourceMode":"existing","endpoint":"cache.code2.upstash.io"}}}`), &sites); err != nil { t.Fatal(err) }
	if edge.OriginIP != "203.0.113.10" || sites["code2"].Redis.Endpoint != "cache.code2.upstash.io" { t.Fatalf("decoded config = %+v %+v", edge, sites["code2"]) }
}

func TestValidateHostSpecDeterministicOrdering(t *testing.T) {
	spec := validHostSpec()
	_, layouts, err := ValidateHostSpec(spec)
	if err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
	if layouts[0].SiteID != "code2" || layouts[1].SiteID != "code3" {
		t.Fatalf("sorted layout order = %q, %q, want code2, code3", layouts[0].SiteID, layouts[1].SiteID)
	}
	if layouts[0].ComposeProject != "sub2api-code2" || layouts[1].ComposeProject != "sub2api-code3" {
		t.Fatalf("compose projects = %q, %q", layouts[0].ComposeProject, layouts[1].ComposeProject)
	}
	if layouts[0].RoutePath == layouts[1].RoutePath {
		t.Fatalf("route paths must be unique, got %q", layouts[0].RoutePath)
	}
	if layouts[0].BlueAlias == layouts[1].BlueAlias || layouts[0].GreenAlias == layouts[1].GreenAlias {
		t.Fatalf("edge aliases must be unique per site")
	}
}

func drainSecondsPointer(value int) *int { return &value }

func TestValidateHostSpecPreservesExplicitZeroDrainSeconds(t *testing.T) {
	spec := validHostSpec()
	code3 := spec.Sites["code3"]
	code3.DrainSeconds = drainSecondsPointer(0)
	spec.Sites["code3"] = code3
	resolved, _, err := ValidateHostSpec(spec)
	if err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
	if resolved.Sites["code3"].DrainSeconds == nil || *resolved.Sites["code3"].DrainSeconds != 0 {
		t.Fatalf("explicit zero drainSeconds was defaulted: %v", resolved.Sites["code3"].DrainSeconds)
	}
}

func TestValidateHostSpecNormalizesDomainsAndSNI(t *testing.T) {
	spec := validHostSpec()
	code2 := spec.Sites["code2"]
	code2.Domain = "Code2.ContextID.CN"
	spec.Sites["code2"] = code2
	spec.Edge.SingBox.ServerName = "WWW.Cloudflare.COM"
	resolved, _, err := ValidateHostSpec(spec)
	if err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
	if got := resolved.Sites["code2"].Domain; got != "code2.contextid.cn" {
		t.Fatalf("normalized domain = %q, want code2.contextid.cn", got)
	}
	if got := resolved.Edge.SingBox.ServerName; got != "www.cloudflare.com" {
		t.Fatalf("normalized sing-box server name = %q, want www.cloudflare.com", got)
	}
}

func TestValidateHostSpecAcceptsIPSingBoxTarget(t *testing.T) {
	spec := validHostSpec()
	spec.Edge.SingBox.Target = "203.0.113.10:8443"
	if _, _, err := ValidateHostSpec(spec); err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
}

func TestValidateHostSpecRejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(spec *HostSpec)
		message string
	}{
		{
			name: "duplicate domain",
			mutate: func(spec *HostSpec) {
				code3 := spec.Sites["code3"]
				code3.Domain = spec.Sites["code2"].Domain
				spec.Sites["code3"] = code3
			},
			message: "must not share domain",
		},
		{
			name: "duplicate domain after normalization",
			mutate: func(spec *HostSpec) {
				code3 := spec.Sites["code3"]
				code3.Domain = "CODE2.CONTEXTID.CN"
				spec.Sites["code3"] = code3
			},
			message: "must not share domain",
		},
		{
			name: "malformed domain",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.Domain = "https://code2.contextid.cn/path"
				spec.Sites["code2"] = code2
			},
			message: "domain must be a valid DNS hostname",
		},
		{
			name: "one-label domain",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.Domain = "code2"
				spec.Sites["code2"] = code2
			},
			message: "domain must be a valid DNS hostname",
		},
		{
			name: "invalid site id",
			mutate: func(spec *HostSpec) {
				spec.Sites["Code2"] = spec.Sites["code2"]
				delete(spec.Sites, "code2")
				spec.SiteSecrets["Code2"] = spec.SiteSecrets["code2"]
				delete(spec.SiteSecrets, "code2")
			},
			message: "must contain only lowercase letters, numbers, and hyphens",
		},
		{
			name: "reserved edge site id",
			mutate: func(spec *HostSpec) {
				spec.Sites["edge"] = spec.Sites["code2"]
				delete(spec.Sites, "code2")
				spec.SiteSecrets["edge"] = spec.SiteSecrets["code2"]
				delete(spec.SiteSecrets, "code2")
			},
			message: `site ID "edge" is reserved for the shared Edge`,
		},
		{
			name: "invalid probe health",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.AppProbePath = "/health"
				spec.Sites["code2"] = code2
			},
			message: "appProbePath must not be /health",
		},
		{
			name: "invalid probe relative",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.AppProbePath = "api/ready"
				spec.Sites["code2"] = code2
			},
			message: "appProbePath must be an absolute path",
		},
		{
			name: "invalid image",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.Image = "weishaw/sub2api:latest"
				spec.Sites["code2"] = code2
			},
			message: "image must contain an immutable @sha256 digest",
		},
		{
			name: "short image digest",
			mutate: func(spec *HostSpec) {
				site := spec.Sites["code2"]
				site.Image = "weishaw/sub2api@sha256:abcdef"
				spec.Sites["code2"] = site
			},
			message: "64 lowercase hexadecimal characters",
		},
		{
			name: "invalid database mode",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.Database.Mode = "mysql"
				spec.Sites["code2"] = code2
			},
			message: "database.mode must be docker or neon",
		},
		{
			name: "invalid redis mode",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.Redis.Mode = "valkey"
				spec.Sites["code2"] = code2
			},
			message: "redis.mode must be docker or upstash",
		},
		{
			name: "invalid resource mode",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.Database.ResourceMode = "destroy"
				spec.Sites["code2"] = code2
			},
			message: "database.resourceMode must be existing or create",
		},
		{
			name: "duplicate resource prefix",
			mutate: func(spec *HostSpec) {
				code3 := spec.Sites["code3"]
				code3.ResourcePrefix = "contextid-us"
				spec.Sites["code3"] = code3
			},
			message: "must not share resourcePrefix",
		},
		{
			name: "missing edge originIp",
			mutate: func(spec *HostSpec) {
				spec.Edge.OriginIP = ""
			},
			message: "edge.originIp is required",
		},
		{
			name: "invalid sing-box target",
			mutate: func(spec *HostSpec) {
				spec.Edge.SingBox.Target = "host.docker.internal"
			},
			message: "edge.singBox.target must be host:port",
		},
		{
			name: "yaml-unsafe sing-box target",
			mutate: func(spec *HostSpec) {
				spec.Edge.SingBox.Target = `host"name:8443`
			},
			message: "edge.singBox.target must be host:port",
		},
		{
			name: "sing-box target with path",
			mutate: func(spec *HostSpec) {
				spec.Edge.SingBox.Target = "host.docker.internal/path:8443"
			},
			message: "edge.singBox.target must be host:port",
		},
		{
			name: "invalid sing-box server name",
			mutate: func(spec *HostSpec) {
				spec.Edge.SingBox.ServerName = `www.cloudflare.com" # unsafe`
			},
			message: "edge.singBox.serverName must be a valid DNS hostname",
		},
		{
			name: "one-label sing-box server name",
			mutate: func(spec *HostSpec) {
				spec.Edge.SingBox.ServerName = "localhost"
			},
			message: "edge.singBox.serverName must be a valid DNS hostname",
		},
		{
			name: "negative drain seconds",
			mutate: func(spec *HostSpec) {
				code2 := spec.Sites["code2"]
				code2.DrainSeconds = drainSecondsPointer(-1)
				spec.Sites["code2"] = code2
			},
			message: "drainSeconds must be zero or greater",
		},
		{
			name: "missing edge cloudflare token",
			mutate: func(spec *HostSpec) {
				spec.EdgeSecrets.CloudflareAPIToken = ""
			},
			message: "edgeSecrets.cloudflareApiToken is required",
		},
		{
			name: "missing siteSecrets entry",
			mutate: func(spec *HostSpec) {
				delete(spec.SiteSecrets, "code3")
			},
			message: "siteSecrets.code3 is required",
		},
		{
			name: "missing selected-mode database apiToken",
			mutate: func(spec *HostSpec) {
				secrets := spec.SiteSecrets["code2"]
				secrets.Database.APIToken = ""
				spec.SiteSecrets["code2"] = secrets
			},
			message: "sites.code2.database.apiToken is required",
		},
		{
			name: "missing selected-mode redis apiKey",
			mutate: func(spec *HostSpec) {
				secrets := spec.SiteSecrets["code2"]
				secrets.Redis.APIKey = ""
				spec.SiteSecrets["code2"] = secrets
			},
			message: "sites.code2.redis.apiKey is required",
		},
		{
			name: "missing admin password",
			mutate: func(spec *HostSpec) {
				secrets := spec.SiteSecrets["code2"]
				secrets.AdminPassword = ""
				spec.SiteSecrets["code2"] = secrets
			},
			message: "sites.code2.adminPassword is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validHostSpec()
			tt.mutate(&spec)
			_, _, err := ValidateHostSpec(spec)
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("error = %v, want substring %q", err, tt.message)
			}
		})
	}
}

func TestValidateHostSpecRequiresAtLeastOneSite(t *testing.T) {
	spec := validHostSpec()
	spec.Sites = map[string]SiteSpec{}
	spec.SiteSecrets = map[string]SiteSecrets{}
	_, _, err := ValidateHostSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "at least one site") {
		t.Fatalf("error = %v, want at least one site", err)
	}
}

func TestValidateHostSpecSelectedModeSecrets(t *testing.T) {
	tests := []struct {
		name    string
		site    SiteSpec
		secrets SiteSecrets
		valid   bool
		message string
	}{
		{
			name:    "docker database requires postgres password",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "docker"}},
			secrets: SiteSecrets{},
			valid:   false,
			message: "sites.code2.database.password is required",
		},
		{
			name:    "docker database with postgres password",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "docker"}},
			secrets: SiteSecrets{Database: DatabaseSecrets{Password: "postgres-secret"}},
			valid:   true,
		},
		{
			name:    "neon existing requires dsn",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "neon", ResourceMode: "existing"}},
			secrets: SiteSecrets{},
			valid:   false,
			message: "sites.code2.database.dsn is required",
		},
		{
			name:    "neon existing with dsn",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "neon", ResourceMode: "existing"}},
			secrets: SiteSecrets{Database: DatabaseSecrets{DSN: "postgresql://sub2api:secret@ep.example.neon.tech/sub2api?sslmode=require"}},
			valid:   true,
		},
		{
			name:    "neon existing rejects password without dsn",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "neon", ResourceMode: "existing"}},
			secrets: SiteSecrets{Database: DatabaseSecrets{Password: "neon-secret"}},
			valid:   false,
			message: "sites.code2.database.dsn is required",
		},
		{
			name:    "docker redis requires password",
			site:    SiteSpec{Redis: RedisSpec{Mode: "docker"}},
			secrets: SiteSecrets{},
			valid:   false,
			message: "sites.code2.redis.password is required",
		},
		{
			name:    "upstash create requires apiKey",
			site:    SiteSpec{Redis: RedisSpec{Mode: "upstash", ResourceMode: "create"}},
			secrets: SiteSecrets{},
			valid:   false,
			message: "sites.code2.redis.apiKey is required",
		},
		{
			name:    "upstash existing requires password",
			site:    SiteSpec{Redis: RedisSpec{Mode: "upstash", ResourceMode: "existing", Endpoint: "cache.upstash.io"}},
			secrets: SiteSecrets{},
			valid:   false,
			message: "sites.code2.redis.password is required",
		},
		{
			name:    "upstash existing with password",
			site:    SiteSpec{Redis: RedisSpec{Mode: "upstash", ResourceMode: "existing", Endpoint: "cache.upstash.io"}},
			secrets: SiteSecrets{Redis: RedisSecrets{Password: "upstash-secret"}},
			valid:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := tt.site
			site.Domain = "code2.contextid.cn"
			site.Image = "weishaw/sub2api@sha256:abcdef1234567890"
			site.AdminEmail = "code2-admin@example.com"
			site.AppProbePath = "/api/ready"
			secrets := tt.secrets
			secrets.AdminPassword = "admin-secret"
			secrets.JWTSecret = "jwt-secret"
			secrets.TOTPEncryptionKey = "totp-secret"
			err := validateSiteSecrets("code2", site, secrets)
			if tt.valid && err != nil {
				t.Fatalf("validateSiteSecrets() error = %v", err)
			}
			if !tt.valid && (err == nil || !strings.Contains(err.Error(), tt.message)) {
				t.Fatalf("error = %v, want substring %q", err, tt.message)
			}
		})
	}
}

func TestValidateHostSpecExistingUpstashEndpoint(t *testing.T) {
	spec := validHostSpec()
	site := spec.Sites["code2"]
	site.Redis = RedisSpec{Mode: "upstash", ResourceMode: "existing", Endpoint: "cache.code2.upstash.io"}
	spec.Sites["code2"] = site
	secrets := spec.SiteSecrets["code2"]
	secrets.Redis = RedisSecrets{Password: "code2-upstash-password"}
	spec.SiteSecrets["code2"] = secrets
	resolved, _, err := ValidateHostSpec(spec)
	if err != nil { t.Fatalf("ValidateHostSpec() error = %v", err) }
	if got := resolved.Sites["code2"].Redis.Endpoint; got != "cache.code2.upstash.io" { t.Fatalf("endpoint = %q", got) }

	for _, endpoint := range []string{"", "rediss://cache.upstash.io", "cache.upstash.io:6379", "cache.upstash.io/path"} {
		spec := validHostSpec()
		site := spec.Sites["code2"]
		site.Redis = RedisSpec{Mode: "upstash", ResourceMode: "existing", Endpoint: endpoint}
		spec.Sites["code2"] = site
		secrets := spec.SiteSecrets["code2"]
		secrets.Redis = RedisSecrets{Password: "code2-upstash-password"}
		spec.SiteSecrets["code2"] = secrets
		if _, _, err := ValidateHostSpec(spec); err == nil { t.Fatalf("endpoint %q was accepted", endpoint) }
	}
}

func TestValidateHostSpecSupportsAllSelectedDataModeCombinations(t *testing.T) {
	for _, modes := range []struct { database, redis string }{{"docker", "docker"}, {"neon", "docker"}, {"docker", "upstash"}, {"neon", "upstash"}} {
		t.Run(modes.database+"/"+modes.redis, func(t *testing.T) {
			spec := validHostSpec()
			site := spec.Sites["code2"]
			secrets := spec.SiteSecrets["code2"]
			site.Database = DatabaseSpec{Mode: modes.database, ResourceMode: "existing"}
			if modes.database == "docker" { secrets.Database = DatabaseSecrets{Password: "postgres-secret"} } else { secrets.Database = DatabaseSecrets{DSN: "postgresql://sub2api:secret@ep.code2.neon.tech/sub2api?sslmode=require"} }
			site.Redis = RedisSpec{Mode: modes.redis, ResourceMode: "existing"}
			if modes.redis == "docker" { secrets.Redis = RedisSecrets{Password: "redis-secret"} } else { site.Redis.Endpoint = "cache.code2.upstash.io"; secrets.Redis = RedisSecrets{Password: "upstash-secret"} }
			spec.Sites["code2"], spec.SiteSecrets["code2"] = site, secrets
			if _, _, err := ValidateHostSpec(spec); err != nil { t.Fatalf("ValidateHostSpec() error = %v", err) }
		})
	}
}

func TestValidateHostSpecEdgeDefaults(t *testing.T) {
	spec := validHostSpec()
	resolved, _, err := ValidateHostSpec(spec)
	if err != nil {
		t.Fatalf("ValidateHostSpec() error = %v", err)
	}
	if resolved.Edge.SingBox.ServerName != "www.cloudflare.com" {
		t.Fatalf("sing-box serverName = %q", resolved.Edge.SingBox.ServerName)
	}
}
