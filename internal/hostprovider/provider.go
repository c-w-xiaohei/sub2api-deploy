// Package hostprovider exposes the single Host resource. Runtime work is deliberately
// unavailable until the Host lifecycle implementation is installed in Task 7.
package hostprovider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/artifact"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/openssh"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

const hostToken = "sub2api-host:index:Host"

var providerExecutable = os.Executable

// New creates the Host provider using only artifacts shipped beside its executable.
func New(version string) p.Provider {
	return newProvider(newHost(version))
}

// NewWithApproval creates a provider whose dangerous lifecycle operations may use
// the supplied process-scoped approval channel.
func NewWithApproval(version string, approve func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error)) p.Provider {
	path, err := providerExecutable()
	if err != nil {
		return newProvider(newHostWithDependencies(version, lifecycleDependencies{
			transport: openssh.New(),
			artifact: func() (artifactBundle, error) {
				return artifactBundle{}, errArtifactUnavailable
			},
			approve: approve,
		}))
	}
	return newProvider(newHostAtExecutableWithApproval(version, path, approve))
}

func newProvider(h *host) p.Provider {
	return p.Provider{GetSchema: h.schema, CheckConfig: h.checkConfig, Configure: h.configure, Check: h.check, Diff: h.diff, Create: h.create, Read: h.read, Update: h.update, Delete: h.delete}
}

func newHost(version string) *host {
	path, err := providerExecutable()
	if err != nil {
		return newHostWithDependencies(version, lifecycleDependencies{transport: openssh.New(), artifact: func() (artifactBundle, error) { return artifactBundle{}, errArtifactUnavailable }})
	}
	return newHostAtExecutable(version, path)
}

func newHostAtExecutable(version, path string) *host {
	return newHostAtExecutableWithApproval(version, path, nil)
}

func newHostAtExecutableWithApproval(version, path string, approve func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error)) *host {
	return newHostWithDependencies(version, lifecycleDependencies{
		transport: openssh.New(),
		artifact: func() (artifactBundle, error) {
			return loadReleaseBundle(path)
		},
		approve: approve,
	})
}

func loadReleaseBundle(providerPath string) (artifactBundle, error) {
	root := filepath.Join(filepath.Dir(filepath.Dir(providerPath)), "artifacts", "sub2api-host")
	bundle, err := artifact.LoadBundle(root)
	if err != nil {
		return artifactBundle{}, err
	}
	return artifactBundle{Root: bundle.Root, Manifest: bundle.Manifest}, nil
}

type host struct {
	version string
	key     hostcontract.RevisionKey
	keyID   string
	deps    lifecycleDependencies
}

func (h *host) schema(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
	// This is an explicit, closed typed schema for the frozen hostcontract structs. It
	// intentionally has no map-of-object escape hatch for target or secrets.
	return p.GetSchemaResponse{Schema: fmt.Sprintf(`{"name":"sub2api-host","version":%q,"config":{"variables":{"revisionKey":{"type":"string","secret":true}},"defaults":["revisionKey"]},"resources":{"%s":{"inputProperties":{"resource":{"$ref":"#/types/sub2api-host:index:ResourceIdentity"},"server":{"$ref":"#/types/sub2api-host:index:ServerTarget"},"target":{"$ref":"#/types/sub2api-host:index:Target"},"secrets":{"$ref":"#/types/sub2api-host:index:Secrets","secret":true}},"requiredInputs":["resource","server","target","secrets"],"properties":{"resource":{"$ref":"#/types/sub2api-host:index:ResourceIdentity"},"server":{"$ref":"#/types/sub2api-host:index:ServerTarget"},"target":{"$ref":"#/types/sub2api-host:index:Target"},"secrets":{"$ref":"#/types/sub2api-host:index:Secrets","secret":true},"machine":{"$ref":"#/types/sub2api-host:index:MachineIdentity"},"ownership":{"$ref":"#/types/sub2api-host:index:OwnershipIdentity"},"appliedRevision":{"type":"string"},"observation":{"$ref":"#/types/sub2api-host:index:StableObservation"}}}},"types":%s}`, h.version, hostToken, schemaTypes)}, nil
}

