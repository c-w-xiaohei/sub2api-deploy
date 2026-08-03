package main

import (
	"fmt"
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
	OriginIP         string      `json:"originIp"`
	CloudflareZoneID string      `json:"cloudflareZoneId"`
	ACMEEmail        string      `json:"acmeEmail"`
	TraefikImage     string      `json:"traefikImage"`
	SingBox          SingBoxSpec `json:"singBox"`
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
	Mode         string `json:"mode"`
	ResourceMode string `json:"resourceMode"`
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
	AdminPassword     string          `json:"adminPassword"`
	JWTSecret         string          `json:"jwtSecret"`
	TOTPEncryptionKey string          `json:"totpEncryptionKey"`
	Database          DatabaseSecrets `json:"database"`
	Redis             RedisSecrets    `json:"redis"`
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

func validateSingBoxTarget(target string) error {
	host, portText, found := strings.Cut(target, ":")
	if !found || host == "" || portText == "" {
		return fmt.Errorf("edge.singBox.target must be host:port, got %q", target)
	}
	if strings.ContainsAny(host, " \t") {
		return fmt.Errorf("edge.singBox.target must be host:port, got %q", target)
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

	return SiteSpec{
		Domain:         site.Domain,
		Image:          site.Image,
		AdminEmail:     site.AdminEmail,
		AppProbePath:   site.AppProbePath,
		DrainSeconds:   &drainSeconds,
		ResourcePrefix: resourcePrefix,
		Database: DatabaseSpec{
			Mode:         databaseMode,
			ResourceMode: databaseResourceMode,
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
	return nil
}

var immutableImagePattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

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
