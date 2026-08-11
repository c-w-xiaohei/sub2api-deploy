package hostprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/artifact"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/openssh"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/sshcheck"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

var errArtifactUnavailable = errors.New("host artifact unavailable")

type lifecycleTransport interface {
	Probe(context.Context, string) (artifact.ProbeInfo, error)
	Bootstrap(context.Context, string, []byte) (hostprotocol.Response, error)
	Run(context.Context, string, openssh.Command, []byte) (hostprotocol.Response, error)
}

type artifactBundle struct {
	Root     string
	Manifest artifact.Manifest
}

type lifecycleDependencies struct {
	transport lifecycleTransport
	artifact  func() (artifactBundle, error)
	approve   func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error)
}

func newHostWithDependencies(version string, deps lifecycleDependencies) *host {
	return &host{version: version, deps: deps}
}

type lifecycleInput struct {
	resource hostcontract.ResourceIdentity
	server   hostcontract.ServerTarget
	target   hostcontract.Target
	secrets  hostcontract.Secrets
	original property.Map
	revision string
}

func (h *host) parseInputs(values property.Map) (lifecycleInput, error) {
	if _, ok := h.configuredKey(); !ok {
		return lifecycleInput{}, fmt.Errorf("revision key is unavailable")
	}
	if hasComputed(values) {
		return lifecycleInput{}, fmt.Errorf("lifecycle inputs must be known")
	}

	validNames := map[string]bool{"resource": true, "server": true, "target": true, "secrets": true}
	unknown := false
	values.All(func(name string, _ property.Value) bool {
		unknown = !validNames[name]
		return !unknown
	})
	if unknown {
		return lifecycleInput{}, fmt.Errorf("lifecycle inputs are invalid")
	}

	for _, name := range []string{"resource", "server", "target", "secrets"} {
		value, ok := values.GetOk(name)
		if !ok || value.IsNull() {
			return lifecycleInput{}, fmt.Errorf("lifecycle inputs are invalid")
		}
	}
	if !valueAtMap(values, "secrets").Secret() {
		return lifecycleInput{}, fmt.Errorf("lifecycle secrets must be secret")
	}
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		if len(validateShape(name, valueAtMap(values, name))) != 0 {
			return lifecycleInput{}, fmt.Errorf("lifecycle inputs are invalid")
		}
	}
	if err := validate(values); err != nil {
		return lifecycleInput{}, fmt.Errorf("lifecycle inputs are invalid")
	}

	var in lifecycleInput
	if decode(valueAtMap(values, "resource"), &in.resource) != nil ||
		decode(valueAtMap(values, "server"), &in.server) != nil ||
		decode(valueAtMap(values, "target"), &in.target) != nil ||
		decode(valueAtMap(values, "secrets"), &in.secrets) != nil {
		return lifecycleInput{}, fmt.Errorf("lifecycle inputs are invalid")
	}
	if err := sshcheck.ValidateAlias(in.server.SSHAlias); err != nil {
		return lifecycleInput{}, fmt.Errorf("ssh alias is invalid")
	}
	revision, err := hostcontract.TargetRevision(h.key, in.resource, in.target, in.secrets)
	if err != nil {
		return lifecycleInput{}, fmt.Errorf("lifecycle inputs are invalid")
	}
	in.original, in.revision = values, revision
	return in, nil
}

func valueAtMap(m property.Map, key string) property.Value {
	value, _ := m.GetOk(key)
	return value
}

func inputProperties(values property.Map) property.Map {
	inputs := property.NewMap(nil)
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		if value, ok := values.GetOk(name); ok {
			inputs = inputs.Set(name, value)
		}
	}
	return inputs
}

