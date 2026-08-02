package main

import (
	"strconv"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func CreateInfraReconcileCommand(ctx *pulumi.Context, config DeploymentConfig, runtimePayload pulumi.StringInput, composeChecksum string, domain pulumi.Resource) (*local.Command, error) {
	triggers := BuildInfraTriggers(InfraTriggerInput{
		ResourceNamespace: config.ResourceNamespace,
		Domain:            config.Domain,
		OriginIP:          config.OriginIP,
		PostgresMode:      config.PostgresMode,
		RedisMode:         config.RedisMode,
		TraefikImage:      config.TraefikImage,
		ACMEEmail:         config.ACMEEmail,
		AppProbePath:      config.AppProbePath,
		DrainSeconds:      config.DrainSeconds,
		ComposeChecksum:   composeChecksum,
		ResourceModes:     BuildResourceModes(config),
	})
	return local.NewCommand(ctx, "infra-reconcile", &local.CommandArgs{
		Create: pulumi.String("bash scripts/infra-reconcile.sh"),
		Update: pulumi.String("bash scripts/infra-reconcile.sh"),
		Environment: pulumi.StringMap{
			"RUNTIME_JSON":   runtimePayload,
			"SUB2API_IMAGE":  pulumi.String(config.Sub2APIImage),
			"POSTGRES_MODE":  pulumi.String(config.PostgresMode),
			"REDIS_MODE":     pulumi.String(config.RedisMode),
			"DOMAIN":         pulumi.String(config.Domain),
			"APP_PROBE_PATH": pulumi.String(config.AppProbePath),
			"ORIGIN_IP":      pulumi.String(config.OriginIP),
			"ACME_EMAIL":     pulumi.String(config.ACMEEmail),
			"DRAIN_SECONDS":  pulumi.String(formatInt(config.DrainSeconds)),
			"TRAEFIK_IMAGE":  pulumi.String(config.TraefikImage),
		},
		Logging:  local.LoggingNone,
		Triggers: stringArray(triggers),
	}, pulumi.Version("1.2.1"), pulumi.DependsOn([]pulumi.Resource{domain}), pulumi.IgnoreChanges([]string{"environment.SUB2API_IMAGE"}), pulumi.AdditionalSecretOutputs([]string{"stdout", "stderr"}))
}

func CreateStrictReadinessCommand(ctx *pulumi.Context, config DeploymentConfig, triggers []string, strictSSL pulumi.Resource) (*local.Command, error) {
	return local.NewCommand(ctx, "post-strict-public-readiness", &local.CommandArgs{
		Create: pulumi.String(`bash scripts/probe-origin.sh "$DOMAIN" "/health"`),
		Update: pulumi.String(`bash scripts/probe-origin.sh "$DOMAIN" "/health"`),
		Environment: pulumi.StringMap{
			"DOMAIN": pulumi.String(config.Domain),
		},
		Logging:  local.LoggingNone,
		Triggers: stringArray(triggers),
	}, pulumi.Version("1.2.1"), pulumi.DependsOn([]pulumi.Resource{strictSSL}), pulumi.AdditionalSecretOutputs([]string{"stdout", "stderr"}))
}

func CreateApplicationReleaseCommand(ctx *pulumi.Context, config DeploymentConfig, strictReadiness, infraReconcile pulumi.Resource) (*local.Command, error) {
	return local.NewCommand(ctx, "application-release", &local.CommandArgs{
		Create: pulumi.String("bash scripts/application-release.sh"),
		Update: pulumi.String("bash scripts/application-release.sh"),
		Environment: pulumi.StringMap{
			"SUB2API_IMAGE": pulumi.String(config.Sub2APIImage),
		},
		Logging:  local.LoggingNone,
		Triggers: stringArray(BuildReleaseTriggers(config.Sub2APIImage)),
	}, pulumi.Version("1.2.1"), pulumi.DependsOn([]pulumi.Resource{infraReconcile, strictReadiness}), pulumi.AdditionalSecretOutputs([]string{"stdout", "stderr"}))
}

func stringArray(values []string) pulumi.Array {
	result := make(pulumi.Array, 0, len(values))
	for _, value := range values {
		result = append(result, pulumi.String(value))
	}
	return result
}

func formatInt(value int) string {
	if value == 0 {
		return "0"
	}
	return strconv.Itoa(value)
}
