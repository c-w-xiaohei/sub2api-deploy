package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() { if err := pulumi.RunErr(deploymentProgram); err != nil { panic(err) } }

type HostGraphExports struct { Sites pulumi.MapOutput }

func deploymentProgram(ctx *pulumi.Context) error {
	programConfig, err := loadProgramConfig(ctx); if err != nil { return err }
	edgeChecksum, err := edgeChecksum(); if err != nil { return err }
	siteChecksum, err := siteChecksum(); if err != nil { return err }
	exports, err := deployHostGraph(ctx, programConfig.Host, programConfig.Layouts, programConfig.Secrets, edgeChecksum, siteChecksum); if err != nil { return err }
	ctx.Export("sites", exports.Sites)
	return nil
}

func deployHostGraph(ctx *pulumi.Context, host HostSpec, layouts []SiteLayout, secrets SecretHostSpec, edgeChecksum, siteChecksum string) (HostGraphExports, error) {
	edge, err := DeployEdge(ctx, host.Edge, secrets.Edge, edgeChecksum); if err != nil { return HostGraphExports{}, err }
	// Register Site modules in sorted layout order; each final command gates the next Site.
	var barrier pulumi.Resource
	outputs := pulumi.Map{}
	for _, layout := range layouts {
		siteID := layout.SiteID
		site, err := DeploySite(ctx, siteID, host.Sites[siteID], host.SiteSecrets[siteID], layout, edge, barrier, siteChecksum)
		if err != nil { return HostGraphExports{}, err }
		barrier = site.FinalBarrier
		outputs[siteID] = site.Status
	}
	return HostGraphExports{Sites: outputs.ToMapOutput()}, nil
}

func edgeChecksum() (string, error) {
	return checksumFiles(edgeChecksumPaths)
}

func siteChecksum() (string, error) {
	return checksumFiles(siteChecksumPaths)
}

var edgeChecksumPaths = []string{"compose/edge.yml", "scripts/edge-compose-common.sh", "scripts/render-edge-config.ts", "traefik/traefik.yml", "traefik/dynamic/sing-box.yml"}
var siteChecksumPaths = []string{"compose/site.yml", "compose/upstream.yml", "scripts/site-compose-common.sh", "scripts/render-site-route.ts", "scripts/render-runtime-env.ts", "traefik/dynamic/site.yml"}

func checksumFiles(candidates []string) (string, error) {
	files := make([]string, 0, len(candidates))
	for _, path := range candidates { if _, err := os.Stat(path); err == nil { files = append(files, path) } else if !os.IsNotExist(err) { return "", err } }
	sort.Strings(files); hash := sha256.New(); for _, path := range files { contents, err := os.ReadFile(path); if err != nil { return "", err }; _, _ = fmt.Fprintf(hash, "%s\x00%s\x00", path, contents) }; return hex.EncodeToString(hash.Sum(nil)), nil
}
