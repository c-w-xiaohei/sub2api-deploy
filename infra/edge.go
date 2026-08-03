package main

import (
	"encoding/json"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type Edge struct {
	pulumi.ResourceState
	Provider  *cloudflare.Provider
	Reconcile *local.Command
	Spec      EdgeSpec
}

func DeployEdge(ctx *pulumi.Context, spec EdgeSpec, apiToken pulumi.StringInput, checksum string, preflight pulumi.Resource, legacyCode2 bool) (*Edge, error) {
	edge := &Edge{}
	if err := ctx.RegisterComponentResource("sub2api:host:Edge", "edge", edge); err != nil { return nil, err }
	provider, err := createCloudflareProvider(ctx, edge, preflight, apiToken, legacyCode2); if err != nil { return nil, err }
	ssl, err := createStrictSSLSetting(ctx, edge, preflight, provider, spec.CloudflareZoneID, legacyCode2); if err != nil { return nil, err }
	singBox, err := json.Marshal(spec.SingBox); if err != nil { return nil, err }
	reconcile, err := newCommand(ctx, "edge-reconcile", "bash scripts/reconcile-edge.sh", pulumi.StringMap{
		"EDGE_RUNTIME_ROOT": pulumi.String(EdgeRuntimeRoot), "EDGE_NETWORK_NAME": pulumi.String(EdgeNetworkName),
		"TRAEFIK_IMAGE": pulumi.String(spec.TraefikImage), "ACME_EMAIL": pulumi.String(spec.ACMEEmail),
		"SING_BOX_CONFIG": pulumi.String(string(singBox)), "CLOUDFLARE_API_TOKEN": apiToken,
	}, BuildEdgeTriggers(EdgeTriggerInput{TraefikImage: spec.TraefikImage, ACMEEmail: spec.ACMEEmail, SingBoxConfig: string(singBox), EdgeChecksum: checksum}), edge, ssl, preflight)
	if err != nil { return nil, err }
	edge.Provider, edge.Reconcile, edge.Spec = provider, reconcile, spec
	ctx.RegisterResourceOutputs(edge, pulumi.Map{})
	return edge, nil
}