func (h *host) checkConfig(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
	v, ok := req.Inputs.GetOk("revisionKey")
	if !ok || v.IsNull() {
		return p.CheckResponse{Failures: []p.CheckFailure{{Property: "revisionKey", Reason: "is required"}}}, nil
	}
	if v.IsComputed() {
		return p.CheckResponse{Inputs: req.Inputs.Set("revisionKey", v.WithSecret(true))}, nil
	}
	v = unwrap(v)
	if !v.IsString() {
		return p.CheckResponse{Failures: []p.CheckFailure{{Property: "revisionKey", Reason: "must be base64-encoded 32-byte key"}}}, nil
	}
	b, err := base64.StdEncoding.Strict().DecodeString(v.AsString())
	if err != nil || len(b) != 32 {
		return p.CheckResponse{Failures: []p.CheckFailure{{Property: "revisionKey", Reason: "must be base64-encoded 32-byte key"}}}, nil
	}
	return p.CheckResponse{Inputs: req.Inputs.Set("revisionKey", property.New(v.AsString()).WithSecret(true))}, nil
}

func (h *host) configure(_ context.Context, req p.ConfigureRequest) error {
	v, ok := req.Args.GetOk("revisionKey")
	if !ok || v.IsNull() || v.HasComputed() || !v.Secret() {
		return fmt.Errorf("revisionKey must be known and secret")
	}
	v = unwrap(v)
	if !v.IsString() {
		return fmt.Errorf("revisionKey must be base64-encoded 32-byte key")
	}
	b, err := base64.StdEncoding.Strict().DecodeString(v.AsString())
	if err != nil || len(b) != 32 {
		return fmt.Errorf("revisionKey must be base64-encoded 32-byte key")
	}
	h.key = hostcontract.RevisionKey(append([]byte(nil), b...))
	digest := sha256.Sum256(b)
	h.keyID = hex.EncodeToString(digest[:8])
	return nil
}

func (h *host) configuredKey() (id string, available bool) { return h.keyID, h.key.Validate() == nil }

func (h *host) check(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
	inputs := req.Inputs
	var failures []p.CheckFailure
	inputs.All(func(name string, _ property.Value) bool {
		if name != "resource" && name != "server" && name != "target" && name != "secrets" {
			failures = append(failures, p.CheckFailure{Property: name, Reason: "is not allowed"})
		}
		return true
	})
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		v, ok := inputs.GetOk(name)
		if !ok || v.IsNull() {
			failures = append(failures, p.CheckFailure{Property: name, Reason: "is required"})
		}
	}
	if v, ok := inputs.GetOk("secrets"); ok && !v.Secret() {
		inputs = inputs.Set("secrets", v.WithSecret(true))
	}
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		if v, ok := inputs.GetOk(name); ok {
			failures = append(failures, validateShape(name, v)...)
		}
	}
	if resource, ok := inputs.GetOk("resource"); ok && !resource.IsComputed() && unwrap(resource).IsMap() {
		for _, name := range []string{"environment", "serverKey"} {
			if v, ok := unwrap(resource).AsMap().GetOk(name); ok && !v.IsComputed() && v.IsString() && v.AsString() == "" {
				failures = append(failures, p.CheckFailure{Property: "resource." + name, Reason: "must not be empty"})
			}
		}
	}
	if server, ok := inputs.GetOk("server"); ok && !server.IsComputed() && unwrap(server).IsMap() {
		if alias, ok := unwrap(server).AsMap().GetOk("sshAlias"); ok && !alias.IsComputed() && alias.IsString() && alias.AsString() == "" {
			failures = append(failures, p.CheckFailure{Property: "server.sshAlias", Reason: "must not be empty"})
		}
	}
	if len(failures) != 0 {
		return p.CheckResponse{Inputs: inputs, Failures: failures}, nil
	}
	if !hasComputed(inputs) {
		if err := validate(inputs); err != nil {
			return p.CheckResponse{Inputs: inputs, Failures: []p.CheckFailure{{Property: "target", Reason: err.Error()}}}, nil
		}
	}
	return p.CheckResponse{Inputs: inputs}, nil
}

