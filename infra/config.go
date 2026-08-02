package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

type DeploymentInput struct {
	ResourceNamespace   string
	Domain              string
	OriginIP            string
	PostgresMode        string
	RedisMode           string
	Sub2APIImage        string
	TraefikImage        string
	NeonResourceMode    string
	UpstashResourceMode string

	CloudflareAPIToken string
	CloudflareZoneID   string
	ACMEEmail          string

	PostgresUser     string
	PostgresPassword string
	PostgresDB       string

	NeonHost     string
	NeonDSN      string
	NeonAPIToken string
	NeonOrgID    string
	NeonPort     *int
	NeonUser     string
	NeonPassword string
	NeonDB       string

	RedisHost     string
	RedisPort     *int
	RedisUsername string
	RedisPassword string

	UpstashHost         string
	UpstashAPIKey       string
	UpstashEmail        string
	UpstashDatabaseName string
	UpstashRegion       string
	UpstashPort         *int
	UpstashUsername     string
	UpstashPassword     string

	AdminEmail        string
	AdminPassword     string
	JWTSecret         string
	TOTPEncryptionKey string
	AppProbePath      string
	DrainSeconds      *int
}

type DeploymentConfig struct {
	DeploymentInput
	NeonPort     int
	RedisPort    int
	UpstashPort  int
	DrainSeconds int
}

type ProgramConfig struct {
	DeploymentConfig
	Secrets map[string]pulumi.StringOutput
}

var resourceNamespacePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