func (h *host) lifecycleCreate(ctx context.Context, req p.CreateRequest) (p.CreateResponse, error) {
	if req.DryRun {
		return p.CreateResponse{Properties: previewState(req.Properties)}, nil
	}

	in, err := h.parseInputs(req.Properties)
	if err != nil {
		return p.CreateResponse{}, err
	}
	if h.deps.artifact == nil {
		return p.CreateResponse{}, errArtifactUnavailable
	}

	bundle, err := h.deps.artifact()
	if err != nil {
		if errors.Is(err, errArtifactUnavailable) {
			return p.CreateResponse{}, errArtifactUnavailable
		}
		return p.CreateResponse{}, fmt.Errorf("host artifact unavailable")
	}
	if bundle.Manifest.Release != in.target.ReleaseArtifact {
		return p.CreateResponse{}, fmt.Errorf("host artifact does not match target release")
	}

	prior, err := hostcontract.TargetRevision(
		h.key,
		in.resource,
		hostcontract.Target{ReleaseArtifact: in.target.ReleaseArtifact},
		hostcontract.Secrets{},
	)
	if err != nil {
		return p.CreateResponse{}, fmt.Errorf("lifecycle inputs are invalid")
	}
	reconcile := hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: in.server, Resource: in.resource, TargetRevision: in.revision, PriorAppliedRevision: prior, Target: &in.target, Secrets: &in.secrets}
	frame, err := hostprotocol.EncodeRequest(reconcile)
	if err != nil {
		return p.CreateResponse{}, fmt.Errorf("invalid lifecycle request")
	}
	if h.deps.transport == nil {
		return p.CreateResponse{}, fmt.Errorf("transport unavailable")
	}

	probe, err := h.deps.transport.Probe(ctx, in.server.SSHAlias)
	if err != nil {
		return p.CreateResponse{}, fmt.Errorf("transport failed")
	}
	if probe.OS != "Linux" || probe.Machine == "" {
		return p.CreateResponse{}, fmt.Errorf("unsupported host")
	}

	pinned, err := artifact.LoadPinned(bundle.Root, bundle.Manifest, probe.Arch)
	if err != nil {
		return p.CreateResponse{}, fmt.Errorf("host artifact unavailable")
	}
	stdin, err := artifact.BootstrapInput(pinned, frame)
	if err != nil {
		return p.CreateResponse{}, fmt.Errorf("host artifact unavailable")
	}
	result, err := h.deps.transport.Bootstrap(ctx, in.server.SSHAlias, stdin)
	if err != nil {
		return p.CreateResponse{}, fmt.Errorf("transport failed")
	}
	if err := expectedResponse(result, hostprotocol.ResultApplied, "bootstrap"); err != nil {
		return p.CreateResponse{}, err
	}
	if result.Result.AppliedRevision != in.revision {
		return p.CreateResponse{}, fmt.Errorf("invalid bootstrap response")
	}

	observation, err := h.inspect(ctx, in)
	if err != nil {
		return p.CreateResponse{}, err
	}
	if err := validateObservation(observation, probe.Machine, "", in.target, in.revision); err != nil {
		return p.CreateResponse{}, err
	}
	state, err := checkpointState(in.original, observation, in.revision)
	if err != nil {
		return p.CreateResponse{}, err
	}
	return p.CreateResponse{ID: stableID(in.resource), Properties: state}, nil
}