func (h *host) diff(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
	old, next := req.OldInputs, req.Inputs
	oldResource, oldOK := old.GetOk("resource")
	resource, newOK := next.GetOk("resource")
	if oldOK && newOK && knownDifferent(field(oldResource, "serverKey"), field(resource, "serverKey")) {
		return p.DiffResponse{}, fmt.Errorf("serverKey changes require an explicit Pulumi alias or state move")
	}
	if hasComputed(old) || hasComputed(next) {
		return p.DiffResponse{HasChanges: true, DetailedDiff: map[string]p.PropertyDiff{}}, nil
	}
	if err := validate(next); err != nil {
		return p.DiffResponse{}, err
	}
	d := map[string]p.PropertyDiff{}
	for _, key := range []string{"target", "secrets"} {
		if changed(old, next, key) {
			d[key] = p.PropertyDiff{Kind: p.Update, InputDiff: true}
		}
	}
	if changed(old, next, "server") {
		d["server.sshAlias"] = p.PropertyDiff{Kind: p.Update, InputDiff: true}
	}
	if hasCheckpointOutputs(req.State) {
		_, _, applied, observation, err := parseCheckpoint(req.State)
		if err != nil {
			return p.DiffResponse{}, fmt.Errorf("invalid checkpoint")
		}
		if observation.Drifted || applied != observation.AppliedRevision {
			d["observation"] = p.PropertyDiff{Kind: p.Update}
		}
		if _, configured := h.configuredKey(); configured {
			previous, err := h.parseInputs(old)
			if err != nil {
				return p.DiffResponse{}, fmt.Errorf("invalid checkpoint inputs")
			}
			if applied != previous.revision || observation.AppliedRevision != previous.revision {
				d["observation"] = p.PropertyDiff{Kind: p.Update}
			}
		}
	}
	return p.DiffResponse{HasChanges: len(d) > 0, DetailedDiff: d}, nil
}

func (h *host) create(ctx context.Context, req p.CreateRequest) (p.CreateResponse, error) {
	return h.lifecycleCreate(ctx, req)
}

func (h *host) read(ctx context.Context, req p.ReadRequest) (p.ReadResponse, error) {
	return h.lifecycleRead(ctx, req)
}

func (h *host) update(ctx context.Context, req p.UpdateRequest) (p.UpdateResponse, error) {
	return h.lifecycleUpdate(ctx, req)
}

func (h *host) delete(ctx context.Context, req p.DeleteRequest) error {
	return h.lifecycleDelete(ctx, req)
}

func changed(old, next property.Map, key string) bool {
	a, aOK := old.GetOk(key)
	b, bOK := next.GetOk(key)
	return aOK != bOK || (aOK && !a.Equals(b))
}
func field(v property.Value, key string) property.Value {
	v = unwrap(v)
	if !v.IsMap() {
		return property.New(property.Computed)
	}
	x, ok := v.AsMap().GetOk(key)
	if !ok {
		return property.New(property.Computed)
	}
	return x
}
func knownDifferent(a, b property.Value) bool {
	return !a.HasComputed() && !b.HasComputed() && !a.IsNull() && !b.IsNull() && !a.Equals(b)
}
func hasComputed(m property.Map) bool {
	computed := false
	m.All(func(_ string, v property.Value) bool { computed = v.HasComputed(); return !computed })
	return computed
}

func hasCheckpointOutputs(state property.Map) bool {
	for _, name := range []string{"machine", "ownership", "appliedRevision", "observation"} {
		if _, ok := state.GetOk(name); ok {
			return true
		}
	}
	return false
}

