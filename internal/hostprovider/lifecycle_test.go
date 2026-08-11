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

func TestReleaseBundleLocatorLoadsOnlyTheProviderSiblingHostArtifacts(t *testing.T) {
	bundleRoot := t.TempDir()
	providerPath := filepath.Join(bundleRoot, "bin", "pulumi-resource-sub2api-host")
	if err := os.MkdirAll(filepath.Dir(providerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(providerPath, []byte("provider"), 0o700); err != nil {
		t.Fatal(err)
	}
	want, _ := releaseBundleHostArtifacts(t, bundleRoot, "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	got, err := loadReleaseBundle(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != filepath.Join(bundleRoot, "artifacts", "sub2api-host") || !reflect.DeepEqual(got.Manifest, want.Manifest) {
		t.Fatalf("locator selected %#v, want exact released Host artifacts %#v", got, want)
	}
}

func TestNewHostAtExecutableWiresOnlyItsReleaseRelativeHostArtifacts(t *testing.T) {
	bundleRoot := t.TempDir()
	providerPath := releaseBundleProvider(t, bundleRoot)
	want, _ := releaseBundleHostArtifacts(t, bundleRoot, "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	h := newHostAtExecutable("1.0.0", providerPath)
	got, err := h.deps.artifact()
	if err != nil || got.Root != want.Root || !reflect.DeepEqual(got.Manifest, want.Manifest) {
		t.Fatalf("constructor artifact wiring = %#v, %v; want release-relative bundle %#v", got, err, want)
	}
}

func TestNewHostAtExecutableWithApprovalWiresExactCallbackAndKeepsReleaseArtifactLookup(t *testing.T) {
	bundleRoot := t.TempDir()
	providerPath := releaseBundleProvider(t, bundleRoot)
	want, _ := releaseBundleHostArtifacts(t, bundleRoot, "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	subject := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: "prod", Resource: hostcontract.ResourceIdentity{Environment: "prod", ServerKey: "edge"}, Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PreserveData: true}
	called := false
	approve := func(_ context.Context, got hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) {
		called = true
		if got != subject {
			t.Fatalf("approval subject = %#v, want %#v", got, subject)
		}
		return &subject, nil
	}

	h := newHostAtExecutableWithApproval("1.0.0", providerPath, approve)
	got, err := h.deps.approve(t.Context(), subject)
	if err != nil || got == nil || *got != subject || !called {
		t.Fatalf("wired approval = %#v, %v, called=%t", got, err, called)
	}
	artifact, err := h.deps.artifact()
	if err != nil || artifact.Root != want.Root || !reflect.DeepEqual(artifact.Manifest, want.Manifest) {
		t.Fatalf("approval constructor changed release-relative artifact lookup: %#v, %v", artifact, err)
	}
}

func TestPublicNewUsesResolvedProviderExecutableForReleaseBundle(t *testing.T) {
	bundleRoot := t.TempDir()
	providerPath := releaseBundleProvider(t, bundleRoot)
	releaseBundleHostArtifacts(t, bundleRoot, "other-release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	original := providerExecutable
	providerExecutable = func() (string, error) { return providerPath, nil }
	t.Cleanup(func() { providerExecutable = original })

	provider := New("1.0.0")
	configureProvider(t, provider)
	_, err := provider.Create(t.Context(), p.CreateRequest{Properties: lifecycleInputs("edge")})
	if err == nil || errors.Is(err, errArtifactUnavailable) || !strings.Contains(err.Error(), "does not match target release") {
		t.Fatalf("public New did not use the executable-relative release bundle: %v", err)
	}
}

func TestReleaseBundleLocatorFailsClosedForMissingMalformedAndSymlinkedArtifacts(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		setup func(t *testing.T, bundleRoot string)
	}{
		{"missing", func(t *testing.T, _ string) {}},
		{"malformed manifest", func(t *testing.T, bundleRoot string) {
			root := filepath.Join(bundleRoot, "artifacts", "sub2api-host")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte("not-json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked ancestor", func(t *testing.T, bundleRoot string) {
			real := t.TempDir()
			releaseBundleHostArtifacts(t, real, "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			if err := os.MkdirAll(filepath.Join(bundleRoot, "artifacts"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(real, "artifacts", "sub2api-host"), filepath.Join(bundleRoot, "artifacts", "sub2api-host")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked manifest", func(t *testing.T, bundleRoot string) {
			root := filepath.Join(bundleRoot, "artifacts", "sub2api-host")
			releaseBundleHostArtifacts(t, bundleRoot, "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			real := t.TempDir()
			realBundle, _ := releaseBundleHostArtifacts(t, real, "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			if err := os.Remove(filepath.Join(root, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(realBundle.Root, "manifest.json"), filepath.Join(root, "manifest.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			bundleRoot := t.TempDir()
			providerPath := releaseBundleProvider(t, bundleRoot)
			cwd := t.TempDir()
			releaseBundleProvider(t, cwd)
			releaseBundleHostArtifacts(t, cwd, "decoy-cwd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			pathDir := t.TempDir()
			releaseBundleProvider(t, pathDir)
			releaseBundleHostArtifacts(t, pathDir, "decoy-path@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			originalCWD, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			originalPath := os.Getenv("PATH")
			if err := os.Chdir(cwd); err != nil {
				t.Fatal(err)
			}
			if err := os.Setenv("PATH", filepath.Join(pathDir, "bin")+string(os.PathListSeparator)+originalPath); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(originalCWD); _ = os.Setenv("PATH", originalPath) })
			scenario.setup(t, bundleRoot)
			if _, err := loadReleaseBundle(providerPath); err == nil {
				t.Fatal("unsafe or absent released Host artifacts were accepted")
			}
			if h := newHostAtExecutable("1.0.0", providerPath); h.deps.artifact == nil {
				t.Fatal("constructor did not retain release-relative artifact source")
			} else if _, err := h.deps.artifact(); err == nil {
				t.Fatal("constructor used cwd or PATH artifact fallback")
			}
		})
	}
}

func TestLifecycleCreateBootstrapsThenInspectsAndCheckpoints(t *testing.T) {
	inputs := lifecycleInputs("edge")
	bundle, binary := lifecycleBundle(t, release(t, inputs))
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, probes: []artifact.ProbeInfo{{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}}}
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
	if len(r.calls) != 4 || r.calls[0].kind != "probe" || r.calls[1].command != openssh.BootstrapReceiver || r.calls[2].kind != "probe" || r.calls[3].command != openssh.Host {
		t.Fatal("Create transport sequence was Probe(old), BootstrapReceiver, Probe(new), Host")
	}
	if r.calls[0].alias != "edge" || r.calls[1].alias != "edge" || r.calls[2].alias != "edge" || r.calls[3].alias != "edge" {
		t.Fatal("Create transport did not use the configured alias")
	}
	bootstrap := decodeBootstrapRequest(t, r.calls[1].stdin, binary)
	assertReconcile(t, bootstrap, inputs, desired, prior, nil)
	assertInspect(t, r.calls[3], inputs, desired)
	assertCheckpoint(t, got.Properties, inputs, observation(desired), desired)
}

func TestLifecycleCreateRejectsBadPostBootstrapInstalledDigest(t *testing.T) {
	inputs := lifecycleInputs("edge")
	bundle, binary := lifecycleBundle(t, release(t, inputs))
	for _, digest := range []string{"missing", strings.Repeat("0", 64)} {
		r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, probes: []artifact.ProbeInfo{{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}}}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }})
		desired, prior := revision(t, h, inputs), baselineRevision(t, h, inputs)
		r.outcomes = []lifecycleOutcome{response(applied(desired))}
		got, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
		if err == nil || got.ID != "" || got.Properties.Len() != 0 || len(r.calls) != 3 || r.calls[0].kind != "probe" || r.calls[1].command != openssh.BootstrapReceiver || r.calls[2].kind != "probe" || len(r.outcomes) != 0 {
			t.Fatalf("bad post-bootstrap digest checkpointed or inspected final: %#v, %v, %#v", got, err, r.calls)
		}
		assertReconcile(t, decodeBootstrapRequest(t, r.calls[1].stdin, binary), inputs, desired, prior, nil)
		assertNoCanary(t, errString(err))
	}
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
		{"final machine", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongMachine(observation(revisionForInputs(t, inputs)))))}, 4},
		{"final empty owner", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(emptyOwner(observation(revisionForInputs(t, inputs)))))}, 4},
		{"final revision", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongRevision(observation(revisionForInputs(t, inputs)))))}, 4},
		{"final ready", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(notReady(observation(revisionForInputs(t, inputs)))))}, 4},
		{"final drift", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(drifted(observation(revisionForInputs(t, inputs)))))}, 4},
		{"final app image", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongAppImage(observation(revisionForInputs(t, inputs)))))}, 4},
		{"final app coverage", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(missingApps(observation(revisionForInputs(t, inputs)))))}, 4},
		{"final release", []lifecycleOutcome{response(applied(revisionForInputs(t, inputs))), response(inspected(wrongRelease(observation(revisionForInputs(t, inputs)))))}, 4},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			bundle, binary := lifecycleBundle(t, release(t, inputs))
			digest := fmt.Sprintf("%x", sha256.Sum256(binary))
			r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, outcomes: scenario.outcomes}
			if scenario.calls == 4 { r.probes = []artifact.ProbeInfo{r.probe, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}} }
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
				if r.calls[0].kind != "probe" || r.calls[1].command != openssh.BootstrapReceiver || r.calls[2].kind != "probe" {
					t.Fatal("invalid final observation did not follow post-install Probe")
				}
				assertInspect(t, r.calls[3], inputs, revisionForInputs(t, inputs))
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
		r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a"}}
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

