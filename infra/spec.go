package main

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// HostSpec is the pure public configuration model of one host: one shared
// edge and one independent Sub2API installation per Sites entry.
type HostSpec struct {
	Edge        EdgeSpec
	Sites       map[string]SiteSpec
	EdgeSecrets EdgeSecrets
	SiteSecrets map[string]SiteSecrets
}

type EdgeSpec struct {
	OriginIP         string `json:"originIp"`
	CloudflareZoneID string `json:"cloudflareZoneId"`
	ACMEEmail        string `json:"acmeEmail"`
	// The shared Edge Traefik image intentionally permits a stable version tag.
	TraefikImage string      `json:"traefikImage"`
	SingBox      SingBoxSpec `json:"singBox"`
}

type SingBoxSpec struct {
	ServerName string `json:"serverName"`
	Target     string `json:"target"`
}

type SiteSpec struct {
	Domain         string       `json:"domain"`
	Image          string       `json:"image"`
	AdminEmail     string       `json:"adminEmail"`
	AppProbePath   string       `json:"appProbePath"`
	DrainSeconds   *int         `json:"drainSeconds"`
	ResourcePrefix string       `json:"resourcePrefix"`
	Database       DatabaseSpec `json:"database"`
	Redis          RedisSpec    `json:"redis"`
}

type DatabaseSpec struct {
	Mode         string          `json:"mode"`
	ResourceMode string          `json:"resourceMode"`
	Compute      NeonComputeSpec `json:"compute"`
}

type NeonComputeSpec struct {
	MinCU                 *float64 `json:"minCU"`
	MaxCU                 *float64 `json:"maxCU"`
	SuspendTimeoutSeconds *int     `json:"suspendTimeoutSeconds"`
}

type RedisSpec struct {
	Mode         string `json:"mode"`
	ResourceMode string `json:"resourceMode"`
	Endpoint     string `json:"endpoint"`
	Region       string `json:"region"`
}

type EdgeSecrets struct {
	CloudflareAPIToken string `json:"cloudflareApiToken"`
}

type SiteSecrets struct {
	AdminPassword     string             `json:"adminPassword"`
	JWTSecret         string             `json:"jwtSecret"`
	TOTPEncryptionKey string             `json:"totpEncryptionKey"`
	Database          DatabaseSecrets    `json:"database"`
	Redis             RedisSecrets       `json:"redis"`
	AppEnv            *map[string]string `json:"appEnv"`
}

type DatabaseSecrets struct {
	APIToken string `json:"apiToken"`
	DSN      string `json:"dsn"`
	Password string `json:"password"`
}

type RedisSecrets struct {
	APIKey   string `json:"apiKey"`
	Password string `json:"password"`
}

func normalizeDNSHostname(value string, requireDot bool) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 253 || strings.HasSuffix(value, ".") {
		return "", fmt.Errorf("must be a valid DNS hostname")
	}
	normalized := strings.ToLower(value)
	if requireDot && !strings.Contains(normalized, ".") {
		return "", fmt.Errorf("must be a valid DNS hostname")
	}
	for _, label := range strings.Split(normalized, ".") {
		if len(label) == 0 || len(label) > 63 || !dnsLabelPattern.MatchString(label) {
			return "", fmt.Errorf("must be a valid DNS hostname")
		}
	}
	return normalized, nil
}