// property.Value keeps its secret marker alongside its payload; unlike the legacy
// resource.PropertyValue API it does not need an element unwrap.
func unwrap(v property.Value) property.Value { return v }

func previewState(inputs property.Map) property.Map {
	return inputs.Set("machine", property.New(property.Computed)).Set("ownership", property.New(property.Computed)).Set("appliedRevision", property.New(property.Computed)).Set("observation", property.New(property.Computed))
}

func validate(m property.Map) error {
	resource, _ := m.GetOk("resource")
	r := unwrap(resource).AsMap()
	environment, eo := r.GetOk("environment")
	serverKey, ko := r.GetOk("serverKey")
	if !eo || !ko || !unwrap(environment).IsString() || !unwrap(serverKey).IsString() || unwrap(environment).AsString() == "" || unwrap(serverKey).AsString() == "" {
		return fmt.Errorf("resource environment and serverKey are required")
	}
	server, _ := m.GetOk("server")
	alias, ok := unwrap(server).AsMap().GetOk("sshAlias")
	if !ok || !unwrap(alias).IsString() || unwrap(alias).AsString() == "" {
		return fmt.Errorf("server sshAlias is required")
	}
	target, _ := m.GetOk("target")
	secrets, _ := m.GetOk("secrets")
	var t hostcontract.Target
	var s hostcontract.Secrets
	if err := decode(target, &t); err != nil {
		return fmt.Errorf("target is invalid")
	}
	if err := decode(secrets, &s); err != nil {
		return fmt.Errorf("secrets are invalid")
	}
	return hostcontract.ValidateTarget(t, s)
}
func decode(value property.Value, out any) error {
	raw, err := rawValue(unwrap(value))
	if err != nil {
		return err
	}
	bytes, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, out)
}
func rawValue(value property.Value) (any, error) {
	value = unwrap(value)
	if value.IsString() {
		return value.AsString(), nil
	}
	if value.IsBool() {
		return value.AsBool(), nil
	}
	if value.IsNumber() {
		return value.AsNumber(), nil
	}
	if value.IsNull() {
		return nil, nil
	}
	if value.IsMap() {
		raw := map[string]any{}
		var err error
		value.AsMap().All(func(key string, nested property.Value) bool { raw[key], err = rawValue(nested); return err == nil })
		return raw, err
	}
	if value.IsArray() {
		values := value.AsArray().AsSlice()
		raw := make([]any, len(values))
		for i, nested := range values {
			var err error
			raw[i], err = rawValue(nested)
			if err != nil {
				return nil, err
			}
		}
		return raw, nil
	}
	return nil, fmt.Errorf("unsupported property")
}

type shape struct {
	fields      map[string]shape
	mapValue    *shape
	array       *shape
	kind        string
	required    []string
	nonEmpty    bool
	oneOf       []string
	minimum     float64
	maximum     float64
	keyNonEmpty bool
}

