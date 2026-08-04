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

// Existing code2 resources were Stack-owned. New Sites must not acquire aliases.
func legacyCode2Aliases(layout SiteLayout, oldName string) []pulumi.Alias {
	if layout.SiteID != "code2" || layout.ComposeProject != "sub2api" || layout.RuntimeRoot != "runtime" { return nil }
	return []pulumi.Alias{{Name: pulumi.String(oldName), NoParent: pulumi.Bool(true)}}
}

func newSiteCommand(ctx *pulumi.Context, layout SiteLayout, name, oldName, script string, environment pulumi.StringMap, triggers []string, parent pulumi.Resource, dependencies ...pulumi.Resource) (*local.Command, error) {
	opts := []pulumi.ResourceOption{pulumi.Parent(parent), pulumi.Aliases(legacyCode2Aliases(layout, oldName)), pulumi.Version("1.2.1"), pulumi.DependsOn(dependencies), pulumi.AdditionalSecretOutputs([]string{"stdout", "stderr"})}
	return local.NewCommand(ctx, name, &local.CommandArgs{Create: pulumi.String(script), Update: pulumi.String(script), Environment: environment, Logging: local.LoggingNone, Triggers: stringArray(triggers)}, opts...)
}

func newHostCommand(ctx *pulumi.Context, name, script string, environment pulumi.StringMap, triggers []string, dependencies ...pulumi.Resource) (*local.Command, error) {
	opts := []pulumi.ResourceOption{pulumi.Version("1.2.1"), pulumi.DependsOn(dependencies), pulumi.AdditionalSecretOutputs([]string{"stdout", "stderr"})}
	return local.NewCommand(ctx, name, &local.CommandArgs{Create: pulumi.String(script), Update: pulumi.String(script), Environment: environment, Logging: local.LoggingNone, Triggers: stringArray(triggers)}, opts...)
}

func siteEnvironment(siteID string, spec SiteSpec, layout SiteLayout, runtime pulumi.StringInput, configuredSiteIDs, originIP string) pulumi.StringMap {
	return pulumi.StringMap{
		"SITE_ID": pulumi.String(siteID), "SITE_RUNTIME_ROOT": pulumi.String(layout.RuntimeRoot),
		"COMPOSE_PROJECT_NAME": pulumi.String(layout.ComposeProject), "SITE_ROUTE_PATH": pulumi.String(layout.RoutePath),
		"SITE_RUNTIME_ENV_PATH": pulumi.String(layout.RuntimeEnvPath), "SITE_DEPLOY_STATE_PATH": pulumi.String(layout.DeployStatePath),
		"SITE_BOOTSTRAP_MARKER_PATH": pulumi.String(layout.BootstrapMarkerPath), "BLUE_DATA_PATH": pulumi.String(layout.BlueDataPath),
		"GREEN_DATA_PATH": pulumi.String(layout.GreenDataPath), "BLUE_EDGE_ALIAS": pulumi.String(layout.BlueAlias),
		"GREEN_EDGE_ALIAS": pulumi.String(layout.GreenAlias), "ACTIVE_EDGE_ALIAS": pulumi.String(layout.BlueAlias),
		"EDGE_NETWORK_NAME": pulumi.String(EdgeNetworkName), "DOMAIN": pulumi.String(spec.Domain),
		"APP_PROBE_PATH": pulumi.String(spec.AppProbePath), "DRAIN_SECONDS": pulumi.String(formatInt(*spec.DrainSeconds)), "ORIGIN_IP": pulumi.String(originIP),
		"ADMIN_EMAIL": pulumi.String(spec.AdminEmail), "POSTGRES_MODE": pulumi.String(spec.Database.Mode),
		"REDIS_MODE": pulumi.String(spec.Redis.Mode), "RUNTIME_JSON": runtime,
		"CONFIGURED_SITE_IDS": pulumi.String(configuredSiteIDs), "HOST_STATE_PATH": pulumi.String("runtime/host-state.json"),
	}
}

func stringArray(values []string) pulumi.Array {
	result := make(pulumi.Array, 0, len(values))
	for _, value := range values { result = append(result, pulumi.String(value)) }
	return result
}

func formatInt(value int) string { return strconv.Itoa(value) }
