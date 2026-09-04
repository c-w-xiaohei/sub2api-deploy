package hostprovider

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestProviderSchemaHasOnlyHostAndSecretRevisionKey(t *testing.T) {
	provider := New("1.0.0")
	schema, err := p.GetSchema(context.Background(), "sub2api-host", "1.0.0", provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.Resources) != 1 || !schema.Resources["sub2api-host:index:Host"].InputProperties["secrets"].Secret {
		t.Fatalf("unexpected schema: %#v", schema.Resources)
	}
	if len(schema.Functions) != 0 {
		t.Fatalf("unexpected provider surface: %#v", schema)
	}
	if !schema.Config.Variables["revisionKey"].Secret {
		t.Fatal("revisionKey is not secret")
	}
	if !contains(schema.Config.Required, "revisionKey") {
		t.Fatalf("revisionKey is not required: %#v", schema.Config)
	}
	resource := schema.Resources[hostToken]
	if resource.InputProperties["resource"].Ref != "#/types/sub2api-host:index:ResourceIdentity" || resource.InputProperties["server"].Ref != "#/types/sub2api-host:index:ServerTarget" || resource.InputProperties["target"].Ref != "#/types/sub2api-host:index:Target" {
		t.Fatalf("Host input references are not typed: %#v", resource.InputProperties)
	}
	if target := schema.Types["sub2api-host:index:Target"]; target.Properties["apps"].Items.Ref != "#/types/sub2api-host:index:AppTarget" || target.Properties["reverseProxy"].Ref != "#/types/sub2api-host:index:ReverseProxyTarget" {
		t.Fatalf("Target shape = %#v", target)
	}
	if app := schema.Types["sub2api-host:index:AppTarget"]; app.Properties["initialBootstrap"].Type != "boolean" || app.Properties["initialAdminEmail"].Type != "string" || !contains(app.Required, "initialAdminEmail") || app.Properties["dataLinks"].Items.Ref != "#/types/sub2api-host:index:DataLink" {
		t.Fatalf("App shape = %#v", app)
	}
	if data := schema.Types["sub2api-host:index:DataIdentity"]; data.Properties["port"].Type != "integer" || data.Properties["endpoint"].Type != "string" {
		t.Fatalf("Data identity shape = %#v", data)
	}
	if data := schema.Types["sub2api-host:index:DataIdentity"]; data.Properties["tlsMode"].Type != "string" {
		t.Fatalf("Data identity TLS shape = %#v", data)
	}
	if contains(schema.Types["sub2api-host:index:DataIdentity"].Required, "tlsMode") {
		t.Fatal("tlsMode must remain optional for legacy input compatibility")
	}
	if local := schema.Types["sub2api-host:index:LocalDataServiceTarget"]; local.Properties["bindings"].Items.Ref != "#/types/sub2api-host:index:LocalDataBinding" || local.Properties["clients"].Items.Ref != "#/types/sub2api-host:index:LocalDataClient" {
		t.Fatalf("local data service shape = %#v", local)
	}
	if binding := schema.Types["sub2api-host:index:LocalDataBinding"]; binding.Properties["allowedSources"].Items.Type != "string" {
		t.Fatalf("binding shape = %#v", binding)
	}
	if client := schema.Types["sub2api-host:index:LocalDataClient"]; client.Properties["appId"].Type != "string" || client.Properties["username"].Type != "string" {
		t.Fatalf("client shape = %#v", client)
	}
	if localSecret := schema.Types["sub2api-host:index:LocalDataServiceSecrets"]; localSecret.Properties["clientPasswords"].AdditionalProperties.Type != "string" {
		t.Fatalf("local data secret shape = %#v", localSecret)
	}
	for _, forbidden := range []string{"path", "compose", "slot", "phase", "approval"} {
		if _, ok := schema.Resources["sub2api-host:index:Host"].InputProperties[forbidden]; ok {
			t.Fatalf("forbidden property %q", forbidden)
		}
	}
}

