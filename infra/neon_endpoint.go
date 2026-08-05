package main

import (
	"fmt"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func reconcileNeonEndpointSettings(ctx *pulumi.Context, site pulumi.Resource, siteID string, project *neonProject, apiKey pulumi.StringInput, compute NeonComputeSpec, preflight pulumi.Resource, endpointChecksum string) (*local.Command, error) {
	environment := pulumi.StringMap{
		"NEON_API_KEY":                 apiKey,
		"NEON_PROJECT_ID":              project.ID(),
		"NEON_ENDPOINT_HOST":           project.Default_endpoint_host,
		"NEON_AUTOSCALING_MIN_CU":      pulumi.String(formatFloat(*compute.MinCU)),
		"NEON_AUTOSCALING_MAX_CU":      pulumi.String(formatFloat(*compute.MaxCU)),
		"NEON_SUSPEND_TIMEOUT_SECONDS": pulumi.String(fmt.Sprintf("%d", *compute.SuspendTimeoutSeconds)),
	}
	triggers := []string{"neon-endpoint-settings-v1", siteID, endpointChecksum, formatFloat(*compute.MinCU), formatFloat(*compute.MaxCU), fmt.Sprintf("%d", *compute.SuspendTimeoutSeconds)}
	return newCommand(ctx, "site-"+siteID+"-neon-endpoint-settings", "bash scripts/node-env.sh npx --no-install tsx scripts/reconcile-neon-endpoint.ts", environment, triggers, site, preflight, project)
}

func formatFloat(value float64) string { return fmt.Sprintf("%g", value) }
