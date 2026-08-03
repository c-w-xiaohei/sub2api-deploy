package main

import (
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
				Image:          "weishaw/sub2api@sha256:abcdef1234567890",
				AdminEmail:     "code2-admin@example.com",
				AppProbePath:   "/api/ready",
				ResourcePrefix: "contextid-us",
				Database:       DatabaseSpec{Mode: "neon", ResourceMode: "create"},
				Redis:          RedisSpec{Mode: "upstash", ResourceMode: "create"},
			},
			"code3": {
				Domain:       "code3.contextid.cn",
				Image:        "weishaw/sub2api@sha256:abcdef1234567890",
				AdminEmail:   "code3-admin@example.com",
				AppProbePath: "/api/ready",
				Database:     DatabaseSpec{Mode: "neon", ResourceMode: "create"},
				Redis:        RedisSpec{Mode: "upstash", ResourceMode: "create"},
			},
		},
		EdgeSecrets: EdgeSecrets{CloudflareAPIToken: "cloudflare-secret"},
		SiteSecrets: map[string]SiteSecrets{
			"code2": {
				AdminPassword:     "admin-secret",
				JWTSecret:         "jwt-secret",
				TOTPEncryptionKey: "totp-secret",
				Database:          DatabaseSecrets{APIToken: "neon-api-token"},
				Redis:             RedisSecrets{APIKey: "upstash-api-key"},
			},
			"code3": {
				AdminPassword:     "admin-secret",
				JWTSecret:         "jwt-secret",
				TOTPEncryptionKey: "totp-secret",
				Database:          DatabaseSecrets{APIToken: "neon-api-token"},
				Redis:             RedisSecrets{APIKey: "upstash-api-key"},
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
			name:    "neon existing requires dsn or password",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "neon", ResourceMode: "existing"}},
			secrets: SiteSecrets{},
			valid:   false,
			message: "requires database.dsn or database.password",
		},
		{
			name:    "neon existing with dsn",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "neon", ResourceMode: "existing"}},
			secrets: SiteSecrets{Database: DatabaseSecrets{DSN: "postgresql://sub2api:secret@ep.example.neon.tech/sub2api?sslmode=require"}},
			valid:   true,
		},
		{
			name:    "neon existing with password",
			site:    SiteSpec{Database: DatabaseSpec{Mode: "neon", ResourceMode: "existing"}},
			secrets: SiteSecrets{Database: DatabaseSecrets{Password: "neon-secret"}},
			valid:   true,
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
			site:    SiteSpec{Redis: RedisSpec{Mode: "upstash", ResourceMode: "existing"}},
			secrets: SiteSecrets{},
			valid:   false,
			message: "sites.code2.redis.password is required",
		},
		{
			name:    "upstash existing with password",
			site:    SiteSpec{Redis: RedisSpec{Mode: "upstash", ResourceMode: "existing"}},
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