func required(value, name string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func selectedMode(value, name string) (string, error) {
	allowed := "docker or neon"
	if name == "redisMode" {
		allowed = "docker or upstash"
	}
	if value != "docker" && (name != "postgresMode" || value != "neon") && (name != "redisMode" || value != "upstash") {
		return "", fmt.Errorf("%s must be %s", name, allowed)
	}
	return value, nil
}

func resourceMode(value, name string) (string, error) {
	if value != "existing" && value != "create" {
		return "", fmt.Errorf("%s must be existing or create", name)
	}
	return value, nil
}

func ValidateDeploymentConfig(input DeploymentInput) (DeploymentConfig, error) {
	postgresMode, err := selectedMode(input.PostgresMode, "postgresMode")
	if err != nil {
		return DeploymentConfig{}, err
	}
	redisMode, err := selectedMode(input.RedisMode, "redisMode")
	if err != nil {
		return DeploymentConfig{}, err
	}

	resourceNamespace := strings.TrimSpace(input.ResourceNamespace)
	if resourceNamespace == "" {
		resourceNamespace = "sub2api"
	}
	if !resourceNamespacePattern.MatchString(resourceNamespace) {
		return DeploymentConfig{}, errorsf("resourceNamespace must contain only lowercase letters, numbers, and hyphens and be 1-32 characters")
	}

	for _, requiredValue := range []struct {
		value string
		name  string
	}{
		{input.Domain, "domain"},
		{input.OriginIP, "originIp"},
		{input.Sub2APIImage, "sub2apiImage"},
		{input.TraefikImage, "traefikImage"},
		{input.CloudflareZoneID, "cloudflareZoneId"},
		{input.ACMEEmail, "acmeEmail"},
	} {
		if _, err := required(requiredValue.value, requiredValue.name); err != nil {
			return DeploymentConfig{}, err
		}
	}

	input.ResourceNamespace = resourceNamespace
	input.PostgresMode = postgresMode
	input.RedisMode = redisMode
	input.PostgresUser = defaultString(input.PostgresUser, "sub2api")
	input.PostgresDB = defaultString(input.PostgresDB, "sub2api")
	input.NeonUser = defaultString(input.NeonUser, "sub2api")
	input.NeonDB = defaultString(input.NeonDB, "sub2api")
	input.UpstashUsername = defaultString(input.UpstashUsername, "default")
	input.UpstashDatabaseName = defaultString(input.UpstashDatabaseName, resourceNamespace+"-redis")
	input.UpstashRegion = defaultString(input.UpstashRegion, "us-east-1")
	input.AdminEmail = defaultString(input.AdminEmail, "admin@sub2api.local")

	input.NeonResourceMode, err = resourceMode(defaultString(input.NeonResourceMode, "existing"), "neonResourceMode")
	if err != nil {
		return DeploymentConfig{}, err
	}
	input.UpstashResourceMode, err = resourceMode(defaultString(input.UpstashResourceMode, "existing"), "upstashResourceMode")
	if err != nil {
		return DeploymentConfig{}, err
	}

	if !strings.Contains(input.Sub2APIImage, "@sha256:") {
		return DeploymentConfig{}, errorsf("sub2apiImage must contain an immutable @sha256 digest")
	}
	if _, err := required(input.CloudflareAPIToken, "cloudflareApiToken"); err != nil {
		return DeploymentConfig{}, err
	}
	if _, err := required(input.AppProbePath, "appProbePath"); err != nil {
		return DeploymentConfig{}, err
	}
	if !strings.HasPrefix(input.AppProbePath, "/") {
		return DeploymentConfig{}, errorsf("appProbePath must be an absolute path")
	}
	if input.AppProbePath == "/health" {
		return DeploymentConfig{}, errorsf("appProbePath must not be /health; /health is only a liveness probe")
	}

	if postgresMode == "neon" {
		if input.NeonResourceMode == "create" {
			if _, err := required(input.NeonAPIToken, "neonApiToken"); err != nil {
				return DeploymentConfig{}, err
			}
		} else if input.NeonDSN == "" {
			if _, err := required(input.NeonHost, "neonHost"); err != nil {
				return DeploymentConfig{}, err
			}
			if _, err := required(input.NeonPassword, "neonPassword"); err != nil {
				return DeploymentConfig{}, err
			}
		}
	} else if _, err := required(input.PostgresPassword, "postgresPassword"); err != nil {
		return DeploymentConfig{}, err
	}

	if redisMode == "upstash" {
		if input.UpstashResourceMode == "create" {
			if _, err := required(input.UpstashAPIKey, "upstashApiKey"); err != nil {
				return DeploymentConfig{}, err
			}
			if _, err := required(input.UpstashEmail, "upstashEmail"); err != nil {
				return DeploymentConfig{}, err
			}
		} else {
			if _, err := required(input.UpstashHost, "upstashHost"); err != nil {
				return DeploymentConfig{}, err
			}
			if _, err := required(input.UpstashPassword, "upstashPassword"); err != nil {
				return DeploymentConfig{}, err
			}
		}
	} else if _, err := required(input.RedisPassword, "redisPassword"); err != nil {
		return DeploymentConfig{}, err
	}

	return DeploymentConfig{
		DeploymentInput: input,
		NeonPort:        defaultInt(input.NeonPort, 5432),
		RedisPort:       defaultInt(input.RedisPort, 6379),
		UpstashPort:     defaultInt(input.UpstashPort, 6379),
		DrainSeconds:    defaultInt(input.DrainSeconds, 10),
	}, nil
}

func errorsf(message string) error { return fmt.Errorf("%s", message) }

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func defaultInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func loadProgramConfig(ctx *pulumi.Context) (ProgramConfig, error) {
	pulumiConfig := config.New(ctx, "")
	secretConfigured := func(key string) string {
		if _, err := pulumiConfig.TrySecret(key); err == nil {
			return "__configured_secret__"
		}
		return ""
	}
	get := pulumiConfig.Get
	getInt := func(key string) *int {
		if value := get(key); value != "" {
			var parsed int
			if _, err := fmt.Sscan(value, &parsed); err == nil {
				return &parsed
			}
		}
		return nil
	}

	input := DeploymentInput{
		ResourceNamespace:   get("resourceNamespace"),
		Domain:              pulumiConfig.Require("domain"),
		OriginIP:            pulumiConfig.Require("originIp"),
		PostgresMode:        defaultString(get("postgresMode"), "docker"),
		RedisMode:           defaultString(get("redisMode"), "docker"),
		Sub2APIImage:        pulumiConfig.Require("sub2apiImage"),
		TraefikImage:        defaultString(get("traefikImage"), "traefik:v3.3.3"),
		NeonResourceMode:    defaultString(get("neonResourceMode"), "existing"),
		UpstashResourceMode: defaultString(get("upstashResourceMode"), "existing"),
		CloudflareAPIToken:  secretConfigured("cloudflareApiToken"),
		CloudflareZoneID:    get("cloudflareZoneId"),
		ACMEEmail:           get("acmeEmail"),
		PostgresUser:        get("postgresUser"),
		PostgresPassword:    secretConfigured("postgresPassword"),
		PostgresDB:          get("postgresDb"),
		NeonHost:            get("neonHost"),
		NeonDSN:             secretConfigured("neonDsn"),
		NeonAPIToken:        secretConfigured("neonApiToken"),
		NeonOrgID:           get("neonOrgId"),
		NeonPort:            getInt("neonPort"),
		NeonUser:            get("neonUser"),
		NeonPassword:        secretConfigured("neonPassword"),
		NeonDB:              get("neonDb"),
		RedisHost:           get("redisHost"),
		RedisPort:           getInt("redisPort"),
		RedisUsername:       get("redisUsername"),
		RedisPassword:       secretConfigured("redisPassword"),
		UpstashHost:         get("upstashHost"),
		UpstashAPIKey:       secretConfigured("upstashApiKey"),
		UpstashEmail:        get("upstashEmail"),
		UpstashDatabaseName: get("upstashDatabaseName"),
		UpstashRegion:       get("upstashRegion"),
		UpstashPort:         getInt("upstashPort"),
		UpstashUsername:     get("upstashUsername"),
		UpstashPassword:     secretConfigured("upstashPassword"),
		AdminEmail:          get("adminEmail"),
		AdminPassword:       secretConfigured("adminPassword"),
		JWTSecret:           secretConfigured("jwtSecret"),
		TOTPEncryptionKey:   secretConfigured("totpEncryptionKey"),
		AppProbePath:        get("appProbePath"),
		DrainSeconds:        getInt("drainSeconds"),
	}
	validated, err := ValidateDeploymentConfig(input)
	if err != nil {
		return ProgramConfig{}, err
	}

	secret := func(key string) pulumi.StringOutput {
		return pulumiConfig.GetSecret(key)
	}
	secrets := map[string]pulumi.StringOutput{}
	for _, key := range []string{
		"cloudflareApiToken", "postgresPassword", "redisPassword", "neonPassword", "neonDsn", "neonApiToken",
		"upstashPassword", "upstashApiKey", "adminPassword", "jwtSecret", "totpEncryptionKey",
	} {
		secrets[key] = secret(key)
	}
	return ProgramConfig{DeploymentConfig: validated, Secrets: secrets}, nil
}