func (h *host) lifecycleUpdate(ctx context.Context, req p.UpdateRequest) (p.UpdateResponse, error) {
	if req.DryRun {
		return p.UpdateResponse{Properties: previewState(req.Inputs)}, nil
	}

	next, err := h.parseInputs(req.Inputs)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	old, err := h.parseInputs(inputProperties(req.State))
	if err != nil {
		return p.UpdateResponse{}, fmt.Errorf("invalid checkpoint")
	}
	if old.resource != next.resource {
		return p.UpdateResponse{}, fmt.Errorf("resource identity changes are not supported")
	}
	if req.OldInputs.Len() != 0 {
		oldInputs, err := h.parseInputs(req.OldInputs)
		if err != nil {
			return p.UpdateResponse{}, fmt.Errorf("invalid checkpoint inputs")
		}
		if oldInputs.resource != old.resource ||
			oldInputs.server != old.server ||
			oldInputs.target.ReleaseArtifact != old.target.ReleaseArtifact ||
			oldInputs.revision != old.revision ||
			!reflect.DeepEqual(oldInputs.target, old.target) ||
			!reflect.DeepEqual(oldInputs.secrets, old.secrets) {
			return p.UpdateResponse{}, fmt.Errorf("checkpoint inputs do not match")
		}
	}

	machine, owner, applied, checkpointObservation, err := parseCheckpoint(req.State)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	if checkpointObservation.Machine != machine ||
		checkpointObservation.Ownership != owner ||
		checkpointObservation.AppliedRevision != applied ||
		applied != old.revision {
		return p.UpdateResponse{}, fmt.Errorf("invalid checkpoint")
	}
	if _, err := hostcontract.ParseRevision(applied); err != nil {
		return p.UpdateResponse{}, fmt.Errorf("invalid checkpoint")
	}

	initial, err := h.inspect(ctx, next)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	if initial.Machine != machine ||
		initial.Ownership != owner ||
		initial.HostRelease != old.target.ReleaseArtifact ||
		(initial.AppliedRevision != applied && initial.AppliedRevision != next.revision) {
		return p.UpdateResponse{}, fmt.Errorf("unsafe remote observation")
	}
	if next.target.ReleaseArtifact != old.target.ReleaseArtifact {
		return p.UpdateResponse{}, fmt.Errorf("unsupported upgrade")
	}

	approval, err := dangerousApproval(ctx, h.deps.approve, old, next)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	reconcile := hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: next.server, Resource: next.resource, TargetRevision: next.revision, PriorAppliedRevision: applied, Target: &next.target, Secrets: &next.secrets, Approval: approval}
	frame, err := hostprotocol.EncodeRequest(reconcile)
	if err != nil {
		return p.UpdateResponse{}, fmt.Errorf("invalid lifecycle request")
	}
	if h.deps.transport == nil {
		return p.UpdateResponse{}, fmt.Errorf("transport unavailable")
	}
	result, err := h.deps.transport.Run(ctx, next.server.SSHAlias, openssh.Host, frame)
	if err != nil {
		return p.UpdateResponse{}, fmt.Errorf("transport failed")
	}
	if err := expectedResponse(result, hostprotocol.ResultApplied, "reconcile"); err != nil {
		return p.UpdateResponse{}, err
	}
	if result.Result.AppliedRevision != next.revision {
		return p.UpdateResponse{}, fmt.Errorf("invalid reconcile response")
	}

	final, err := h.inspect(ctx, next)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	if err := validateObservation(final, machine.Value, owner.Value, next.target, next.revision); err != nil {
		return p.UpdateResponse{}, err
	}
	state, err := checkpointState(next.original, final, next.revision)
	if err != nil {
		return p.UpdateResponse{}, err
	}
	return p.UpdateResponse{Properties: state}, nil
}

func (h *host) inspect(ctx context.Context, in lifecycleInput) (hostcontract.StableObservation, error) {
	if h.deps.transport == nil {
		return hostcontract.StableObservation{}, fmt.Errorf("transport unavailable")
	}

	request := hostprotocol.Request{
		Action:         hostcontract.ActionInspect,
		Server:         in.server,
		Resource:       in.resource,
		TargetRevision: in.revision,
	}
	frame, err := hostprotocol.EncodeRequest(request)
	if err != nil {
		return hostcontract.StableObservation{}, fmt.Errorf("invalid lifecycle request")
	}
	response, err := h.deps.transport.Run(ctx, in.server.SSHAlias, openssh.Host, frame)
	if err != nil {
		return hostcontract.StableObservation{}, fmt.Errorf("transport failed")
	}
	if err := expectedResponse(response, hostprotocol.ResultInspected, "inspect"); err != nil {
		return hostcontract.StableObservation{}, err
	}
	return *response.Result.Observation, nil
}

func expectedResponse(response hostprotocol.Response, status hostprotocol.ResultStatus, stage string) error {
	if _, err := hostprotocol.EncodeResponse(response); err != nil {
		return fmt.Errorf("invalid %s response", stage)
	}
	if response.Error != nil {
		return fmt.Errorf("%s remote response", stage)
	}
	if response.Result == nil || response.Result.Status != status {
		return fmt.Errorf("invalid %s response", stage)
	}
	return nil
}

func parseCheckpoint(state property.Map) (hostcontract.MachineIdentity, hostcontract.OwnershipIdentity, string, hostcontract.StableObservation, error) {
	var machine hostcontract.MachineIdentity
	var owner hostcontract.OwnershipIdentity
	var observation hostcontract.StableObservation

	machineValue, hasMachine := state.GetOk("machine")
	ownerValue, hasOwner := state.GetOk("ownership")
	revisionValue, hasRevision := state.GetOk("appliedRevision")
	observationValue, hasObservation := state.GetOk("observation")
	if !hasMachine || !hasOwner || !hasRevision || !hasObservation ||
		machineValue.HasComputed() || ownerValue.HasComputed() ||
		revisionValue.HasComputed() || observationValue.HasComputed() ||
		!revisionValue.IsString() ||
		decode(machineValue, &machine) != nil ||
		decode(ownerValue, &owner) != nil ||
		decode(observationValue, &observation) != nil ||
		observation.Validate() != nil {
		return machine, owner, "", observation, fmt.Errorf("invalid checkpoint")
	}
	return machine, owner, revisionValue.AsString(), observation, nil
}

