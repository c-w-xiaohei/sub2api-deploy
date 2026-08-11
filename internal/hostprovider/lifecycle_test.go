package hostprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/artifact"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/openssh"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

const lifecycleCanary = "CANARY_SECRET_DO_NOT_EXPOSE"

func TestLifecyclePreviewPreservesInputsAndHasNoEffects(t *testing.T) {
	inputs := lifecycleInputs("edge").Set("server", object("sshAlias", property.New("edge").WithDependencies([]urn.URN{"urn:pulumi:stack::project::dep"})))
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
	got, err := h.create(t.Context(), p.CreateRequest{DryRun: true, Properties: inputs})
	if err != nil || got.ID != "" {
		t.Fatal("preview did not return an empty ID without error")
	}
	assertInputsPreserved(t, got.Properties, inputs)
	for _, name := range []string{"machine", "ownership", "appliedRevision", "observation"} {
		if value := valueAt(t, got.Properties, name); !value.HasComputed() {
			t.Fatalf("preview output %s was not computed", name)
		}
	}
	assertNoCalls(t, r)
}

func TestPublicCreateFailsClosedWhenArtifactIsUnavailable(t *testing.T) {
	provider := New("1.0.0")
	configureProvider(t, provider)
	_, err := provider.Create(t.Context(), p.CreateRequest{Properties: lifecycleInputs("edge")})
	if !errors.Is(err, errArtifactUnavailable) {
		t.Fatal("public Create did not return the artifact-unavailable sentinel")
	}
	assertNoCanary(t, errString(err))
}

func TestLifecycleCreateBootstrapsThenInspectsAndCheckpoints(t *testing.T) {
	inputs := lifecycleInputs("edge")
	bundle, binary := lifecycleBundle(t, release(t, inputs))
	r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a"}}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }})
	desired := revision(t, h, inputs)
	prior := baselineRevision(t, h, inputs)
	r.outcomes = []lifecycleOutcome{response(applied(desired)), response(inspected(observation(desired)))}

	got, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
	if err != nil {
		t.Fatal("Create returned an error")
	}
	if got.ID == "" {
		t.Fatal("Create returned an empty ID")
	}
	assertNoCanary(t, got.ID)
	if len(r.calls) != 3 || r.calls[0].kind != "probe" || r.calls[1].command != openssh.BootstrapReceiver || r.calls[2].command != openssh.Host {
		t.Fatal("Create transport sequence was not Probe, BootstrapReceiver, Host")
	}
	if r.calls[0].alias != "edge" || r.calls[1].alias != "edge" || r.calls[2].alias != "edge" {
		t.Fatal("Create transport did not use the configured alias")
	}
	bootstrap := decodeBootstrapRequest(t, r.calls[1].stdin, binary)
	assertReconcile(t, bootstrap, inputs, desired, prior, nil)
	assertInspect(t, r.calls[2], inputs, desired)
	assertCheckpoint(t, got.Properties, inputs, observation(desired), desired)
}

func TestLifecycleCreateIDIsStableAcrossAliasAndTargetChanges(t *testing.T) {
	base := lifecycleInputs("edge-a")
	changed := base.Set("server", object("sshAlias", property.New("edge-b")))
	changedTarget := decodeTarget(t, changed)
	changedTarget.Apps[0].Image = "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	changed = changed.Set("target", encodeValue(t, changedTarget))
	first := createWithReadyHost(t, base)
	second := createWithReadyHost(t, changed)
	if first == "" || first != second {
		t.Fatal("resource ID changed with alias or target")
	}
	for _, changedResource := range []hostcontract.ResourceIdentity{{Environment: "other", ServerKey: "edge"}, {Environment: "prod", ServerKey: "other"}} {
		inputs := base.Set("resource", object("environment", property.New(changedResource.Environment), "serverKey", property.New(changedResource.ServerKey)))
		if createWithReadyHost(t, inputs) == first {
			t.Fatal("resource ID did not distinguish resource identity")
		}
	}
}