var scalar = shape{kind: "string"}
var boolean = shape{kind: "bool"}
var number = shape{kind: "number"}
var requiredString = shape{kind: "string", nonEmpty: true}
var port = shape{kind: "number", minimum: 1, maximum: 65535}
var dataIdentity = shape{fields: map[string]shape{"kind": {kind: "string", nonEmpty: true, oneOf: []string{"postgres", "redis"}}, "providerId": requiredString, "endpoint": requiredString, "port": port, "database": requiredString, "tlsServerName": scalar}, required: []string{"kind", "providerId", "endpoint", "port", "database"}}
var appTarget = shape{fields: map[string]shape{"id": requiredString, "image": requiredString, "hostname": requiredString, "readinessPath": requiredString, "drainTimeout": scalar, "initialBootstrap": boolean, "runtimeSettings": {mapValue: &scalar, keyNonEmpty: true}, "dataLinks": {array: &shape{fields: map[string]shape{"name": requiredString, "identity": dataIdentity}, required: []string{"name", "identity"}}}}, required: []string{"id", "image", "hostname", "readinessPath"}}
var targetShape = shape{fields: map[string]shape{"releaseArtifact": requiredString, "apps": {array: &appTarget}, "dataServices": {array: &shape{fields: map[string]shape{"id": requiredString, "type": {kind: "string", nonEmpty: true, oneOf: []string{"postgres", "redis"}}, "port": port, "persistence": boolean}, required: []string{"id", "type", "port"}}}, "reverseProxy": {fields: map[string]shape{"image": requiredString, "acmeEmail": requiredString}, required: []string{"image", "acmeEmail"}}, "microSocks": {fields: map[string]shape{"server": boolean, "clients": {array: &shape{fields: map[string]shape{"id": requiredString}, required: []string{"id"}}}}}, "connectors": {array: &shape{fields: map[string]shape{"id": requiredString, "tunnelId": requiredString, "appIds": {array: &requiredString}}, required: []string{"id", "tunnelId"}}}}, required: []string{"releaseArtifact"}}
var credentials = shape{fields: map[string]shape{"username": requiredString, "password": requiredString}, required: []string{"username", "password"}}
var secretsShape = shape{fields: map[string]shape{"apps": {mapValue: &shape{fields: map[string]shape{"initialAdminPassword": scalar, "jwtSecret": scalar, "totpEncryptionKey": scalar, "adminApiKey": scalar, "runtimeEnvironment": {mapValue: &scalar, keyNonEmpty: true}, "postgres": credentials, "redis": credentials}}, keyNonEmpty: true}, "localDataServices": {mapValue: &shape{fields: map[string]shape{"adminPassword": requiredString}, required: []string{"adminPassword"}}, keyNonEmpty: true}, "reverseProxy": {fields: map[string]shape{"dnsChallengeToken": requiredString}, required: []string{"dnsChallengeToken"}}, "microSocks": {fields: map[string]shape{"serverUsername": scalar, "serverPassword": scalar, "clientCredentials": {mapValue: &credentials, keyNonEmpty: true}}}, "connectors": {mapValue: &shape{fields: map[string]shape{"token": requiredString}, required: []string{"token"}}, keyNonEmpty: true}}}

