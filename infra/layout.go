package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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
	AppEnvPath          string
	DeployStatePath     string
	BootstrapMarkerPath string
	BlueDataPath        string
	GreenDataPath       string
	RoutePath           string
	BlueAlias           string
	GreenAlias          string
	ResourcePrefix      string
}

// LegacySiteLayout is an internal, persisted compatibility fact. It is never
// derived from public configuration: only a completed, strictly validated host
// state can select it for the original code2 Site.
type LegacySiteLayout struct {
	RuntimeRoot      string `json:"runtimeRoot"`
	ComposeProject   string `json:"composeProject"`
	RouteLayout      string `json:"routeLayout"`
	HandoverComplete bool   `json:"handoverComplete"`
}

type persistedHostLayout struct {
	Version     int               `json:"version"`
	Sites       []string          `json:"sites"`
	LegacyCode2 *LegacySiteLayout `json:"legacyCode2"`
}

// allowPendingPreview is computed from ctx.DryRun and an explicit operator opt-in.
// It must never be derived from an environment variable alone.
func ResolveHostLayouts(hostStatePath string, layouts []SiteLayout, allowPendingPreview bool) ([]SiteLayout, bool, error) {
	contents, err := os.ReadFile(hostStatePath)
	if os.IsNotExist(err) {
		return layouts, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var state persistedHostLayout
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil || decoder.More() {
		return nil, false, fmt.Errorf("host state is malformed; inspect and repair it before proceeding")
	}
	if state.Version != 1 || state.LegacyCode2 == nil {
		return layouts, false, nil
	}
	legacy := state.LegacyCode2
	if legacy.RuntimeRoot != "runtime" || legacy.ComposeProject != "sub2api" || legacy.RouteLayout != "flat" {
		return nil, false, fmt.Errorf("host state legacy mapping is invalid")
	}
	foundCode2 := false
	for _, id := range state.Sites {
		if id == "code2" {
			foundCode2 = true
		}
	}
	if !foundCode2 {
		return nil, false, fmt.Errorf("host state legacy mapping requires recorded code2")
	}
	if !legacy.HandoverComplete {
		if len(layouts) != 1 || layouts[0].SiteID != "code2" {
			return nil, false, fmt.Errorf("pending legacy code2 adoption requires exactly one configured Site: code2")
		}
		if !allowPendingPreview {
			return nil, false, fmt.Errorf("legacy code2 handover is pending; ordinary Pulumi operations are blocked until the approved handover completes")
		}
	}
	resolved := append([]SiteLayout(nil), layouts...)
	for index := range resolved {
		if resolved[index].SiteID != "code2" {
			continue
		}
		prefix := resolved[index].ResourcePrefix
		resolved[index] = SiteLayout{SiteID: "code2", ComposeProject: "sub2api", RuntimeRoot: "runtime", RuntimeEnvPath: "runtime/runtime.env", AppEnvPath: "runtime/app.env", DeployStatePath: "runtime/deploy-state.json", BootstrapMarkerPath: "runtime/bootstrap.marker", BlueDataPath: "runtime/data/blue", GreenDataPath: "runtime/data/green", RoutePath: filepath.Join(EdgeDynamicRouteDir, "site-code2.yml"), BlueAlias: "sub2api-blue", GreenAlias: "sub2api-green", ResourcePrefix: prefix}
	}
	return resolved, legacy.HandoverComplete || allowPendingPreview, nil
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
		AppEnvPath:          filepath.Join(runtimeRoot, "app.env"),
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