func TestLifecycleCreateRejectsUnsecretTopLevelSecretsBeforeArtifactOrTransport(t *testing.T) {
	valid := lifecycleInputs("edge")
	inputs := valid.Set("secrets", encodeValue(t, decodeSecrets(t, valid)))
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
	got, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
	if err == nil || got.ID != "" || got.Properties.Len() != 0 {
		t.Fatal("Create accepted unsecret top-level secrets")
	}
	assertNoCalls(t, r)
	assertNoCanary(t, errString(err))
}

func TestLifecycleUpdateReconcilesWithOldCheckpointRevision(t *testing.T) {
	old, next := lifecycleInputs("edge-old"), lifecycleInputs("edge-new")
	next = rotateSecret(t, next)
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	prior := observation(oldRevision)
	r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), response(applied(desired)), response(inspected(observation(desired)))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, prior, oldRevision), OldInputs: old, Inputs: next})
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

func TestLifecycleUpdateResponseLossWithTerminalNextReleaseReplaysPinnedBootstrapUntilDigestVerified(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old)
	nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	bundle, binary := lifecycleBundle(t, nextTarget.ReleaseArtifact)
	oldDigest := strings.Repeat("0", 64)
	newDigest := fmt.Sprintf("%x", sha256.Sum256(binary))
	r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: oldDigest}}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	nextObservation := observationFor(nextTarget, desired)
	r.outcomes = []lifecycleOutcome{response(inspected(nextObservation)), response(applied(desired)), response(inspected(nextObservation))}
	r.probes = []artifact.ProbeInfo{r.probe, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: newDigest}}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err != nil || len(r.calls) != 5 || len(r.outcomes) != 0 {
		t.Fatalf("response-loss recovery = %#v, calls=%#v err=%v", got, r.calls, err)
	}
	assertInspect(t, r.calls[0], next, desired)
	if r.calls[1].kind != "probe" || r.calls[2].command != openssh.BootstrapReceiver || r.calls[3].kind != "probe" || r.calls[4].command != openssh.Host {
		t.Fatalf("release recovery sequence = %#v, want inspect, probe(old), bootstrap, probe(new), inspect", r.calls)
	}
	bootstrap := decodeBootstrapRequest(t, r.calls[2].stdin, binary)
	assertReconcile(t, bootstrap, next, desired, oldRevision, nil)
	assertInspect(t, r.calls[4], next, desired)
	if newDigest == oldDigest { t.Fatal("digest fixture is not distinct") }
	assertCheckpoint(t, got.Properties, next, nextObservation, desired)
}

func TestLifecycleUpdateTerminalNextReleaseWithPinnedInstalledDigestCheckpointsWithoutReplay(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old); nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	bundle, binary := lifecycleBundle(t, nextTarget.ReleaseArtifact)
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	final := observationFor(nextTarget, desired)
	r.outcomes = []lifecycleOutcome{response(inspected(final))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err != nil || len(r.calls) != 2 || r.calls[0].request.Action != hostcontract.ActionInspect || r.calls[1].kind != "probe" || hasWrite(r) { t.Fatalf("terminal pinned digest shortcut = %#v, %v, %#v", got, err, r.calls) }
	assertCheckpoint(t, got.Properties, next, final, desired)
}

func TestLifecycleUpdateTerminalNextReleaseWithMissingInstalledDigestReplaysPinnedBootstrap(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old); nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	bundle, binary := lifecycleBundle(t, nextTarget.ReleaseArtifact)
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, probes: []artifact.ProbeInfo{{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}}}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	final := observationFor(nextTarget, desired)
	r.outcomes = []lifecycleOutcome{response(inspected(final)), response(applied(desired)), response(inspected(final))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err != nil || len(r.calls) != 5 || r.calls[1].kind != "probe" || r.calls[2].command != openssh.BootstrapReceiver || r.calls[3].kind != "probe" || r.calls[4].command != openssh.Host { t.Fatalf("missing digest terminal recovery = %#v, %v, %#v", got, err, r.calls) }
	assertReconcile(t, decodeBootstrapRequest(t, r.calls[2].stdin, binary), next, desired, oldRevision, nil)
	assertCheckpoint(t, got.Properties, next, final, desired)
}

func TestLifecycleUpdateRejectsWrongPostBootstrapInstalledDigestBeforeFinalInspect(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old); nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	bundle, _ := lifecycleBundle(t, nextTarget.ReleaseArtifact)
	for _, digest := range []string{"missing", strings.Repeat("0", 64)} {
		r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, probes: []artifact.ProbeInfo{{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}}}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }, approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), response(applied(desired))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || len(r.calls) != 4 || r.calls[3].kind != "probe" || len(r.outcomes) != 0 { t.Fatalf("wrong post-bootstrap digest checkpointed or inspected final: %#v, %v, %#v", got, err, r.calls) }
		_ = desired
	}
}

func TestLifecycleUpdateTerminalShortcutValidatesCompleteNextObservation(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old)
	nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	for _, mutate := range []func(hostcontract.StableObservation) hostcontract.StableObservation{wrongMachine, wrongOwner, wrongRelease, wrongRevision, notReady, drifted, wrongAppImage, notReadyApp, wrongAppID, duplicateAppID, missingApps} {
		r := &recordingLifecycleTransport{}
		artifactCalls := 0
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { artifactCalls++; return artifactBundle{}, errors.New("must not load") }, approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		r.outcomes = []lifecycleOutcome{response(inspected(mutate(observationFor(nextTarget, desired))))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || !onlyInspect(r) || artifactCalls != 0 || hasWrite(r) {
			t.Fatal("invalid terminal release observation returned state or repeated effects")
		}
	}

	old = localDataInputs(t, "edge")
	nextTarget = decodeTarget(t, old)
	nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next = old.Set("target", encodeValue(t, nextTarget))
	for _, mutate := range []func(hostcontract.StableObservation) hostcontract.StableObservation{emptyLocalDataIdentity, wrongLocalDataKind, wrongLocalDataPort, wrongLocalDataDatabase, wrongLocalDataTLS, mismatchedLocalDataProviderAndEndpoint, notReadyLocalData, missingLocalData, duplicateLocalData} {
		r := &recordingLifecycleTransport{}
		artifactCalls := 0
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { artifactCalls++; return artifactBundle{}, errors.New("must not load") }, approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		r.outcomes = []lifecycleOutcome{response(inspected(mutate(observationFor(nextTarget, desired))))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observationFor(decodeTarget(t, old), oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || !onlyInspect(r) || artifactCalls != 0 || hasWrite(r) {
			t.Fatal("invalid terminal local-data observation returned state or repeated effects")
		}
	}
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
	if _, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, prior, oldRevision), OldInputs: old, Inputs: next}); err != nil {
		t.Fatal("approved Update returned an error")
	}
	if approvals != 1 || len(r.events) != 4 || strings.Join(r.events, ",") != "inspect,approve,reconcile,inspect" {
		t.Fatal("approval ordering was not inspect, approve, reconcile, inspect")
	}
	if r.calls[1].request.Approval == nil || !reflect.DeepEqual(*r.calls[1].request.Approval, expected) {
		t.Fatal("reconcile did not include the exact approval")
	}
}