func TestLifecycleCreateRejectsInvalidBootstrapAndFinalObservation(t *testing.T) {
	inputs := lifecycleInputs("edge")
	for _, scenario := range []struct {
		name     string
		outcomes []lifecycleOutcome
		calls    int
	}{
		{"bootstrap status", []lifecycleOutcome{response(inspected(observation(revisionForInputs(t, inputs))))}, 2},
		{"final machine", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongMachine(observation(revisionForInputs(t, inputs)))))}, 3},
		{"final owner", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongOwner(observation(revisionForInputs(t, inputs)))))}, 3},
		{"final revision", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongRevision(observation(revisionForInputs(t, inputs)))))}, 3},
		{"final ready", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(notReady(observation(revisionForInputs(t, inputs)))))}, 3},
		{"final drift", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(drifted(observation(revisionForInputs(t, inputs)))))}, 3},
		{"final app image", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongAppImage(observation(revisionForInputs(t, inputs)))))}, 3},
		{"final app coverage", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(missingApps(observation(revisionForInputs(t, inputs)))))}, 3},
		{"final release", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongRelease(observation(revisionForInputs(t, inputs)))))}, 3},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			bundle, _ := lifecycleBundle(t, release(t, inputs))
			r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64"}, outcomes: scenario.outcomes}
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }})
			got, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
			if err == nil || got.ID != "" || got.Properties.Len() != 0 || len(r.calls) != scenario.calls || len(r.outcomes) != 0 {
				t.Fatal("invalid Create response fabricated a checkpoint")
			}
			if scenario.calls == 2 {
				if r.calls[0].kind != "probe" || r.calls[1].command != openssh.BootstrapReceiver {
					t.Fatal("invalid bootstrap response did not stop after Probe and Bootstrap")
				}
			} else {
				if r.calls[0].kind != "probe" || r.calls[1].command != openssh.BootstrapReceiver {
					t.Fatal("invalid final observation did not follow Probe and Bootstrap")
				}
				assertInspect(t, r.calls[2], inputs, revisionForInputs(t, inputs))
			}
			assertNoCanary(t, errString(err))
		})
	}
}

func TestLifecycleCreateArtifactAndConfigurationFailuresPrecedeTransport(t *testing.T) {
	inputs := lifecycleInputs("edge")
	for _, source := range []func() (artifactBundle, error){
		func() (artifactBundle, error) { return artifactBundle{}, errors.New("artifact unavailable") },
		func() (artifactBundle, error) { bundle, _ := lifecycleBundle(t, "other-release"); return bundle, nil },
	} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: source})
		_, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
		if err == nil {
			t.Fatal("artifact failure was accepted")
		}
		assertNoCalls(t, r)
		assertNoCanary(t, errString(err))
	}

	r := &recordingLifecycleTransport{}
	h := newHostWithDependencies("1.0.0", lifecycleDependencies{transport: r, artifact: fatalArtifact(t)})
	_, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
	if err == nil {
		t.Fatal("unconfigured Create was accepted")
	}
	assertNoCalls(t, r)
	assertNoCanary(t, errString(err))
}

func TestLifecycleCreateSelectedArtifactFailureStopsBeforeBootstrap(t *testing.T) {
	inputs := lifecycleInputs("edge")
	for _, mutate := range []func(*artifact.Manifest){
		func(manifest *artifact.Manifest) { manifest.LinuxAMD64.Path = "" },
		func(manifest *artifact.Manifest) { manifest.LinuxAMD64.SHA256 = strings.Repeat("0", 64) },
	} {
		bundle, _ := lifecycleBundle(t, release(t, inputs))
		mutate(&bundle.Manifest)
		r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64"}}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }})
		_, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
		if err == nil || len(r.calls) != 1 || r.calls[0].kind != "probe" || hasWrite(r) {
			t.Fatal("selected artifact failure reached Bootstrap")
		}
		assertNoCanary(t, errString(err))
	}
}

func TestLifecycleCreateGuardsFailBeforeArtifactOrTransport(t *testing.T) {
	for _, inputs := range []property.Map{
		lifecycleInputs("host; unsafe"),
		lifecycleInputs("edge").Set("resource", property.New(property.Computed)),
		lifecycleInputs("edge").Set("server", property.New(property.Computed)),
		lifecycleInputs("edge").Set("target", property.New(property.Computed)),
		lifecycleInputs("edge").Set("secrets", property.New(property.Computed).WithSecret(true)),
	} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
		_, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
		if err == nil {
			t.Fatal("invalid Create input was accepted")
		}
		assertNoCalls(t, r)
		assertNoCanary(t, errString(err))
	}
}

func TestLifecycleUpdateReconcilesWithOldCheckpointRevision(t *testing.T) {
	old, next := lifecycleInputs("edge-old"), lifecycleInputs("edge-new")
	next = rotateSecret(t, next)
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	prior := observation(oldRevision)
	r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), response(applied(desired)), response(inspected(observation(desired)))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, prior, oldRevision), OldInputs: old, Inputs: next})
	if err != nil {
		t.Fatal("Update returned an error")
	}
	if len(r.calls) != 3 {
		t.Fatal("Update did not issue three Host calls")
	}
	assertInspect(t, r.calls[0], next, desired)
	assertReconcile(t, r.calls[1].request, next, desired, oldRevision, nil)
	assertInspect(t, r.calls[2], next, desired)
	assertCheckpoint(t, got.Properties, next, observation(desired), desired)
}