func TestFrameworkServerConfigAndVersionContract(t *testing.T) {
	server, err := p.RawServer("sub2api-host", "9.8.7", New("9.8.7"))(nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := server.GetSchema(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if schema.GetSchema() == "" {
		t.Fatal("schema is empty")
	}
	parsed, err := p.GetSchema(t.Context(), "sub2api-host", "9.8.7", New("9.8.7"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Version != "9.8.7" || !parsed.Config.Variables["revisionKey"].Secret || !contains(parsed.Config.Required, "revisionKey") || schema.GetSchema() == "" {
		t.Fatalf("schema contract = %#v", parsed.Config)
	}

	provider := New("9.8.7")
	response, err := provider.CheckConfig(t.Context(), p.CheckRequest{Inputs: property.NewMap(nil)})
	if err != nil || len(response.Failures) != 1 {
		t.Fatalf("absent revision key = %#v, %v", response, err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	response, err = provider.CheckConfig(t.Context(), p.CheckRequest{Inputs: property.NewMap(map[string]property.Value{"revisionKey": property.New(key)})})
	if err != nil || len(response.Failures) != 0 {
		t.Fatalf("valid revision key = %#v, %v", response, err)
	}
	checked, _ := response.Inputs.GetOk("revisionKey")
	if !checked.Secret() {
		t.Fatalf("revision key was not tainted secret: %#v", checked)
	}
	if err := provider.Configure(t.Context(), p.ConfigureRequest{Args: response.Inputs}); err != nil {
		t.Fatalf("Configure = %v", err)
	}
}

func TestConfigureKeepsOnlyDerivedKeyID(t *testing.T) {
	h := newHost("1.0.0")
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if err := h.configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New(key).WithSecret(true)})}); err != nil {
		t.Fatal(err)
	}
	id, available := h.configuredKey()
	if !available || len(id) != 16 || id == key {
		t.Fatalf("configured key status = %q, %t", id, available)
	}
	if err := h.configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New(property.Computed).WithSecret(true)})}); err == nil {
		t.Fatal("unknown key accepted")
	}
	if err := h.configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New(key)})}); err == nil {
		t.Fatal("unsecret key accepted")
	}
	if err := h.configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New("bad").WithSecret(true)})}); err == nil {
		t.Fatal("malformed key accepted")
	}
}

