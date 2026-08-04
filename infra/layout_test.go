package main

import (
	"reflect"
	"testing"
)

func TestDeriveSiteLayoutCode2(t *testing.T) {
	want := SiteLayout{
		SiteID:              "code2",
		ComposeProject:      "sub2api-code2",
		RuntimeRoot:         "runtime/sites/code2",
		RuntimeEnvPath:      "runtime/sites/code2/runtime.env",
		AppEnvPath:          "runtime/sites/code2/app.env",
		DeployStatePath:     "runtime/sites/code2/deploy-state.json",
		BootstrapMarkerPath: "runtime/sites/code2/bootstrap.marker",
		BlueDataPath:        "runtime/sites/code2/data/blue",
		GreenDataPath:       "runtime/sites/code2/data/green",
		RoutePath:           "runtime/edge/dynamic/site-code2.yml",
		BlueAlias:           "sub2api-code2-blue",
		GreenAlias:          "sub2api-code2-green",
		ResourcePrefix:      "contextid-us",
	}
	got := DeriveSiteLayout("code2", "contextid-us")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeriveSiteLayout() = %+v, want %+v", got, want)
	}
	appEnvPath := reflect.ValueOf(got).FieldByName("AppEnvPath")
	if !appEnvPath.IsValid() || appEnvPath.String() != "runtime/sites/code2/app.env" {
		t.Fatalf("app env path = %v", appEnvPath)
	}
}

func TestLegacyCode2LayoutUsesSharedAppEnvPath(t *testing.T) {
	layout := DeriveSiteLayout("code2", "code2")
	layout = legacyCode2LayoutWithAppEnv(layout)
	appEnvPath := reflect.ValueOf(layout).FieldByName("AppEnvPath")
	if !appEnvPath.IsValid() || appEnvPath.String() != "runtime/app.env" {
		t.Fatalf("legacy app env path = %v", appEnvPath)
	}
}

func legacyCode2LayoutWithAppEnv(layout SiteLayout) SiteLayout {
	layout = legacyCode2Layout(layout)
	value := reflect.ValueOf(&layout).Elem().FieldByName("AppEnvPath")
	value.SetString("runtime/app.env")
	return layout
}

func TestDeriveSiteLayoutSitesAreIsolated(t *testing.T) {
	code2 := DeriveSiteLayout("code2", "code2")
	code3 := DeriveSiteLayout("code3", "code3")

	if code2.ComposeProject == code3.ComposeProject {
		t.Fatalf("compose projects must be unique, got %q", code2.ComposeProject)
	}
	if code2.RuntimeRoot == code3.RuntimeRoot {
		t.Fatalf("runtime roots must be unique, got %q", code2.RuntimeRoot)
	}
	if code2.RoutePath == code3.RoutePath {
		t.Fatalf("route paths must be unique, got %q", code2.RoutePath)
	}
	if code2.BlueAlias == code3.BlueAlias || code2.GreenAlias == code3.GreenAlias {
		t.Fatalf("edge aliases must be unique per site")
	}
	if code2.RoutePath != "runtime/edge/dynamic/site-code2.yml" || code3.RoutePath != "runtime/edge/dynamic/site-code3.yml" {
		t.Fatalf("unexpected route paths %q and %q", code2.RoutePath, code3.RoutePath)
	}
	if code2.BlueDataPath != "runtime/sites/code2/data/blue" || code3.GreenDataPath != "runtime/sites/code3/data/green" {
		t.Fatalf("unexpected data paths %q and %q", code2.BlueDataPath, code3.GreenDataPath)
	}
}