func TestLifecycleUpdateReplaysOldCheckpointKeyAfterResponseLoss(t *testing.T) {
	old, next := lifecycleInputs("edge"), rotateSecret(t, lifecycleInputs("edge"))
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	r.outcomes = []lifecycleOutcome{response(inspected(observation(desired))), response(applied(desired)), response(inspected(observation(desired)))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err != nil || len(r.calls) != 3 || len(r.outcomes) != 0 {
		t.Fatal("response-loss retry did not complete with three Host calls")
	}
	assertInspect(t, r.calls[0], next, desired)
	assertReconcile(t, r.calls[1].request, next, desired, oldRevision, nil)
	assertReconcileFrame(t, r.calls[1].stdin, next, desired, oldRevision)
	assertInspect(t, r.calls[2], next, desired)
	assertCheckpoint(t, got.Properties, next, observation(desired), desired)
}

func TestLifecycleUpdateRequestsOnlyExactSingleDataLinkApproval(t *testing.T) {
	old, next := lifecycleInputs("edge"), lifecycleInputs("edge")
	oldTarget := decodeTarget(t, old)
	oldTarget.Apps[0].DataLinks[0].Identity.Endpoint = "old-db.example"
	old = old.Set("target", encodeValue(t, oldTarget))
	r := &recordingLifecycleTransport{}
	var expected hostcontract.ApprovalSubject
	approvals := 0
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: func(_ context.Context, subject hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) {
		approvals++
		r.events = append(r.events, "approve")
		if !reflect.DeepEqual(subject, expected) {
			t.Fatal("approval subject was not exact")
		}
		return &expected, nil
	}})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	prior := observation(oldRevision)
	expected = hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: "prod", Resource: lifecycleResource(t, old), AppID: "api", DataKind: "postgres", OldData: oldTarget.Apps[0].DataLinks[0].Identity, NewData: decodeTarget(t, next).Apps[0].DataLinks[0].Identity, TargetRevision: desired}
	r.outcomes = []lifecycleOutcome{response(inspected(prior)), response(applied(desired)), response(inspected(observation(desired)))}
	if _, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, prior, oldRevision), OldInputs: old, Inputs: next}); err != nil {
		t.Fatal("approved Update returned an error")
	}
	if approvals != 1 || len(r.events) != 4 || strings.Join(r.events, ",") != "inspect,approve,reconcile,inspect" {
		t.Fatal("approval ordering was not inspect, approve, reconcile, inspect")
	}
	if r.calls[1].request.Approval == nil || !reflect.DeepEqual(*r.calls[1].request.Approval, expected) {
		t.Fatal("reconcile did not include the exact approval")
	}
}

func TestLifecycleUpdateApprovalFailuresAndMultipleChangesDoNotWrite(t *testing.T) {
	old, next := dangerousChange(t)
	for _, approval := range []func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error){
		nil,
		func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { return nil, errors.New("approval unavailable") },
		func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { return nil, nil },
		func(_ context.Context, subject hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { wrong := subject; wrong.NewData.Endpoint = "wrong"; return &wrong, nil },
	} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: approval})
		oldRevision := revision(t, h, old)
		r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision)))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || !onlyInspect(r) {
			t.Fatal("unapproved data-link change wrote or fabricated state")
		}
		assertNoCanary(t, errString(err))
	}

	multipleOld, multipleNext := twoDangerousChanges(t)
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	oldRevision := revision(t, h, multipleOld)
	r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision)))}
	_, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, multipleOld, observation(oldRevision), oldRevision), OldInputs: multipleOld, Inputs: multipleNext})
	if err == nil || !onlyInspect(r) {
		t.Fatal("multiple dangerous changes reached write path")
	}
	assertNoCanary(t, errString(err))
}

func TestLifecycleUpdateRejectsUnsafeInitialObservations(t *testing.T) {
	old, next := lifecycleInputs("edge"), rotateSecret(t, lifecycleInputs("edge"))
	for _, scenario := range []struct { name string; mutate func(hostcontract.StableObservation) hostcontract.StableObservation; outcome lifecycleOutcome }{
		{"machine", wrongMachine, lifecycleOutcome{}},
		{"owner", wrongOwner, lifecycleOutcome{}},
		{"third party revision", wrongRevision, lifecycleOutcome{}},
		{"transport recovery", nil, failure(openssh.ErrTransport)},
		{"remote recovery", nil, failure(openssh.ErrRemote)},
		{"retired", nil, response(retired())},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := &recordingLifecycleTransport{}
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
			oldRevision := revision(t, h, old)
			initial := observation(oldRevision)
			if scenario.mutate != nil { r.outcomes = []lifecycleOutcome{response(scenario.mutate(initial))} } else if scenario.outcome.err != nil || scenario.outcome.response.Result != nil { r.outcomes = []lifecycleOutcome{scenario.outcome} }
			got, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
			if err == nil || got.Properties.Len() != 0 || !onlyInspect(r) {
				t.Fatal("unsafe initial observation reached reconciliation")
			}
			assertNoCanary(t, errString(err))
		})
	}
}