func checkpointState(inputs property.Map, observation hostcontract.StableObservation, revision string) (property.Map, error) {
	machine, err := propertyValue(observation.Machine)
	if err != nil {
		return property.NewMap(nil), err
	}
	owner, err := propertyValue(observation.Ownership)
	if err != nil {
		return property.NewMap(nil), err
	}
	observed, err := propertyValue(observation)
	if err != nil {
		return property.NewMap(nil), err
	}
	return inputs.Set("machine", machine).Set("ownership", owner).Set("appliedRevision", property.New(revision)).Set("observation", observed), nil
}

func propertyValue(value any) (property.Value, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return property.Value{}, fmt.Errorf("checkpoint encoding failed")
	}

	var raw any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return property.Value{}, fmt.Errorf("checkpoint encoding failed")
	}
	return valueFromRaw(raw)
}

func valueFromRaw(raw any) (property.Value, error) {
	switch value := raw.(type) {
	case nil:
		return property.New(property.Null), nil
	case string:
		return property.New(value), nil
	case bool:
		return property.New(value), nil
	case float64:
		return property.New(value), nil
	case []any:
		values := make([]property.Value, len(value))
		for i := range value {
			nested, err := valueFromRaw(value[i])
			if err != nil {
				return property.Value{}, err
			}
			values[i] = nested
		}
		return property.New(property.NewArray(values)), nil
	case map[string]any:
		values := map[string]property.Value{}
		for key, rawValue := range value {
			nested, err := valueFromRaw(rawValue)
			if err != nil {
				return property.Value{}, err
			}
			values[key] = nested
		}
		return property.New(property.NewMap(values)), nil
	default:
		return property.Value{}, fmt.Errorf("checkpoint encoding failed")
	}
}

func stableID(r hostcontract.ResourceIdentity) string {
	payload := fmt.Sprintf("sub2api-host-resource-id-v1:%d:%s%d:%s", len(r.Environment), r.Environment, len(r.ServerKey), r.ServerKey)
	sum := sha256.Sum256([]byte(payload))
	return "host-" + hex.EncodeToString(sum[:])
}
func validateObservation(observation hostcontract.StableObservation, machine, owner string, target hostcontract.Target, revision string) error {
	if observation.Validate() != nil ||
		observation.Machine.Value != machine ||
		(owner != "" && observation.Ownership.Value != owner) ||
		observation.Ownership.Value == "" ||
		observation.HostRelease != target.ReleaseArtifact ||
		observation.AppliedRevision != revision ||
		!observation.Ready ||
		observation.Drifted {
		return fmt.Errorf("invalid final observation")
	}
	if !matchesTargetApps(observation.Apps, target.Apps) {
		return fmt.Errorf("invalid final observation")
	}
	if !matchesTargetDataServices(observation.Data, target.DataServices) {
		return fmt.Errorf("invalid final observation")
	}
	return nil
}

func matchesTargetApps(observed []hostcontract.AppObservation, target []hostcontract.AppTarget) bool {
	observedByID := map[string]hostcontract.AppObservation{}
	for _, app := range observed {
		if _, duplicate := observedByID[app.ID]; duplicate {
			return false
		}
		observedByID[app.ID] = app
	}
	if len(observedByID) != len(target) {
		return false
	}
	for _, app := range target {
		observedApp, ok := observedByID[app.ID]
		if !ok || observedApp.ActiveImage != app.Image || !observedApp.Ready {
			return false
		}
	}
	return true
}

type localDataKey struct {
	kind     string
	port     int
	database string
}

func matchesTargetDataServices(observed []hostcontract.DataObservation, target []hostcontract.LocalDataServiceTarget) bool {
	remaining := map[localDataKey]int{}
	for _, service := range target {
		database := "sub2api"
		if service.Type == "redis" {
			database = "0"
		}
		remaining[localDataKey{service.Type, service.Port, database}]++
	}

	seen := map[hostcontract.DataIdentity]bool{}
	for _, data := range observed {
		identity := data.Identity
		if !data.Ready ||
			identity.ProviderID == "" ||
			identity.ProviderID != identity.Endpoint ||
			seen[identity] {
			return false
		}
		if (identity.Kind == "postgres" && identity.TLSServerName != identity.Endpoint) ||
			(identity.Kind == "redis" && identity.TLSServerName != "") {
			return false
		}

		key := localDataKey{identity.Kind, identity.Port, identity.Database}
		if remaining[key] == 0 {
			return false
		}
		seen[identity] = true
		remaining[key]--
	}
	for _, count := range remaining {
		if count != 0 {
			return false
		}
	}
	return true
}