func TestLifecycleUpdateDangerousTerminalRequiresExactCompleteEvidence(t *testing.T) {
	old, next := dangerousChange(t)
	for _, mutate := range []func(*hostprotocol.OperationEvidence){nil, func(e *hostprotocol.OperationEvidence) { e.Key.PriorAppliedRevision = mismatchedRevision() }, func(e *hostprotocol.OperationEvidence) { e.Status = hostprotocol.OperationPending }, func(e *hostprotocol.OperationEvidence) { e.Approval.NewData.Endpoint = "wrong" }} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		approval := dangerousApprovalFixture(t, old, next, desired)
		var evidence *hostprotocol.OperationEvidence
		if mutate != nil { evidence = &hostprotocol.OperationEvidence{Key: hostcontract.OperationKey{Resource: lifecycleResource(t, next), Action: hostcontract.ActionReconcile, TargetRevision: desired, PriorAppliedRevision: oldRevision}, Status: hostprotocol.OperationComplete, Approval: &approval}; mutate(evidence) }
		r.outcomes = []lifecycleOutcome{response(inspectedEvidence(observationFor(decodeTarget(t, next), desired), evidence))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || !onlyInspect(r) || hasWrite(r) { t.Fatal("unproven dangerous terminal state was checkpointed") }
	}
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	approval := dangerousApprovalFixture(t, old, next, desired)
	evidence := &hostprotocol.OperationEvidence{Key: hostcontract.OperationKey{Resource: lifecycleResource(t, next), Action: hostcontract.ActionReconcile, TargetRevision: desired, PriorAppliedRevision: oldRevision}, Status: hostprotocol.OperationComplete, Approval: &approval}
	nextObservation := observationFor(decodeTarget(t, next), desired)
	r.outcomes = []lifecycleOutcome{response(inspectedEvidence(nextObservation, evidence))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err != nil || !onlyInspect(r) { t.Fatalf("exact dangerous completion = %#v, %v", got, err) }
	assertCheckpoint(t, got.Properties, next, nextObservation, desired)
}

func TestLifecycleUpdateDangerousPendingEvidenceResumesWithoutApprovalReplay(t *testing.T) {
	old, next := dangerousChange(t)
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	approval := dangerousApprovalFixture(t, old, next, desired)
	evidence := &hostprotocol.OperationEvidence{Key: hostcontract.OperationKey{Resource: lifecycleResource(t, next), Action: hostcontract.ActionReconcile, TargetRevision: desired, PriorAppliedRevision: oldRevision}, Status: hostprotocol.OperationPending, Approval: &approval}
	nextObservation := observationFor(decodeTarget(t, next), desired)
	r.outcomes = []lifecycleOutcome{response(inspectedEvidence(pendingObservation(oldRevision), evidence)), response(applied(desired)), response(inspected(nextObservation))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err != nil || len(r.calls) != 3 || r.calls[1].request.Approval != nil { t.Fatalf("dangerous pending resume = %#v, %v, %#v", got, err, r.calls) }
	assertReconcile(t, r.calls[1].request, next, desired, oldRevision, nil)
	assertCheckpoint(t, got.Properties, next, nextObservation, desired)
}

func TestLifecycleUpdateDangerousMismatchedPendingEvidenceFailsClosed(t *testing.T) {
	old, next := dangerousChange(t)
	r := &recordingLifecycleTransport{}
	approvals := 0
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: func(_ context.Context, subject hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { approvals++; return &subject, nil }})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	approval := dangerousApprovalFixture(t, old, next, desired)
	wrong := &hostprotocol.OperationEvidence{Key: hostcontract.OperationKey{Resource: lifecycleResource(t, next), Action: hostcontract.ActionReconcile, TargetRevision: desired, PriorAppliedRevision: mismatchedRevision()}, Status: hostprotocol.OperationPending, Approval: &approval}
	r.outcomes = []lifecycleOutcome{response(inspectedEvidence(observation(oldRevision), wrong))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err == nil || got.Properties.Len() != 0 || approvals != 0 || !onlyInspect(r) || hasWrite(r) {
		t.Fatalf("mismatched pending evidence was accepted: %v, approvals=%d, calls=%#v", err, approvals, r.calls)
	}
}

func TestLifecycleUpdateRequestsApprovalForRenamedExactSingleDataLink(t *testing.T) {
	old, next := lifecycleInputs("edge"), lifecycleInputs("edge")
	oldTarget, nextTarget := decodeTarget(t, old), decodeTarget(t, next)
	oldIdentity := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old-db", Endpoint: "old-db.example", Port: 5432, Database: "app", TLSServerName: "old-db.example"}
	newIdentity := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new-db", Endpoint: "new-db.example", Port: 5432, Database: "app", TLSServerName: "new-db.example"}
	oldTarget.Apps[0].DataLinks[0] = hostcontract.DataLink{Name: "old-main", Identity: oldIdentity}
	nextTarget.Apps[0].DataLinks[0] = hostcontract.DataLink{Name: "new-main", Identity: newIdentity}
	old, next = old.Set("target", encodeValue(t, oldTarget)), next.Set("target", encodeValue(t, nextTarget))
	r := &recordingLifecycleTransport{}
	var expected hostcontract.ApprovalSubject
	approvals := 0
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: func(_ context.Context, subject hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) {
		approvals++
		r.events = append(r.events, "approve")
		if !reflect.DeepEqual(subject, expected) { t.Fatal("renamed link approval subject was not exact") }
		return &expected, nil
	}})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	oldObservation := observationFor(oldTarget, oldRevision)
	expected = hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: "prod", Resource: lifecycleResource(t, old), AppID: "api", DataKind: "postgres", OldData: oldIdentity, NewData: newIdentity, TargetRevision: desired}
	r.outcomes = []lifecycleOutcome{response(inspected(oldObservation)), response(applied(desired)), response(inspected(observationFor(nextTarget, desired)))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, oldObservation, oldRevision), OldInputs: old, Inputs: next})
	if err != nil || got.Properties.Len() == 0 || approvals != 1 || strings.Join(r.events, ",") != "inspect,approve,reconcile,inspect" {
		t.Fatal("renamed exact single data-link did not use inspect, approve, reconcile, inspect")
	}
	if r.calls[1].request.Approval == nil || !reflect.DeepEqual(*r.calls[1].request.Approval, expected) { t.Fatal("renamed link reconcile did not include the exact approval") }
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
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
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
	_, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, multipleOld)), State: checkpoint(t, multipleOld, observation(oldRevision), oldRevision), OldInputs: multipleOld, Inputs: multipleNext})
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
			if scenario.mutate != nil { r.outcomes = []lifecycleOutcome{response(inspected(scenario.mutate(initial)))} } else if scenario.outcome.err != nil || scenario.outcome.response.Result != nil { r.outcomes = []lifecycleOutcome{scenario.outcome} }
			got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
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
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || len(r.calls) != 3 {
			t.Fatal("invalid final observation fabricated a checkpoint")
		}
		assertNoCanary(t, errString(err))
	}
}

func TestLifecycleUpdateRejectsInvalidFinalLocalDataObservation(t *testing.T) {
	old, next := localDataInputs(t, "edge"), rotateSecret(t, localDataInputs(t, "edge"))
	for _, mutate := range []func(hostcontract.StableObservation) hostcontract.StableObservation{emptyLocalDataIdentity, wrongLocalDataKind, wrongLocalDataPort, wrongLocalDataDatabase, wrongLocalDataTLS, mismatchedLocalDataProviderAndEndpoint, notReadyLocalData, missingLocalData, duplicateLocalData} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		oldObservation := observationFor(decodeTarget(t, old), oldRevision)
		r.outcomes = []lifecycleOutcome{response(inspected(oldObservation)), response(applied(desired)), response(inspected(mutate(observationFor(decodeTarget(t, next), desired))))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, oldObservation, oldRevision), OldInputs: old, Inputs: next})
		if err == nil || got.Properties.Len() != 0 || len(r.calls) != 3 {
			t.Fatal("invalid final local-data observation fabricated a checkpoint")
		}
		assertNoCanary(t, errString(err))
	}
}

func TestLifecycleUpdateAcceptsOwnershipScopedLocalDataIdentity(t *testing.T) {
	old, next := localDataInputs(t, "edge"), rotateSecret(t, localDataInputs(t, "edge"))
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	oldObservation, nextObservation := observationFor(decodeTarget(t, old), oldRevision), observationFor(decodeTarget(t, next), desired)
	if len(nextObservation.Data) != 2 || nextObservation.Data[0].Identity.ProviderID == "primary" || nextObservation.Data[1].Identity.ProviderID == "replica" || nextObservation.Data[0].Identity.ProviderID == nextObservation.Data[1].Identity.ProviderID {
		t.Fatal("local data fixture did not contain two distinct opaque managed identities")
	}
	r.outcomes = []lifecycleOutcome{response(inspected(oldObservation)), response(applied(desired)), response(inspected(nextObservation))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, oldObservation, oldRevision), OldInputs: old, Inputs: next})
	if err != nil || len(r.calls) != 3 { t.Fatal("Update rejected an ownership-scoped local data identity") }
	assertCheckpoint(t, got.Properties, next, nextObservation, desired)
}

func TestLifecycleUpdateOrdinaryChangesAndUnknownInputs(t *testing.T) {
	old := lifecycleInputs("edge")
	for _, next := range []property.Map{rotateSecret(t, old), changeImage(t, old), changeHostname(t, old), old.Set("resource", property.New(property.Computed)), old.Set("server", property.New(property.Computed)), old.Set("target", property.New(property.Computed)), old.Set("secrets", property.New(property.Computed).WithSecret(true))} {
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t), artifact: fatalArtifact(t)})
		oldRevision := revision(t, h, old)
		if hasComputed(next) {
			if _, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next}); err == nil {
				t.Fatal("unknown Update input was accepted")
			} else {
				assertNoCanary(t, errString(err))
			}
			assertNoCalls(t, r)
			continue
		}
		desired := revision(t, h, next)
		r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), response(applied(desired)), response(inspected(observationFor(decodeTarget(t, next), desired)))}
		if _, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next}); err != nil {
			t.Fatal("ordinary Update returned an error")
		}
		if len(r.calls) != 3 || strings.Join(r.events, ",") != "inspect,reconcile,inspect" {
			t.Fatal("ordinary Update did not use inspect, reconcile, inspect")
		}
	}
}

