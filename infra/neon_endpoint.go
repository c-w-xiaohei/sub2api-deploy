package main

import (
	"fmt"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type neonProjectLike interface {
	pulumi.Resource
	ProjectID() pulumi.IDOutput
	EndpointHostOutput() pulumi.StringOutput
}

func validateNeonRegion(ctx *pulumi.Context, site pulumi.Resource, siteID string, project *neonProject, apiKey pulumi.StringInput, region string, preflight pulumi.Resource, endpointChecksum string) (*local.Command, error) {
	environment := pulumi.StringMap{"NEON_API_KEY": apiKey, "NEON_PROJECT_ID": project.ID(), "NEON_ENDPOINT_HOST": project.Default_endpoint_host, "NEON_REGION": pulumi.String(region)}
	triggers := []string{"neon-region-validation-v1", siteID, endpointChecksum, region}
	return newCommand(ctx, "site-"+siteID+"-neon-region", "bash scripts/node-env.sh npx --no-install tsx scripts/validate-neon-region.ts", environment, triggers, site, preflight, project)
}

func reconcileNeonEndpointSettings(ctx *pulumi.Context, site pulumi.Resource, siteID string, project neonProjectLike, apiKey pulumi.StringInput, region string, compute NeonComputeSpec, preflight, regionValidation pulumi.Resource, endpointChecksum string) (*local.Command, error) {
	environment := pulumi.StringMap{
		"NEON_API_KEY":                 apiKey,
		"NEON_PROJECT_ID":              project.ProjectID(),
		"NEON_ENDPOINT_HOST":           project.EndpointHostOutput(),
		"NEON_AUTOSCALING_MIN_CU":      pulumi.String(formatFloat(*compute.MinCU)),
		"NEON_AUTOSCALING_MAX_CU":      pulumi.String(formatFloat(*compute.MaxCU)),
		"NEON_SUSPEND_TIMEOUT_SECONDS": pulumi.String(fmt.Sprintf("%d", *compute.SuspendTimeoutSeconds)),
	}
	triggers := []string{"neon-endpoint-settings-v1", siteID, endpointChecksum, region, formatFloat(*compute.MinCU), formatFloat(*compute.MaxCU), fmt.Sprintf("%d", *compute.SuspendTimeoutSeconds)}
	dependencies := []pulumi.Resource{preflight, project}
	if regionValidation != nil {
		dependencies = append(dependencies, regionValidation)
	}
	return newCommand(ctx, "site-"+siteID+"-neon-endpoint-settings", "bash scripts/node-env.sh npx --no-install tsx scripts/reconcile-neon-endpoint.ts", environment, triggers, site, dependencies...)
}

func formatFloat(value float64) string { return fmt.Sprintf("%g", value) }