func dangerousApproval(ctx context.Context, approve func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error), old, next lifecycleInput) (*hostcontract.ApprovalSubject, error) {
	changes := []hostcontract.ApprovalSubject{}
	oldApps := map[string]hostcontract.AppTarget{}
	nextApps := map[string]hostcontract.AppTarget{}
	for _, app := range old.target.Apps {
		oldApps[app.ID] = app
	}
	for _, app := range next.target.Apps {
		nextApps[app.ID] = app
	}

	appIDs := sortedKeys(nextApps)
	for _, appID := range appIDs {
		before, exists := oldApps[appID]
		if !exists {
			continue
		}
		pairs, ambiguous := matchDataLinks(before.DataLinks, nextApps[appID].DataLinks)
		if ambiguous {
			return nil, fmt.Errorf("approval required")
		}
		for _, pair := range pairs {
			changes = append(changes, hostcontract.ApprovalSubject{
				Kind:           hostcontract.ApprovalDataLink,
				Environment:    next.resource.Environment,
				Resource:       next.resource,
				AppID:          appID,
				DataKind:       pair.old.Kind,
				OldData:        pair.old,
				NewData:        pair.new,
				TargetRevision: next.revision,
			})
		}
	}
	if len(changes) == 0 {
		return nil, nil
	}
	if len(changes) != 1 || approve == nil {
		return nil, fmt.Errorf("approval required")
	}

	approved, err := approve(ctx, changes[0])
	if err != nil || approved == nil || !reflect.DeepEqual(*approved, changes[0]) {
		return nil, fmt.Errorf("approval required")
	}
	return approved, nil
}

type dataLinkChange struct {
	old hostcontract.DataIdentity
	new hostcontract.DataIdentity
}

func matchDataLinks(old, next []hostcontract.DataLink) ([]dataLinkChange, bool) {
	oldByName := map[string]hostcontract.DataIdentity{}
	nextByName := map[string]hostcontract.DataIdentity{}
	for _, link := range old {
		oldByName[link.Name] = link.Identity
	}
	for _, link := range next {
		nextByName[link.Name] = link.Identity
	}

	changes := []dataLinkChange{}
	for _, name := range sortedKeys(oldByName) {
		newIdentity, ok := nextByName[name]
		if !ok {
			continue
		}

		oldIdentity := oldByName[name]
		delete(oldByName, name)
		delete(nextByName, name)
		if oldIdentity == newIdentity {
			continue
		}
		if oldIdentity.Kind != newIdentity.Kind || !isAppDataKind(oldIdentity.Kind) {
			return nil, true
		}
		changes = append(changes, dataLinkChange{old: oldIdentity, new: newIdentity})
	}

	for _, oldName := range sortedKeys(oldByName) {
		for _, newName := range sortedKeys(nextByName) {
			if oldByName[oldName] == nextByName[newName] {
				delete(oldByName, oldName)
				delete(nextByName, newName)
				break
			}
		}
	}

	oldByKind := dataIdentitiesByKind(oldByName)
	nextByKind := dataIdentitiesByKind(nextByName)
	kinds := map[string]bool{}
	for kind := range oldByKind {
		kinds[kind] = true
	}
	for kind := range nextByKind {
		kinds[kind] = true
	}
	for _, kind := range sortedKeys(kinds) {
		oldIdentities := oldByKind[kind]
		newIdentities := nextByKind[kind]
		if len(oldIdentities) == 1 && len(newIdentities) == 1 {
			if !isAppDataKind(kind) {
				return nil, true
			}
			changes = append(changes, dataLinkChange{old: oldIdentities[0], new: newIdentities[0]})
		} else if len(oldIdentities) > 0 && len(newIdentities) > 0 {
			return nil, true
		}
	}
	return changes, false
}

func dataIdentitiesByKind(byName map[string]hostcontract.DataIdentity) map[string][]hostcontract.DataIdentity {
	byKind := map[string][]hostcontract.DataIdentity{}
	for _, name := range sortedKeys(byName) {
		identity := byName[name]
		byKind[identity.Kind] = append(byKind[identity.Kind], identity)
	}
	return byKind
}

func isAppDataKind(kind string) bool {
	return kind == "postgres" || kind == "redis"
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