func TestLifecycleUpdateHealthyNoOpAndCompletedSameReleaseRetryOnlyInspect(t *testing.T) {
	old := lifecycleInputs("edge")
	for _, next := range []property.Map{old, rotateSecret(t, old), changeImage(t, old)} {
		r := &recordingLifecycleTransport{}
		artifactCalls := 0
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { artifactCalls++; return artifactBundle{}, errors.New("must not load") }, approve: fatalApproval(t)})
		oldRevision, desired := revision(t, h, old), revision(t, h, next)
		nextObservation := observationFor(decodeTarget(t, next), desired)
		r.outcomes = []lifecycleOutcome{response(inspected(nextObservation))}
		got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
		if err != nil || !onlyInspect(r) || artifactCalls != 0 || got.Properties.Len() == 0 {
			t.Fatal("healthy completed Update did not finish with one validated inspect and no repeated effects")
		}
		assertInspect(t, r.calls[0], next, desired)
		assertCheckpoint(t, got.Properties, next, nextObservation, desired)
	}
}

func TestLifecycleUpdateUpgradesReleasedHostArtifactInPlace(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old)
	nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	bundle, binary := lifecycleBundle(t, nextTarget.ReleaseArtifact)
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, probes: []artifact.ProbeInfo{{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}}}
	artifactCalls := 0
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { artifactCalls++; return bundle, nil }, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), response(applied(desired)), response(inspected(observationFor(nextTarget, desired)))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
	if err != nil || artifactCalls != 1 || len(r.calls) != 5 || r.calls[0].request.Action != hostcontract.ActionInspect || r.calls[1].kind != "probe" || r.calls[2].command != openssh.BootstrapReceiver || r.calls[3].kind != "probe" || r.calls[4].request.Action != hostcontract.ActionInspect {
		t.Fatal("release upgrade did not use inspect, probe(old), BootstrapReceiver, probe(new), inspect")
	}
	assertInspect(t, r.calls[0], next, desired)
	bootstrap := decodeBootstrapRequest(t, r.calls[2].stdin, binary)
	assertReconcile(t, bootstrap, next, desired, oldRevision, nil)
	assertInspect(t, r.calls[4], next, desired)
	assertCheckpoint(t, got.Properties, next, observationFor(nextTarget, desired), desired)
}

func TestLifecycleUpdateReleaseUpgradeGuardsAndPostBootstrapFailures(t *testing.T) {
	old := lifecycleInputs("edge")
	nextTarget := decodeTarget(t, old)
	nextTarget.ReleaseArtifact = "release-next@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	next := old.Set("target", encodeValue(t, nextTarget))
	for _, scenario := range []struct {
		name    string
		bundle  func(t *testing.T) (artifactBundle, error)
		probe   artifact.ProbeInfo
		calls   int
		write   bool
	}{
		{"artifact unavailable", func(*testing.T) (artifactBundle, error) { return artifactBundle{}, errors.New("absent") }, artifact.ProbeInfo{}, 1, false},
		{"manifest release mismatch", func(t *testing.T) (artifactBundle, error) { bundle, _ := lifecycleBundle(t, "other-release"); return bundle, nil }, artifact.ProbeInfo{}, 1, false},
		{"unsupported architecture", func(t *testing.T) (artifactBundle, error) { return lifecycleArtifactBundle(t, nextTarget.ReleaseArtifact), nil }, artifact.ProbeInfo{OS: "Linux", Arch: "mips", Machine: "machine-a"}, 2, false},
		{"checksum mismatch", func(t *testing.T) (artifactBundle, error) { bundle := lifecycleArtifactBundle(t, nextTarget.ReleaseArtifact); bundle.Manifest.LinuxAMD64.SHA256 = strings.Repeat("0", 64); return bundle, nil }, artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a"}, 2, false},
		{"probe OS mismatch", func(t *testing.T) (artifactBundle, error) { return lifecycleArtifactBundle(t, nextTarget.ReleaseArtifact), nil }, artifact.ProbeInfo{OS: "Darwin", Arch: "amd64", Machine: "machine-a"}, 2, false},
		{"probe machine mismatch", func(t *testing.T) (artifactBundle, error) { return lifecycleArtifactBundle(t, nextTarget.ReleaseArtifact), nil }, artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-b"}, 2, false},
		{"bootstrap invalid response", func(t *testing.T) (artifactBundle, error) { return lifecycleArtifactBundle(t, nextTarget.ReleaseArtifact), nil }, artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a"}, 3, true},
		{"bootstrap transport failure", func(t *testing.T) (artifactBundle, error) { return lifecycleArtifactBundle(t, nextTarget.ReleaseArtifact), nil }, artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a"}, 3, true},
		{"final observation mismatch", func(t *testing.T) (artifactBundle, error) { return lifecycleArtifactBundle(t, nextTarget.ReleaseArtifact), nil }, artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, 5, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := &recordingLifecycleTransport{probe: scenario.probe}
			if scenario.calls == 5 { r.probes = []artifact.ProbeInfo{scenario.probe, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: pinnedTestDigest()}} }
			artifactCalls := 0
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { artifactCalls++; return scenario.bundle(t) }, approve: fatalApproval(t)})
			oldRevision, desired := revision(t, h, old), revision(t, h, next)
			r.outcomes = []lifecycleOutcome{response(inspected(observationFor(decodeTarget(t, old), oldRevision)))}
			if scenario.calls > 1 {
				if scenario.calls == 3 {
					r.outcomes = append(r.outcomes, failure(openssh.ErrTransport))
				}
				if scenario.calls == 5 {
					r.outcomes = append(r.outcomes, response(applied(desired)), response(inspected(wrongRelease(observationFor(nextTarget, desired)))))
				}
			}
			if scenario.name == "bootstrap invalid response" {
				r.outcomes[1] = response(inspected(observationFor(nextTarget, desired)))
			}
			got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
			if err == nil || got.Properties.Len() != 0 || artifactCalls != 1 || len(r.calls) != scenario.calls || hasWrite(r) != scenario.write {
				t.Fatal("release upgrade guard or post-bootstrap failure had an unexpected artifact call, state, or write")
			}
			assertNoCanary(t, errString(err))
		})
	}
}

func TestLifecycleUpdateDirectGuardsFailBeforeTransport(t *testing.T) {
	old := lifecycleInputs("edge")
	for _, scenario := range []struct { name string; id string; state, oldInputs, inputs property.Map }{
		{"wrong physical ID", "other", checkpointFor(t, old), old, old},
		{"environment", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old, old.Set("resource", object("environment", property.New("other"), "serverKey", property.New("edge")))},
		{"server key", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old, old.Set("resource", object("environment", property.New("prod"), "serverKey", property.New("other")))},
		{"hostile alias", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old, old.Set("server", object("sshAlias", property.New("host; unsafe")))},
		{"missing machine", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Delete("machine"), old, old},
		{"missing ownership", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Delete("ownership"), old, old},
		{"missing revision", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Delete("appliedRevision"), old, old},
		{"missing observation", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Delete("observation"), old, old},
		{"malformed machine", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Set("machine", property.New("bad")), old, old},
		{"malformed ownership", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Set("ownership", property.New("bad")), old, old},
		{"malformed revision", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Set("appliedRevision", property.New("bad")), old, old},
		{"malformed observation", stableID(lifecycleResource(t, old)), checkpointFor(t, old).Set("observation", property.New("bad")), old, old},
		{"computed old inputs", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old.Set("target", property.New(property.Computed)), old},
		{"missing old inputs field", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old.Delete("target"), old},
		{"malformed old inputs", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old.Set("server", property.New("bad")), old},
		{"unsecret old inputs", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old.Set("secrets", encodeValue(t, decodeSecrets(t, old))), old},
		{"unsecret next inputs", stableID(lifecycleResource(t, old)), checkpointFor(t, old), old, old.Set("secrets", encodeValue(t, decodeSecrets(t, old)))},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := &recordingLifecycleTransport{}
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
			got, err := h.update(t.Context(), p.UpdateRequest{ID: scenario.id, State: scenario.state, OldInputs: scenario.oldInputs, Inputs: scenario.inputs})
			if err == nil || got.Properties.Len() != 0 { t.Fatal("invalid Update guard was accepted or fabricated state") }
			assertNoCalls(t, r)
			assertNoCanary(t, errString(err))
		})
	}
}

