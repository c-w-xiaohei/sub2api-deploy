package main

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() { if err := pulumi.RunErr(deploymentProgram); err != nil { panic(err) } }

type HostGraphExports struct { Sites pulumi.MapOutput; HostStateID pulumi.IDOutput }

type hostDesiredSite struct { ID string; Spec SiteSpec; Layout SiteLayout }
type hostDesiredState struct { Edge EdgeSpec; Sites []hostDesiredSite }
type expectedSiteMode struct { PostgresMode string `json:"postgresMode"`; RedisMode string `json:"redisMode"` }

func deploymentProgram(ctx *pulumi.Context) error {
	programConfig, err := loadProgramConfig(ctx); if err != nil { return err }
	allowPendingPreview := ctx.DryRun() && os.Getenv("ALLOW_PENDING_LEGACY_PREVIEW") == "1"
	layouts, adoptedCode2, err := ResolveHostLayouts("runtime/host-state.json", programConfig.Layouts, allowPendingPreview); if err != nil { return err }
	edgeChecksum, err := edgeChecksum(); if err != nil { return err }
	siteChecksum, err := siteChecksum(); if err != nil { return err }
	hostChecksum, err := hostChecksum(); if err != nil { return err }
	exports, err := deployHostGraph(ctx, programConfig.Host, layouts, programConfig.Secrets, edgeChecksum, siteChecksum, hostChecksum, adoptedCode2); if err != nil { return err }
	ctx.Export("sites", exports.Sites)
	ctx.Export("hostStateId", exports.HostStateID)
	return nil
}

func deployHostGraph(ctx *pulumi.Context, host HostSpec, layouts []SiteLayout, secrets SecretHostSpec, edgeChecksum, siteChecksum, hostChecksum string, adoptedCode2 ...bool) (HostGraphExports, error) {
	configuredSiteIDs := ""
	for index, layout := range layouts { if index > 0 { configuredSiteIDs += "," }; configuredSiteIDs += layout.SiteID }
	hostStatePath := "runtime/host-state.json"
	desiredState, err := hostDesiredStateDigest(host, layouts)
	if err != nil { return HostGraphExports{}, err }
	expectedModes, err := expectedSiteModesJSON(host, layouts)
	if err != nil { return HostGraphExports{}, err }
	preflight, err := newHostCommand(ctx, "host-preflight", "npx --no-install tsx scripts/host-preflight.ts check \"$CONFIGURED_SITE_IDS\" \"$HOST_STATE_PATH\" \"$ALLOW_PENDING_LEGACY_PREVIEW\" \"$EXPECTED_SITE_MODES\"", pulumi.StringMap{
		"CONFIGURED_SITE_IDS": pulumi.String(configuredSiteIDs), "HOST_STATE_PATH": pulumi.String(hostStatePath),
		"ALLOW_PENDING_LEGACY_PREVIEW": pulumi.String(fmt.Sprintf("%t", ctx.DryRun() && os.Getenv("ALLOW_PENDING_LEGACY_PREVIEW") == "1")),
		"EXPECTED_SITE_MODES": pulumi.String(expectedModes),
	}, []string{"host-preflight-v2", configuredSiteIDs, hostStatePath, expectedModes, desiredState, edgeChecksum, siteChecksum, hostChecksum})
	if err != nil { return HostGraphExports{}, err }
	legacyCode2 := len(adoptedCode2) == 1 && adoptedCode2[0]
	edge, err := DeployEdge(ctx, host.Edge, secrets.Edge, edgeChecksum, preflight, legacyCode2); if err != nil { return HostGraphExports{}, err }
	// Register Site modules in sorted layout order; each final command gates the next Site.
	var barrier pulumi.Resource
	outputs := pulumi.Map{}
	for _, layout := range layouts {
		siteID := layout.SiteID
		site, err := DeploySite(ctx, siteID, host.Sites[siteID], host.SiteSecrets[siteID], layout, edge, preflight, barrier, siteChecksum, configuredSiteIDs)
		if err != nil { return HostGraphExports{}, err }
		barrier = site.FinalBarrier
		outputs[siteID] = site.Status
	}
	statePaths := ""
	for index, layout := range layouts { if index > 0 { statePaths += "," }; statePaths += layout.DeployStatePath }
	finalize, err := newHostCommand(ctx, "host-finalize-state", "bash scripts/finalize-host-state.sh", pulumi.StringMap{
		"CONFIGURED_SITE_IDS": pulumi.String(configuredSiteIDs), "HOST_STATE_PATH": pulumi.String(hostStatePath), "SITE_DEPLOY_STATE_PATHS": pulumi.String(statePaths),
	}, []string{"host-finalize-state-v1", configuredSiteIDs, hostStatePath, statePaths, hostChecksum}, preflight, barrier)
	if err != nil { return HostGraphExports{}, err }
	return HostGraphExports{Sites: outputs.ToMapOutput(), HostStateID: finalize.ID()}, nil
}