func validateShape(root string, v property.Value) []p.CheckFailure {
	var s shape
	switch root {
	case "resource":
		s = shape{fields: map[string]shape{"environment": scalar, "serverKey": scalar}, required: []string{"environment", "serverKey"}}
	case "server":
		s = shape{fields: map[string]shape{"sshAlias": scalar}, required: []string{"sshAlias"}}
	case "target":
		s = targetShape
	case "secrets":
		s = secretsShape
	}
	var failures []p.CheckFailure
	var walk func(string, property.Value, shape)
	walk = func(path string, value property.Value, expected shape) {
		if value.IsComputed() {
			return
		}
		value = unwrap(value)
		if value.IsNull() {
			failures = append(failures, p.CheckFailure{Property: path, Reason: "must not be null"})
			return
		}
		if expected.kind == "string" && !value.IsString() || expected.kind == "bool" && !value.IsBool() || expected.kind == "number" && !value.IsNumber() {
			failures = append(failures, p.CheckFailure{Property: path, Reason: "has the wrong type"})
			return
		}
		if expected.nonEmpty && value.IsString() && value.AsString() == "" {
			failures = append(failures, p.CheckFailure{Property: path, Reason: "must not be empty"})
		}
		if len(expected.oneOf) > 0 && value.IsString() && !containsString(expected.oneOf, value.AsString()) {
			failures = append(failures, p.CheckFailure{Property: path, Reason: "has an invalid value"})
		}
		if expected.kind == "number" && (value.AsNumber() < expected.minimum || expected.maximum > 0 && value.AsNumber() > expected.maximum) {
			failures = append(failures, p.CheckFailure{Property: path, Reason: "is out of range"})
		}
		if expected.array != nil {
			if !value.IsArray() {
				failures = append(failures, p.CheckFailure{Property: path, Reason: "must be an array"})
				return
			}
			for i, x := range value.AsArray().AsSlice() {
				walk(fmt.Sprintf("%s[%d]", path, i), x, *expected.array)
			}
			return
		}
		if expected.mapValue != nil {
			if !value.IsMap() {
				failures = append(failures, p.CheckFailure{Property: path, Reason: "must be an object"})
				return
			}
			value.AsMap().All(func(k string, x property.Value) bool {
				if expected.keyNonEmpty && k == "" {
					failures = append(failures, p.CheckFailure{Property: path + ".", Reason: "key must not be empty"})
				}
				walk(path+"."+k, x, *expected.mapValue)
				return true
			})
			return
		}
		if expected.fields == nil {
			return
		}
		if !value.IsMap() {
			failures = append(failures, p.CheckFailure{Property: path, Reason: "must be an object"})
			return
		}
		m := value.AsMap()
		for _, key := range expected.required {
			if x, ok := m.GetOk(key); !ok || x.IsNull() {
				failures = append(failures, p.CheckFailure{Property: path + "." + key, Reason: "is required"})
			}
		}
		m.All(func(k string, x property.Value) bool {
			child, ok := expected.fields[k]
			if !ok {
				failures = append(failures, p.CheckFailure{Property: path + "." + k, Reason: "is not allowed"})
				return true
			}
			walk(path+"."+k, x, child)
			return true
		})
	}
	walk(root, v, s)
	return failures
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

const schemaTypes = `{"sub2api-host:index:ResourceIdentity":{"type":"object","properties":{"environment":{"type":"string"},"serverKey":{"type":"string"}},"required":["environment","serverKey"]},"sub2api-host:index:ServerTarget":{"type":"object","properties":{"sshAlias":{"type":"string"}},"required":["sshAlias"]},"sub2api-host:index:MachineIdentity":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]},"sub2api-host:index:OwnershipIdentity":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]},"sub2api-host:index:DataIdentity":{"type":"object","properties":{"kind":{"type":"string"},"providerId":{"type":"string"},"endpoint":{"type":"string"},"port":{"type":"integer"},"database":{"type":"string"},"tlsServerName":{"type":"string"}},"required":["kind","providerId","endpoint","port","database"]},"sub2api-host:index:Target":{"type":"object","properties":{"releaseArtifact":{"type":"string"},"apps":{"type":"array","items":{"$ref":"#/types/sub2api-host:index:AppTarget"}},"dataServices":{"type":"array","items":{"$ref":"#/types/sub2api-host:index:LocalDataServiceTarget"}},"reverseProxy":{"$ref":"#/types/sub2api-host:index:ReverseProxyTarget"},"microSocks":{"$ref":"#/types/sub2api-host:index:MicroSocksTarget"},"connectors":{"type":"array","items":{"$ref":"#/types/sub2api-host:index:TunnelConnectorTarget"}}},"required":["releaseArtifact"]},"sub2api-host:index:AppTarget":{"type":"object","properties":{"id":{"type":"string"},"image":{"type":"string"},"hostname":{"type":"string"},"readinessPath":{"type":"string"},"drainTimeout":{"type":"string"},"initialBootstrap":{"type":"boolean"},"runtimeSettings":{"type":"object","additionalProperties":{"type":"string"}},"dataLinks":{"type":"array","items":{"$ref":"#/types/sub2api-host:index:DataLink"}}},"required":["id","image","hostname","readinessPath"]},"sub2api-host:index:DataLink":{"type":"object","properties":{"name":{"type":"string"},"identity":{"$ref":"#/types/sub2api-host:index:DataIdentity"}},"required":["name","identity"]},"sub2api-host:index:LocalDataServiceTarget":{"type":"object","properties":{"id":{"type":"string"},"type":{"type":"string"},"port":{"type":"integer"},"persistence":{"type":"boolean"}},"required":["id","type","port"]},"sub2api-host:index:ReverseProxyTarget":{"type":"object","properties":{"image":{"type":"string"},"acmeEmail":{"type":"string"}},"required":["image","acmeEmail"]},"sub2api-host:index:MicroSocksTarget":{"type":"object","properties":{"server":{"type":"boolean"},"clients":{"type":"array","items":{"$ref":"#/types/sub2api-host:index:MicroSocksClientTarget"}}}},"sub2api-host:index:MicroSocksClientTarget":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},"sub2api-host:index:TunnelConnectorTarget":{"type":"object","properties":{"id":{"type":"string"},"tunnelId":{"type":"string"},"appIds":{"type":"array","items":{"type":"string"}}},"required":["id","tunnelId"]},"sub2api-host:index:Secrets":{"type":"object","properties":{"apps":{"type":"object","additionalProperties":{"$ref":"#/types/sub2api-host:index:AppSecrets"}},"localDataServices":{"type":"object","additionalProperties":{"$ref":"#/types/sub2api-host:index:LocalDataServiceSecrets"}},"reverseProxy":{"$ref":"#/types/sub2api-host:index:ReverseProxySecrets"},"microSocks":{"$ref":"#/types/sub2api-host:index:MicroSocksSecrets"},"connectors":{"type":"object","additionalProperties":{"$ref":"#/types/sub2api-host:index:TunnelConnectorSecrets"}}}},"sub2api-host:index:AppSecrets":{"type":"object","properties":{"initialAdminPassword":{"type":"string"},"jwtSecret":{"type":"string"},"totpEncryptionKey":{"type":"string"},"adminApiKey":{"type":"string"},"runtimeEnvironment":{"type":"object","additionalProperties":{"type":"string"}},"postgres":{"$ref":"#/types/sub2api-host:index:DataCredentials"},"redis":{"$ref":"#/types/sub2api-host:index:DataCredentials"}}},"sub2api-host:index:DataCredentials":{"type":"object","properties":{"username":{"type":"string"},"password":{"type":"string"}},"required":["username","password"]},"sub2api-host:index:LocalDataServiceSecrets":{"type":"object","properties":{"adminPassword":{"type":"string"}},"required":["adminPassword"]},"sub2api-host:index:ReverseProxySecrets":{"type":"object","properties":{"dnsChallengeToken":{"type":"string"}},"required":["dnsChallengeToken"]},"sub2api-host:index:MicroSocksSecrets":{"type":"object","properties":{"serverUsername":{"type":"string"},"serverPassword":{"type":"string"},"clientCredentials":{"type":"object","additionalProperties":{"$ref":"#/types/sub2api-host:index:DataCredentials"}}}},"sub2api-host:index:TunnelConnectorSecrets":{"type":"object","properties":{"token":{"type":"string"}},"required":["token"]},"sub2api-host:index:StableObservation":{"type":"object","properties":{"machine":{"$ref":"#/types/sub2api-host:index:MachineIdentity"},"ownership":{"$ref":"#/types/sub2api-host:index:OwnershipIdentity"},"hostRelease":{"type":"string"},"appliedRevision":{"type":"string"},"drifted":{"type":"boolean"},"ready":{"type":"boolean"},"apps":{"type":"array","items":{"$ref":"#/types/sub2api-host:index:AppObservation"}},"data":{"type":"array","items":{"$ref":"#/types/sub2api-host:index:DataObservation"}}},"required":["machine","ownership","hostRelease","appliedRevision","ready"]},"sub2api-host:index:AppObservation":{"type":"object","properties":{"id":{"type":"string"},"activeImage":{"type":"string"},"ready":{"type":"boolean"}},"required":["id","activeImage","ready"]},"sub2api-host:index:DataObservation":{"type":"object","properties":{"identity":{"$ref":"#/types/sub2api-host:index:DataIdentity"},"ready":{"type":"boolean"}},"required":["identity","ready"]}}`