func TestLifecycleUpdateRejectsInvalidFinalObservation(t *testing.T) {
	old, next := lifecycleInputs("edge"), rotateSecret(t, lifecycleInputs("edge"))
	for _, mutate := range []func(hostcontract.StableObservation) hostcontract.StableObservation{wrongMachine, wrongOwner, wrongRelease, wrongRevision, notReady, drifted, wrongAppImage, notReadyApp, wrongAppID, duplicateAppID, missingApps} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), response(applied(desired)), response(inspected(mutate(observation(desired))))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || len(r.calls) != 3 {
			t.Fatal("invalid final observation fabricated a checkpoint")
		}
		assertNoCanary(t, errString(err))
	}
}

func TestLifecycleUpdateRejectsInvalidFinalLocalDataObservation(t *testing.T) {
	old, next := localDataInputs(t, "edge"), rotateSecret(t, localDataInputs(t, "edge"))
	for _, mutate := range []func(hostcontract.StableObservation) hostcontract.StableObservation{wrongLocalData, missingLocalData, duplicateLocalData, notReadyLocalData} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		oldObservation := observationFor(decodeTarget(t, old), oldRevision)
		r.outcomes = []lifecycleOutcome{response(inspected(oldObservation)), response(applied(desired)), response(inspected(mutate(observationFor(decodeTarget(t, next), desired))))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, oldObservation, oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || len(r.calls) != 3 {
			t.Fatal("invalid final local-data observation fabricated a checkpoint")
		}
		assertNoCanary(t, errString(err))
	}
}

func TestLifecycleUpdateOrdinaryChangesAndUnknownInputs(t *testing.T) {
	old := lifecycleInputs("edge")
	for _, next := range []property.Map{old, rotateSecret(t, old), changeImage(t, old), changeHostname(t, old), old.Set("resource", property.New(property.Computed)), old.Set("server", property.New(property.Computed)), old.Set("target", property.New(property.Computed)), old.Set("secrets", property.New(property.Computed).WithSecret(true))} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t), artifact: fatalArtifact(t)})
		oldRevision := revision(t, h, old)
		if hasComputed(next) {
			if _, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next}); err == nil {
				t.Fatal("unknown Update input was accepted")
			} else {
				assertNoCanary(t, errString(err))
			}
			assertNoCalls(t, r)
			continue
		}
		desired := revision(t, h, next)
		r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), response(applied(desired)), response(inspected(observationFor(decodeTarget(t, next), desired)))}
		if _, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next}); err != nil {
			t.Fatal("ordinary Update returned an error")
		}
		if len(r.calls) != 3 || strings.Join(r.events, ",") != "inspect,reconcile,inspect" {
			t.Fatal("ordinary Update did not use inspect, reconcile, inspect")
		}
	}
}

func TestLifecycleUpdateRejectsReleaseUpgradeWithoutWrite(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old)
	nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
	oldRevision := revision(t, h, old)
	r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision)))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: "stable", State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err == nil || got.Properties.Len() != 0 || !strings.Contains(err.Error(), "unsupported upgrade") || hasWrite(r) {
		t.Fatal("release upgrade was not rejected before write")
	}
	assertNoCanary(t, errString(err))
}

func TestLifecycleUpdateDirectGuardsFailBeforeTransport(t *testing.T) {
	old := lifecycleInputs("edge")
	for _, scenario := range []struct { name string; id string; state, oldInputs, inputs property.Map }{
		{"environment", "stable", checkpointFor(t, old), old, old.Set("resource", object("environment", property.New("other"), "serverKey", property.New("edge")))},
		{"server key", "stable", checkpointFor(t, old), old, old.Set("resource", object("environment", property.New("prod"), "serverKey", property.New("other")))},
		{"hostile alias", "stable", checkpointFor(t, old), old, old.Set("server", object("sshAlias", property.New("host; unsafe")))},
		{"missing machine", "stable", checkpointFor(t, old).Delete("machine"), old, old},
		{"missing ownership", "stable", checkpointFor(t, old).Delete("ownership"), old, old},
		{"missing revision", "stable", checkpointFor(t, old).Delete("appliedRevision"), old, old},
		{"missing observation", "stable", checkpointFor(t, old).Delete("observation"), old, old},
		{"malformed machine", "stable", checkpointFor(t, old).Set("machine", property.New("bad")), old, old},
		{"malformed ownership", "stable", checkpointFor(t, old).Set("ownership", property.New("bad")), old, old},
		{"malformed revision", "stable", checkpointFor(t, old).Set("appliedRevision", property.New("bad")), old, old},
		{"malformed observation", "stable", checkpointFor(t, old).Set("observation", property.New("bad")), old, old},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := &recordingLifecycleTransport{}
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
			_, err := h.update(t.Context(), p.UpdateRequest{ID: scenario.id, State: scenario.state, OldInputs: scenario.oldInputs, Inputs: scenario.inputs})
			if err == nil { t.Fatal("invalid Update guard was accepted") }
			assertNoCalls(t, r)
			assertNoCanary(t, errString(err))
		})
	}
}

