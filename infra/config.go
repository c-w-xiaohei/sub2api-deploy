package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

var resourceNamespacePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

type ProgramConfig struct {
	Host    HostSpec
	Layouts []SiteLayout
	Secrets SecretHostSpec
}

// SecretHostSpec retains the Pulumi-secret wrapper from the two secret object
// settings while keeping resource construction independent from config loading.
type SecretHostSpec struct {
	Edge   pulumi.StringOutput
	Sites  map[string]pulumi.StringOutput
	AppEnv map[string]pulumi.StringOutput
}

// resolveHostConfig is the shared boundary after Pulumi decodes the four
// public configuration objects and before their secret values are wrapped.
func resolveHostConfig(edge EdgeSpec, sites map[string]SiteSpec, edgeSecrets EdgeSecrets, siteSecrets map[string]SiteSecrets) (HostSpec, []SiteLayout, error) {
	return ValidateHostSpec(HostSpec{
		Edge:        edge,
		Sites:       sites,
		EdgeSecrets: edgeSecrets,
		SiteSecrets: siteSecrets,
	})
}

func required(value, name string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func resourceMode(value, name string) (string, error) {
	if value != "existing" && value != "create" {
		return "", fmt.Errorf("%s must be existing or create", name)
	}
	return value, nil
}

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
	var edge EdgeSpec
	var sites map[string]SiteSpec
	if err := pulumiConfig.GetObject("edge", &edge); err != nil {
		return ProgramConfig{}, err
	}
	if err := pulumiConfig.GetObject("sites", &sites); err != nil {
		return ProgramConfig{}, err
	}

	var edgeSecrets EdgeSecrets
	_, err := pulumiConfig.GetSecretObject("edgeSecrets", &edgeSecrets)
	if err != nil {
		return ProgramConfig{}, err
	}
	var siteSecrets map[string]SiteSecrets
	_, err = pulumiConfig.GetSecretObject("siteSecrets", &siteSecrets)
	if err != nil {
		return ProgramConfig{}, err
	}

	host, layouts, err := resolveHostConfig(edge, sites, edgeSecrets, siteSecrets)
	if err != nil {
		return ProgramConfig{}, err
	}
	secrets := wrapHostSecrets(host, layouts)
	return ProgramConfig{Host: host, Layouts: layouts, Secrets: secrets}, nil
}

// wrapHostSecrets is the only transition from decoded secret objects to
// runtime-compatible Pulumi secret outputs.
func wrapHostSecrets(host HostSpec, layouts []SiteLayout) SecretHostSpec {
	secrets := SecretHostSpec{Edge: pulumi.ToSecret(pulumi.String(host.EdgeSecrets.CloudflareAPIToken)).(pulumi.StringOutput), Sites: make(map[string]pulumi.StringOutput, len(layouts)), AppEnv: make(map[string]pulumi.StringOutput, len(layouts))}
	for _, layout := range layouts {
		siteID := layout.SiteID
		secrets.Sites[siteID] = pulumi.ToSecret(pulumi.String(marshalRuntimeSecrets(host.SiteSecrets[siteID]))).(pulumi.StringOutput)
		secrets.AppEnv[siteID] = pulumi.ToSecret(pulumi.String(marshalAppEnv(host.SiteSecrets[siteID].AppEnv))).(pulumi.StringOutput)
	}
	return secrets
}