func TestLifecycleRemoteErrorsReturnEmptyResponsesWithoutPanic(t *testing.T) {
	inputs := lifecycleInputs("edge")
	bundle, _ := lifecycleBundle(t, release(t, inputs))
	t.Run("Create bootstrap conflict", func(t *testing.T) {
		r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a"}, outcomes: []lifecycleOutcome{remoteError(hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict)}}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }})
		assertNoPanic(t, func() {
			got, err := h.create(t.Context(), p.CreateRequest{Properties: inputs})
			if err == nil || got.ID != "" || got.Properties.Len() != 0 || len(r.calls) != 2 || r.calls[0].kind != "probe" || r.calls[1].command != openssh.BootstrapReceiver { t.Fatal("Create remote conflict did not return a bounded empty response") }
			assertNoCanary(t, errString(err))
		})
	})
	t.Run("Update inspect recovery required", func(t *testing.T) {
		old, next := lifecycleInputs("edge"), rotateSecret(t, lifecycleInputs("edge"))
		r := &recordingLifecycleTransport{outcomes: []lifecycleOutcome{remoteError(hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired)}}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
		oldRevision := revision(t, h, old)
		assertNoPanic(t, func() {
			got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
			if err == nil || got.Properties.Len() != 0 || !onlyInspect(r) { t.Fatal("Update remote recovery error did not return a bounded empty response") }
			assertNoCanary(t, errString(err))
		})
	})
	t.Run("Update reconcile conflict", func(t *testing.T) {
		old, next := lifecycleInputs("edge"), rotateSecret(t, lifecycleInputs("edge"))
		r := &recordingLifecycleTransport{}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
		oldRevision := revision(t, h, old)
		r.outcomes = []lifecycleOutcome{response(inspected(observation(oldRevision))), remoteError(hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict)}
		assertNoPanic(t, func() {
			got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, observation(oldRevision), oldRevision), OldInputs: old, Inputs: next})
			if err == nil || got.Properties.Len() != 0 || len(r.calls) != 2 || !onlyInspectThenReconcile(r) { t.Fatal("Update reconcile remote conflict did not return a bounded empty response") }
			assertNoCanary(t, errString(err))
		})
	})
}

func TestLifecycleReadRefreshesTrustedObservationWithOneInspect(t *testing.T) {
	inputs := lifecycleInputs("edge")
	revision := revisionForInputs(t, inputs)
	for _, scenario := range []struct {
		name     string
		observed hostcontract.StableObservation
	}{
		{"healthy", observation(revision)},
		{"drifted", pendingObservation(revision)},
		{"pending", pendingObservation(revision)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := &recordingLifecycleTransport{outcomes: []lifecycleOutcome{response(inspected(scenario.observed))}}
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
			prior := checkpoint(t, inputs, observation(revision), revision)
			got, err := h.read(t.Context(), p.ReadRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: prior, Inputs: inputs})
			if err != nil || got.ID != stableID(lifecycleResource(t, inputs)) || !onlyInspect(r) {
				t.Fatal("Read did not preserve the resource through exactly one inspect")
			}
			assertInspect(t, r.calls[0], inputs, revision)
			assertInputsPreserved(t, got.Properties, inputs)
			assertInputsPreserved(t, got.Inputs, inputs)
			if !valueAt(t, got.Properties, "secrets").Secret() || !valueAt(t, got.Inputs, "secrets").Secret() || !valueAt(t, got.Properties, "observation").Equals(encodeValue(t, scenario.observed)) {
				t.Fatal("Read did not preserve input property classes or refresh observation")
			}
		})
	}
}

func TestLifecycleReadFailuresPreserveCheckpointAndDoNotClaimNotFound(t *testing.T) {
	inputs := lifecycleInputs("edge")
	priorRevision := revisionForInputs(t, inputs)
	prior := checkpoint(t, inputs, observation(priorRevision), priorRevision)
	for _, outcome := range []lifecycleOutcome{
		failure(openssh.ErrTransport),
		remoteError(hostprotocol.ErrorProtocol, hostprotocol.CodeMalformedFrame),
		response(inspected(wrongMachine(observation(priorRevision)))),
		response(hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultInspected}}),
	} {
		r := &recordingLifecycleTransport{outcomes: []lifecycleOutcome{outcome}}
		h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
		got, err := h.read(t.Context(), p.ReadRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: prior, Inputs: inputs})
		if err == nil || !onlyInspect(r) || hasWrite(r) {
			t.Fatal("Read failure did not fail closed without remote write")
		}
		assertInspect(t, r.calls[0], inputs, priorRevision)
		_ = got // Pulumi may discard provider response fields when Read returns an error.
		assertNoCanary(t, errString(err))
	}
}

func TestLifecycleReadRetirementEvidenceMustMatchManagedCheckpoint(t *testing.T) {
	inputs := lifecycleInputs("edge")
	revision := revisionForInputs(t, inputs)
	prior := checkpoint(t, inputs, observation(revision), revision)
	matching := retired()
	for _, scenario := range []struct {
		name    string
		response hostprotocol.Response
		ended   bool
	}{
		{"matching", matching, true},
		{"wrong machine", func() hostprotocol.Response { value := retired(); value.Result.Machine.Value = "machine-b"; return value }(), false},
		{"wrong ownership", func() hostprotocol.Response { value := retired(); value.Result.Ownership.Value = "owner-b"; return value }(), false},
		{"malformed", hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: &hostcontract.OwnershipIdentity{Value: "owner-a"}}}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := &recordingLifecycleTransport{outcomes: []lifecycleOutcome{response(scenario.response)}}
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
			got, err := h.read(t.Context(), p.ReadRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: prior, Inputs: inputs})
			if scenario.ended {
				if err != nil || got.ID != "" || got.Properties.Len() != 0 || !onlyInspect(r) { t.Fatal("matching managed retirement did not report lifecycle ended") }
			} else if err == nil || !onlyInspect(r) || hasWrite(r) {
				t.Fatal("untrusted retirement evidence was accepted as NotFound")
			}
			assertInspect(t, r.calls[0], inputs, revision)
		})
	}
}

func TestLifecycleReadRefreshDriftDiffAndUpdateRepairChain(t *testing.T) {
	inputs := lifecycleInputs("edge")
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	desired := revision(t, h, inputs)
	refreshed := observation(revision(t, h, rotateSecret(t, inputs)))
	if refreshed.AppliedRevision == desired { t.Fatal("drift fixture revision did not differ from desired") }
	r.outcomes = []lifecycleOutcome{response(inspected(refreshed))}
	read, err := h.read(t.Context(), p.ReadRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: checkpoint(t, inputs, observation(desired), desired), Inputs: inputs})
	if err != nil || read.ID != stableID(lifecycleResource(t, inputs)) || !onlyInspect(r) {
		t.Fatal("Read did not refresh a trusted drift checkpoint")
	}
	assertInspect(t, r.calls[0], inputs, desired)
	assertInputsPreserved(t, read.Properties, inputs)
	assertInputsPreserved(t, read.Inputs, inputs)
	if !valueAt(t, read.Inputs, "secrets").Secret() || !valueAt(t, read.Properties, "appliedRevision").Equals(property.New(refreshed.AppliedRevision)) || !valueAt(t, read.Properties, "observation").Equals(encodeValue(t, refreshed)) {
		t.Fatal("Read did not preserve inputs separately from refreshed drift state")
	}
	diff, err := h.diff(t.Context(), p.DiffRequest{OldInputs: inputs, State: read.Properties, Inputs: inputs})
	if err != nil || !diff.HasChanges {
		t.Fatal("Diff did not report a refreshed applied-revision mismatch")
	}
	r.outcomes = []lifecycleOutcome{response(inspected(refreshed)), response(applied(desired)), response(inspected(observation(desired)))}
	updated, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, inputs)), State: read.Properties, OldInputs: inputs, Inputs: inputs})
	if err != nil || len(r.calls) != 4 || r.calls[1].request.Action != hostcontract.ActionInspect || r.calls[2].request.Action != hostcontract.ActionReconcile || r.calls[3].request.Action != hostcontract.ActionInspect {
		t.Fatal("Update did not repair the refreshed drift checkpoint")
	}
	assertInspect(t, r.calls[1], inputs, desired)
	assertReconcile(t, r.calls[2].request, inputs, desired, refreshed.AppliedRevision, nil)
	assertInspect(t, r.calls[3], inputs, desired)
	assertCheckpoint(t, updated.Properties, inputs, observation(desired), desired)
}

func TestLifecycleUpdateRepairsValidatedDriftCheckpoint(t *testing.T) {
	old, next := lifecycleInputs("edge"), rotateSecret(t, lifecycleInputs("edge"))
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	oldRevision, desired := revision(t, h, old), revision(t, h, next)
	drift := pendingObservation(oldRevision)
	r.outcomes = []lifecycleOutcome{response(inspected(drift)), response(applied(desired)), response(inspected(observation(desired)))}
	got, err := h.update(t.Context(), p.UpdateRequest{ID: stableID(lifecycleResource(t, old)), State: checkpoint(t, old, drift, oldRevision), OldInputs: old, Inputs: next})
	if err != nil || len(r.calls) != 3 || !onlyInspectThenReconcileThenInspect(r) {
		t.Fatal("Update did not repair a validated drift checkpoint")
	}
	assertCheckpoint(t, got.Properties, next, observation(desired), desired)
}

func TestLifecycleDeleteApprovesExactPreserveDataRetirementAfterInspect(t *testing.T) {
	inputs := drainedLifecycleInputs(t, "edge")
	r := &recordingLifecycleTransport{}
	var expected hostcontract.ApprovalSubject
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: func(_ context.Context, subject hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) {
		r.events = append(r.events, "approve")
		if !reflect.DeepEqual(subject, expected) { t.Fatal("retirement approval subject was not exact") }
		return &expected, nil
	}})
	revision := revision(t, h, inputs)
	checkpointObservation := observationFor(decodeTarget(t, inputs), revision)
	expected = hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: "prod", Resource: lifecycleResource(t, inputs), Machine: checkpointObservation.Machine, Ownership: checkpointObservation.Ownership, TargetRevision: revision, PreserveData: true}
	r.outcomes = []lifecycleOutcome{response(inspected(checkpointObservation)), response(retired())}
	err := h.delete(t.Context(), p.DeleteRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: checkpoint(t, inputs, checkpointObservation, revision), OldInputs: inputs})
	if err != nil || strings.Join(r.events, ",") != "inspect,approve,retire" || len(r.calls) != 2 { t.Fatal("Delete did not order inspect, approval, preserve-data retirement") }
	assertInspect(t, r.calls[0], inputs, revision)
	assertRetire(t, r.calls[1], inputs, revision, expected)
}