func TestRawServerWiresLifecycleAndPropertyWrappers(t *testing.T) {
	provider := New("1.0.0")
	server, err := p.RawServer("sub2api-host", "1.0.0", provider)(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	config, err := server.CheckConfig(t.Context(), &pulumirpc.CheckRequest{News: rpcProperties(t, property.NewMap(map[string]property.Value{"revisionKey": property.New(key)}))})
	if err != nil || len(config.Failures) != 0 {
		t.Fatalf("raw CheckConfig = %#v, %v", config, err)
	}
	if _, err := server.Configure(t.Context(), &pulumirpc.ConfigureRequest{Args: config.Inputs}); err != nil {
		t.Fatalf("raw Configure = %v", err)
	}
	inputs := rpcProperties(t, hostInputs(property.New("edge")))
	check, err := server.Check(t.Context(), &pulumirpc.CheckRequest{News: inputs})
	if err != nil || len(check.Failures) != 0 {
		t.Fatalf("raw Check = %#v, %v", check, err)
	}
	if diff, err := server.Diff(t.Context(), &pulumirpc.DiffRequest{Olds: inputs, OldInputs: inputs, News: inputs}); err != nil || diff.Changes != pulumirpc.DiffResponse_DIFF_NONE {
		t.Fatalf("raw Diff = %#v, %v", diff, err)
	}
	preview, err := server.Create(t.Context(), &pulumirpc.CreateRequest{Properties: inputs, Preview: true})
	if err != nil || preview.Id != "" {
		t.Fatalf("raw preview Create = %#v, %v", preview, err)
	}
	if _, err := server.Create(t.Context(), &pulumirpc.CreateRequest{Properties: inputs}); err == nil {
		t.Fatal("raw non-preview Create accepted")
	}
	if _, err := server.Read(t.Context(), &pulumirpc.ReadRequest{Id: "host-imported-without-context"}); err == nil {
		t.Fatal("raw Import Read accepted")
	}
	if _, err := server.Update(t.Context(), &pulumirpc.UpdateRequest{}); err == nil {
		t.Fatal("raw Update accepted")
	}
	if _, err := server.Delete(t.Context(), &pulumirpc.DeleteRequest{}); err == nil {
		t.Fatal("raw Delete accepted")
	}
}

func TestFrameworkServerDiffUsesOldInputs(t *testing.T) {
	old := hostInputs(property.New("edge"))
	state := old.Set("resource", object("environment", property.New("prod"), "serverKey", property.New("state-only")))
	diff, err := New("1.0.0").Diff(t.Context(), p.DiffRequest{OldInputs: old, State: state, Inputs: old})
	if err != nil {
		t.Fatalf("Diff must use OldInputs, not state: %v", err)
	}
	if diff.HasChanges {
		t.Fatalf("equal inputs diffed: %#v", diff)
	}
}

func TestDiffReportsRefreshedDriftAndAppliedRevisionMismatch(t *testing.T) {
	inputs := hostInputs(property.New("edge"))
	state := inputs.Set("machine", object("value", property.New("machine-a"))).Set("ownership", object("value", property.New("owner-a"))).Set("appliedRevision", property.New("tr1:0000000000000000:0000000000000000000000000000000000000000000000000000000000000000"))
	observation := object("machine", object("value", property.New("machine-a")), "ownership", object("value", property.New("owner-a")), "hostRelease", property.New("release"), "appliedRevision", property.New("tr1:0000000000000000:1111111111111111111111111111111111111111111111111111111111111111"), "drifted", property.New(true), "ready", property.New(false))
	state = state.Set("observation", observation)
	diff, err := New("1.0.0").Diff(t.Context(), p.DiffRequest{OldInputs: inputs, State: state, Inputs: inputs})
	if err != nil || !diff.HasChanges {
		t.Fatalf("refreshed drift/applied-revision mismatch was hidden: %#v, %v", diff, err)
	}
}

// TestReadRejectsOpaqueHostImportToken is baseline-compilable RED coverage.
func TestReadRejectsOpaqueHostImportTokenBeforeTransport(t *testing.T) {
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	got, err := h.read(t.Context(), p.ReadRequest{ID: "hit1:opaque"})
	if err == nil || got.ID != "" || got.Properties.Len() != 0 || len(r.calls) != 0 || !strings.Contains(err.Error(), "import") {
		t.Fatalf("empty import-style Read did not fail explicitly before transport: %#v, %v", got, err)
	}
}

func TestCheckRejectsKnownSiblingAlongsideUnknownAndTaintsSecrets(t *testing.T) {
	provider := New("1.0.0")
	inputs := hostInputs(property.New(property.Computed))
	inputs = inputs.Set("resource", object("environment", property.New(""), "serverKey", property.New("edge")))
	inputs = inputs.Set("unexpected", property.New("no"))
	inputs = inputs.Set("secrets", property.New(property.NewMap(nil)))
	response, err := provider.Check(t.Context(), p.CheckRequest{Inputs: inputs})
	if err != nil || len(response.Failures) < 2 {
		t.Fatalf("known invalid siblings were skipped: %#v, %v", response, err)
	}
	secrets, _ := response.Inputs.GetOk("secrets")
	if !secrets.Secret() {
		t.Fatalf("unsecret secrets input was not tainted: %#v", secrets)
	}
}

func TestCheckValidatesKnownSiblingsOfPartialComputedObjects(t *testing.T) {
	provider := New("1.0.0")
	inputs := hostInputs(property.New("edge"))
	inputs = inputs.Set("target", object(
		"releaseArtifact", property.New(""),
		"apps", property.New(property.NewArray([]property.Value{property.New(property.Computed)})),
		"dataServices", property.New(property.NewArray([]property.Value{object("id", property.New(""), "type", property.New("mysql"), "port", property.New(0.0))})),
		"connectors", property.New(property.NewArray([]property.Value{object("id", property.New(""), "tunnelId", property.New(""), "appIds", property.New(property.NewArray([]property.Value{})))})),
		"phase", property.New("forbidden"),
	))
	inputs = inputs.Set("resource", object("environment", property.New(""), "serverKey", property.New(property.Computed)))
	inputs = inputs.Set("server", object("sshAlias", property.New(""), "future", property.New(property.Computed)))
	response, err := provider.Check(t.Context(), p.CheckRequest{Inputs: inputs})
	if err != nil {
		t.Fatalf("partial computed objects skipped known violations: %#v, %v", response, err)
	}
	for _, property := range []string{"target.releaseArtifact", "target.dataServices[0].id", "target.dataServices[0].type", "target.dataServices[0].port", "target.connectors[0].id", "target.connectors[0].tunnelId", "target.phase", "resource.environment", "server.sshAlias", "server.future"} {
		if !hasFailure(response.Failures, property) {
			t.Fatalf("missing failure for %q: %#v", property, response.Failures)
		}
	}
}

func TestCheckConfigComputedRevisionKeyIsSecretUnknown(t *testing.T) {
	provider := New("1.0.0")
	for _, input := range []property.Value{property.New(property.Computed), property.New(property.Computed).WithSecret(true)} {
		response, err := provider.CheckConfig(t.Context(), p.CheckRequest{Inputs: property.NewMap(map[string]property.Value{"revisionKey": input})})
		if err != nil || len(response.Failures) != 0 {
			t.Fatalf("computed revision key = %#v, %v", response, err)
		}
		key, _ := response.Inputs.GetOk("revisionKey")
		if !key.Secret() || !key.HasComputed() {
			t.Fatalf("computed revision key lost secret-unknown class: %#v", key)
		}
	}
}

func TestCheckRejectsNestedUnknownNullAndAbsentInputs(t *testing.T) {
	provider := New("1.0.0")
	invalid := hostInputs(property.New("edge"))
	invalid = invalid.Set("target", object("releaseArtifact", property.New("release"), "phase", property.New("forbidden")))
	response, err := provider.Check(t.Context(), p.CheckRequest{Inputs: invalid})
	if err != nil || len(response.Failures) == 0 {
		t.Fatalf("nested unknown accepted: %#v, %v", response, err)
	}
	null := hostInputs(property.New("edge")).Set("server", property.New(property.Null))
	response, err = provider.Check(t.Context(), p.CheckRequest{Inputs: null})
	if err != nil || len(response.Failures) == 0 {
		t.Fatalf("null server accepted: %#v, %v", response, err)
	}
	response, err = provider.Check(t.Context(), p.CheckRequest{Inputs: hostInputs(property.New("edge")).Delete("target")})
	if err != nil || len(response.Failures) == 0 {
		t.Fatalf("absent target accepted: %#v, %v", response, err)
	}
}

func TestDiffIsConservativeForUnknownAndPreviewHasNoFakeID(t *testing.T) {
	provider := New("1.0.0")
	old := hostInputs(property.New("edge"))
	unknown := hostInputs(property.New("edge"))
	unknown = unknown.Set("target", property.New(property.Computed))
	diff, err := provider.Diff(t.Context(), p.DiffRequest{OldInputs: old, State: old, Inputs: unknown})
	if err != nil || !diff.HasChanges {
		t.Fatalf("unknown diff claimed no change: %#v, %v", diff, err)
	}
	changedKey := unknown.Set("resource", object("environment", property.New("prod"), "serverKey", property.New("other")))
	if _, err := provider.Diff(t.Context(), p.DiffRequest{OldInputs: old, Inputs: changedKey}); err == nil {
		t.Fatal("known serverKey change was hidden by unknown sibling")
	}
	preview, err := provider.Create(t.Context(), p.CreateRequest{DryRun: true, Properties: old})
	if err != nil || preview.ID != "" {
		t.Fatalf("preview identity = %#v, %v", preview, err)
	}
	for _, name := range []string{"machine", "ownership", "appliedRevision", "observation"} {
		v, _ := preview.Properties.GetOk(name)
		if !v.HasComputed() {
			t.Fatalf("preview output %q is not computed: %#v", name, v)
		}
	}
	secrets, _ := preview.Properties.GetOk("secrets")
	if !secrets.Secret() {
		t.Fatalf("preview lost secret taint: %#v", secrets)
	}
}

func TestConfigAndHostCheckPreservePropertyClassesWithoutEffects(t *testing.T) {
	provider := New("1.0.0")
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := provider.CheckConfig(context.Background(), p.CheckRequest{Inputs: property.NewMap(map[string]property.Value{"revisionKey": property.New(key).WithSecret(true)})}); err != nil {
		t.Fatal(err)
	}
	if response, err := provider.CheckConfig(context.Background(), p.CheckRequest{Inputs: property.NewMap(map[string]property.Value{"revisionKey": property.New("bad").WithSecret(true)})}); err != nil || len(response.Failures) == 0 {
		t.Fatalf("invalid revision key accepted: %#v %v", response, err)
	}
	for _, value := range []property.Value{property.New("edge"), property.New(property.Computed), property.New("edge").WithSecret(true), property.New(property.Computed).WithSecret(true)} {
		response, err := provider.Check(context.Background(), p.CheckRequest{Inputs: hostInputs(value)})
		if err != nil || len(response.Failures) != 0 {
			t.Fatalf("Check: %v %#v", err, response.Failures)
		}
		server, _ := response.Inputs.GetOk("server")
		got, _ := server.AsMap().GetOk("sshAlias")
		if got.Secret() != value.Secret() || got.HasComputed() != value.HasComputed() {
			t.Fatalf("property class lost: got %#v want %#v", got, value)
		}
	}
	response, err := provider.Check(context.Background(), p.CheckRequest{Inputs: hostInputs(property.New("edge")).Set("secrets", property.New(property.Computed))})
	if err != nil || len(response.Failures) != 0 {
		t.Fatalf("secret unknown Check: %v %#v", err, response.Failures)
	}
	secrets, _ := response.Inputs.GetOk("secrets")
	if !secrets.Secret() || !secrets.HasComputed() {
		t.Fatalf("secret unknown class lost: %#v", secrets)
	}
}

func TestDiffAndCreateFailClosedWithoutEffects(t *testing.T) {
	provider := New("1.0.0")
	old := hostInputs(property.New("edge"))
	alias := hostInputs(property.New("edge"))
	alias = alias.Set("server", object("sshAlias", property.New("edge-new")))
	diff, err := provider.Diff(context.Background(), p.DiffRequest{State: old, Inputs: alias, OldInputs: old})
	if err != nil || !diff.HasChanges || diff.DetailedDiff["server.sshAlias"].Kind != p.Update {
		t.Fatalf("alias diff = %#v, %v", diff, err)
	}
	for _, key := range []string{"target", "secrets"} {
		changed := old
		if key == "target" {
			changed = changed.Set(key, object("releaseArtifact", property.New("release-next")))
		} else {
			changed = changed.Set(key, property.New(property.NewMap(map[string]property.Value{"apps": property.New(property.NewMap(nil))})).WithSecret(true))
		}
		diff, err := provider.Diff(context.Background(), p.DiffRequest{State: old, Inputs: changed, OldInputs: old})
		if err != nil || !diff.HasChanges || diff.DetailedDiff[key].Kind != p.Update {
			t.Fatalf("%s diff = %#v, %v", key, diff, err)
		}
	}
	changedKey := hostInputs(property.New("edge"))
	changedKey = changedKey.Set("resource", object("environment", property.New("prod"), "serverKey", property.New("other")))
	if _, err := provider.Diff(context.Background(), p.DiffRequest{State: old, Inputs: changedKey, OldInputs: old}); err == nil {
		t.Fatal("server key change accepted")
	}
	if _, err := provider.Create(context.Background(), p.CreateRequest{Properties: old}); err == nil {
		t.Fatal("non-preview Create accepted")
	}
}

func TestCheckAcceptsCompleteCrossHostDataContract(t *testing.T) {
	inputs := completeCrossHostInputs(true, "disable")
	response, err := New("1.0.0").Check(t.Context(), p.CheckRequest{Inputs: inputs})
	if err != nil || len(response.Failures) != 0 {
		t.Fatalf("complete cross-host Check = %#v, %v", response, err)
	}
	secrets, _ := response.Inputs.GetOk("secrets")
	if !secrets.Secret() {
		t.Fatalf("Check lost secret class: %#v", secrets)
	}
}

func TestCheckAcceptsLegacyOmittedTLSModeAndRejectsInvalidExplicitMode(t *testing.T) {
	if response, err := New("1.0.0").Check(t.Context(), p.CheckRequest{Inputs: completeCrossHostInputs(false, "")}); err != nil || len(response.Failures) != 0 {
		t.Fatalf("legacy omitted tlsMode = %#v, %v", response, err)
	}
	if response, err := New("1.0.0").Check(t.Context(), p.CheckRequest{Inputs: completeCrossHostInputs(true, "invalid")}); err != nil || len(response.Failures) == 0 {
		t.Fatalf("invalid tlsMode accepted = %#v, %v", response, err)
	}
}

func TestCheckRejectsUnsafePostgresClientContract(t *testing.T) {
	response, err := New("1.0.0").Check(t.Context(), p.CheckRequest{Inputs: completeCrossHostInputsForUsername(true, "disable", "s2h_client")})
	if err != nil || len(response.Failures) == 0 {
		t.Fatalf("Provider Check accepted controller-reserved PostgreSQL role: %#v, %v", response, err)
	}
}

func TestCheckRequiresSafeExplicitInitialAdminEmail(t *testing.T) {
	for name, email := range map[string]property.Value{
		"absent":  property.New("unused"),
		"null":    property.New(property.Null),
		"empty":   property.New(""),
		"control": property.New("admin@example.test\x00"),
		"newline": property.New("admin@example.test\nINJECTED=yes"),
		"valid":   property.New("admin@example.test"),
	} {
		t.Run(name, func(t *testing.T) {
			inputs := completeCrossHostInputs(true, "disable")
			if name == "absent" {
				target, _ := inputs.GetOk("target")
				apps, _ := target.AsMap().GetOk("apps")
				app := apps.AsArray().AsSlice()[0].AsMap().Delete("initialAdminEmail")
				dataServices, _ := target.AsMap().GetOk("dataServices")
				inputs = inputs.Set("target", object("releaseArtifact", property.New("release"), "apps", property.New(property.NewArray([]property.Value{property.New(app)})), "dataServices", dataServices))
			} else {
				target, _ := inputs.GetOk("target")
				apps, _ := target.AsMap().GetOk("apps")
				app := apps.AsArray().AsSlice()[0].AsMap().Set("initialAdminEmail", email)
				dataServices, _ := target.AsMap().GetOk("dataServices")
				inputs = inputs.Set("target", object("releaseArtifact", property.New("release"), "apps", property.New(property.NewArray([]property.Value{property.New(app)})), "dataServices", dataServices))
			}
			response, err := New("1.0.0").Check(t.Context(), p.CheckRequest{Inputs: inputs})
			if err != nil || (name == "valid") != (len(response.Failures) == 0) {
				t.Fatalf("initialAdminEmail %s = %#v, %v", name, response, err)
			}
		})
	}
}

func TestCheckRejectsRuntimeAdminEmailOverrides(t *testing.T) {
	for name, mutate := range map[string]func(property.Map) property.Map{
		"settings": func(inputs property.Map) property.Map {
			target, _ := inputs.GetOk("target")
			apps, _ := target.AsMap().GetOk("apps")
			app := apps.AsArray().AsSlice()[0].AsMap()
			app = app.Set("runtimeSettings", property.New(property.NewMap(map[string]property.Value{"ADMIN_EMAIL": property.New("attacker@example.test")})))
			dataServices, _ := target.AsMap().GetOk("dataServices")
			return inputs.Set("target", object("releaseArtifact", property.New("release"), "apps", property.New(property.NewArray([]property.Value{property.New(app)})), "dataServices", dataServices))
		},
		"secrets": func(inputs property.Map) property.Map {
			secrets, _ := inputs.GetOk("secrets")
			apps := property.NewMap(nil)
			if appsValue, ok := secrets.AsMap().GetOk("apps"); ok {
				apps = appsValue.AsMap()
			}
			app := property.NewMap(nil)
			if appValue, ok := apps.GetOk("app"); ok {
				app = appValue.AsMap()
			}
			app = app.Set("runtimeEnvironment", property.New(property.NewMap(map[string]property.Value{"ADMIN_EMAIL": property.New("attacker@example.test")})))
			localDataServices, _ := secrets.AsMap().GetOk("localDataServices")
			return inputs.Set("secrets", property.New(property.NewMap(map[string]property.Value{"apps": property.New(apps.Set("app", property.New(app))), "localDataServices": localDataServices})).WithSecret(true))
		},
	} {
		t.Run(name, func(t *testing.T) {
			response, err := New("1.0.0").Check(t.Context(), p.CheckRequest{Inputs: mutate(completeCrossHostInputs(true, "disable"))})
			if err != nil || len(response.Failures) == 0 {
				t.Fatalf("ADMIN_EMAIL override accepted = %#v, %v", response, err)
			}
		})
	}
}

func completeCrossHostInputs(includeTLS bool, tlsMode string) property.Map {
	return completeCrossHostInputsForUsername(includeTLS, tlsMode, "app")
}

func completeCrossHostInputsForUsername(includeTLS bool, tlsMode, username string) property.Map {
	identityFields := []any{"kind", property.New("postgres"), "providerId", property.New("docker:data:db"), "endpoint", property.New("10.0.0.1"), "port", property.New(5432.0), "database", property.New("app")}
	if includeTLS {
		identityFields = append(identityFields, "tlsMode", property.New(tlsMode))
	}
	identity := object(identityFields...)
	link := object("name", property.New("db"), "identity", identity)
	app := object("id", property.New("app"), "image", property.New("image"), "hostname", property.New("app.example"), "readinessPath", property.New("/ready"), "initialAdminEmail", property.New("admin@example.test"), "dataLinks", property.New(property.NewArray([]property.Value{link})))
	binding := object("address", property.New("10.0.0.1"), "allowedSources", property.New(property.NewArray([]property.Value{property.New("10.0.0.2")})))
	client := object("appId", property.New("app"), "username", property.New(username), "database", property.New("app"))
	service := object("id", property.New("db"), "type", property.New("postgres"), "port", property.New(5432.0), "bindings", property.New(property.NewArray([]property.Value{binding})), "clients", property.New(property.NewArray([]property.Value{client})))
	serviceSecrets := object("adminPassword", property.New("admin"), "clientPasswords", property.New(property.NewMap(map[string]property.Value{"app": property.New("password")})))
	return property.NewMap(map[string]property.Value{"resource": object("environment", property.New("prod"), "serverKey", property.New("data")), "server": object("sshAlias", property.New("data-ssh")), "target": object("releaseArtifact", property.New("release"), "apps", property.New(property.NewArray([]property.Value{app})), "dataServices", property.New(property.NewArray([]property.Value{service}))), "secrets": property.New(property.NewMap(map[string]property.Value{"localDataServices": property.New(property.NewMap(map[string]property.Value{"db": serviceSecrets}))})).WithSecret(true)})
}

func hostInputs(server property.Value) property.Map {
	return property.NewMap(map[string]property.Value{"resource": object("environment", property.New("prod"), "serverKey", property.New("edge")), "server": object("sshAlias", server), "target": object("releaseArtifact", property.New("release")), "secrets": property.New(property.NewMap(nil)).WithSecret(true)})
}

func object(entries ...any) property.Value {
	m := map[string]property.Value{}
	for i := 0; i < len(entries); i += 2 {
		m[entries[i].(string)] = entries[i+1].(property.Value)
	}
	return property.New(property.NewMap(m))
}

func rpcProperties(t *testing.T, values property.Map) *structpb.Struct {
	t.Helper()
	properties := resource.PropertyMap{}
	values.All(func(key string, value property.Value) bool {
		properties[resource.PropertyKey(key)] = resource.ToResourcePropertyValue(value)
		return true
	})
	encoded, err := plugin.MarshalProperties(properties, plugin.MarshalOptions{KeepUnknowns: true, KeepSecrets: true, KeepResources: true})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasFailure(failures []p.CheckFailure, property string) bool {
	for _, failure := range failures {
		if failure.Property == property {
			return true
		}
	}
	return false
}
