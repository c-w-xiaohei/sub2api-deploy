package main

import (
	"strings"
	"testing"
)

func validDeploymentInput() DeploymentInput {
	return DeploymentInput{
		Domain:             "sub2api.example.com",
		OriginIP:           "203.0.113.10",
		PostgresMode:       "docker",
		RedisMode:          "docker",
		Sub2APIImage:       "weishaw/sub2api@sha256:abcdef1234567890",
		TraefikImage:       "traefik:v3.3.3",
		CloudflareAPIToken: "cloudflare-secret",
		CloudflareZoneID:   "zone-id",
		ACMEEmail:          "ops@example.com",
		AppProbePath:       "/api/ready",
		PostgresPassword:   "postgres-secret",
		RedisPassword:      "redis-secret",
	}
}

func TestValidateDeploymentConfigDefaultsAndDigest(t *testing.T) {
	config, err := ValidateDeploymentConfig(validDeploymentInput())
	if err != nil {
		t.Fatalf("ValidateDeploymentConfig() error = %v", err)
	}
	if config.ResourceNamespace != "sub2api" || config.PostgresUser != "sub2api" || config.PostgresDB != "sub2api" {
		t.Fatalf("unexpected PostgreSQL defaults: %+v", config)
	}
	if config.TraefikImage != "traefik:v3.3.3" || config.NeonPort != 5432 || config.RedisPort != 6379 {
		t.Fatalf("unexpected provider defaults: %+v", config)
	}
	if config.UpstashDatabaseName != "sub2api-redis" || config.UpstashRegion != "us-east-1" || config.DrainSeconds != 10 {
		t.Fatalf("unexpected managed defaults: %+v", config)
	}
	if config.NeonResourceMode != "existing" || config.UpstashResourceMode != "existing" {
		t.Fatalf("unexpected resource mode defaults: %+v", config)
	}
}

func TestDefaultIntPreservesExplicitZero(t *testing.T) {
	zero := 0
	if got := defaultInt(&zero, 10); got != 0 {
		t.Fatalf("defaultInt(explicit zero) = %d, want 0", got)
	}
}

func TestValidateDeploymentConfigPreservesExplicitZeroIntegers(t *testing.T) {
	input := validDeploymentInput()
	input.PostgresMode = "neon"
	input.PostgresPassword = ""
	input.NeonHost = "ep.example.neon.tech"
	input.NeonPassword = "neon-secret"
	input.RedisMode = "upstash"
	input.RedisPassword = ""
	input.UpstashHost = "upstash.example.com"
	input.UpstashPassword = "upstash-secret"
	input.NeonPort = intPointer(0)
	input.RedisPort = intPointer(0)
	input.UpstashPort = intPointer(0)
	input.DrainSeconds = intPointer(0)

	config, err := ValidateDeploymentConfig(input)
	if err != nil {
		t.Fatalf("ValidateDeploymentConfig() error = %v", err)
	}
	if config.NeonPort != 0 || config.RedisPort != 0 || config.UpstashPort != 0 || config.DrainSeconds != 0 {
		t.Fatalf("explicit zero values were defaulted: %+v", config)
	}
}

func intPointer(value int) *int { return &value }

func TestValidateDeploymentConfigPreservesValidationContracts(t *testing.T) {
	tests := []struct {
		name    string
		input   func() DeploymentInput
		message string
	}{
		{
			name: "unsupported postgres mode",
			input: func() DeploymentInput {
				input := validDeploymentInput()
				input.PostgresMode = "mysql"
				return input
			},
			message: "postgresMode must be docker or neon",
		},
		{
			name: "mutable image",
			input: func() DeploymentInput {
				input := validDeploymentInput()
				input.Sub2APIImage = "weishaw/sub2api:latest"
				return input
			},
			message: "sub2apiImage must contain an immutable @sha256 digest",
		},
		{
			name: "health probe",
			input: func() DeploymentInput {
				input := validDeploymentInput()
				input.AppProbePath = "/health"
				return input
			},
			message: "appProbePath must not be /health",
		},
		{
			name: "unsafe namespace",
			input: func() DeploymentInput {
				input := validDeploymentInput()
				input.ResourceNamespace = "Sub2API prod"
				return input
			},
			message: "resourceNamespace must contain only lowercase letters, numbers, and hyphens",
		},
		{
			name: "missing cloudflare token",
			input: func() DeploymentInput {
				input := validDeploymentInput()
				input.CloudflareAPIToken = ""
				return input
			},
			message: "cloudflareApiToken is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateDeploymentConfig(tt.input())
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("error = %v, want substring %q", err, tt.message)
			}
		})
	}
}

func TestValidateDeploymentConfigAllowsManagedAndDSNModes(t *testing.T) {
	neon := validDeploymentInput()
	neon.PostgresMode = "neon"
	neon.PostgresPassword = ""
	neon.NeonResourceMode = "create"
	neon.NeonAPIToken = "neon-api-token"
	if config, err := ValidateDeploymentConfig(neon); err != nil || config.NeonResourceMode != "create" {
		t.Fatalf("managed Neon config = %+v, error = %v", config, err)
	}

	dsn := validDeploymentInput()
	dsn.PostgresMode = "neon"
	dsn.PostgresPassword = ""
	dsn.NeonDSN = "postgresql://sub2api:secret@ep.example.neon.tech/sub2api?sslmode=require"
	if config, err := ValidateDeploymentConfig(dsn); err != nil || config.NeonDSN == "" {
		t.Fatalf("Neon DSN config = %+v, error = %v", config, err)
	}

	upstash := validDeploymentInput()
	upstash.RedisMode = "upstash"
	upstash.RedisPassword = ""
	upstash.UpstashResourceMode = "create"
	upstash.UpstashAPIKey = "upstash-api-key"
	upstash.UpstashEmail = "ops@example.com"
	config, err := ValidateDeploymentConfig(upstash)
	if err != nil || config.UpstashResourceMode != "create" {
		t.Fatalf("managed Upstash config = %+v, error = %v", config, err)
	}
}