func TestLifecycleDeleteRejectsInvalidCheckpointIdentityOrApprovalWithoutWrite(t *testing.T) {
	inputs := drainedLifecycleInputs(t, "edge")
	revision := revisionForInputs(t, inputs)
	checkpointObservation := observationFor(decodeTarget(t, inputs), revision)
	valid := checkpoint(t, inputs, checkpointObservation, revision)
	for _, scenario := range []struct { name, id string; properties property.Map; approve func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error); outcome lifecycleOutcome; calls int }{
		{"wrong ID", "other", valid, fatalApproval(t), lifecycleOutcome{}, 0},
		{"missing checkpoint", stableID(lifecycleResource(t, inputs)), valid.Delete("machine"), fatalApproval(t), lifecycleOutcome{}, 0},
		{"missing approval", stableID(lifecycleResource(t, inputs)), valid, nil, response(inspected(checkpointObservation)), 1},
		{"wrong approval", stableID(lifecycleResource(t, inputs)), valid, func(_ context.Context, subject hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { subject.Machine.Value = "wrong"; return &subject, nil }, response(inspected(checkpointObservation)), 1},
		{"identity mismatch", stableID(lifecycleResource(t, inputs)), valid, fatalApproval(t), response(inspected(wrongMachine(checkpointObservation))), 1},
		{"unreachable inspect", stableID(lifecycleResource(t, inputs)), valid, fatalApproval(t), failure(openssh.ErrTransport), 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := &recordingLifecycleTransport{}
			if scenario.outcome.err != nil || scenario.outcome.response.Result != nil { r.outcomes = []lifecycleOutcome{scenario.outcome} }
			h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: scenario.approve})
			if err := h.delete(t.Context(), p.DeleteRequest{ID: scenario.id, Properties: scenario.properties, OldInputs: inputs}); err == nil || len(r.calls) != scenario.calls || hasWrite(r) { t.Fatal("unsafe Delete reached retirement write") }
			if scenario.calls == 1 { assertInspect(t, r.calls[0], inputs, revision) }
		})
	}
}

func TestLifecycleDeleteRejectsReferencedTargetBeforeTransport(t *testing.T) {
	inputs := lifecycleInputs("edge")
	revision := revisionForInputs(t, inputs)
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	if err := h.delete(t.Context(), p.DeleteRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: checkpoint(t, inputs, observation(revision), revision), OldInputs: inputs}); err == nil || len(r.calls) != 0 || hasWrite(r) { t.Fatal("Delete accepted a target with still-referenced runtime") }
}

func TestLifecycleDeleteAlreadyRetiredIsIdempotentWithoutApprovalOrWrite(t *testing.T) {
	inputs := drainedLifecycleInputs(t, "edge")
	revision := revisionForInputs(t, inputs)
	r := &recordingLifecycleTransport{outcomes: []lifecycleOutcome{response(retired())}}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	if err := h.delete(t.Context(), p.DeleteRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: checkpoint(t, inputs, observationFor(decodeTarget(t, inputs), revision), revision), OldInputs: inputs}); err != nil || !onlyInspect(r) || hasWrite(r) { t.Fatal("already-retired Delete was not idempotent") }
	assertInspect(t, r.calls[0], inputs, revision)
}

func TestLifecycleDeleteResponseLossRetriesTheSameRetirementOperation(t *testing.T) {
	inputs := drainedLifecycleInputs(t, "edge")
	revision := revisionForInputs(t, inputs)
	checkpointObservation := observationFor(decodeTarget(t, inputs), revision)
	checkpoint := checkpoint(t, inputs, checkpointObservation, revision)
	r := &recordingLifecycleTransport{}
	expected := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: "prod", Resource: lifecycleResource(t, inputs), Machine: checkpointObservation.Machine, Ownership: checkpointObservation.Ownership, TargetRevision: revision, PreserveData: true}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: func(_ context.Context, subject hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) {
		r.events = append(r.events, "approve")
		if !reflect.DeepEqual(subject, expected) { t.Fatal("response-loss retirement approval subject was not exact") }
		return &expected, nil
	}})
	r.outcomes = []lifecycleOutcome{response(inspected(checkpointObservation)), failure(openssh.ErrTransport)}
	if err := h.delete(t.Context(), p.DeleteRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: checkpoint, OldInputs: inputs}); err == nil || len(r.calls) != 2 || !hasWrite(r) { t.Fatal("Delete response loss did not leave one unknown retirement request") }
	assertInspect(t, r.calls[0], inputs, revision)
	assertRetire(t, r.calls[1], inputs, revision, expected)
	r.outcomes = []lifecycleOutcome{response(retired())}
	retry := configuredLifecycleHost(t, lifecycleDependencies{transport: r, approve: fatalApproval(t)})
	if err := retry.delete(t.Context(), p.DeleteRequest{ID: stableID(lifecycleResource(t, inputs)), Properties: checkpoint, OldInputs: inputs}); err != nil || len(r.calls) != 3 || !r.calls[2].decoded || r.calls[2].request.Action != hostcontract.ActionInspect || strings.Join(r.events, ",") != "inspect,approve,retire,inspect" { t.Fatal("fresh-provider Delete retry did not accept matching retirement evidence without another write") }
	assertInspect(t, r.calls[2], inputs, revision)
}

type lifecycleCall struct { kind, alias string; command openssh.Command; stdin []byte; request hostprotocol.Request; hostAttempted, decoded bool }
type lifecycleOutcome struct { response hostprotocol.Response; err error }
type recordingLifecycleTransport struct { probe artifact.ProbeInfo; probes []artifact.ProbeInfo; probeErr error; outcomes []lifecycleOutcome; calls []lifecycleCall; events []string }
func response(value hostprotocol.Response) lifecycleOutcome { return lifecycleOutcome{response: value} }
func failure(err error) lifecycleOutcome { return lifecycleOutcome{err: err} }
func remoteError(category hostprotocol.ErrorCategory, code hostprotocol.ErrorCode) lifecycleOutcome { return response(hostprotocol.Response{Version: hostprotocol.Version, Error: &hostprotocol.RemoteError{Category: category, Code: code}}) }
func (r *recordingLifecycleTransport) Probe(_ context.Context, alias string) (artifact.ProbeInfo, error) { r.calls = append(r.calls, lifecycleCall{kind: "probe", alias: alias}); if len(r.probes) != 0 { probe := r.probes[0]; r.probes = r.probes[1:]; return probe, r.probeErr }; return r.probe, r.probeErr }
func (r *recordingLifecycleTransport) Bootstrap(ctx context.Context, alias string, stdin []byte) (hostprotocol.Response, error) { return r.Run(ctx, alias, openssh.BootstrapReceiver, stdin) }
func (r *recordingLifecycleTransport) Run(_ context.Context, alias string, command openssh.Command, stdin []byte) (hostprotocol.Response, error) {
	call := lifecycleCall{kind: "run", alias: alias, command: command, stdin: append([]byte(nil), stdin...)}
	r.calls = append(r.calls, call)
	if command == openssh.Host { r.calls[len(r.calls)-1].hostAttempted = true; request, err := hostprotocol.DecodeRequest(stdin); if err != nil { return hostprotocol.Response{}, err }; r.calls[len(r.calls)-1].request, r.calls[len(r.calls)-1].decoded = request, true; switch request.Action { case hostcontract.ActionInspect: r.events = append(r.events, "inspect"); case hostcontract.ActionRetirePreserveData: r.events = append(r.events, "retire"); default: r.events = append(r.events, "reconcile") } }
	if len(r.outcomes) == 0 { return hostprotocol.Response{}, errors.New("missing fake outcome") }
	outcome := r.outcomes[0]; r.outcomes = r.outcomes[1:]
	return outcome.response, outcome.err
}