type lifecycleCall struct { kind, alias string; command openssh.Command; stdin []byte; request hostprotocol.Request; hostAttempted, decoded bool }
type lifecycleOutcome struct { response hostprotocol.Response; err error }
type recordingLifecycleTransport struct { probe artifact.ProbeInfo; probeErr error; outcomes []lifecycleOutcome; calls []lifecycleCall; events []string }
func response(value hostprotocol.Response) lifecycleOutcome { return lifecycleOutcome{response: value} }
func failure(err error) lifecycleOutcome { return lifecycleOutcome{err: err} }
func (r *recordingLifecycleTransport) Probe(_ context.Context, alias string) (artifact.ProbeInfo, error) { r.calls = append(r.calls, lifecycleCall{kind: "probe", alias: alias}); return r.probe, r.probeErr }
func (r *recordingLifecycleTransport) Bootstrap(ctx context.Context, alias string, stdin []byte) (hostprotocol.Response, error) { return r.Run(ctx, alias, openssh.BootstrapReceiver, stdin) }
func (r *recordingLifecycleTransport) Run(_ context.Context, alias string, command openssh.Command, stdin []byte) (hostprotocol.Response, error) {
	call := lifecycleCall{kind: "run", alias: alias, command: command, stdin: append([]byte(nil), stdin...)}
	r.calls = append(r.calls, call)
	if command == openssh.Host { r.calls[len(r.calls)-1].hostAttempted = true; request, err := hostprotocol.DecodeRequest(stdin); if err != nil { return hostprotocol.Response{}, err }; r.calls[len(r.calls)-1].request, r.calls[len(r.calls)-1].decoded = request, true; if request.Action == hostcontract.ActionInspect { r.events = append(r.events, "inspect") } else { r.events = append(r.events, "reconcile") } }
	if len(r.outcomes) == 0 { return hostprotocol.Response{}, errors.New("missing fake outcome") }
	outcome := r.outcomes[0]; r.outcomes = r.outcomes[1:]
	return outcome.response, outcome.err
}

