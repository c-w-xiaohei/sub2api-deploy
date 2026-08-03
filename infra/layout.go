package main

import (
	"fmt"
	"path/filepath"
)

const (
	EdgeNetworkName     = "sub2api-edge"
	EdgeRuntimeRoot     = "runtime/edge"
	EdgeDynamicRouteDir = "runtime/edge/dynamic"
)

// SiteLayout is the deterministic derived identity of one site. Every Compose
// name, runtime path, route path, and edge alias is derived from the Site ID
// and is not caller-configurable.
type SiteLayout struct {
	SiteID              string
	ComposeProject      string
	RuntimeRoot         string
	RuntimeEnvPath      string
	DeployStatePath     string
	BootstrapMarkerPath string
	BlueDataPath        string
	GreenDataPath       string
	RoutePath           string
	BlueAlias           string
	GreenAlias          string
	ResourcePrefix      string
}

// DeriveSiteLayout derives the complete local identity of one site from its
// validated Site ID and resolved resource prefix. The Site ID must match the
// site resource-name pattern before calling.
func DeriveSiteLayout(siteID, resourcePrefix string) SiteLayout {
	composeProject := "sub2api-" + siteID
	runtimeRoot := filepath.Join("runtime", "sites", siteID)
	return SiteLayout{
		SiteID:              siteID,
		ComposeProject:      composeProject,
		RuntimeRoot:         runtimeRoot,
		RuntimeEnvPath:      filepath.Join(runtimeRoot, "runtime.env"),
		DeployStatePath:     filepath.Join(runtimeRoot, "deploy-state.json"),
		BootstrapMarkerPath: filepath.Join(runtimeRoot, "bootstrap.marker"),
		BlueDataPath:        filepath.Join(runtimeRoot, "data", "blue"),
		GreenDataPath:       filepath.Join(runtimeRoot, "data", "green"),
		RoutePath:           filepath.Join(EdgeDynamicRouteDir, fmt.Sprintf("site-%s.yml", siteID)),
		BlueAlias:           composeProject + "-blue",
		GreenAlias:          composeProject + "-green",
		ResourcePrefix:      resourcePrefix,
	}
}