func configuredLifecycleHost(t *testing.T, deps lifecycleDependencies) *host { t.Helper(); h := newHostWithDependencies("1.0.0", deps); configureHost(t, h); return h }
func configureHost(t *testing.T, h *host) { t.Helper(); key := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")); if err := h.configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New(key).WithSecret(true)})}); err != nil { t.Fatal("Configure failed") } }
func configureProvider(t *testing.T, provider p.Provider) { t.Helper(); key := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")); if err := provider.Configure(t.Context(), p.ConfigureRequest{Args: property.NewMap(map[string]property.Value{"revisionKey": property.New(key).WithSecret(true)})}); err != nil { t.Fatal("Configure failed") } }
func lifecycleInputs(alias string) property.Map { return property.NewMap(map[string]property.Value{"resource": object("environment", property.New("prod"), "serverKey", property.New("edge")), "server": object("sshAlias", property.New(alias)), "target": object("releaseArtifact", property.New("release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "apps", property.New(property.NewArray([]property.Value{object("id", property.New("api"), "image", property.New("api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "hostname", property.New("api.example"), "readinessPath", property.New("/ready"), "dataLinks", property.New(property.NewArray([]property.Value{object("name", property.New("main"), "identity", object("kind", property.New("postgres"), "providerId", property.New("db-1"), "endpoint", property.New("db.example"), "port", property.New(5432.0), "database", property.New("app"), "tlsServerName", property.New("db.example")))})))}))), "secrets": property.New(property.NewMap(map[string]property.Value{"apps": object("api", object("jwtSecret", property.New(lifecycleCanary)))})).WithSecret(true)}) }
func drainedLifecycleInputs(t *testing.T, alias string) property.Map { t.Helper(); inputs := lifecycleInputs(alias); target := decodeTarget(t, inputs); target.Apps = nil; return inputs.Set("target", encodeValue(t, target)).Set("secrets", property.New(property.NewMap(nil)).WithSecret(true)) }
func lifecycleResource(t *testing.T, inputs property.Map) hostcontract.ResourceIdentity { t.Helper(); resource := valueAt(t, inputs, "resource"); return hostcontract.ResourceIdentity{Environment: field(resource, "environment").AsString(), ServerKey: field(resource, "serverKey").AsString()} }
func localDataInputs(t *testing.T, alias string) property.Map { t.Helper(); inputs := lifecycleInputs(alias); target, secrets := decodeTarget(t, inputs), decodeSecrets(t, inputs); target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432, Persistence: true}, {ID: "replica", Type: "postgres", Port: 5432, Persistence: true}}; secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: lifecycleCanary}, "replica": {AdminPassword: lifecycleCanary}}; return inputs.Set("target", encodeValue(t, target)).Set("secrets", encodeValue(t, secrets).WithSecret(true)) }
func revision(t *testing.T, h *host, inputs property.Map) string { t.Helper(); value, err := hostcontract.TargetRevision(h.key, lifecycleResource(t, inputs), decodeTarget(t, inputs), decodeSecrets(t, inputs)); if err != nil { t.Fatal("revision failed") }; return value }
func revisionForInputs(t *testing.T, inputs property.Map) string { t.Helper(); h := configuredLifecycleHost(t, lifecycleDependencies{}); return revision(t, h, inputs) }
func baselineRevision(t *testing.T, h *host, inputs property.Map) string { t.Helper(); value, err := hostcontract.TargetRevision(h.key, lifecycleResource(t, inputs), hostcontract.Target{ReleaseArtifact: release(t, inputs)}, hostcontract.Secrets{}); if err != nil { t.Fatal("baseline revision failed") }; return value }
func release(t *testing.T, inputs property.Map) string { t.Helper(); return field(valueAt(t, inputs, "target"), "releaseArtifact").AsString() }
func observation(revision string) hostcontract.StableObservation { return observationFor(hostcontract.Target{ReleaseArtifact: "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}, revision) }
func pendingObservation(revision string) hostcontract.StableObservation { value := observation(revision); value.Ready, value.Drifted = false, true; return value }
func observationFor(target hostcontract.Target, revision string) hostcontract.StableObservation { observation := hostcontract.StableObservation{Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, HostRelease: target.ReleaseArtifact, AppliedRevision: revision, Ready: true}; for _, app := range target.Apps { observation.Apps = append(observation.Apps, hostcontract.AppObservation{ID: app.ID, ActiveImage: app.Image, Ready: true}) }; for _, service := range target.DataServices { observation.Data = append(observation.Data, hostcontract.DataObservation{Identity: localDataIdentity(service), Ready: true}) }; return observation }
func applied(revision string) hostprotocol.Response { return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: revision}} }
func inspected(value hostcontract.StableObservation) hostprotocol.Response { return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultInspected, Observation: &value}} }
func inspectedEvidence(value hostcontract.StableObservation, evidence *hostprotocol.OperationEvidence) hostprotocol.Response { return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultInspected, Observation: &value, OperationEvidence: evidence}} }
func retired() hostprotocol.Response { return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: &hostcontract.OwnershipIdentity{Value: "owner-a"}, Retirement: &hostprotocol.RetirementEvidence{PreserveData: true}}} }
func wrongMachine(value hostcontract.StableObservation) hostcontract.StableObservation { value.Machine.Value = "machine-b"; return value }
func wrongOwner(value hostcontract.StableObservation) hostcontract.StableObservation { value.Ownership.Value = "owner-b"; return value }
func emptyOwner(value hostcontract.StableObservation) hostcontract.StableObservation { value.Ownership.Value = ""; return value }
func wrongRelease(value hostcontract.StableObservation) hostcontract.StableObservation { value.HostRelease = "other"; return value }
func wrongRevision(value hostcontract.StableObservation) hostcontract.StableObservation { value.AppliedRevision = mismatchedRevision(); return value }
func mismatchedRevision() string { return "tr1:0000000000000000:0000000000000000000000000000000000000000000000000000000000000000" }
func notReady(value hostcontract.StableObservation) hostcontract.StableObservation { value.Ready = false; return value }
func drifted(value hostcontract.StableObservation) hostcontract.StableObservation { value.Drifted = true; return value }
func wrongAppImage(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps[0].ActiveImage = "wrong"; return value }
func notReadyApp(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps[0].Ready = false; return value }
func wrongAppID(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps[0].ID = "other"; return value }
func duplicateAppID(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps = append(value.Apps, value.Apps[0]); return value }
func missingApps(value hostcontract.StableObservation) hostcontract.StableObservation { value.Apps = nil; return value }
func wrongLocalDataKind(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Identity.Kind = "redis"; return value }
func emptyLocalDataIdentity(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Identity.ProviderID, value.Data[0].Identity.Endpoint = "", ""; return value }
func wrongLocalDataPort(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Identity.Port++; return value }
func wrongLocalDataDatabase(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Identity.Database = "other"; return value }
func wrongLocalDataTLS(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Identity.TLSServerName = "other"; return value }
func mismatchedLocalDataProviderAndEndpoint(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Identity.Endpoint = "other"; return value }
func missingLocalData(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data = value.Data[:1]; return value }
func duplicateLocalData(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[1] = value.Data[0]; return value }
func notReadyLocalData(value hostcontract.StableObservation) hostcontract.StableObservation { value.Data[0].Ready = false; return value }
func rotateSecret(t *testing.T, inputs property.Map) property.Map { secrets := decodeSecrets(t, inputs); secrets.Apps["api"] = hostcontract.AppSecrets{JWTSecret: "rotated-" + lifecycleCanary}; return inputs.Set("secrets", encodeValue(t, secrets).WithSecret(true)) }
func changeImage(t *testing.T, inputs property.Map) property.Map { target := decodeTarget(t, inputs); target.Apps[0].Image = "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"; return inputs.Set("target", encodeValue(t, target)) }
func changeHostname(t *testing.T, inputs property.Map) property.Map { target := decodeTarget(t, inputs); target.Apps[0].Hostname = "other.example"; return inputs.Set("target", encodeValue(t, target)) }
func dangerousChange(t *testing.T) (property.Map, property.Map) { old, next := lifecycleInputs("edge"), lifecycleInputs("edge"); target := decodeTarget(t, old); target.Apps[0].DataLinks[0].Identity.Endpoint = "old-db.example"; return old.Set("target", encodeValue(t, target)), next }
func dangerousApprovalFixture(t *testing.T, old, next property.Map, revision string) hostcontract.ApprovalSubject { t.Helper(); return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: lifecycleResource(t, next).Environment, Resource: lifecycleResource(t, next), AppID: "api", DataKind: "postgres", OldData: decodeTarget(t, old).Apps[0].DataLinks[0].Identity, NewData: decodeTarget(t, next).Apps[0].DataLinks[0].Identity, TargetRevision: revision} }
func twoDangerousChanges(t *testing.T) (property.Map, property.Map) { old, next := lifecycleInputs("edge"), lifecycleInputs("edge"); oldTarget, nextTarget := decodeTarget(t, old), decodeTarget(t, next); secondOld := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old-two", Endpoint: "old-two.example", Port: 5432, Database: "two", TLSServerName: "old-two.example"}; secondNew := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new-two", Endpoint: "new-two.example", Port: 5432, Database: "two", TLSServerName: "new-two.example"}; oldTarget.Apps[0].DataLinks = append(oldTarget.Apps[0].DataLinks, hostcontract.DataLink{Name: "second", Identity: secondOld}); nextTarget.Apps[0].DataLinks = append(nextTarget.Apps[0].DataLinks, hostcontract.DataLink{Name: "second", Identity: secondNew}); oldTarget.Apps[0].DataLinks[0].Identity.Endpoint = "old-one.example"; nextTarget.Apps[0].DataLinks[0].Identity.Endpoint = "new-one.example"; return old.Set("target", encodeValue(t, oldTarget)), next.Set("target", encodeValue(t, nextTarget)) }
func localDataIdentity(service hostcontract.LocalDataServiceTarget) hostcontract.DataIdentity { database, tls := "sub2api", ""; managed := "owner-scoped-" + service.Type + "-" + service.ID + "-managed"; if service.Type == "redis" { database = "0" } else { tls = managed }; return hostcontract.DataIdentity{Kind: service.Type, ProviderID: managed, Endpoint: managed, Port: service.Port, Database: database, TLSServerName: tls} }
func lifecycleBundle(t *testing.T, release string) (artifactBundle, []byte) { t.Helper(); root := t.TempDir(); amd64, arm64 := []byte("pinned-host-amd64"), []byte("pinned-host-arm64"); write := func(name string, contents []byte) { if err := os.WriteFile(filepath.Join(root, name), contents, 0o600); err != nil { t.Fatal("artifact fixture write failed") } }; write("host-amd64", amd64); write("host-arm64", arm64); sum := func(value []byte) string { hash := sha256.Sum256(value); return fmt.Sprintf("%x", hash) }; return artifactBundle{Root: root, Manifest: artifact.Manifest{SchemaVersion: 1, Release: release, LinuxAMD64: artifact.Entry{Path: "host-amd64", Size: int64(len(amd64)), SHA256: sum(amd64)}, LinuxARM64: artifact.Entry{Path: "host-arm64", Size: int64(len(arm64)), SHA256: sum(arm64)}}}, amd64 }
func lifecycleArtifactBundle(t *testing.T, release string) artifactBundle { t.Helper(); bundle, _ := lifecycleBundle(t, release); return bundle }
func releaseBundleProvider(t *testing.T, bundleRoot string) string { t.Helper(); provider := filepath.Join(bundleRoot, "bin", "pulumi-resource-sub2api-host"); if err := os.MkdirAll(filepath.Dir(provider), 0o700); err != nil { t.Fatal(err) }; if err := os.WriteFile(provider, []byte("provider"), 0o700); err != nil { t.Fatal(err) }; return provider }
func releaseBundleHostArtifacts(t *testing.T, bundleRoot, release string) (artifactBundle, []byte) { t.Helper(); root := filepath.Join(bundleRoot, "artifacts", "sub2api-host"); if err := os.MkdirAll(root, 0o700); err != nil { t.Fatal(err) }; amd64, arm64 := []byte("released-host-amd64"), []byte("released-host-arm64"); sum := func(value []byte) string { hash := sha256.Sum256(value); return fmt.Sprintf("%x", hash) }; manifest := artifact.Manifest{SchemaVersion: 1, Release: release, LinuxAMD64: artifact.Entry{Path: "sub2api-host-linux-amd64", Size: int64(len(amd64)), SHA256: sum(amd64)}, LinuxARM64: artifact.Entry{Path: "sub2api-host-linux-arm64", Size: int64(len(arm64)), SHA256: sum(arm64)}}; for name, contents := range map[string][]byte{manifest.LinuxAMD64.Path: amd64, manifest.LinuxARM64.Path: arm64} { if err := os.WriteFile(filepath.Join(root, name), contents, 0o700); err != nil { t.Fatal(err) } }; encoded, err := json.Marshal(manifest); if err != nil { t.Fatal(err) }; if err := os.WriteFile(filepath.Join(root, "manifest.json"), encoded, 0o600); err != nil { t.Fatal(err) }; return artifactBundle{Root: root, Manifest: manifest}, amd64 }
func decodeBootstrapRequest(t *testing.T, stdin, binary []byte) hostprotocol.Request { t.Helper(); hash := sha256.Sum256(binary); prefix := []byte(fmt.Sprintf("s2a1:%d:%x\n", len(binary), hash)); if len(stdin) < len(prefix)+len(binary) || !bytes.Equal(stdin[:len(prefix)], prefix) || !bytes.Equal(stdin[len(prefix):len(prefix)+len(binary)], binary) { t.Fatal("bootstrap input did not contain the pinned artifact") }; request, err := hostprotocol.DecodeRequest(stdin[len(prefix)+len(binary):]); if err != nil { t.Fatal("bootstrap did not contain one valid request frame") }; return request }
func assertInspect(t *testing.T, call lifecycleCall, inputs property.Map, desired string) { t.Helper(); alias := field(valueAt(t, inputs, "server"), "sshAlias").AsString(); request := call.request; if call.command != openssh.Host || call.alias != alias || request.Action != hostcontract.ActionInspect || request.Server.SSHAlias != alias || request.Resource != lifecycleResource(t, inputs) || request.TargetRevision != desired || request.Target != nil || request.Secrets != nil || request.Approval != nil || request.PriorAppliedRevision != "" || request.PriorObservation != "" { t.Fatal("Inspect request contract was not exact") } }
func assertReconcile(t *testing.T, request hostprotocol.Request, inputs property.Map, desired, prior string, approval *hostcontract.ApprovalSubject) { t.Helper(); alias := field(valueAt(t, inputs, "server"), "sshAlias").AsString(); if request.Action != hostcontract.ActionReconcile || request.Server.SSHAlias != alias || request.Resource != lifecycleResource(t, inputs) || request.TargetRevision != desired || request.PriorAppliedRevision != prior || request.PriorObservation != "" || !reflect.DeepEqual(request.Approval, approval) || request.Target == nil || request.Secrets == nil || !reflect.DeepEqual(*request.Target, decodeTarget(t, inputs)) || !reflect.DeepEqual(*request.Secrets, decodeSecrets(t, inputs)) { t.Fatal("Reconcile request contract was not exact") } }
func assertRetire(t *testing.T, call lifecycleCall, inputs property.Map, revision string, approval hostcontract.ApprovalSubject) { t.Helper(); request := call.request; if call.command != openssh.Host || request.Action != hostcontract.ActionRetirePreserveData || request.Server != (hostcontract.ServerTarget{SSHAlias: field(valueAt(t, inputs, "server"), "sshAlias").AsString()}) || request.Resource != lifecycleResource(t, inputs) || request.TargetRevision != revision || request.PriorAppliedRevision != revision || request.PriorObservation != "" || request.Target != nil || request.Secrets != nil || request.Approval == nil || !reflect.DeepEqual(*request.Approval, approval) { t.Fatal("retire request contract was not exact") } }
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
func createWithReadyHost(t *testing.T, inputs property.Map) string { t.Helper(); bundle, binary := lifecycleBundle(t, release(t, inputs)); digest := fmt.Sprintf("%x", sha256.Sum256(binary)); r := &recordingLifecycleTransport{probe: artifact.ProbeInfo{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, probes: []artifact.ProbeInfo{{OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: "missing"}, {OS: "Linux", Arch: "amd64", Machine: "machine-a", InstalledDigest: digest}}}; h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: func() (artifactBundle, error) { return bundle, nil }}); desired := revision(t, h, inputs); r.outcomes = []lifecycleOutcome{response(applied(desired)), response(inspected(observationFor(decodeTarget(t, inputs), desired)))}; got, err := h.create(t.Context(), p.CreateRequest{Properties: inputs}); if err != nil { t.Fatal("Create failed") }; return got.ID }
func pinnedTestDigest() string { return fmt.Sprintf("%x", sha256.Sum256([]byte("pinned-host-amd64"))) }
func fatalArtifact(t *testing.T) func() (artifactBundle, error) { return func() (artifactBundle, error) { t.Fatal("artifact source was called"); return artifactBundle{}, nil } }
func fatalApproval(t *testing.T) func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { return func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) { t.Fatal("approval source was called"); return nil, nil } }
func assertNoCalls(t *testing.T, r *recordingLifecycleTransport) { t.Helper(); if len(r.calls) != 0 { t.Fatal("transport was called") } }
func onlyInspect(r *recordingLifecycleTransport) bool { return len(r.calls) == 1 && r.calls[0].command == openssh.Host && r.calls[0].decoded && r.calls[0].request.Action == hostcontract.ActionInspect }
func onlyInspectThenReconcile(r *recordingLifecycleTransport) bool { return len(r.calls) == 2 && r.calls[0].command == openssh.Host && r.calls[0].decoded && r.calls[0].request.Action == hostcontract.ActionInspect && r.calls[1].command == openssh.Host && r.calls[1].decoded && r.calls[1].request.Action == hostcontract.ActionReconcile }
func onlyInspectThenReconcileThenInspect(r *recordingLifecycleTransport) bool { return len(r.calls) == 3 && r.calls[0].decoded && r.calls[0].request.Action == hostcontract.ActionInspect && r.calls[1].decoded && r.calls[1].request.Action == hostcontract.ActionReconcile && r.calls[2].decoded && r.calls[2].request.Action == hostcontract.ActionInspect }
func hasWrite(r *recordingLifecycleTransport) bool { for _, call := range r.calls { if call.command == openssh.BootstrapReceiver || call.hostAttempted && (!call.decoded || call.request.Action != hostcontract.ActionInspect) { return true } }; return false }
func errString(err error) string { if err == nil { return "" }; return err.Error() }
func assertNoCanary(t *testing.T, values ...string) { t.Helper(); for _, value := range values { if strings.Contains(value, lifecycleCanary) { t.Fatal("secret canary leaked") } } }
func assertNoPanic(t *testing.T, run func()) { t.Helper(); defer func() { if recover() != nil { t.Fatal("lifecycle panicked") } }(); run() }
