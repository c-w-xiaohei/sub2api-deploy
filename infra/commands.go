package main

import (
	"strconv"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func newCommand(ctx *pulumi.Context, name, script string, environment pulumi.StringMap, triggers []string, parent pulumi.Resource, dependencies ...pulumi.Resource) (*local.Command, error) {
	opts := []pulumi.ResourceOption{pulumi.Parent(parent), pulumi.Version("1.2.1"), pulumi.DependsOn(dependencies), pulumi.AdditionalSecretOutputs([]string{"stdout", "stderr"})}
	return local.NewCommand(ctx, name, &local.CommandArgs{Create: pulumi.String(script), Update: pulumi.String(script), Environment: environment, Logging: local.LoggingNone, Triggers: stringArray(triggers)}, opts...)
}

func siteEnvironment(siteID string, spec SiteSpec, layout SiteLayout, runtime pulumi.StringInput) pulumi.StringMap {
	return pulumi.StringMap{
		"SITE_ID": pulumi.String(siteID), "SITE_RUNTIME_ROOT": pulumi.String(layout.RuntimeRoot),
		"COMPOSE_PROJECT_NAME": pulumi.String(layout.ComposeProject), "SITE_ROUTE_PATH": pulumi.String(layout.RoutePath),
		"SITE_RUNTIME_ENV_PATH": pulumi.String(layout.RuntimeEnvPath), "SITE_DEPLOY_STATE_PATH": pulumi.String(layout.DeployStatePath),
		"SITE_BOOTSTRAP_MARKER_PATH": pulumi.String(layout.BootstrapMarkerPath), "BLUE_DATA_PATH": pulumi.String(layout.BlueDataPath),
		"GREEN_DATA_PATH": pulumi.String(layout.GreenDataPath), "BLUE_EDGE_ALIAS": pulumi.String(layout.BlueAlias),
		"GREEN_EDGE_ALIAS": pulumi.String(layout.GreenAlias), "ACTIVE_EDGE_ALIAS": pulumi.String(layout.BlueAlias),
		"EDGE_NETWORK_NAME": pulumi.String(EdgeNetworkName), "DOMAIN": pulumi.String(spec.Domain),
		"APP_PROBE_PATH": pulumi.String(spec.AppProbePath), "DRAIN_SECONDS": pulumi.String(formatInt(*spec.DrainSeconds)),
		"ADMIN_EMAIL": pulumi.String(spec.AdminEmail), "POSTGRES_MODE": pulumi.String(spec.Database.Mode),
		"REDIS_MODE": pulumi.String(spec.Redis.Mode), "RUNTIME_JSON": runtime,
	}
}

func stringArray(values []string) pulumi.Array {
	result := make(pulumi.Array, 0, len(values))
	for _, value := range values { result = append(result, pulumi.String(value)) }
	return result
}

func formatInt(value int) string { return strconv.Itoa(value) }