func expectedSiteModesJSON(host HostSpec, layouts []SiteLayout) (string, error) {
	modes := make(map[string]expectedSiteMode, len(layouts))
	for _, layout := range layouts {
		site := host.Sites[layout.SiteID]
		modes[layout.SiteID] = expectedSiteMode{PostgresMode: site.Database.Mode, RedisMode: site.Redis.Mode}
	}
	encoded, err := json.Marshal(modes)
	if err != nil { return "", err }
	return string(encoded), nil
}

func hostDesiredStateDigest(host HostSpec, layouts []SiteLayout) (string, error) {
	sites := make([]hostDesiredSite, 0, len(layouts))
	for _, layout := range layouts { sites = append(sites, hostDesiredSite{ID: layout.SiteID, Spec: host.Sites[layout.SiteID], Layout: layout}) }
	encoded, err := json.Marshal(hostDesiredState{Edge: host.Edge, Sites: sites})
	if err != nil { return "", err }
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func edgeChecksum() (string, error) {
	return checksumFiles(edgeChecksumPaths)
}

func siteChecksum() (string, error) {
	return checksumFiles(siteChecksumPaths)
}

func hostChecksum() (string, error) {
	return checksumFiles(hostChecksumPaths)
}

var edgeChecksumPaths = []string{"compose/edge.yml", "scripts/edge-compose-common.sh", "scripts/reconcile-edge.sh", "scripts/render-edge-config.ts", "scripts/render-runtime-env.ts", "traefik/traefik.yml", "traefik/dynamic/sing-box.yml"}
var siteChecksumPaths = []string{"compose/site.yml", "compose/upstream.yml", "scripts/site-compose-common.sh", "scripts/read-runtime-env.cjs", "scripts/reconcile-site.sh", "scripts/bootstrap-site.sh", "scripts/application-release.sh", "scripts/switch-slot.sh", "scripts/rollback-slot.sh", "scripts/probe-origin.sh", "scripts/probe-origin-strict.sh", "scripts/render-site-route.ts", "scripts/render-runtime-env.ts", "scripts/deployment-mode.ts", "scripts/write-deploy-state.ts", "scripts/write-bootstrap-marker.ts", "src/deployment-preflight.ts", "traefik/dynamic/site.yml"}
var hostChecksumPaths = []string{"scripts/host-preflight.ts", "scripts/finalize-host-state.sh", "scripts/write-host-state.ts"}

func checksumFiles(candidates []string) (string, error) {
	files := make([]string, 0, len(candidates))
	for _, path := range candidates { if _, err := os.Stat(path); err == nil { files = append(files, path) } else if !os.IsNotExist(err) { return "", err } }
	sort.Strings(files); hash := sha256.New(); for _, path := range files { contents, err := os.ReadFile(path); if err != nil { return "", err }; _, _ = fmt.Fprintf(hash, "%s\x00%s\x00", path, contents) }; return hex.EncodeToString(hash.Sum(nil)), nil
}