func validateSingBoxTarget(target string) error {
	host, portText, err := net.SplitHostPort(target)
	if err != nil || host == "" || portText == "" || !decimalPortPattern.MatchString(portText) {
		return fmt.Errorf("edge.singBox.target must be host:port, got %q", target)
	}
	if net.ParseIP(host) == nil {
		if _, err := normalizeDNSHostname(host, false); err != nil {
			return fmt.Errorf("edge.singBox.target must be host:port, got %q", target)
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("edge.singBox.target port must be between 1 and 65535, got %q", target)
	}
	return nil
}

// ValidateHostSpec validates one host description and returns the resolved
// spec with defaults applied plus one SiteLayout per site in sorted Site ID
// order. Layouts are deterministic and independent of map iteration order.
func ValidateHostSpec(spec HostSpec) (HostSpec, []SiteLayout, error) {
	edge, err := validateEdge(spec.Edge)
	if err != nil {
		return HostSpec{}, nil, err
	}
	if _, err := required(spec.EdgeSecrets.CloudflareAPIToken, "edgeSecrets.cloudflareApiToken"); err != nil {
		return HostSpec{}, nil, err
	}
	if len(spec.Sites) == 0 {
		return HostSpec{}, nil, fmt.Errorf("host requires at least one site")
	}

	siteIDs := make([]string, 0, len(spec.Sites))
	for siteID := range spec.Sites {
		siteIDs = append(siteIDs, siteID)
	}
	sort.Strings(siteIDs)

	resolved := HostSpec{
		Edge:        edge,
		Sites:       make(map[string]SiteSpec, len(siteIDs)),
		EdgeSecrets: spec.EdgeSecrets,
		SiteSecrets: make(map[string]SiteSecrets, len(siteIDs)),
	}
	layouts := make([]SiteLayout, 0, len(siteIDs))
	domains := map[string]string{}
	prefixes := map[string]string{}
	for _, siteID := range siteIDs {
		if !resourceNamespacePattern.MatchString(siteID) {
			return HostSpec{}, nil, fmt.Errorf("site %q must contain only lowercase letters, numbers, and hyphens and be 1-32 characters", siteID)
		}
		if siteID == "edge" {
			return HostSpec{}, nil, fmt.Errorf("site ID %q is reserved for the shared Edge", siteID)
		}
		site, err := validateSiteSpec(siteID, spec.Sites[siteID])
		if err != nil {
			return HostSpec{}, nil, err
		}
		if previous, exists := domains[site.Domain]; exists {
			return HostSpec{}, nil, fmt.Errorf("sites.%s and sites.%s must not share domain %q", previous, siteID, site.Domain)
		}
		domains[site.Domain] = siteID
		if previous, exists := prefixes[site.ResourcePrefix]; exists {
			return HostSpec{}, nil, fmt.Errorf("sites.%s and sites.%s must not share resourcePrefix %q", previous, siteID, site.ResourcePrefix)
		}
		prefixes[site.ResourcePrefix] = siteID

		secrets, hasSecrets := spec.SiteSecrets[siteID]
		if !hasSecrets {
			return HostSpec{}, nil, fmt.Errorf("siteSecrets.%s is required", siteID)
		}
		if err := validateSiteSecrets(siteID, site, secrets); err != nil {
			return HostSpec{}, nil, err
		}

		resolved.Sites[siteID] = site
		resolved.SiteSecrets[siteID] = secrets
		layouts = append(layouts, DeriveSiteLayout(siteID, site.ResourcePrefix))
	}
	return resolved, layouts, nil
}

func validateEdge(edge EdgeSpec) (EdgeSpec, error) {
	for _, requiredValue := range []struct {
		value string
		name  string
	}{
		{edge.OriginIP, "edge.originIp"},
		{edge.CloudflareZoneID, "edge.cloudflareZoneId"},
		{edge.ACMEEmail, "edge.acmeEmail"},
		{edge.TraefikImage, "edge.traefikImage"},
		{edge.SingBox.ServerName, "edge.singBox.serverName"},
	} {
		if _, err := required(requiredValue.value, requiredValue.name); err != nil {
			return EdgeSpec{}, err
		}
	}
	if err := validateSingBoxTarget(edge.SingBox.Target); err != nil {
		return EdgeSpec{}, err
	}
	serverName, err := normalizeDNSHostname(edge.SingBox.ServerName, true)
	if err != nil {
		return EdgeSpec{}, fmt.Errorf("edge.singBox.serverName %w", err)
	}
	edge.SingBox.ServerName = serverName
	return edge, nil
}

func validateSiteSpec(siteID string, site SiteSpec) (SiteSpec, error) {
	name := func(field string) string { return "sites." + siteID + "." + field }
	for _, requiredValue := range []struct {
		value string
		field string
	}{
		{site.Domain, "domain"},
		{site.Image, "image"},
		{site.AdminEmail, "adminEmail"},
		{site.AppProbePath, "appProbePath"},
	} {
		if _, err := required(requiredValue.value, name(requiredValue.field)); err != nil {
			return SiteSpec{}, err
		}
	}
	if !immutableImagePattern.MatchString(site.Image) {
		return SiteSpec{}, fmt.Errorf("%s must contain an immutable @sha256 digest with 64 lowercase hexadecimal characters", name("image"))
	}
	domain, err := normalizeDNSHostname(site.Domain, true)
	if err != nil {
		return SiteSpec{}, fmt.Errorf("%s %w", name("domain"), err)
	}
	if !strings.HasPrefix(site.AppProbePath, "/") {
		return SiteSpec{}, fmt.Errorf("%s must be an absolute path", name("appProbePath"))
	}
	if site.AppProbePath == "/health" {
		return SiteSpec{}, fmt.Errorf("%s must not be /health; /health is only a liveness probe", name("appProbePath"))
	}

	databaseMode := defaultString(site.Database.Mode, "docker")
	redisMode := defaultString(site.Redis.Mode, "docker")
	if databaseMode != "docker" && databaseMode != "neon" {
		return SiteSpec{}, fmt.Errorf("%s must be docker or neon", name("database.mode"))
	}
	if redisMode != "docker" && redisMode != "upstash" {
		return SiteSpec{}, fmt.Errorf("%s must be docker or upstash", name("redis.mode"))
	}
	databaseResourceMode, err := resourceMode(defaultString(site.Database.ResourceMode, "existing"), name("database.resourceMode"))
	if err != nil {
		return SiteSpec{}, err
	}
	var compute NeonComputeSpec
	if databaseMode == "neon" {
		compute = NeonComputeSpec{
			MinCU:                 floatPtr(defaultFloat(site.Database.Compute.MinCU, 0.25)),
			MaxCU:                 floatPtr(defaultFloat(site.Database.Compute.MaxCU, 0.25)),
			SuspendTimeoutSeconds: intPtr(defaultInt(site.Database.Compute.SuspendTimeoutSeconds, 300)),
		}
		if *compute.MinCU < 0.25 || *compute.MinCU > 16 {
			return SiteSpec{}, fmt.Errorf("%s.compute.minCU must be between 0.25 and 16", name("database"))
		}
		if *compute.MaxCU < *compute.MinCU || *compute.MaxCU > 16 {
			return SiteSpec{}, fmt.Errorf("%s.compute.maxCU must be between minCU and 16", name("database"))
		}
		if *compute.SuspendTimeoutSeconds < 60 || *compute.SuspendTimeoutSeconds > 604800 {
			return SiteSpec{}, fmt.Errorf("%s.compute.suspendTimeoutSeconds must be between 60 and 604800", name("database"))
		}
	}
	redisResourceMode, err := resourceMode(defaultString(site.Redis.ResourceMode, "existing"), name("redis.resourceMode"))
	if err != nil {
		return SiteSpec{}, err
	}
	if redisMode == "upstash" && redisResourceMode == "existing" {
		if _, err := normalizeUpstashEndpoint(site.Redis.Endpoint); err != nil {
			return SiteSpec{}, fmt.Errorf("%s: %w", name("redis.endpoint"), err)
		}
	}

	resourcePrefix := defaultString(site.ResourcePrefix, siteID)
	if !resourceNamespacePattern.MatchString(resourcePrefix) {
		return SiteSpec{}, fmt.Errorf("%s must contain only lowercase letters, numbers, and hyphens and be 1-32 characters", name("resourcePrefix"))
	}
	drainSeconds := defaultInt(site.DrainSeconds, 10)
	if drainSeconds < 0 {
		return SiteSpec{}, fmt.Errorf("%s must be zero or greater", name("drainSeconds"))
	}

	return SiteSpec{
		Domain:         domain,
		Image:          site.Image,
		AdminEmail:     site.AdminEmail,
		AppProbePath:   site.AppProbePath,
		DrainSeconds:   &drainSeconds,
		ResourcePrefix: resourcePrefix,
		Database: DatabaseSpec{
			Mode:         databaseMode,
			ResourceMode: databaseResourceMode,
			Compute:      compute,
		},
		Redis: RedisSpec{
			Mode:         redisMode,
			ResourceMode: redisResourceMode,
			Endpoint:     strings.TrimSpace(site.Redis.Endpoint),
			Region:       defaultString(site.Redis.Region, "us-east-1"),
		},
	}, nil
}

func validateSiteSecrets(siteID string, site SiteSpec, secrets SiteSecrets) error {
	name := func(field string) string { return "sites." + siteID + "." + field }
	for _, requiredValue := range []struct {
		value string
		field string
	}{
		{secrets.AdminPassword, "adminPassword"},
		{secrets.JWTSecret, "jwtSecret"},
		{secrets.TOTPEncryptionKey, "totpEncryptionKey"},
	} {
		if _, err := required(requiredValue.value, name(requiredValue.field)); err != nil {
			return err
		}
	}
	switch {
	case site.Database.Mode == "docker":
		if _, err := required(secrets.Database.Password, name("database.password")); err != nil {
			return err
		}
	case site.Database.Mode == "neon" && site.Database.ResourceMode == "create":
		if _, err := required(secrets.Database.APIToken, name("database.apiToken")); err != nil {
			return err
		}
	case site.Database.Mode == "neon":
		if _, err := required(secrets.Database.DSN, name("database.dsn")); err != nil {
			return err
		}
		if _, err := ParsePostgresDSN(secrets.Database.DSN); err != nil {
			return fmt.Errorf("%s is invalid: %w", name("database.dsn"), err)
		}
	}
	switch {
	case site.Redis.Mode == "docker":
		if _, err := required(secrets.Redis.Password, name("redis.password")); err != nil {
			return err
		}
	case site.Redis.Mode == "upstash" && site.Redis.ResourceMode == "create":
		if _, err := required(secrets.Redis.APIKey, name("redis.apiKey")); err != nil {
			return err
		}
	case site.Redis.Mode == "upstash":
		if _, err := required(secrets.Redis.Password, name("redis.password")); err != nil {
			return err
		}
	}
	if err := validateAppEnv(siteID, secrets.AppEnv); err != nil {
		return err
	}
	return nil
}

var appEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedAppEnvKeys = map[string]bool{
	"SITE_ID": true, "SITE_RUNTIME_ROOT": true, "SITE_RUNTIME_ENV_PATH": true, "SITE_APP_ENV_PATH": true,
	"SITE_DEPLOY_STATE_PATH": true, "SITE_BOOTSTRAP_MARKER_PATH": true, "COMPOSE_PROJECT_NAME": true,
	"SITE_ROUTE_PATH": true, "BLUE_DATA_PATH": true, "GREEN_DATA_PATH": true, "BLUE_EDGE_ALIAS": true,
	"GREEN_EDGE_ALIAS": true, "ACTIVE_EDGE_ALIAS": true, "EDGE_NETWORK_NAME": true, "DOMAIN": true,
	"ORIGIN_IP": true, "APP_PROBE_PATH": true, "DRAIN_SECONDS": true, "CONFIGURED_SITE_IDS": true,
	"HOST_STATE_PATH": true, "SUB2API_IMAGE": true, "SLOT": true, "SLOT_DATA_DIR": true,
	"AUTO_SETUP": true, "RUNTIME_ROOT": true, "ACTIVE_SLOT": true, "APP_ENV_CONFIGURED": true, "APP_ENV_JSON": true, "RUNTIME_JSON": true,
	"TRAEFIK_IMAGE": true, "CLOUDFLARE_DNS_API_TOKEN": true, "CLOUDFLARE_API_TOKEN": true, "ACME_EMAIL": true,
	"SING_BOX_SERVER_NAME": true, "SING_BOX_TARGET": true, "SING_BOX_CONFIG": true, "EDGE_RUNTIME_ROOT": true,
	"POSTGRES_MODE": true, "REDIS_MODE": true, "PROBE_RETRIES": true, "PROBE_DELAY_SECONDS": true,
}

var reservedAppEnvPrefixes = []string{"DATABASE_", "POSTGRES_", "REDIS_", "SITE_", "COMPOSE_"}

var upstreamEnvironmentKeys = map[string]bool{
	"BIND_HOST": true, "SERVER_HOST": true, "SERVER_PORT": true, "SERVER_MODE": true, "ENABLE_SERVER_TIMING": true, "RUN_MODE": true,
	"UPDATE_GITHUB_TOKEN": true, "ALIPAY_MOBILE_PRECREATE_DEEP_LINK": true, "DATABASE_HOST": true, "DATABASE_PORT": true,
	"DATABASE_USER": true, "DATABASE_PASSWORD": true, "DATABASE_DBNAME": true, "DATABASE_SSLMODE": true,
	"DATABASE_MAX_OPEN_CONNS": true, "DATABASE_MAX_IDLE_CONNS": true, "DATABASE_CONN_MAX_LIFETIME_MINUTES": true,
	"DATABASE_CONN_MAX_IDLE_TIME_MINUTES": true, "REDIS_HOST": true, "REDIS_PORT": true, "REDIS_USERNAME": true,
	"REDIS_PASSWORD": true, "REDIS_DB": true, "REDIS_POOL_SIZE": true, "REDIS_MIN_IDLE_CONNS": true, "REDIS_ENABLE_TLS": true,
	"ADMIN_EMAIL": true, "ADMIN_PASSWORD": true, "JWT_SECRET": true, "JWT_EXPIRE_HOUR": true, "SETUP_MIGRATION_TIMEOUT_SECONDS": true,
	"TOTP_ENCRYPTION_KEY": true, "TZ": true, "POSTGRES_USER": true, "POSTGRES_PASSWORD": true, "POSTGRES_DB": true,
	"PGDATA": true, "REDISCLI_AUTH": true, "GEMINI_OAUTH_CLIENT_ID": true, "GEMINI_OAUTH_CLIENT_SECRET": true,
	"GEMINI_OAUTH_SCOPES": true, "GEMINI_QUOTA_POLICY": true, "GEMINI_CLI_OAUTH_CLIENT_SECRET": true,
	"ANTIGRAVITY_OAUTH_CLIENT_SECRET": true, "ANTIGRAVITY_USER_AGENT_VERSION": true, "SECURITY_URL_ALLOWLIST_ENABLED": true,
	"SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP": true, "SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS": true,
	"SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS": true, "UPDATE_PROXY_URL": true, "GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT": true,
	"GATEWAY_OPENAI_HTTP2_ENABLED": true, "GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1": true,
	"GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD": true, "GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS": true,
	"GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS": true, "GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_FAILURE_THRESHOLD": true,
	"GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_WINDOW_SECONDS": true, "GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_TTL_SECONDS": true,
	"GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT": true, "GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL": true,
	"GATEWAY_IMAGE_CONCURRENCY_ENABLED": true, "GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS": true,
	"GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE": true, "GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS": true,
	"GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS": true,
	"POSTGRES_MAX_CONNECTIONS": true, "POSTGRES_SHARED_BUFFERS": true, "POSTGRES_EFFECTIVE_CACHE_SIZE": true,
	"POSTGRES_MAINTENANCE_WORK_MEM": true,
}

func validateAppEnv(siteID string, appEnv *map[string]string) error {
	if appEnv == nil {
		return nil
	}
	for key, value := range *appEnv {
		if !appEnvKeyPattern.MatchString(key) {
			return fmt.Errorf("siteSecrets.%s.appEnv key %q is invalid", siteID, key)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("siteSecrets.%s.appEnv.%s contains NUL or newline", siteID, key)
		}
		reserved := reservedAppEnvKeys[key] || upstreamEnvironmentKeys[key]
		for _, prefix := range reservedAppEnvPrefixes {
			reserved = reserved || strings.HasPrefix(key, prefix)
		}
		if reserved {
			return fmt.Errorf("siteSecrets.%s.appEnv key %q is deployment or Compose-owned", siteID, key)
		}
	}
	return nil
}

var immutableImagePattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
var decimalPortPattern = regexp.MustCompile(`^[0-9]+$`)

// normalizeUpstashEndpoint accepts only the hostname endpoint returned by
// Upstash. Ports, schemes, and paths are not configuration inputs here.
func normalizeUpstashEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("is required")
	}
	if strings.ContainsAny(endpoint, ":/\t\n ") {
		return "", fmt.Errorf("must be a hostname without scheme, port, or path")
	}
	return endpoint, nil
}