func configuredLifecycleHost(t *testing.T, deps lifecycleDependencies) *host { t.Helper(); h := newHostWithDependencies("1.0.0", deps); configureHost(t, h); return h }
func configureHost(t *testing.T, h *host) { t.Helper(); key := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")); if err := h.configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New(key).WithSecret(true)})}); err != nil { t.Fatal("Configure failed") } }
func configureProvider(t *testing.T, provider p.Provider) { t.Helper(); key := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")); if err := provider.Configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New(key).WithSecret(true)})}); err != nil { t.Fatal("Configure failed") } }
func lifecycleInputs(alias string) property.Map { return property.NewMap(map[string]property.Value{"resource": object("environment", property.New("prod"), "serverKey", property.New("edge")), "server": object("sshAlias", property.New(alias)), "target": object("releaseArtifact", property.New("release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "apps", property.New(property.NewArray([]property.Value{object("id", property.New("api"), "image", property.New("api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "hostname", property.New("api.example"), "readinessPath", property.New("/ready"), "dataLinks", property.New(property.NewArray([]property.Value{object("name", property.New("main"), "identity", object("kind", property.New("postgres"), "providerId", property.New("db-1"), "endpoint", property.New("db.example"), "port", property.New(5432.0), "database", property.New("app"), "tlsServerName", property.New("db.example")))}))}))), "secrets": property.New(property.NewMap(map[string]property.Value{"apps": object("api", object("jwtSecret", property.New(lifecycleCanary)))})).WithSecret(true)}) }
func lifecycleResource(t *testing.T, inputs property.Map) hostcontract.ResourceIdentity { t.Helper(); resource := valueAt(t, inputs, "resource"); return hostcontract.ResourceIdentity{Environment: field(resource, "environment").AsString(), ServerKey: field(resource, "serverKey").AsString()} }
func localDataInputs(t *testing.T, alias string) property.Map { t.Helper(); inputs := lifecycleInputs(alias); target, secrets := decodeTarget(t, inputs), decodeSecrets(t, inputs); target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432, Persistence: true}}; secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: lifecycleCanary}}; return inputs.Set("target", encodeValue(t, target)).Set("secrets", encodeValue(t, secrets).WithSecret(true)) }
func revision(t *testing.T, h *host, inputs property.Map) string { t.Helper(); value, err := hostcontract.TargetRevision(h.key, lifecycleResource(t, inputs), decodeTarget(t, inputs), decodeSecrets(t, inputs)); if err != nil { t.Fatal("revision failed") }; return value }
func revisionForInputs(t *testing.T, inputs property.Map) string { t.Helper(); h := configuredLifecycleHost(t, lifecycleDependencies{}); return revision(t, h, inputs) }
func baselineRevision(t *testing.T, h *host, inputs property.Map) string { t.Helper(); value, err := hostcontract.TargetRevision(h.key, lifecycleResource(t, inputs), hostcontract.Target{ReleaseArtifact: release(t, inputs)}, hostcontract.Secrets{}); if err != nil { t.Fatal("baseline revision failed") }; return value }
func release(t *testing.T, inputs property.Map) string { t.Helper(); return field(valueAt(t, inputs, "target"), "releaseArtifact").AsString() }
func observation(revision string) hostcontract.StableObservation { return observationFor(hostcontract.Target{ReleaseArtifact: "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}, revision) }
func observationFor(target hostcontract.Target, revision string) hostcontract.StableObservation { observation := hostcontract.StableObservation{Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, HostRelease: target.ReleaseArtifact, AppliedRevision: revision, Ready: true}; for _, app := range target.Apps { observation.Apps = append(observation.Apps, hostcontract.AppObservation{ID: app.ID, ActiveImage: app.Image, Ready: true}) }; for _, service := range target.DataServices { observation.Data = append(observation.Data, hostcontract.DataObservation{Identity: localDataIdentity(service), Ready: true}) }; return observation }
func applied(revision string) hostprotocol.Response { return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: revision}} }
func inspected(value hostcontract.StableObservation) hostprotocol.Response { return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultInspected, Observation: &value}} }
func retired() hostprotocol.Response { return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: &hostcontract.OwnershipIdentity{Value: "owner-a"}, Retirement: &hostprotocol.RetirementEvidence{PreserveData: true}}} }
func wrongMachine(value hostcontract.StableObservation) hostcontract.StableObservation { value.Machine.Value = "machine-b"; return value }
func wrongOwner(value hostcontract.StableObservation) hostcontract.StableObservation { value.Ownership.Value = "owner-b"; return value }
func wrongRelease(value hostcontract.StableObservation) hostcontract.StableObservation { value.HostRelease = "other"; return value }
func wrongRevision(value hostcontract.StableObservation) hostcontract.StableObservation { value.AppliedRevision = "tr1:0000000000000000:0000000000000000000000000000000000000000000000000000000000000000"; return value }
func notReady(value hostcontract.StableObservation) hostcontract.StableObservation { value.Ready = false; return value }
func drifted(value hostcontract.StableObservation) hostcontract.StableObservation { value.Drifted = true; return value }
func wrongAppImage(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps[0].ActiveImage = "wrong"; return value }
func notReadyApp(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps[0].Ready = false; return value }
func wrongAppID(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps[0].ID = "other"; return value }
func duplicateAppID(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps = append(value.Apps, value.Apps[0]); return value }
func missingApps(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps = nil; return value }
func wrongLocalData(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Identity.Port++; return value }
func missingLocalData(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data = nil; return value }
func duplicateLocalData(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data = append(value.Data, value.Data[0]); return value }
func notReadyLocalData(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Ready = false; return value }
func rotateSecret(t *testing.T, inputs property.Map) property.Map { secrets := decodeSecrets(t, inputs); secrets.Apps["api"] = hostcontract.AppSecrets{JWTSecret: "rotated-" + lifecycleCanary}; return inputs.Set("secrets", encodeValue(t, secrets).WithSecret(true)) }
func changeImage(t *testing.T, inputs property.Map) property.Map { target := decodeTarget(t, inputs); target.Apps[0].Image = "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"; return inputs.Set("target", encodeValue(t, target)) }
func changeHostname(t *testing.T, inputs property.Map) property.Map { target := decodeTarget(t, inputs); target.Apps[0].Hostname = "other.example"; return inputs.Set("target", encodeValue(t, target)) }
func dangerousChange(t *testing.T) (property.Map, property.Map) { old, next := lifecycleInputs("edge"), lifecycleInputs("edge"); target := decodeTarget(t, old); target.Apps[0].DataLinks[0].Identity.Endpoint = "old-db.example"; return old.Set("target", encodeValue(t, target)), next }
func twoDangerousChanges(t *testing.T) (property.Map, property.Map) { old, next := lifecycleInputs("edge"), lifecycleInputs("edge"); oldTarget, nextTarget := decodeTarget(t, old), decodeTarget(t, next); secondOld := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old-two", Endpoint: "old-two.example", Port: 5432, Database: "two", TLSServerName: "old-two.example"}; secondNew := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new-two", Endpoint: "new-two.example", Port: 5432, Database: "two", TLSServerName: "new-two.example"}; oldTarget.Apps[0].DataLinks = append(oldTarget.Apps[0].DataLinks, hostcontract.DataLink{Name: "second", Identity: secondOld}); nextTarget.Apps[0].DataLinks = append(nextTarget.Apps[0].DataLinks, hostcontract.DataLink{Name: "second", Identity: secondNew}); oldTarget.Apps[0].DataLinks[0].Identity.Endpoint = "old-one.example"; nextTarget.Apps[0].DataLinks[0].Identity.Endpoint = "new-one.example"; return old.Set("target", encodeValue(t, oldTarget)), next.Set("target", encodeValue(t, nextTarget)) }
func localDataIdentity(service hostcontract.LocalDataServiceTarget) hostcontract.DataIdentity { database, tls := "postgres", ""; if service.Type == "redis" { database = "0" } else { tls = service.ID }; return hostcontract.DataIdentity{Kind: service.Type, ProviderID: service.ID, Endpoint: service.ID, Port: service.Port, Database: database, TLSServerName: tls} }
func lifecycleBundle(t *testing.T, release string) (artifactBundle, []byte) { t.Helper(); root := t.TempDir(); amd64, arm64 := []byte("pinned-host-amd64"), []byte("pinned-host-arm64"); write := func(name string, contents []byte) { if err := os.WriteFile(filepath.Join(root, name), contents, 0o600); err != nil { t.Fatal("artifact fixture write failed") } }; write("host-amd64", amd64); write("host-arm64", arm64); sum := func(value []byte) string { hash := sha256.Sum256(value); return fmt.Sprintf("%x", hash) }; return artifactBundle{Root: root, Manifest: artifact.Manifest{SchemaVersion: 1, Release: release, LinuxAMD64: artifact.Entry{Path: "host-amd64", Size: int64(len(amd64)), SHA256: sum(amd64)}, LinuxARM64: artifact.Entry{Path: "host-arm64", Size: int64(len(arm64)), SHA256: sum(arm64)}}}, amd64 }
func decodeBootstrapRequest(t *testing.T, stdin, binary []byte) hostprotocol.Request { t.Helper(); hash := sha256.Sum256(binary); prefix := []byte(fmt.Sprintf("s2a1:%d:%x\n", len(binary), hash)); if len(stdin) < len(prefix)+len(binary) || !bytes.Equal(stdin[:len(prefix)], prefix) || !bytes.Equal(stdin[len(prefix):len(prefix)+len(binary)], binary) { t.Fatal("bootstrap input did not contain the pinned artifact") }; request, err := hostprotocol.DecodeRequest(stdin[len(prefix)+len(binary):]); if err != nil { t.Fatal("bootstrap did not contain one valid request frame") }; return request }
func assertInspect(t *testing.T, call lifecycleCall, inputs property.Map, desired string) { t.Helper(); alias := field(valueAt(t, inputs, "server"), "sshAlias").AsString(); request := call.request; if call.command != openssh.Host || call.alias != alias || request.Action != hostcontract.ActionInspect || request.Server.SSHAlias != alias || request.Resource != lifecycleResource(t, inputs) || request.TargetRevision != desired || request.Target != nil || request.Secrets != nil || request.Approval != nil || request.PriorAppliedRevision != "" || request.PriorObservation != "" { t.Fatal("Inspect request contract was not exact") } }
func assertReconcile(t *testing.T, request hostprotocol.Request, inputs property.Map, desired, prior string, approval *hostcontract.ApprovalSubject) { t.Helper(); alias := field(valueAt(t, inputs, "server"), "sshAlias").AsString(); if request.Action != hostcontract.ActionReconcile || request.Server.SSHAlias != alias || request.Resource != lifecycleResource(t, inputs) || request.TargetRevision != desired || request.PriorAppliedRevision != prior || request.PriorObservation != "" || !reflect.DeepEqual(request.Approval, approval) || request.Target == nil || request.Secrets == nil || !reflect.DeepEqual(*request.Target, decodeTarget(t, inputs)) || !reflect.DeepEqual(*request.Secrets, decodeSecrets(t, inputs)) { t.Fatal("Reconcile request contract was not exact") } }
func assertReconcileFrame(t *testing.T, actual []byte, inputs property.Map, desired, prior string) { t.Helper(); target, secrets := decodeTarget(t, inputs), decodeSecrets(t, inputs); expected, err := hostprotocol.EncodeRequest(hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: field(valueAt(t, inputs, "server"), "sshAlias").AsString()}, Resource: lifecycleResource(t, inputs), TargetRevision: desired, PriorAppliedRevision: prior, Target: &target, Secrets: &secrets}); if err != nil || !bytes.Equal(actual, expected) { t.Fatal("response-loss Reconcile frame was not byte-equivalent") } }
func checkpoint(t *testing.T, inputs property.Map, value hostcontract.StableObservation, revision string) property.Map { t.Helper(); return inputs.Set("machine", encodeValue(t, value.Machine)).Set("ownership", encodeValue(t, value.Ownership)).Set("appliedRevision", property.New(revision)).Set("observation", encodeValue(t, value)) }
func checkpointFor(t *testing.T, inputs property.Map) property.Map { t.Helper(); h := configuredLifecycleHost(t, lifecycleDependencies{}); revision := revision(t, h, inputs); return checkpoint(t, inputs, observationFor(decodeTarget(t, inputs), revision), revision) }
func assertCheckpoint(t *testing.T, state, inputs property.Map, value hostcontract.StableObservation, revision string) { t.Helper(); assertInputsPreserved(t, state, inputs); if !valueAt(t, state, "secrets").Secret() || !valueAt(t, state, "machine").Equals(encodeValue(t, value.Machine)) || !valueAt(t, state, "ownership").Equals(encodeValue(t, value.Ownership)) || !valueAt(t, state, "appliedRevision").Equals(property.New(revision)) || !valueAt(t, state, "observation").Equals(encodeValue(t, value)) { t.Fatal("checkpoint ordinary outputs were not exact") }; assertNoCanary(t, fmt.Sprint(valueAt(t, state, "machine")), fmt.Sprint(valueAt(t, state, "ownership")), fmt.Sprint(valueAt(t, state, "appliedRevision")), fmt.Sprint(valueAt(t, state, "observation"))) }
func assertInputsPreserved(t *testing.T, state, inputs property.Map) { t.Helper(); for _, name := range []string{"resource", "server", "target", "secrets"} { if !valueAt(t, state, name).Equals(valueAt(t, inputs, name)) { t.Fatalf("checkpoint changed input %s", name) } } }
func valueAt(t *testing.T, values property.Map, key string) property.Value { t.Helper(); value, ok := values.GetOk(key); if !ok { t.Fatalf("missing property %s", key) }; return value }
func decodeTarget(t *testing.T, values property.Map) hostcontract.Target { t.Helper(); var target hostcontract.Target; if err := decode(valueAt(t, values, "target"), &target); err != nil { t.Fatal("target fixture decode failed") }; return target }
func decodeSecrets(t *testing.T, values property.Map) hostcontract.Secrets { t.Helper(); var secrets hostcontract.Secrets; if err := decode(valueAt(t, values, "secrets"), &secrets); err != nil { t.Fatal("secret fixture decode failed") }; return secrets }
func encodeValue(t *testing.T, value any) property.Value { t.Helper(); encoded, err := json.Marshal(value); if err != nil { t.Fatal("fixture encode failed") }; var raw any; if err := json.Unmarshal(encoded, &raw); err != nil { t.Fatal("fixture decode failed") }; return propertyFromRaw(t, raw) }
func propertyFromRaw(t *testing.T, raw any) property.Value { t.Helper(); switch value := raw.(type) { case nil: return property.New(property.Null); case string: return property.New(value); case bool: return property.New(value); case float64: return property.New(value); case []any: values := make([]property.Value, len(value)); for i := range value { values[i] = propertyFromRaw(t, value[i]) }; return property.New(property.NewArray(values)); case map[string]any: values := map[string]property.Value{}; for key, nested := range value { values[key] = propertyFromRaw(t, nested) }; return property.New(property.NewMap(values)); default: t.Fatal("unsupported fixture value"); return property.New(property.Null) } }
func createWithReadyHost(t *testing.T, inputs property.Map) string { t.Helper(); bundle, _ := lifecycleBundle(t, release(t, inputs)); r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64"}}; h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }}); desired := revision(t, h, inputs); r.outcomes = []lifecycleOutcome{response(applied(desired)), response(inspected(observationFor(decodeTarget(t, inputs), desired)))}; got, err := h.create(t.Context(), p.CreateRequest{Properties: inputs}); if err != nil { t.Fatal("Create failed") }; return got.ID }
func fatalArtifact(t *testing.T) func() (artifactBundle, error) { return func() (artifactBundle, error) { t.Fatal("artifact source was called"); return artifactBundle{}, nil } }
func fatalApproval(t *testing.T) func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { return func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { t.Fatal("approval source was called"); return nil, nil } }
func assertNoCalls(t *testing.T, r *recordingLifecycleTransport) { t.Helper(); if len(r.calls) != 0 { t.Fatal("transport was called") } }
func onlyInspect(r *recordingLifecycleTransport) bool { return len(r.calls) == 1 && r.calls[0].command == openssh.Host && r.calls[0].decoded && r.calls[0].request.Action == hostcontract.ActionInspect }
func hasWrite(r *recordingLifecycleTransport) bool { for _, call := range r.calls { if call.command == openssh.BootstrapReceiver || call.hostAttempted && (!call.decoded || call.request.Action != hostcontract.ActionInspect) { return true } }; return false }
func errString(err error) string { if err == nil { return "" }; return err.Error() }
func assertNoCanary(t *testing.T, values ...string) { t.Helper(); for _, value := range values { if strings.Contains(value, lifecycleCanary) { t.Fatal("secret canary leaked") } } }
