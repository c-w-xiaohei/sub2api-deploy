//go:build linux

package providerruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostapproval"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime/testonly"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	ciHelperGuard = "SUB2API_PROVIDER_RUNTIME_CI_HELPER"
	ciHelperRoot  = "SUB2API_PROVIDER_RUNTIME_ROOT"
	ciHelperMID   = "SUB2API_PROVIDER_RUNTIME_MACHINE_ID"
	ciHelperMode  = "SUB2API_PROVIDER_RUNTIME_MODE"
	ciKey         = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="
	ciRelease     = "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ciSecret      = "PROVIDER_RUNTIME_SECRET_CANARY"
)

// TestProviderRuntimeCIHelper is selected only by this integration test binary.
// It is a CI helper, never a released or installed sub2api-host binary.
func TestProviderRuntimeCIHelper(t *testing.T) {
	if os.Getenv(ciHelperGuard) != "1" {
		return
	}
	if os.Getenv(ciHelperMode) == "docker" {
		if err := dockerFixture(os.Args); err != nil {
			_ = os.WriteFile(filepath.Join(os.Getenv("PROVIDER_RUNTIME_TRACE"), "docker.error"), []byte(err.Error()+"\n"), 0o600)
			os.Exit(64)
		}
		os.Exit(0)
	}
	serve := testonly.Serve
	if os.Getenv(ciHelperMode) == "bootstrap" {
		if err := testonly.ServeBootstrapWithRequestDigest(os.Stdout, os.Stdin, os.Getenv(ciHelperRoot), os.Getenv(ciHelperMID), os.Getenv("PROVIDER_RUNTIME_REQUEST_DIGEST")); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if digestPath := os.Getenv("PROVIDER_RUNTIME_REQUEST_DIGEST"); digestPath != "" {
		serve = func(out io.Writer, in io.Reader, root, machinePath string) error {
			return testonly.ServeWithRequestDigest(out, in, root, machinePath, digestPath)
		}
	}
	if err := serve(os.Stdout, os.Stdin, os.Getenv(ciHelperRoot), os.Getenv(ciHelperMID)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestProviderLifecycleWithHostProcessTempRuntime is the Task 7 acceptance
// matrix. It uses the Provider process, fixed SSH endpoint, and the same
// Runtime.Serve implementation; all roots and fakes are owned by each subtest.
func TestProviderLifecycleWithHostProcessTempRuntime(t *testing.T) {
	assertFrozenNormalizationOracle(t)
	providerBinary := buildProviderForMatrix(t)
	t.Run("A-create-read-ordinary-update-delete", func(t *testing.T) {
		h := startProviderWithApproval(t, providerBinary, approvalExact)
		created := createProviderResource(t, h, createInputs())
		before := runtimeSnapshot(t, h)
		read, err := readProviderResource(t, h, readRequest(t, created), hostcontract.ActionInspect)
		if err != nil || read.Id != created.Id {
			t.Fatalf("Read = %#v, %v; want preserved ID and checkpoint", read, err)
		}
		assertReadOnlyRuntime(t, before, runtimeSnapshot(t, h))

		next := createInputsWithImage("api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
		updated, err := updateProviderResource(t, h, updateRequest(t, created.Id, created.Properties, createInputs(), next), next, hostcontract.ActionInspect, hostcontract.ActionReconcile, hostcontract.ActionInspect)
		if err != nil || updated == nil {
			t.Fatalf("ordinary Update: %v", err)
		}
		assertCompletedReconcile(t, h, unmarshalProperties(t, updated.Properties), 0)

		writeDataSentinel(t, h)
		drained := createInputsWithTarget(hostcontract.Target{ReleaseArtifact: ciRelease})
		beforeDrain := dockerEffects(t, h)
		preDrainApps := inventoryApps(t, h)
		drain, err := updateProviderResource(t, h, updateRequest(t, created.Id, updated.Properties, next, drained), drained, hostcontract.ActionInspect, hostcontract.ActionReconcile, hostcontract.ActionInspect)
		if err != nil || drain == nil {
			t.Fatalf("drain Update: %v", err)
		}
		assertDrainEffectDelta(t, h, beforeDrain, dockerEffects(t, h), preDrainApps)
		beforeRetire := dockerEffects(t, h)
		preRetire := runtimeState(t, h).Journal
		h.approvals.Expect(retireApprovalSubject(t, unmarshalProperties(t, drain.Properties), drained))
		if _, err := deleteProviderResource(t, h, deleteRequest(t, created.Id, drain.Properties, drained), drained, hostcontract.ActionInspect, hostcontract.ActionRetirePreserveData); err != nil {
			t.Fatalf("exact retire approval Delete: %v", err)
		}
		h.approvals.AssertExpectedConsumed(t)
		assertRetiredPreservingData(t, h, beforeRetire, preRetire)
		assertNoSecretCanary(t, h, "")
	})

	for _, scenario := range []struct {
		name      string
		changes   int
		decision  approvalDecision
		wantCalls int
	}{
		{"C-zero-dangerous-absent", 0, approvalAbsent, 0},
		{"C-one-dangerous-absent", 1, approvalAbsent, 0},
		{"C-one-dangerous-deny", 1, approvalDeny, 1},
		{"C-one-dangerous-exact", 1, approvalExact, 1},
		{"C-two-dangerous-fail-closed", 2, approvalExact, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := startProviderWithApproval(t, providerBinary, scenario.decision)
			old := createInputsWithDataLinks(scenario.changes, "old")
			created := createProviderResource(t, h, old)
			assertInitialCreateEffects(t, h, dockerEffects(t, h), old)
			next := createInputsWithDataLinks(scenario.changes, "new")
			if scenario.changes == 0 {
				next = createInputsWithImage("api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
			}
			if scenario.changes == 1 && scenario.decision == approvalExact {
				h.approvals.Expect(dataLinkApprovalSubject("old", "new", frozenRevisionForInputs(t, next)))
			}
			before := runtimeSnapshot(t, h)
			effectsBefore := dockerEffects(t, h)
			actions := []hostcontract.Action{hostcontract.ActionInspect}
			if scenario.changes == 0 || scenario.changes == 1 && scenario.decision == approvalExact {
				actions = append(actions, hostcontract.ActionReconcile, hostcontract.ActionInspect)
			}
			updated, err := updateProviderResource(t, h, updateRequest(t, created.Id, created.Properties, old, next), next, actions...)
			if scenario.changes == 0 || scenario.changes == 1 && scenario.decision == approvalExact {
				if err != nil || updated == nil {
					t.Fatalf("exact approval Update: %v", err)
				}
				assertReconcileEffectDelta(t, h, effectsBefore, dockerEffects(t, h), old, next)
			} else if err == nil {
				t.Fatal("non-admitted Update succeeded")
			}
			if got := h.approvals.Count(); got != scenario.wantCalls {
				t.Fatalf("approval requests = %d, want %d", got, scenario.wantCalls)
			}
			if scenario.changes == 1 && scenario.decision == approvalDeny {
				subjects := h.approvals.Subjects()
				if len(subjects) != 1 || subjects[0] != dataLinkApprovalSubject("old", "new", frozenRevisionForInputs(t, next)) {
					t.Fatalf("denied approval subject = %#v", subjects)
				}
			}
			h.approvals.AssertExpectedConsumed(t)
			if scenario.changes == 1 && scenario.decision != approvalExact || scenario.changes == 2 {
				assertNoRuntimeWrite(t, before, runtimeSnapshot(t, h))
			}
		})
	}

	t.Run("B-response-loss-same-revision-resumes-once", func(t *testing.T) {
		h := startProviderWithApproval(t, providerBinary, approvalExact)
		old := createInputsWithDataLinks(1, "old")
		created := createProviderResource(t, h, old)
		next := createInputsWithDataLinks(1, "new")
		h.approvals.Expect(dataLinkApprovalSubject("old", "new", frozenRevisionForInputs(t, next)))
		h.dropHostResponse(t, hostcontract.ActionReconcile)
		request := updateRequest(t, created.Id, created.Properties, old, next)
		if _, err := updateProviderResource(t, h, request, next, hostcontract.ActionInspect, hostcontract.ActionReconcile); err == nil {
			t.Fatal("lost response Update succeeded")
		}
		effects := dockerEffects(t, h)
		assertDroppedHostResponse(t, h, hostcontract.ActionReconcile)
		updated, err := updateProviderResource(t, h, request, next, hostcontract.ActionInspect)
		if err != nil || updated == nil {
			t.Fatalf("same revision retry: %v", err)
		}
		if got := dockerEffects(t, h); !reflect.DeepEqual(got, effects) {
			t.Fatalf("retry Docker effects = %#v, want %#v", got, effects)
		}
		if h.approvals.Count() != 1 {
			t.Fatalf("retry approval count = %d, want 1", h.approvals.Count())
		}
		assertCompletedReconcile(t, h, unmarshalProperties(t, updated.Properties), 1)
		h.approvals.AssertExpectedConsumed(t)
	})

	t.Run("C-different-revision-cannot-reuse-approval", func(t *testing.T) {
		h := startProviderWithApproval(t, providerBinary, approvalExact)
		old := createInputsWithDataLinks(1, "old")
		created := createProviderResource(t, h, old)
		createdEffects := dockerEffects(t, h)
		first := createInputsWithDataLinks(1, "new")
		firstRevision := frozenRevisionForInputs(t, first)
		firstRequest := updateRequest(t, created.Id, created.Properties, old, first)
		h.approvals.Expect(dataLinkApprovalSubject("old", "new", firstRevision))
		h.dropHostResponse(t, hostcontract.ActionReconcile)
		if _, err := updateProviderResource(t, h, firstRequest, first, hostcontract.ActionInspect, hostcontract.ActionReconcile); err == nil {
			t.Fatal("lost exact approval response succeeded")
		}
		firstEffects := dockerEffects(t, h)
		assertDroppedHostResponse(t, h, hostcontract.ActionReconcile)
		recovered, err := updateProviderResource(t, h, firstRequest, first, hostcontract.ActionInspect)
		if err != nil || recovered == nil {
			t.Fatalf("same revision recovery: %v", err)
		}
		if got := dockerEffects(t, h); !reflect.DeepEqual(got, firstEffects) {
			t.Fatalf("same revision recovery effects = %#v, want unchanged %#v", got, firstEffects)
		}
		second := createInputsWithDataLinks(1, "different")
		secondRevision := frozenRevisionForInputs(t, second)
		h.approvals.Expect(dataLinkApprovalSubject("new", "different", secondRevision))
		updated, err := updateProviderResource(t, h, updateRequest(t, created.Id, recovered.Properties, first, second), second, hostcontract.ActionInspect, hostcontract.ActionReconcile, hostcontract.ActionInspect)
		if err != nil || updated == nil {
			t.Fatalf("different revision Update with new exact approval: %v", err)
		}
		subjects := h.approvals.Subjects()
		if len(subjects) != 2 {
			t.Fatalf("different revision approval requests = %d, want 2", len(subjects))
		}
		assertDataLinkApprovalSubject(t, subjects[0], "old", "new", firstRevision)
		assertDataLinkApprovalSubject(t, subjects[1], "new", "different", secondRevision)
		if subjects[0].TargetRevision == subjects[1].TargetRevision {
			t.Fatal("different revision reused the first approval subject")
		}
		secondEffects := dockerEffects(t, h)
		assertReconcileEffectDelta(t, h, createdEffects, firstEffects, old, first)
		if len(secondEffects) < len(firstEffects) || !reflect.DeepEqual(secondEffects[:len(firstEffects)], firstEffects) {
			t.Fatalf("second Update duplicated completed first-operation effects: before %#v, after %#v", firstEffects, secondEffects)
		}
		assertReconcileEffectDelta(t, h, firstEffects, secondEffects, first, second)
		assertCompletedReconcileForRevision(t, h, unmarshalProperties(t, updated.Properties), secondRevision, 1)
		h.approvals.AssertExpectedConsumed(t)
	})

	t.Run("D-retire-response-loss-preserves-data-and-removes-secrets", func(t *testing.T) {
		h := startProviderWithApproval(t, providerBinary, approvalExact)
		created := createProviderResource(t, h, createInputs())
		writeDataSentinel(t, h)
		drained := createInputsWithTarget(hostcontract.Target{ReleaseArtifact: ciRelease})
		beforeDrain := dockerEffects(t, h)
		preDrainApps := inventoryApps(t, h)
		drain, err := updateProviderResource(t, h, updateRequest(t, created.Id, created.Properties, createInputs(), drained), drained, hostcontract.ActionInspect, hostcontract.ActionReconcile, hostcontract.ActionInspect)
		if err != nil {
			 t.Fatalf("drain before retire: %v", err)
		}
		assertDrainEffectDelta(t, h, beforeDrain, dockerEffects(t, h), preDrainApps)
		beforeRetire := dockerEffects(t, h)
		preRetire := runtimeState(t, h).Journal
		h.approvals.Expect(retireApprovalSubject(t, unmarshalProperties(t, drain.Properties), drained))
		h.dropHostResponse(t, hostcontract.ActionRetirePreserveData)
		request := deleteRequest(t, created.Id, drain.Properties, drained)
		if _, err := deleteProviderResource(t, h, request, drained, hostcontract.ActionInspect, hostcontract.ActionRetirePreserveData); err == nil {
			t.Fatal("lost retire response succeeded")
		}
		assertDroppedHostResponse(t, h, hostcontract.ActionRetirePreserveData)
		if _, err := deleteProviderResource(t, h, request, drained, hostcontract.ActionInspect); err != nil {
			t.Fatalf("retire retry: %v", err)
		}
		if h.approvals.Count() != 1 {
			t.Fatalf("retire retry approvals = %d, want 1", h.approvals.Count())
		}
		h.approvals.AssertExpectedConsumed(t)
		assertRetiredPreservingData(t, h, beforeRetire, preRetire)
		assertNoSecretCanary(t, h, "")
	})

	t.Run("E-secret-canary-is-confined-to-env-before-retire", func(t *testing.T) {
		h := startProviderWithApproval(t, providerBinary, approvalExact)
		created := createProviderResource(t, h, createInputs())
		assertSecretIsolation(t, h, created)
	})
}

// TestProviderProcessReachesSharedTemporaryRuntimeServe is the permanent 7a
// boundary oracle for Provider Create through the shared temporary Runtime.
func TestProviderProcessReachesSharedTemporaryRuntimeServe(t *testing.T) {
	h := startProvider(t)
	inputs := createInputs()
	writeTargetExpectation(t, h, inputs)
	writeHostActionQueue(t, h, hostcontract.ActionInspect)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := h.client.Create(ctx, &pulumirpc.CreateRequest{
		Urn:        "urn:pulumi:test::runtime::sub2api-host:index:Host::edge",
		Properties: rpcProperties(t, inputs),
	})
	if err != nil {
		if detail, readErr := os.ReadFile(filepath.Join(h.trace, "docker.error")); readErr == nil {
			t.Fatalf("Provider Create through shared temporary Runtime: %v; Docker fixture: %s", err, detail)
		}
		t.Fatalf("Provider Create through shared temporary Runtime: %v", err)
	}
	if response.Id == "" || response.Properties == nil {
		t.Fatal("Provider Create returned no checkpoint")
	}
	if response.Id != expectedStableID() {
		t.Fatalf("Create ID = %q, want %q", response.Id, expectedStableID())
	}
	checkpoint := unmarshalProperties(t, response.Properties)
	assertCreateCheckpoint(t, checkpoint, inputs)
	if got, want := string(mustRead(t, filepath.Join(h.trace, "request.sha256"))), "operationDigest="+expectedRequestDigest(t)+"\naction=reconcile\n"; got != want {
		t.Fatalf("bootstrap request digest = %q, want %q", got, want)
	}
	assertSSHRecords(t, h.trace)
	assertBootstrapMetadata(t, h.trace)
	assertDockerTrace(t, h)
	assertRuntimePersistence(t, h, checkpoint)
	for _, name := range []string{"machine", "ownership", "appliedRevision", "observation"} {
		value, _ := checkpoint.GetOk(name)
		if strings.Contains(fmt.Sprint(value), ciSecret) {
			t.Fatalf("non-secret checkpoint output %s leaked secret canary", name)
		}
	}
}

type providerProcess struct {
	client    pulumirpc.ResourceProviderClient
	caseDir   string
	trace     string
	root      string
	approvals *approvalRecorder
}

type targetExpectation struct {
	Revision    string              `json:"revision"`
	Apps        []expectedTargetApp `json:"apps"`
	CurrentApps []expectedTargetApp `json:"currentApps,omitempty"`
}
type expectedTargetApp struct {
	ID            string `json:"id"`
	Image         string `json:"image"`
	Hostname      string `json:"hostname"`
	ReadinessPath string `json:"readinessPath"`
	DrainSeconds  int    `json:"drainSeconds"`
	DataLinks     []hostcontract.DataLink `json:"dataLinks,omitempty"`
}

func startProvider(t *testing.T) *providerProcess {
	return startProviderApproval(t, buildProviderForPrerequisite(t), approvalAbsent, false)
}

func startProviderWithApproval(t *testing.T, providerBinary string, decision approvalDecision) *providerProcess {
	return startProviderApproval(t, providerBinary, decision, true)
}

func buildProviderForMatrix(t *testing.T) string {
	t.Helper()
	return buildProvider(t, t.TempDir())
}

func buildProviderForPrerequisite(t *testing.T) string {
	t.Helper()
	return buildProvider(t, t.TempDir())
}

func buildProvider(t *testing.T, directory string) string {
	t.Helper()
	workspace := repositoryRoot(t)
	providerPath := filepath.Join(directory, "pulumi-resource-sub2api-host")
	writeArtifactFixture(t, filepath.Dir(filepath.Dir(providerPath)))
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelBuild()
	args := []string{"build", "-o", providerPath, "./cmd/pulumi-resource-sub2api-host"}
	if providerRaceEnabled {
		args = append([]string{"build", "-race", "-o", providerPath}, args[3:]...)
	}
	build := exec.CommandContext(buildCtx, "go", args...)
	build.Dir = workspace
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test Provider: %v: %s", err, output)
	}
	return providerPath
}

func startProviderApproval(t *testing.T, providerBinary string, decision approvalDecision, lifecycleScenario bool) *providerProcess {
	t.Helper()
	workspace := repositoryRoot(t)
	caseDir := t.TempDir()
	root, machinePath := runtimePaths(t, caseDir)
	trace, binDir := filepath.Join(caseDir, "trace"), filepath.Join(caseDir, "bin")
	for _, dir := range []string{trace, binDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeArtifactFixture(t, caseDir)
	writeSSHFixture(t, filepath.Join(binDir, "ssh"))
	writeDockerFixture(t, filepath.Join(binDir, "docker"))
	writeExpectedSSHCommands(t, trace)
	clientLogDir := filepath.Join(caseDir, "ssh-client-logs")
	if err := os.MkdirAll(clientLogDir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(providerBinary)
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	approvals := &approvalRecorder{decision: decision}
	var approvalFile *os.File
	if decision != approvalAbsent {
		approvals, approvalFile = startApprovalServer(t, decision)
		cmd.ExtraFiles = []*os.File{approvalFile}
	}
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PROVIDER_RUNTIME_TEST_BINARY="+os.Args[0],
		"PROVIDER_RUNTIME_ROOT="+root,
		"PROVIDER_RUNTIME_MACHINE_ID="+machinePath,
		"PROVIDER_RUNTIME_TRACE="+trace,
		"PROVIDER_RUNTIME_ARTIFACT="+filepath.Join(caseDir, "artifacts", "sub2api-host", "host-amd64"),
		"PROVIDER_RUNTIME_REQUEST_DIGEST="+filepath.Join(trace, "request.sha256"),
		"PROVIDER_RUNTIME_DOCKER_LOG="+filepath.Join(trace, "docker.args"),
		"PROVIDER_RUNTIME_DOCKER_STATE="+filepath.Join(trace, "docker-state"),
		"PROVIDER_RUNTIME_PROBE_COMMAND="+filepath.Join(trace, "probe.command"),
		"PROVIDER_RUNTIME_BOOTSTRAP_COMMAND="+filepath.Join(trace, "bootstrap.command"),
		"PROVIDER_RUNTIME_HOST_COMMAND="+filepath.Join(trace, "host.command"),
		"PROVIDER_RUNTIME_CLIENT_LOG_DIR="+clientLogDir,
		"TMPDIR="+clientLogDir,
		"PROVIDER_RUNTIME_LIFECYCLE_SCENARIO="+strconv.FormatBool(lifecycleScenario),
	)
	if approvalFile != nil {
		cmd.Env = append(cmd.Env, "SUB2API_HOST_APPROVAL_FD=3")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if approvalFile != nil {
		if err := approvalFile.Close(); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	cleanup := &providerCleanup{cmd: cmd, done: done}
	t.Cleanup(func() { cleanupProvider(t, cleanup) })
	identity, err := providerIdentity(cmd.Process.Pid, providerBinary)
	if err != nil {
		t.Fatal(err)
	}
	cleanup.identity = &identity
	port := readProviderPort(t, stdout)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, net.JoinHostPort("127.0.0.1", port), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	t.Cleanup(func() { cleanupScriptedSSH(t, trace) })
	if lifecycleScenario {
		t.Cleanup(func() { assertHostActionQueueEmpty(t, &providerProcess{trace: trace}) })
	}
	client := pulumirpc.NewResourceProviderClient(conn)
	if _, err := configureProvider(t, client); err != nil {
		t.Fatal(err)
	}
	return &providerProcess{client: client, caseDir: caseDir, trace: trace, root: root, approvals: approvals}
}

func createInputs() property.Map {
	target := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}}
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: ciSecret}}}
	return property.NewMap(map[string]property.Value{
		"resource": jsonProperty(hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}),
		"server":   jsonProperty(hostcontract.ServerTarget{SSHAlias: "edge"}),
		"target":   jsonProperty(target),
		"secrets":  jsonProperty(secrets).WithSecret(true),
	})
}

func assertCreateCheckpoint(t *testing.T, checkpoint, inputs property.Map) {
	t.Helper()
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		got, ok := checkpoint.GetOk(name)
		want, _ := inputs.GetOk(name)
		if !ok || !got.Equals(want) {
			t.Fatalf("Create checkpoint did not preserve input %s", name)
		}
	}
	secrets, _ := checkpoint.GetOk("secrets")
	if !secrets.Secret() {
		t.Fatal("Create checkpoint lost Pulumi secret marking")
	}
	for _, name := range []string{"machine", "ownership", "appliedRevision", "observation"} {
		value, ok := checkpoint.GetOk(name)
		if !ok || value.HasComputed() || value.IsNull() {
			t.Fatalf("Create checkpoint has no concrete %s", name)
		}
	}
	machine, ownership, revision, observation, err := checkpointValues(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if machine.Value != "mid1:0911601b3b0a5f6fdc51f3661518ee20e26ea0cbadfb4f7283e5b1f288941f54" {
		t.Fatalf("machine = %q", machine.Value)
	}
	if revision != expectedRevision(t) {
		t.Fatalf("applied revision = %q, want %q", revision, expectedRevision(t))
	}
	if ownership.Value == "" || observation.Machine != machine || observation.Ownership != ownership || observation.AppliedRevision != revision || observation.HostRelease != ciRelease || !observation.Ready || observation.Drifted || len(observation.Apps) != 1 || observation.Apps[0] != (hostcontract.AppObservation{ID: "api", ActiveImage: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ready: true}) {
		t.Fatal("Create checkpoint observation is not exact")
	}
}

func checkpointValues(checkpoint property.Map) (hostcontract.MachineIdentity, hostcontract.OwnershipIdentity, string, hostcontract.StableObservation, error) {
	var machine hostcontract.MachineIdentity
	var ownership hostcontract.OwnershipIdentity
	var observation hostcontract.StableObservation
	for _, field := range []struct {
		name string
		into any
	}{{"machine", &machine}, {"ownership", &ownership}, {"observation", &observation}} {
		value, _ := checkpoint.GetOk(field.name)
		if err := decodeProperty(value, field.into); err != nil {
			return machine, ownership, "", observation, err
		}
	}
	revision, _ := checkpoint.GetOk("appliedRevision")
	if !revision.IsString() {
		return machine, ownership, "", observation, errors.New("checkpoint revision is not a string")
	}
	return machine, ownership, revision.AsString(), observation, nil
}

func decodeProperty(value property.Value, into any) error {
	raw, err := propertyRaw(value)
	if err != nil {
		return err
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func propertyRaw(value property.Value) (any, error) {
	if value.IsString() {
		return value.AsString(), nil
	}
	if value.IsBool() {
		return value.AsBool(), nil
	}
	if value.IsNumber() {
		return value.AsNumber(), nil
	}
	if value.IsMap() {
		out := map[string]any{}
		var err error
		value.AsMap().All(func(k string, v property.Value) bool { out[k], err = propertyRaw(v); return err == nil })
		return out, err
	}
	if value.IsArray() {
		out := make([]any, len(value.AsArray().AsSlice()))
		for i, v := range value.AsArray().AsSlice() {
			var err error
			out[i], err = propertyRaw(v)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, errors.New("unsupported checkpoint property")
}

func unmarshalProperties(t *testing.T, encoded *structpb.Struct) property.Map {
	t.Helper()
	decoded, err := plugin.UnmarshalProperties(encoded, plugin.MarshalOptions{KeepUnknowns: true, KeepSecrets: true, KeepResources: true})
	if err != nil {
		t.Fatal(err)
	}
	values := property.NewMap(nil)
	for key, value := range decoded {
		values = values.Set(string(key), resource.FromResourcePropertyValue(value))
	}
	return values
}

func runtimePaths(t *testing.T, root string) (string, string) {
	t.Helper()
	machinePath := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machinePath, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "host-state"), machinePath
}

func writeArtifactFixture(t *testing.T, caseDir string) {
	t.Helper()
	root := filepath.Join(caseDir, "artifacts", "sub2api-host")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := []byte("#!/bin/sh\nset -eu\ncase \"$1\" in\ninstall-attest) printf %s sub2api-bootstrap-attested-v1 >&3 ;;\nbootstrap-stdio) exec env SUB2API_PROVIDER_RUNTIME_CI_HELPER=1 SUB2API_PROVIDER_RUNTIME_MODE=bootstrap \"$PROVIDER_RUNTIME_TEST_BINARY\" -test.run '^TestProviderRuntimeCIHelper$' ;;\nstdio) exec env SUB2API_PROVIDER_RUNTIME_CI_HELPER=1 SUB2API_PROVIDER_RUNTIME_MODE=serve \"$PROVIDER_RUNTIME_TEST_BINARY\" -test.run '^TestProviderRuntimeCIHelper$' ;;\n*) exit 2 ;;\nesac\n")
	sum := sha256.Sum256(binary)
	for _, name := range []string{"host-amd64", "host-arm64"} {
		if err := os.WriteFile(filepath.Join(root, name), binary, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifest := fmt.Sprintf(`{"schemaVersion":1,"release":%q,"linux-amd64":{"path":"host-amd64","size":%d,"sha256":%q},"linux-arm64":{"path":"host-arm64","size":%d,"sha256":%q}}`, ciRelease, len(binary), hex.EncodeToString(sum[:]), len(binary), hex.EncodeToString(sum[:]))
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSSHFixture(t *testing.T, destination string) {
	t.Helper()
	source := filepath.Join(repositoryRoot(t), "internal", "integration", "providerruntime", "testdata", "ssh-runtime.sh")
	b, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, b, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeDockerFixture(t *testing.T, destination string) {
	t.Helper()
	const shim = "#!/bin/sh\nexec env SUB2API_PROVIDER_RUNTIME_CI_HELPER=1 SUB2API_PROVIDER_RUNTIME_MODE=docker \"$PROVIDER_RUNTIME_TEST_BINARY\" -test.run '^TestProviderRuntimeCIHelper$' -- \"$@\"\n"
	if err := os.WriteFile(destination, []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
}

type dockerTrace struct {
	Ownership      string                     `json:"ownership"`
	OwnershipLabel string                     `json:"ownershipLabel"`
	Network        string                     `json:"network"`
	Networks       map[string]dockerNetwork   `json:"networks"`
	Containers     map[string]dockerContainer `json:"containers"`
	Effects        []dockerEffect             `json:"effects"`
	Reads          []string                   `json:"reads"`
}

type dockerNetwork struct{ Owner, Label string }
type dockerEffect struct {
	Action, Name, AppToken, Slot, OwnerDigest, TargetDigest string
}
type dockerContainer struct {
	Owner, Target, Image, Slot, AppToken string
}

// dockerFixture is a stateful, strict Docker oracle. It accepts only command
// families emitted by Runtime and derives every accepted name and label from
// the ownership captured from the runtime state.
func dockerFixture(argv []string) error {
	separator := -1
	for i, arg := range argv {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator == len(argv)-1 {
		return errors.New("missing docker argv")
	}
	args := append([]string(nil), argv[separator+1:]...)
	stateDir, tracePath, root := os.Getenv("PROVIDER_RUNTIME_DOCKER_STATE"), os.Getenv("PROVIDER_RUNTIME_DOCKER_LOG"), os.Getenv("PROVIDER_RUNTIME_ROOT")
	if stateDir == "" || tracePath == "" || root == "" {
		return errors.New("missing docker fixture environment")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	statePath := filepath.Join(stateDir, "state.json")
	state := dockerTrace{Networks: map[string]dockerNetwork{}, Containers: map[string]dockerContainer{}}
	if b, readErr := os.ReadFile(statePath); readErr == nil {
		if json.Unmarshal(b, &state) != nil {
			return errors.New("invalid docker state")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if err := state.apply(root, args); err != nil {
		return err
	}
	if err := writeJSONAtomic(statePath, state); err != nil {
		return err
	}
	if err := appendDockerTrace(tracePath, state); err != nil {
		return err
	}
	return nil
}

func (s *dockerTrace) apply(root string, args []string) error {
	listNetwork := func() (string, bool) {
		return "{{.Name}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.network\"}}", len(args) == 6 && args[0] == "network" && args[1] == "ls" && args[2] == "--filter" && strings.HasPrefix(args[3], "name=^s2h-net-") && strings.HasSuffix(args[3], "$") && args[4] == "--format" && args[5] == "{{.Name}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.network\"}}"
	}
	listContainer := func() (string, bool) {
		return "{{.Names}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.target\"}}", len(args) == 7 && args[0] == "container" && args[1] == "ls" && args[2] == "--all" && args[3] == "--filter" && strings.HasPrefix(args[4], "name=^/s2h-") && strings.HasSuffix(args[4], "$") && args[5] == "--format" && args[6] == "{{.Names}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.target\"}}"
	}
	if len(args) == 7 && args[0] == "container" && args[1] == "ls" && args[2] == "--all" && args[3] == "--filter" && args[4] == "label=sub2api.host" && args[5] == "--format" && args[6] == "{{.Names}}\t{{index .Labels \"sub2api.host\"}}" {
		s.Reads = append(s.Reads, "bootstrap-container-list")
		return nil
	}
	if len(args) == 6 && args[0] == "network" && args[1] == "ls" && args[2] == "--filter" && args[3] == "label=sub2api.host" && args[4] == "--format" && args[5] == "{{.Name}}\t{{index .Labels \"sub2api.host\"}}" {
		s.Reads = append(s.Reads, "bootstrap-network-list")
		return nil
	}
	if _, ok := listNetwork(); ok {
		name := strings.TrimSuffix(strings.TrimPrefix(args[3], "name=^"), "$")
		var runtimeState hostruntime.State
		if json.Unmarshal(mustReadFixture(filepath.Join(root, "state.json")), &runtimeState) != nil || !validOwnership(runtimeState.Ownership.Value) || name != "s2h-net-"+fixtureToken("test", "edge", runtimeState.Ownership.Value) {
			return errors.New("invalid network list")
		}
		s.Reads = append(s.Reads, "network-list")
		if n, exists := s.Networks[name]; exists {
			_, _ = io.WriteString(os.Stdout, name+"\t"+n.Owner+"\t"+n.Label+"\n")
		}
		return nil
	}
	if _, ok := listContainer(); ok {
		name := strings.TrimSuffix(strings.TrimPrefix(args[4], "name=^/"), "$")
		if _, known := s.Containers[name]; !known {
			var runtimeState hostruntime.State
			var expectation targetExpectation
			if json.Unmarshal(mustReadFixture(filepath.Join(root, "state.json")), &runtimeState) != nil || json.Unmarshal(mustReadFixture(filepath.Join(os.Getenv("PROVIDER_RUNTIME_TRACE"), "target.expectation.json")), &expectation) != nil {
				return errors.New("invalid container list")
			}
			valid := false
			for _, app := range append(append([]expectedTargetApp(nil), expectation.Apps...), expectation.CurrentApps...) {
				token := fixtureToken("app", app.ID)
				for _, slot := range []string{"blue", "green"} {
					if name == "s2h-"+fixtureToken("test", "edge", runtimeState.Ownership.Value, "app", token, slot) {
						valid = true
					}
				}
			}
			if !valid {
				return errors.New("invalid container list")
			}
		}
		s.Reads = append(s.Reads, "container-list")
		if c, exists := s.Containers[name]; exists {
			_, _ = io.WriteString(os.Stdout, name+"\t"+c.Owner+"\t"+c.Target+"\n")
		}
		return nil
	}
	if len(args) == 7 && args[0] == "network" && args[1] == "create" && args[2] == "--label" && strings.HasPrefix(args[3], "sub2api.host=") && args[4] == "--label" && strings.HasPrefix(args[5], "sub2api.host.network=") {
		owner, label, name := strings.TrimPrefix(args[3], "sub2api.host="), strings.TrimPrefix(args[5], "sub2api.host.network="), args[6]
		if !validOwnershipLabel(owner) || !strings.HasPrefix(label, "s2hnet1:") || !strings.HasPrefix(name, "s2h-net-") || s.Networks[name].Owner != "" {
			return errors.New("invalid network create")
		}
		var runtimeState hostruntime.State
		if json.Unmarshal(mustReadFixture(filepath.Join(root, "state.json")), &runtimeState) != nil || !validOwnership(runtimeState.Ownership.Value) {
			return errors.New("runtime ownership unavailable")
		}
		if owner != "s2h1:"+fixtureToken("test", "edge", runtimeState.Ownership.Value, "network", "", "") || label != "s2hnet1:"+fixtureToken("test", "edge", runtimeState.Ownership.Value) || name != "s2h-net-"+fixtureToken("test", "edge", runtimeState.Ownership.Value) {
			return errors.New("network ownership mismatch")
		}
		s.Ownership, s.OwnershipLabel, s.Network = runtimeState.Ownership.Value, owner, name
		s.Networks[name] = dockerNetwork{owner, label}
		s.effect("network-create", name, "", "", owner, label)
		return nil
	}
	if len(args) == 17 && args[0] == "run" && args[1] == "-d" && args[2] == "--restart" && args[3] == "unless-stopped" && args[4] == "--label" && strings.HasPrefix(args[5], "sub2api.host=") && args[6] == "--label" && strings.HasPrefix(args[7], "sub2api.host.target=") && args[8] == "--name" && args[10] == "--network" && args[11] == s.Network && args[12] == "--env-file" && args[14] == "-v" {
		name, owner, target, image := args[9], strings.TrimPrefix(args[5], "sub2api.host="), strings.TrimPrefix(args[7], "sub2api.host.target="), args[16]
		var runtimeState hostruntime.State
		if json.Unmarshal(mustReadFixture(filepath.Join(root, "state.json")), &runtimeState) != nil || runtimeState.Journal == nil || runtimeState.Journal.Key.TargetRevision == "" {
			return errors.New("invalid container run")
		}
		var expectation targetExpectation
		if json.Unmarshal(mustReadFixture(filepath.Join(os.Getenv("PROVIDER_RUNTIME_TRACE"), "target.expectation.json")), &expectation) != nil || expectation.Revision != runtimeState.Journal.Key.TargetRevision {
			return errors.New("missing target expectation")
		}
		var app expectedTargetApp
		var appToken string
		slot := ""
		for _, candidate := range expectation.Apps {
			token := fixtureToken("app", candidate.ID)
			for _, candidateSlot := range []string{"blue", "green"} {
				if name == "s2h-"+fixtureToken("test", "edge", runtimeState.Ownership.Value, "app", token, candidateSlot) {
					app, appToken, slot = candidate, token, candidateSlot
				}
			}
		}
		if appToken == "" {
			return errors.New("unexpected target app")
		}
		revision := expectation.Revision
		wantName := "s2h-" + fixtureToken("test", "edge", runtimeState.Ownership.Value, "app", appToken, slot)
		wantOwner := "s2h1:" + fixtureToken("test", "edge", runtimeState.Ownership.Value, "app", appToken, slot)
		wantTarget := "s2ht1:" + fixtureToken("app", appToken, slot, revision, app.Image, "", "0", "false")
		wantEnv := filepath.Join(root, "runtime", "managed", "env-"+appToken+fixtureToken(revision))
		wantData := filepath.Join(root, "runtime", "data", fixtureToken("app-data", appToken)) + ":/app/data"
		if s.Ownership == "" || name != wantName || owner != wantOwner || target != wantTarget || image != app.Image || args[13] != wantEnv || args[15] != wantData || s.Containers[name].Owner != "" {
			return errors.New("invalid container run")
		}
		s.Containers[name] = dockerContainer{Owner: owner, Target: target, Image: image, Slot: slot, AppToken: appToken}
		s.effect("container-run", name, appToken, slot, owner, target)
		return nil
	}
	if len(args) == 7 && args[0] == "exec" && args[2] == "wget" && args[3] == "-q" && args[4] == "-O" && args[5] == "/dev/null" {
		container, ok := s.Containers[args[1]]
		if !ok {
			return errors.New("exec absent container")
		}
		var expectation targetExpectation
		if json.Unmarshal(mustReadFixture(filepath.Join(os.Getenv("PROVIDER_RUNTIME_TRACE"), "target.expectation.json")), &expectation) != nil {
			return errors.New("missing target expectation")
		}
		matched := false
		for _, app := range append(append([]expectedTargetApp(nil), expectation.Apps...), expectation.CurrentApps...) {
			if container.Image == app.Image && args[6] == "http://localhost:8080"+app.ReadinessPath {
				matched = true
			}
		}
		if !matched {
			return errors.New("invalid container probe")
		}
		s.Reads = append(s.Reads, "container-probe")
		return nil
	}
	if len(args) == 4 && args[0] == "stop" && args[1] == "--time" && args[2] == "30" {
		container, ok := s.Containers[args[3]]
		if !ok || !validDestructiveContainer(root, args[3], container) {
			return errors.New("stop absent container")
		}
		s.effect("container-stop", args[3], container.AppToken, container.Slot, container.Owner, container.Target)
		return nil
	}
	if len(args) == 3 && args[0] == "rm" && args[1] == "-f" {
		container, ok := s.Containers[args[2]]
		if !ok || !validDestructiveContainer(root, args[2], container) {
			return errors.New("rm absent container")
		}
		delete(s.Containers, args[2])
		s.effect("container-rm", args[2], container.AppToken, container.Slot, container.Owner, container.Target)
		return nil
	}
	if len(args) == 3 && args[0] == "network" && args[1] == "rm" {
		network, ok := s.Networks[args[2]]
		var state hostruntime.State
		if !ok || json.Unmarshal(mustReadFixture(filepath.Join(root, "state.json")), &state) != nil || !validOwnership(state.Ownership.Value) {
			return errors.New("rm absent network")
		}
		name := "s2h-net-" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value)
		owner := "s2h1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "network", "", "")
		label := "s2hnet1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value)
		if args[2] != name || network.Owner != owner || network.Label != label {
			return errors.New("rm absent network")
		}
		delete(s.Networks, args[2])
		s.effect("network-rm", args[2], "", "", network.Owner, network.Label)
		return nil
	}
	return fmt.Errorf("unsupported docker argv: %q", args)
}

func validDestructiveContainer(root, name string, container dockerContainer) bool {
	var state hostruntime.State
	if json.Unmarshal(mustReadFixture(filepath.Join(root, "state.json")), &state) != nil {
		return false
	}
	if len(container.AppToken) != 24 || strings.Trim(container.AppToken, "0123456789abcdef") != "" || (container.Slot != "blue" && container.Slot != "green") {
		return false
	}
	var inventory struct {
		Objects []struct {
			Role, AppToken, Name, Image, Revision, Active string
		} `json:"objects"`
	}
	if json.Unmarshal(mustReadFixture(filepath.Join(root, "runtime", "managed", "inventory.json")), &inventory) != nil {
		return false
	}
	var object *struct { Role, AppToken, Name, Image, Revision, Active string }
	for i := range inventory.Objects {
		if inventory.Objects[i].Role == "app" && inventory.Objects[i].AppToken == container.AppToken {
			object = &inventory.Objects[i]
		}
	}
	if object == nil || object.Active != container.Slot || object.Image != container.Image || object.Revision == "" {
		return false
	}
	var expectation targetExpectation
	if json.Unmarshal(mustReadFixture(filepath.Join(os.Getenv("PROVIDER_RUNTIME_TRACE"), "target.expectation.json")), &expectation) != nil {
		return false
	}
	member := false
	for _, app := range append(append([]expectedTargetApp(nil), expectation.Apps...), expectation.CurrentApps...) {
		if fixtureToken("app", app.ID) == container.AppToken && app.Image == container.Image {
			member = true
		}
	}
	if !member {
		return false
	}
	wantName := "s2h-" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "app", container.AppToken, container.Slot)
	wantOwner := "s2h1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "app", container.AppToken, container.Slot)
	wantTarget := "s2ht1:" + fixtureToken("app", container.AppToken, container.Slot, object.Revision, object.Image, "", "0", "false")
	return name == wantName && object.Name == wantName && container.Owner == wantOwner && container.Target == wantTarget
}
func (s *dockerTrace) effect(action, name, appToken, slot, owner, target string) {
	ownerSum, targetSum := sha256.Sum256([]byte(owner)), sha256.Sum256([]byte(target))
	s.Effects = append(s.Effects, dockerEffect{Action: action, Name: name, AppToken: appToken, Slot: slot, OwnerDigest: hex.EncodeToString(ownerSum[:]), TargetDigest: hex.EncodeToString(targetSum[:])})
}
func appendDockerTrace(path string, state dockerTrace) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, value := range state.Effects {
		if _, err = fmt.Fprintf(f, "%s %s %s %s %s %s\n", value.Action, value.Name, value.AppToken, value.Slot, value.OwnerDigest, value.TargetDigest); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err = os.WriteFile(temporary, b, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
func validOwnership(value string) bool {
	if !strings.HasPrefix(value, "oid1:") || len(value) != 69 {
		return false
	}
	for _, c := range value[5:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validOwnershipLabel(value string) bool {
	if !strings.HasPrefix(value, "s2h1:") || len(value) != 29 {
		return false
	}
	for _, c := range value[5:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func mustReadFixture(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}
func fixtureToken(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}
func expectedRevision(t *testing.T) string { t.Helper(); return expectedRevisionNoTest() }
func expectedRevisionNoTest() string {
	return frozenRevision(hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}}, hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: ciSecret}}})
}

func assertDockerTrace(t *testing.T, h *providerProcess) {
	t.Helper()
	var trace dockerTrace
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.trace, "docker-state", "state.json")), &trace); err != nil {
		t.Fatal(err)
	}
	if !validOwnership(trace.Ownership) || !validOwnershipLabel(trace.OwnershipLabel) {
		t.Fatalf("captured ownership values are invalid")
	}
	if len(trace.Effects) != 2 || trace.Effects[0].Action != "network-create" || trace.Effects[1].Action != "container-run" || trace.Effects[1].AppToken != fixtureToken("app", "api") || trace.Effects[1].Slot != "green" {
		t.Fatalf("Create Docker effects = %#v, want one network and one candidate run", trace.Effects)
	}
	if len(trace.Reads) == 0 || len(trace.Containers) != 1 || len(trace.Networks) != 1 {
		t.Fatal("Create Docker model did not retain exact live inventory")
	}
	if trace.OwnershipLabel != "s2h1:"+fixtureToken("test", "edge", trace.Ownership, "network", "", "") {
		t.Fatal("captured Docker ownership label is not derived from runtime ownership")
	}
}

func assertRuntimePersistence(t *testing.T, h *providerProcess, checkpoint property.Map) {
	t.Helper()
	machine, ownership, revision, observation, err := checkpointValues(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var state hostruntime.State
	statePath := filepath.Join(h.root, "state.json")
	if err := json.Unmarshal(mustRead(t, statePath), &state); err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || state.Resource != (hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}) || state.Machine != machine || state.Ownership != ownership || state.AppliedRevision != revision || !reflect.DeepEqual(state.Observation, observation) || state.Journal == nil || state.Journal.Status != "complete" || state.Journal.Approval != nil || state.Journal.Result == nil || state.LastOperation != nil || state.Retirement != nil {
		t.Fatal("state journal/checkpoint semantics differ")
	}
	if state.Journal.Key != (hostcontract.OperationKey{Resource: state.Resource, Action: hostcontract.ActionReconcile, TargetRevision: revision, PriorAppliedRevision: expectedPriorRevision(t)}) || *state.Journal.Result != (hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: revision}) {
		t.Fatal("state journal key/result differ")
	}
	var trace dockerTrace
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.trace, "docker-state", "state.json")), &trace); err != nil {
		t.Fatal(err)
	}
	if ownership.Value != trace.Ownership {
		t.Fatal("checkpoint ownership is not the captured Docker oracle")
	}
	appToken := fixtureToken("app", "api")
	appName := "s2h-" + fixtureToken("test", "edge", ownership.Value, "app", appToken, "green")
	wantInventory := map[string]any{"version": float64(2), "resource": map[string]any{"environment": "test", "serverKey": "edge"}, "ownership": map[string]any{"value": ownership.Value}, "objects": []any{map[string]any{"role": "app", "appToken": appToken, "name": appName, "image": "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "revision": revision, "active": "green", "env": "env-" + appToken + fixtureToken(revision), "dataIdentity": map[string]any{"kind": ""}, "hostname": "api.example", "readinessPath": "/ready", "drainSeconds": float64(30)}}}
	inventoryPath := filepath.Join(h.root, "runtime", "managed", "inventory.json")
	info, statErr := os.Stat(inventoryPath)
	if statErr != nil {
		t.Fatalf("stat inventory: %v", statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("inventory mode = %v, want 0600", info.Mode().Perm())
	}
	var gotInventory any
	if err := json.Unmarshal(mustRead(t, inventoryPath), &gotInventory); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotInventory, wantInventory) {
		gotJSON, gotErr := json.Marshal(gotInventory)
		wantJSON, wantErr := json.Marshal(wantInventory)
		if gotErr != nil || wantErr != nil {
			t.Fatal("inventory mismatch diagnostic encoding failed")
		}
		if strings.Contains(string(gotJSON), ciSecret) || strings.Contains(string(wantJSON), ciSecret) {
			t.Fatal("inventory mismatch diagnostic would leak secret")
		}
		t.Fatalf("inventory content is not exact\ngot: %s\nwant: %s", gotJSON, wantJSON)
	}
	envPath := filepath.Join(h.root, "runtime", "managed", "env-"+appToken+fixtureToken(revision))
	if got := string(mustRead(t, envPath)); got != "JWT_SECRET="+ciSecret+"\n" {
		t.Fatalf("app env content = %q", got)
	}
	if err := filepath.Walk(h.root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(b), ciSecret) && path != envPath {
				return fmt.Errorf("secret leaked to %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func expectedPriorRevision(t *testing.T) string {
	t.Helper()
	return frozenRevision(hostcontract.Target{ReleaseArtifact: ciRelease}, hostcontract.Secrets{})
}

func expectedStableID() string {
	payload := "sub2api-host-resource-id-v1:4:test4:edge"
	sum := sha256.Sum256([]byte(payload))
	return "host-" + hex.EncodeToString(sum[:])
}

// These test-owned byte-exact remote-command goldens are from reviewed
// production baseline acda72e. They must not be derived from current source.
const goldenProbeCommand = `set -eu
[ "$(uname -s)" = Linux ]
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) exit 64 ;;
esac
[ -r '/etc/machine-id' ]
bytes=$(wc -c < '/etc/machine-id')
case "$bytes" in 32|33) ;; *) exit 64 ;; esac
machine=$(cat '/etc/machine-id')
case "$machine" in
  00000000000000000000000000000000|*[!0123456789abcdef]*) exit 64 ;;
esac
[ ${#machine} -eq 32 ]
command -v openssl >/dev/null 2>&1
identity=$(printf %s "$machine" | openssl dgst -sha256 -mac HMAC -macopt key:sub2api-host-machine-identity-v1 | awk '{print $NF}')
case "$identity" in *[!0123456789abcdef]*) exit 64 ;; esac
[ ${#identity} -eq 64 ]
digest=missing
if [ -f '/usr/local/libexec/sub2api-host' ]; then
  digest=$(sha256sum '/usr/local/libexec/sub2api-host' | awk '{print $1}')
  case "$digest" in *[!0123456789abcdef]*) exit 64 ;; esac
  [ ${#digest} -eq 64 ]
fi
printf 's2p1:Linux\n%s\n%s\n%s\n' "$arch" "$identity" "$digest"
`

const goldenBootstrapReceiverScript = `set -eu
umask 077
stage='%s'
lock='%s'
final='%s'
ok="$stage.ok"
command -v flock >/dev/null 2>&1
command -v stat >/dev/null 2>&1
if [ -L "$lock" ]; then exit 64; fi
if [ -e "$lock" ] && [ ! -f "$lock" ]; then exit 64; fi
exec 9>>"$lock"
owner=$(id -u)
[ -f /proc/self/fd/9 ]
[ "$(stat -Lc '%%a:%%u:%%h' /proc/self/fd/9)" = "600:$owner:1" ]
flock -n 9
child=
cancelled=
interrupted=
cleanup() {
  rm -f "$stage" "$ok"
}
stop() {
  cancelled=143
  interrupted=1
}
trap cleanup EXIT
trap stop HUP INT TERM
IFS= read -r header
case "$header" in s2a1:*:*) ;; *) exit 64 ;; esac
body=${header#s2a1:}
size=${body%%%%:*}
digest=${body#*:}
case "$size" in ''|*[!0-9]*) exit 64 ;; esac
case "$digest" in *[!0123456789abcdef]*|?????????????????????????????????????????????????????????????????) exit 64 ;; esac
[ ${#digest} -eq 64 ]
[ "$size" -le 67108864 ]
dd of="$stage" bs=1 count="$size" status=none
[ "$(wc -c < "$stage")" -eq "$size" ]
[ "$(sha256sum "$stage" | awk '{print $1}')" = "$digest" ]
exec 4<&0
chmod 700 "$stage"
if [ -L "$final" ]; then exit 64; fi
if [ -e "$final" ] && [ ! -f "$final" ]; then exit 64; fi
set +e
"$stage" install-attest </dev/null 3>"$ok" 4<&- 9>&- >/dev/null 2>/dev/null
status=$?
set -e
[ "$status" -eq 0 ]
[ -s "$ok" ]
printf %%s 'sub2api-bootstrap-attested-v1' | cmp -s "$ok" -
[ "$(wc -c < "$stage")" -eq "$size" ]
[ "$(sha256sum "$stage" | awk '{print $1}')" = "$digest" ]
if [ -L "$final" ]; then exit 64; fi
if [ -e "$final" ] && [ ! -f "$final" ]; then exit 64; fi
mv -T -- "$stage" "$final"
"$final" bootstrap-stdio <&4 4<&- 9>&- &
child=$!
exec 4<&-
set +e
while :; do
  interrupted=
  wait "$child"
  status=$?
  [ -z "$interrupted" ] && break
done
set -e
trap '' HUP INT TERM
child=
[ -z "$cancelled" ] || status=$cancelled
exit "$status"
`

func writeExpectedSSHCommands(t *testing.T, trace string) {
	t.Helper()
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	bootstrap := "sudo -n /bin/sh -c " + quote(fmt.Sprintf(goldenBootstrapReceiverScript, "/usr/local/libexec/.sub2api-host.stage", "/usr/local/libexec/.sub2api-host.stage.lock", "/usr/local/libexec/sub2api-host")) + " fixed-argv0"
	probe := goldenProbeCommand
	if err := os.WriteFile(filepath.Join(trace, "probe.command"), []byte(probe), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trace, "bootstrap.command"), []byte(bootstrap), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trace, "host.command"), []byte("/usr/local/libexec/sub2api-host stdio"), 0600); err != nil {
		t.Fatal(err)
	}
}

func expectedRequestDigest(t *testing.T) string {
	t.Helper()
	resource := hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}
	target := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}}
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: "PROVIDER_RUNTIME_SECRET_CANARY"}}}
	revision := frozenRevision(target, secrets)
	prior := frozenRevision(hostcontract.Target{ReleaseArtifact: ciRelease}, hostcontract.Secrets{})
	frame, err := hostprotocol.EncodeRequest(hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource, TargetRevision: revision, PriorAppliedRevision: prior, Target: &target, Secrets: &secrets})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(frame)
	return hex.EncodeToString(sum[:])
}

func assertSSHRecords(t *testing.T, trace string) {
	t.Helper()
	if got := string(mustRead(t, filepath.Join(trace, "ssh.ordinal"))); got != "4\n" {
		t.Fatalf("SSH transition ordinal = %q, want %q", got, "4\\n")
	}
	for _, name := range []string{"ssh.probe.1.args", "ssh.probe.2.args", "ssh.bootstrap.args", "ssh.host.args", "bootstrap.meta"} {
		if len(mustRead(t, filepath.Join(trace, name))) == 0 {
			t.Fatalf("missing scripted SSH record %s", name)
		}
	}
	for _, name := range []string{"ssh.probe.1.args", "ssh.probe.2.args", "ssh.bootstrap.args", "ssh.host.args"} {
		lines := strings.Split(strings.TrimSuffix(string(mustRead(t, filepath.Join(trace, name))), "\n"), "\n")
		if len(lines) != 48 {
			t.Fatalf("scripted SSH argv record %s is not the fixed transport shape", name)
		}
		for index, line := range lines {
			parts := strings.Split(line, " ")
			if len(parts) != 2 || parts[0] != strconv.Itoa(index+1) || len(parts[1]) != 64 {
				t.Fatalf("invalid sanitized SSH argv record %s", name)
			}
			if index != 44 && parts[1] != sshArgumentDigest(t, index, name, trace) {
				t.Fatalf("scripted SSH argv digest mismatch at index %d", index+1)
			}
		}
	}
}

func assertBootstrapMetadata(t *testing.T, trace string) {
	t.Helper()
	artifact, err := os.ReadFile(filepath.Join(filepath.Dir(trace), "artifacts", "sub2api-host", "host-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(artifact)
	want := fmt.Sprintf("size=%d\ndigest=%s\n", len(artifact), hex.EncodeToString(sum[:]))
	if got := string(mustRead(t, filepath.Join(trace, "bootstrap.meta"))); got != want {
		t.Fatalf("bootstrap artifact metadata = %q, want %q", got, want)
	}
}

func sshArgumentDigest(t *testing.T, index int, record, trace string) string {
	t.Helper()
	fixed := []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "-E", "", "--", "edge"}
	if index == 47 {
		name := "bootstrap.command"
		if strings.HasPrefix(record, "ssh.probe.") {
			name = "probe.command"
		}
		if record == "ssh.host.args" {
			name = "host.command"
		}
		sum := sha256.Sum256(mustRead(t, filepath.Join(trace, name)))
		return hex.EncodeToString(sum[:])
	}
	value := fixed[index]
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func readProviderPort(t *testing.T, output io.ReadCloser) string {
	t.Helper()
	result := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(output).ReadString('\n'); result <- strings.TrimSpace(line) }()
	select {
	case port := <-result:
		if _, err := strconv.Atoi(port); err != nil {
			t.Fatal("Provider emitted no valid gRPC port")
		}
		return port
	case <-time.After(5 * time.Second):
		_ = output.Close()
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Error("Provider port reader did not exit")
		}
		t.Fatal("Provider did not emit gRPC port")
		return ""
	}
}

type processIdentity struct {
	pid, pgid         int
	start, executable string
}
type providerCleanup struct {
	cmd      *exec.Cmd
	done     <-chan error
	identity *processIdentity
}

func providerIdentity(pid int, executable string) (processIdentity, error) {
	state, start, pgid, err := processState(pid)
	if err != nil || state == "Z" || pgid <= 0 {
		return processIdentity{}, fmt.Errorf("invalid Provider process identity")
	}
	actual, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || actual != executable {
		return processIdentity{}, fmt.Errorf("Provider executable identity mismatch")
	}
	return processIdentity{pid: pid, pgid: pgid, start: start, executable: executable}, nil
}

func processState(pid int) (state, start string, pgid int, err error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", "", 0, err
	}
	closeParen := strings.LastIndexByte(string(b), ')')
	if closeParen < 0 {
		return "", "", 0, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(b)[closeParen+1:])
	if len(fields) < 20 {
		return "", "", 0, fmt.Errorf("invalid process stat")
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return "", "", 0, err
	}
	return fields[0], fields[19], pgid, nil
}

func sameProvider(identity processIdentity) bool {
	state, start, pgid, err := processState(identity.pid)
	if err != nil || state == "Z" || start != identity.start || pgid != identity.pgid {
		return false
	}
	actual, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(identity.pid), "exe"))
	return err == nil && actual == identity.executable
}

func cleanupProvider(t *testing.T, cleanup *providerCleanup) {
	t.Helper()
	if cleanup.identity == nil {
		if cleanup.cmd.Process != nil {
			_ = cleanup.cmd.Process.Kill()
		}
		select {
		case <-cleanup.done:
		case <-time.After(time.Second):
			t.Error("Provider process did not exit")
		}
		return
	}
	identity := *cleanup.identity
	if sameProvider(identity) {
		_ = syscall.Kill(-identity.pgid, syscall.SIGTERM)
	}
	select {
	case <-cleanup.done:
		return
	case <-time.After(time.Second):
	}
	if sameProvider(identity) {
		_ = syscall.Kill(-identity.pgid, syscall.SIGKILL)
	}
	select {
	case <-cleanup.done:
	case <-time.After(time.Second):
		t.Error("Provider process did not exit")
	}
	if sameProvider(identity) {
		t.Error("Provider process survived cleanup")
	}
}

func cleanupScriptedSSH(t *testing.T, trace string) {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(trace, "ssh.identity"))
	if err != nil {
		return
	}
	fields := strings.Fields(string(value))
	if len(fields) != 3 {
		t.Error("invalid scripted SSH identity")
		return
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Error("invalid scripted SSH pid")
		return
	}
	state, start, observedPGID, err := processState(pid)
	if err != nil || state == "Z" || start != fields[1] {
		return
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid != pid {
		return
	}
	if observedPGID == pgid {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, start, _, err := processState(pid)
		if err != nil || state == "Z" || start != fields[1] {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("scripted SSH/helper process survived cleanup")
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

func jsonProperty(value any) property.Value {
	b, _ := json.Marshal(value)
	var raw any
	_ = json.Unmarshal(b, &raw)
	return rawProperty(raw)
}

func rawProperty(value any) property.Value {
	switch value := value.(type) {
	case string:
		return property.New(value)
	case bool:
		return property.New(value)
	case float64:
		return property.New(value)
	case []any:
		items := make([]property.Value, len(value))
		for i := range value {
			items[i] = rawProperty(value[i])
		}
		return property.New(property.NewArray(items))
	case map[string]any:
		items := map[string]property.Value{}
		for key, item := range value {
			items[key] = rawProperty(item)
		}
		return property.New(property.NewMap(items))
	default:
		return property.New(property.Null)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	path, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			t.Fatal("repository root not found")
		}
		path = parent
	}
}

type approvalDecision uint8

const (
	approvalAbsent approvalDecision = iota
	approvalDeny
	approvalExact
)

type approvalRecorder struct {
	mu       sync.Mutex
	subjects []hostcontract.ApprovalSubject
	decision approvalDecision
	expected []hostcontract.ApprovalSubject
}

func (r *approvalRecorder) Count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.subjects) }
func (r *approvalRecorder) Subjects() []hostcontract.ApprovalSubject {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hostcontract.ApprovalSubject(nil), r.subjects...)
}
func (r *approvalRecorder) Expect(subject hostcontract.ApprovalSubject) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expected = append(r.expected, subject)
}
func (r *approvalRecorder) decide(subject hostcontract.ApprovalSubject) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subjects = append(r.subjects, subject)
	if r.decision != approvalExact || len(r.expected) == 0 || r.expected[0] != subject {
		return false
	}
	r.expected = r.expected[1:]
	return true
}
func (r *approvalRecorder) AssertExpectedConsumed(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.expected) != 0 {
		t.Fatalf("unrequested exact approval subjects remain: %#v", r.expected)
	}
}

func startApprovalServer(t *testing.T, decision approvalDecision) (*approvalRecorder, *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(fds[0]), "provider-runtime-approval-server")
	child := os.NewFile(uintptr(fds[1]), "provider-runtime-approval-client")
	conn, err := net.FileConn(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := &approvalRecorder{decision: decision}
	server := hostapproval.NewServer(func(_ context.Context, subject hostcontract.ApprovalSubject) bool {
		return recorder.decide(subject)
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, conn) }()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("approval server: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("approval server did not exit")
		}
	})
	return recorder, child
}

func createProviderResource(t *testing.T, h *providerProcess, inputs property.Map) *pulumirpc.CreateResponse {
	t.Helper()
	writeTargetExpectation(t, h, inputs)
	writeHostActionQueue(t, h, hostcontract.ActionInspect)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	response, err := h.client.Create(ctx, &pulumirpc.CreateRequest{Urn: "urn:pulumi:test::runtime::sub2api-host:index:Host::edge", Properties: rpcProperties(t, inputs)})
	assertHostActionQueueEmpty(t, h)
	if err != nil || response == nil || response.Id == "" {
		t.Fatalf("Create: %#v, %v", response, err)
	}
	return response
}

func configureProvider(t *testing.T, client pulumirpc.ResourceProviderClient) (*pulumirpc.ConfigureResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return client.Configure(ctx, &pulumirpc.ConfigureRequest{Args: rpcProperties(t, property.NewMap(map[string]property.Value{"revisionKey": property.New(ciKey).WithSecret(true)}))})
}
func updateProviderResource(t *testing.T, h *providerProcess, request *pulumirpc.UpdateRequest, inputs property.Map, actions ...hostcontract.Action) (*pulumirpc.UpdateResponse, error) {
	t.Helper()
	writeTargetExpectation(t, h, inputs, request.OldInputs)
	writeHostActionQueue(t, h, actions...)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	response, err := h.client.Update(ctx, request)
	assertHostActionQueueEmpty(t, h)
	if err == nil && response != nil {
		assertExactUpdateCheckpoint(t, unmarshalProperties(t, response.Properties), inputs, frozenRevisionForInputs(t, inputs))
	}
	return response, err
}
func deleteProviderResource(t *testing.T, h *providerProcess, request *pulumirpc.DeleteRequest, inputs property.Map, actions ...hostcontract.Action) (*emptypb.Empty, error) {
	t.Helper()
	writeTargetExpectation(t, h, inputs)
	writeHostActionQueue(t, h, actions...)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := h.client.Delete(ctx, request)
	assertHostActionQueueEmpty(t, h)
	return response, err
}
func readProviderResource(t *testing.T, h *providerProcess, request *pulumirpc.ReadRequest, actions ...hostcontract.Action) (*pulumirpc.ReadResponse, error) {
	t.Helper()
	writeHostActionQueue(t, h, actions...)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := h.client.Read(ctx, request)
	assertHostActionQueueEmpty(t, h)
	return response, err
}

func writeTargetExpectation(t *testing.T, h *providerProcess, inputs property.Map, current ...*structpb.Struct) {
	t.Helper()
	targetValue, ok := inputs.GetOk("target")
	if !ok {
		t.Fatal("target input is absent")
	}
	var target hostcontract.Target
	if err := json.Unmarshal(mustJSON(t, targetValue), &target); err != nil {
		t.Fatal(err)
	}
	expectation := targetExpectation{Revision: frozenRevisionForInputs(t, inputs)}
	for _, app := range target.Apps {
		expectation.Apps = append(expectation.Apps, expectedTargetApp{ID: app.ID, Image: app.Image, Hostname: app.Hostname, ReadinessPath: app.ReadinessPath, DrainSeconds: frozenDrainSeconds(t, app.DrainTimeout), DataLinks: append([]hostcontract.DataLink(nil), app.DataLinks...)})
	}
	if len(current) != 0 && current[0] != nil {
		old := unmarshalProperties(t, current[0])
		if value, ok := old.GetOk("target"); ok {
			var target hostcontract.Target
			if err := json.Unmarshal(mustJSON(t, value), &target); err != nil {
				t.Fatal(err)
			}
			for _, app := range target.Apps {
				expectation.CurrentApps = append(expectation.CurrentApps, expectedTargetApp{ID: app.ID, Image: app.Image, Hostname: app.Hostname, ReadinessPath: app.ReadinessPath, DrainSeconds: frozenDrainSeconds(t, app.DrainTimeout), DataLinks: append([]hostcontract.DataLink(nil), app.DataLinks...)})
			}
		}
	}
	writeJSONExpectation(t, filepath.Join(h.trace, "target.expectation.json"), expectation)
}

func writeHostActionQueue(t *testing.T, h *providerProcess, actions ...hostcontract.Action) {
	t.Helper()
	for _, action := range actions {
		if action != hostcontract.ActionInspect && action != hostcontract.ActionReconcile && action != hostcontract.ActionRetirePreserveData {
			t.Fatalf("unsupported Host action %q", action)
		}
	}
	lines := make([]string, len(actions))
	for i, action := range actions {
		lines[i] = string(action)
	}
	lock, err := os.OpenFile(filepath.Join(h.trace, "ssh.ordinal.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if existing, err := os.ReadFile(filepath.Join(h.trace, "host-action.queue")); err == nil && strings.TrimSpace(string(existing)) != "" {
		t.Fatalf("cannot replace unconsumed Host action queue: %q", existing)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	writeAtomicFile(t, filepath.Join(h.trace, "host-action.queue"), []byte(strings.Join(lines, "\n")+"\n"))
}

func writeJSONExpectation(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeAtomicFile(t, path, b)
}

func writeAtomicFile(t *testing.T, path string, value []byte) {
	t.Helper()
	file, err := os.CreateTemp(filepath.Dir(path), ".fixture-")
	if err != nil {
		t.Fatal(err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

func assertHostActionQueueEmpty(t *testing.T, h *providerProcess) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(h.trace, "ssh.ordinal.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	b, err := os.ReadFile(filepath.Join(h.trace, "host-action.queue"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "" {
		t.Fatalf("unconsumed Host actions: %q", b)
	}
}

func readRequest(t *testing.T, state *pulumirpc.CreateResponse) *pulumirpc.ReadRequest {
	t.Helper()
	checkpoint := unmarshalProperties(t, state.Properties)
	inputs := property.NewMap(nil)
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		value, _ := checkpoint.GetOk(name)
		inputs = inputs.Set(name, value)
	}
	return &pulumirpc.ReadRequest{Id: state.Id, Urn: "urn:pulumi:test::runtime::sub2api-host:index:Host::edge", Type: "sub2api-host:index:Host", Name: "edge", Inputs: rpcProperties(t, inputs), Properties: state.Properties}
}

func updateRequest(t *testing.T, id string, olds *structpb.Struct, old, next property.Map) *pulumirpc.UpdateRequest {
	t.Helper()
	return &pulumirpc.UpdateRequest{Id: id, Urn: "urn:pulumi:test::runtime::sub2api-host:index:Host::edge", Type: "sub2api-host:index:Host", Name: "edge", Olds: olds, OldInputs: rpcProperties(t, old), News: rpcProperties(t, next)}
}

func deleteRequest(t *testing.T, id string, properties *structpb.Struct, inputs property.Map) *pulumirpc.DeleteRequest {
	t.Helper()
	return &pulumirpc.DeleteRequest{Id: id, Urn: "urn:pulumi:test::runtime::sub2api-host:index:Host::edge", Type: "sub2api-host:index:Host", Name: "edge", Properties: properties, OldInputs: rpcProperties(t, inputs)}
}

func createInputsWithTarget(target hostcontract.Target) property.Map {
	inputs := createInputs()
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{}}
	for _, app := range target.Apps {
		if app.ID == "api" {
			secrets.Apps[app.ID] = hostcontract.AppSecrets{JWTSecret: ciSecret}
		}
	}
	return inputs.Set("target", jsonProperty(target)).Set("secrets", jsonProperty(secrets).WithSecret(true))
}

func createInputsWithImage(image string) property.Map {
	inputs := createInputs()
	target := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: image, Hostname: "api.example", ReadinessPath: "/ready"}}}
	return inputs.Set("target", jsonProperty(target))
}

func createInputsWithDataLinks(changes int, generation string) property.Map {
	apps := []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}
	for i := 0; i < changes; i++ {
		apps = append(apps, hostcontract.AppTarget{ID: fmt.Sprintf("data-%d", i), Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: fmt.Sprintf("data-%d.example", i), ReadinessPath: "/ready", DataLinks: []hostcontract.DataLink{{Name: "main", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: generation + strconv.Itoa(i), Endpoint: generation + ".db", Port: 5432, Database: "app", TLSServerName: generation + ".db"}}}})
	}
	return createInputsWithTarget(hostcontract.Target{ReleaseArtifact: ciRelease, Apps: apps})
}

func frozenRevisionForInputs(t *testing.T, inputs property.Map) string {
	t.Helper()
	targetValue, ok := inputs.GetOk("target")
	if !ok {
		t.Fatal("target input is absent")
	}
	secretsValue, ok := inputs.GetOk("secrets")
	if !ok {
		t.Fatal("secrets input is absent")
	}
	var target hostcontract.Target
	if err := json.Unmarshal(mustJSON(t, targetValue), &target); err != nil {
		t.Fatal(err)
	}
	var secrets hostcontract.Secrets
	if err := json.Unmarshal(mustJSON(t, secretsValue), &secrets); err != nil {
		t.Fatal(err)
	}
	return frozenRevision(target, secrets)
}

func frozenRevision(target hostcontract.Target, secrets hostcontract.Secrets) string {
	key, err := base64.StdEncoding.DecodeString(ciKey)
	if err != nil {
		panic(err)
	}
	target, secrets = frozenNormalize(target, secrets)
	payload, err := json.Marshal(struct {
		Domain   string                        `json:"domain"`
		Resource hostcontract.ResourceIdentity `json:"resource"`
		Target   hostcontract.Target           `json:"target"`
		Secrets  hostcontract.Secrets          `json:"secrets"`
	}{"sub2api-host-target-revision-v1", hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}, target, secrets})
	if err != nil { panic(err) }
	keyID := hmac.New(sha256.New, key)
	_, _ = keyID.Write([]byte("sub2api-host-revision-key-id-v1"))
	commitment := hmac.New(sha256.New, key)
	_, _ = commitment.Write(payload)
	return "tr1:" + hex.EncodeToString(keyID.Sum(nil)[:8]) + ":" + hex.EncodeToString(commitment.Sum(nil))
}

func frozenNormalize(target hostcontract.Target, secrets hostcontract.Secrets) (hostcontract.Target, hostcontract.Secrets) {
	target.Apps = append([]hostcontract.AppTarget(nil), target.Apps...)
	sort.Slice(target.Apps, func(i, j int) bool { return target.Apps[i].ID < target.Apps[j].ID })
	for i := range target.Apps {
		target.Apps[i].RuntimeSettings = frozenCopyStrings(target.Apps[i].RuntimeSettings)
		target.Apps[i].DataLinks = append([]hostcontract.DataLink(nil), target.Apps[i].DataLinks...)
		sort.Slice(target.Apps[i].DataLinks, func(a, b int) bool { return target.Apps[i].DataLinks[a].Name < target.Apps[i].DataLinks[b].Name })
	}
	if target.MicroSocks != nil {
		copy := *target.MicroSocks
		copy.Clients = append([]hostcontract.MicroSocksClientTarget(nil), copy.Clients...)
		sort.Slice(copy.Clients, func(i, j int) bool { return copy.Clients[i].ID < copy.Clients[j].ID })
		if len(copy.Clients) == 0 {
			copy.Clients = nil
		}
		if !copy.Server && len(copy.Clients) == 0 {
			target.MicroSocks = nil
		} else {
			target.MicroSocks = &copy
		}
	}
	target.Connectors = append([]hostcontract.TunnelConnectorTarget(nil), target.Connectors...)
	for i := range target.Connectors {
		target.Connectors[i].AppIDs = append([]string(nil), target.Connectors[i].AppIDs...)
		sort.Strings(target.Connectors[i].AppIDs)
		if len(target.Connectors[i].AppIDs) == 0 {
			target.Connectors[i].AppIDs = nil
		}
	}
	target.DataServices = append([]hostcontract.LocalDataServiceTarget(nil), target.DataServices...)
	sort.Slice(target.DataServices, func(i, j int) bool { return target.DataServices[i].ID < target.DataServices[j].ID })
	sort.Slice(target.Connectors, func(i, j int) bool { return target.Connectors[i].ID < target.Connectors[j].ID })
	if len(target.Apps) == 0 {
		target.Apps = nil
	}
	if len(target.DataServices) == 0 {
		target.DataServices = nil
	}
	if len(target.Connectors) == 0 {
		target.Connectors = nil
	}
	secrets.Apps = frozenCopyAppSecrets(secrets.Apps)
	secrets.LocalDataServices = frozenCopyLocalDataSecrets(secrets.LocalDataServices)
	secrets.Connectors = frozenCopyConnectorSecrets(secrets.Connectors)
	if secrets.MicroSocks != nil {
		copy := *secrets.MicroSocks
		copy.ClientCredentials = frozenCopyCredentials(copy.ClientCredentials)
		if copy.ServerUsername == "" && copy.ServerPassword == "" && len(copy.ClientCredentials) == 0 {
			secrets.MicroSocks = nil
		} else {
			secrets.MicroSocks = &copy
		}
	}
	return target, secrets
}
func frozenCopyStrings(values map[string]string) map[string]string {
	if len(values) == 0 { return nil }
	copy := make(map[string]string, len(values))
	for k, v := range values { copy[k] = v }
	return copy
}
func frozenCopyCredentials(values map[string]hostcontract.DataCredentials) map[string]hostcontract.DataCredentials {
	if len(values) == 0 { return nil }
	copy := make(map[string]hostcontract.DataCredentials, len(values))
	for k, v := range values { copy[k] = v }
	return copy
}
func frozenCopyAppSecrets(values map[string]hostcontract.AppSecrets) map[string]hostcontract.AppSecrets {
	if len(values) == 0 { return nil }
	copy := make(map[string]hostcontract.AppSecrets, len(values))
	for k, v := range values { v.RuntimeEnvironment = frozenCopyStrings(v.RuntimeEnvironment); copy[k] = v }
	return copy
}
func frozenCopyLocalDataSecrets(values map[string]hostcontract.LocalDataServiceSecrets) map[string]hostcontract.LocalDataServiceSecrets {
	if len(values) == 0 { return nil }
	copy := make(map[string]hostcontract.LocalDataServiceSecrets, len(values))
	for k, v := range values { copy[k] = v }
	return copy
}
func frozenCopyConnectorSecrets(values map[string]hostcontract.TunnelConnectorSecrets) map[string]hostcontract.TunnelConnectorSecrets {
	if len(values) == 0 { return nil }
	copy := make(map[string]hostcontract.TunnelConnectorSecrets, len(values))
	for k, v := range values { copy[k] = v }
	return copy
}

func assertFrozenNormalizationOracle(t *testing.T) {
	t.Helper()
	inputTarget := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "z", RuntimeSettings: map[string]string{"Z": "z", "A": "a"}, DataLinks: []hostcontract.DataLink{{Name: "z", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "z"}}, {Name: "a", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "a"}}}}, {ID: "a", RuntimeSettings: map[string]string{}}}, DataServices: []hostcontract.LocalDataServiceTarget{{ID: "z"}, {ID: "a"}}, Connectors: []hostcontract.TunnelConnectorTarget{{ID: "z", AppIDs: []string{"z", "a"}}, {ID: "a", AppIDs: []string{}},}, MicroSocks: &hostcontract.MicroSocksTarget{Server: true, Clients: []hostcontract.MicroSocksClientTarget{{ID: "z"}, {ID: "a"}}}}
	inputSecrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"z": {RuntimeEnvironment: map[string]string{"Z": "z", "A": "a"}, JWTSecret: "z"}, "a": {RuntimeEnvironment: map[string]string{}}}, LocalDataServices: map[string]hostcontract.LocalDataServiceSecrets{"z": {AdminPassword: "z"}, "a": {}}, Connectors: map[string]hostcontract.TunnelConnectorSecrets{"z": {Token: "z"}, "a": {}}, MicroSocks: &hostcontract.MicroSocksSecrets{ClientCredentials: map[string]hostcontract.DataCredentials{"z": {Username: "z"}, "a": {}}}}
	wantTarget := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "a"}, {ID: "z", RuntimeSettings: map[string]string{"A": "a", "Z": "z"}, DataLinks: []hostcontract.DataLink{{Name: "a", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "a"}}, {Name: "z", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "z"}}}}}, DataServices: []hostcontract.LocalDataServiceTarget{{ID: "a"}, {ID: "z"}}, Connectors: []hostcontract.TunnelConnectorTarget{{ID: "a", AppIDs: nil}, {ID: "z", AppIDs: []string{"a", "z"}}}, MicroSocks: &hostcontract.MicroSocksTarget{Server: true, Clients: []hostcontract.MicroSocksClientTarget{{ID: "a"}, {ID: "z"}}}}
	wantSecrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"a": {}, "z": {RuntimeEnvironment: map[string]string{"A": "a", "Z": "z"}, JWTSecret: "z"}}, LocalDataServices: map[string]hostcontract.LocalDataServiceSecrets{"a": {}, "z": {AdminPassword: "z"}}, Connectors: map[string]hostcontract.TunnelConnectorSecrets{"a": {}, "z": {Token: "z"}}, MicroSocks: &hostcontract.MicroSocksSecrets{ClientCredentials: map[string]hostcontract.DataCredentials{"a": {}, "z": {Username: "z"}}}}
	gotTarget, gotSecrets := frozenNormalize(inputTarget, inputSecrets)
	if !reflect.DeepEqual(gotTarget, wantTarget) || !reflect.DeepEqual(gotSecrets, wantSecrets) {
		t.Fatal("frozen normalization differs from explicit contract fixture")
	}
	gotJSON, gotErr := json.Marshal(struct { Target hostcontract.Target `json:"target"`; Secrets hostcontract.Secrets `json:"secrets"` }{gotTarget, gotSecrets})
	wantJSON, wantErr := json.Marshal(struct { Target hostcontract.Target `json:"target"`; Secrets hostcontract.Secrets `json:"secrets"` }{wantTarget, wantSecrets})
	if gotErr != nil || wantErr != nil || !bytes.Equal(gotJSON, wantJSON) {
		t.Fatal("frozen normalization canonical JSON differs from explicit contract fixture")
	}
	nilTarget, nilSecrets := frozenNormalize(hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{}, DataServices: []hostcontract.LocalDataServiceTarget{}, Connectors: []hostcontract.TunnelConnectorTarget{}, MicroSocks: &hostcontract.MicroSocksTarget{}}, hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{}, LocalDataServices: map[string]hostcontract.LocalDataServiceSecrets{}, Connectors: map[string]hostcontract.TunnelConnectorSecrets{}, MicroSocks: &hostcontract.MicroSocksSecrets{ClientCredentials: map[string]hostcontract.DataCredentials{}}})
	if !reflect.DeepEqual(nilTarget, hostcontract.Target{ReleaseArtifact: ciRelease}) || !reflect.DeepEqual(nilSecrets, hostcontract.Secrets{}) {
		t.Fatal("frozen normalization does not equate nil and empty optional fields")
	}
}

func frozenDrainSeconds(t *testing.T, value string) int {
	t.Helper()
	if value == "" {
		return 30
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 || d%time.Second != 0 {
		t.Fatalf("invalid fixture drain timeout %q", value)
	}
	return int(d / time.Second)
}

func mustJSON(t *testing.T, value property.Value) []byte {
	t.Helper()
	raw, err := propertyRaw(value)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertDataLinkApprovalSubject(t *testing.T, subject hostcontract.ApprovalSubject, oldGeneration, newGeneration, revision string) {
	t.Helper()
	old := hostcontract.DataIdentity{Kind: "postgres", ProviderID: oldGeneration + "0", Endpoint: oldGeneration + ".db", Port: 5432, Database: "app", TLSServerName: oldGeneration + ".db"}
	new := hostcontract.DataIdentity{Kind: "postgres", ProviderID: newGeneration + "0", Endpoint: newGeneration + ".db", Port: 5432, Database: "app", TLSServerName: newGeneration + ".db"}
	want := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: "test", Resource: hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}, AppID: "data-0", DataKind: "postgres", OldData: old, NewData: new, TargetRevision: revision}
	if subject != want {
		t.Fatalf("approval subject = %#v, want %#v", subject, want)
	}
}
func dataLinkApprovalSubject(oldGeneration, newGeneration, revision string) hostcontract.ApprovalSubject {
	old := hostcontract.DataIdentity{Kind: "postgres", ProviderID: oldGeneration + "0", Endpoint: oldGeneration + ".db", Port: 5432, Database: "app", TLSServerName: oldGeneration + ".db"}
	new := hostcontract.DataIdentity{Kind: "postgres", ProviderID: newGeneration + "0", Endpoint: newGeneration + ".db", Port: 5432, Database: "app", TLSServerName: newGeneration + ".db"}
	return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: "test", Resource: hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}, AppID: "data-0", DataKind: "postgres", OldData: old, NewData: new, TargetRevision: revision}
}
func retireApprovalSubject(t *testing.T, checkpoint, inputs property.Map) hostcontract.ApprovalSubject {
	t.Helper()
	machine, ownership, revision, _, err := checkpointValues(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if revision != frozenRevisionForInputs(t, inputs) {
		t.Fatal("retire checkpoint revision does not match drained inputs")
	}
	return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: "test", Resource: hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}, Machine: machine, Ownership: ownership, TargetRevision: revision, PreserveData: true}
}

type runtimeEvidence struct{ state, inventory, docker string }

func runtimeSnapshot(t *testing.T, h *providerProcess) runtimeEvidence {
	t.Helper()
	return runtimeEvidence{string(mustRead(t, filepath.Join(h.root, "state.json"))), string(mustRead(t, filepath.Join(h.root, "runtime", "managed", "inventory.json"))), string(mustRead(t, filepath.Join(h.trace, "docker.args")))}
}
func assertReadOnlyRuntime(t *testing.T, before, after runtimeEvidence) {
	t.Helper()
	if before != after {
		t.Fatal("Read changed runtime state, inventory, or Docker effect trace")
	}
}
func assertNoRuntimeWrite(t *testing.T, before, after runtimeEvidence) {
	t.Helper()
	if before != after {
		t.Fatal("rejected operation changed runtime state, inventory, or Docker effect trace")
	}
}
func dockerEffects(t *testing.T, h *providerProcess) []dockerEffect {
	t.Helper()
	var trace dockerTrace
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.trace, "docker-state", "state.json")), &trace); err != nil {
		t.Fatal(err)
	}
	return trace.Effects
}
func runtimeState(t *testing.T, h *providerProcess) hostruntime.State {
	t.Helper()
	var state hostruntime.State
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "state.json")), &state); err != nil {
		t.Fatal(err)
	}
	return state
}
func effectActions(effects []dockerEffect) []string {
	actions := make([]string, len(effects))
	for i, effect := range effects {
		actions[i] = effect.Action
	}
	return actions
}
func assertReconcileEffectDelta(t *testing.T, h *providerProcess, before, after []dockerEffect, current, next property.Map) {
	t.Helper()
	if len(after) < len(before) || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("reconcile changed completed effect prefix: before %#v, after %#v", before, after)
	}
	targetValue, ok := next.GetOk("target")
	if !ok {
		t.Fatal("target input is absent")
	}
	var target hostcontract.Target
	if err := json.Unmarshal(mustJSON(t, targetValue), &target); err != nil {
		t.Fatal(err)
	}
	delta := after[len(before):]
	if len(delta) != len(target.Apps)*3 {
		t.Fatalf("reconcile effect delta = %#v, want three effects per expected target app", delta)
	}
	for index, app := range target.Apps {
		for offset, action := range []string{"container-run", "container-stop", "container-rm"} {
			effect := delta[index*3+offset]
			inputs := next
			if action != "container-run" {
				inputs = current
			}
			if !matchesExpectedAppEffect(t, h, effect, action, app, inputs, "green") && !matchesExpectedAppEffect(t, h, effect, action, app, inputs, "blue") {
				t.Fatalf("reconcile effect %d = %#v, want %s for app %q", index*3+offset, effect, action, app.ID)
			}
		}
	}
}
func assertInitialCreateEffects(t *testing.T, h *providerProcess, effects []dockerEffect, inputs property.Map) {
	t.Helper()
	targetValue, ok := inputs.GetOk("target")
	if !ok {
		t.Fatal("initial target is absent")
	}
	var target hostcontract.Target
	if err := json.Unmarshal(mustJSON(t, targetValue), &target); err != nil {
		t.Fatal(err)
	}
	if len(effects) != len(target.Apps)+1 || effects[0].Action != "network-create" || effects[0].Name == "" || len(effects[0].OwnerDigest) != 64 || len(effects[0].TargetDigest) != 64 {
		t.Fatalf("initial Create effects = %#v", effects)
	}
	state := runtimeState(t, h)
	networkName := "s2h-net-" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value)
	networkOwner := "s2h1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "network", "", "")
	networkLabel := "s2hnet1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value)
	ownerSum, labelSum := sha256.Sum256([]byte(networkOwner)), sha256.Sum256([]byte(networkLabel))
	if effects[0] != (dockerEffect{Action: "network-create", Name: networkName, OwnerDigest: hex.EncodeToString(ownerSum[:]), TargetDigest: hex.EncodeToString(labelSum[:])}) {
		t.Fatalf("initial network effect = %#v", effects[0])
	}
	for i, app := range target.Apps {
		effect := effects[i+1]
		if !matchesExpectedAppEffect(t, h, effect, "container-run", app, inputs, "green") {
			t.Fatalf("initial Create effect %d = %#v, want app %q", i, effect, app.ID)
		}
	}
}
type inventoryApp struct {
	AppToken, Active, Name, Image, Revision string
}
func inventoryApps(t *testing.T, h *providerProcess) []inventoryApp {
	t.Helper()
	var inventory struct {
		Objects []struct {
			Role, AppToken, Active, Name, Image, Revision string
		} `json:"objects"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "runtime", "managed", "inventory.json")), &inventory); err != nil {
		t.Fatal(err)
	}
	var apps []inventoryApp
	for _, object := range inventory.Objects {
		if object.Role == "app" {
			apps = append(apps, inventoryApp{AppToken: object.AppToken, Active: object.Active, Name: object.Name, Image: object.Image, Revision: object.Revision})
		}
	}
	return apps
}
func assertDrainEffectDelta(t *testing.T, h *providerProcess, before, after []dockerEffect, preDrainApps []inventoryApp) {
	t.Helper()
	if len(after) < len(before) || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("drain changed completed effect prefix: before %#v, after %#v", before, after)
	}
	delta := after[len(before):]
	if len(delta) != len(preDrainApps) {
		t.Fatalf("drain effect delta = %#v, want one removal per pre-drain app", delta)
	}
	state := runtimeState(t, h)
	for position, object := range preDrainApps {
		if len(object.AppToken) != 24 || strings.Trim(object.AppToken, "0123456789abcdef") != "" || (object.Active != "blue" && object.Active != "green") || object.Image == "" || object.Revision == "" {
			t.Fatalf("invalid pre-drain inventory app %#v", object)
		}
		name := "s2h-" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "app", object.AppToken, object.Active)
		owner := "s2h1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "app", object.AppToken, object.Active)
		target := "s2ht1:" + fixtureToken("app", object.AppToken, object.Active, object.Revision, object.Image, "", "0", "false")
		ownerSum, targetSum := sha256.Sum256([]byte(owner)), sha256.Sum256([]byte(target))
		want := dockerEffect{Action: "container-rm", Name: name, AppToken: object.AppToken, Slot: object.Active, OwnerDigest: hex.EncodeToString(ownerSum[:]), TargetDigest: hex.EncodeToString(targetSum[:])}
		if object.Name != name || delta[position] != want {
			t.Fatalf("drain effect %d = %#v, want inventory-ordered removal for token %q", position, delta[position], object.AppToken)
		}
	}
}
func matchesExpectedAppEffect(t *testing.T, h *providerProcess, effect dockerEffect, action string, app hostcontract.AppTarget, inputs property.Map, slot string) bool {
	t.Helper()
	var state hostruntime.State
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "state.json")), &state); err != nil {
		return false
	}
	revision := frozenRevisionForInputs(t, inputs)
	token := fixtureToken("app", app.ID)
	if len(token) != 24 || strings.Trim(token, "0123456789abcdef") != "" {
		return false
	}
	name := "s2h-" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "app", token, slot)
	owner := "s2h1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "app", token, slot)
	target := "s2ht1:" + fixtureToken("app", token, slot, revision, app.Image, "", "0", "false")
	ownerSum, targetSum := sha256.Sum256([]byte(owner)), sha256.Sum256([]byte(target))
	return effect.Action == action && effect.Name == name && effect.AppToken == token && effect.Slot == slot && effect.OwnerDigest == hex.EncodeToString(ownerSum[:]) && effect.TargetDigest == hex.EncodeToString(targetSum[:])
}
func assertCompletedReconcile(t *testing.T, h *providerProcess, checkpoint property.Map, approvals int) {
	t.Helper()
	_, _, revision, _, err := checkpointValues(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var state hostruntime.State
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "state.json")), &state); err != nil || state.Journal == nil || state.Journal.Status != "complete" || state.Journal.Key.TargetRevision != revision {
		t.Fatalf("runtime journal was not complete for Update: %v", err)
	}
	if (state.Journal.Approval != nil) != (approvals == 1) {
		t.Fatal("runtime approval evidence cardinality differs")
	}
}
func assertCompletedReconcileForRevision(t *testing.T, h *providerProcess, checkpoint property.Map, revision string, approvals int) {
	t.Helper()
	_, _, checkpointRevision, _, err := checkpointValues(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointRevision != revision {
		t.Fatalf("final checkpoint revision = %q, want %q", checkpointRevision, revision)
	}
	assertCompletedReconcile(t, h, checkpoint, approvals)
	var state hostruntime.State
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "state.json")), &state); err != nil {
		t.Fatal(err)
	}
	wantKey := hostcontract.OperationKey{Resource: hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}, Action: hostcontract.ActionReconcile, TargetRevision: revision}
	wantResult := hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: revision}
	if state.Journal == nil || state.Journal.Key.Resource != wantKey.Resource || state.Journal.Key.Action != wantKey.Action || state.Journal.Key.TargetRevision != wantKey.TargetRevision || state.Journal.Result == nil || *state.Journal.Result != wantResult {
		t.Fatalf("final state journal = %#v, want reconcile key/result for revision %q", state.Journal, revision)
	}
}
func writeDataSentinel(t *testing.T, h *providerProcess) {
	t.Helper()
	var inventory struct {
		Objects []struct {
			Role, AppToken string
		} `json:"objects"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "runtime", "managed", "inventory.json")), &inventory); err != nil {
		t.Fatal(err)
	}
	var token string
	for _, object := range inventory.Objects {
		if object.Role == "app" && object.AppToken == fixtureToken("app", "api") {
			token = object.AppToken
		}
	}
	if token == "" {
		t.Fatal("managed api app inventory object is absent")
	}
	path := filepath.Join(h.root, "runtime", "data", fixtureToken("app-data", token))
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("managed-preserve\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(h.root, "runtime", "data", "unowned-fixture")
	if err := os.MkdirAll(unowned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unowned, "sentinel"), []byte("unowned-preserve\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
func (h *providerProcess) dropHostResponse(t *testing.T, action hostcontract.Action) {
	t.Helper()
	if action != hostcontract.ActionReconcile && action != hostcontract.ActionRetirePreserveData {
		t.Fatal("unsupported response-loss action")
	}
	lock, err := os.OpenFile(filepath.Join(h.trace, "ssh.ordinal.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	writeAtomicFile(t, filepath.Join(h.trace, "drop-host-response."+string(action)), []byte("1\n"))
}
func assertDroppedHostResponse(t *testing.T, h *providerProcess, action hostcontract.Action) {
	t.Helper()
	trace := string(mustRead(t, filepath.Join(h.trace, "ssh.host.response-loss")))
	lines := strings.Split(strings.TrimSuffix(trace, "\n"), "\n")
	if len(lines) != 3 || lines[0] != "action="+string(action) || !strings.HasPrefix(lines[1], "operationDigest=") || len(strings.TrimPrefix(lines[1], "operationDigest=")) != 64 || lines[2] != "dropped-after-complete" {
		t.Fatalf("response-loss trace is not an exact sanitized %s drop", action)
	}
	metadata := string(mustRead(t, filepath.Join(h.trace, "ssh.host.request.sha256")))
	if metadata != "operationDigest="+strings.TrimPrefix(lines[1], "operationDigest=")+"\naction="+string(action)+"\n" {
		t.Fatal("response-loss digest does not match exact Host metadata")
	}
}

func assertRetiredPreservingData(t *testing.T, h *providerProcess, before []dockerEffect, preRetire *hostruntime.Journal) {
	t.Helper()
	sentinel := filepath.Join(h.root, "runtime", "data", fixtureToken("app-data", fixtureToken("app", "api")), "sentinel")
	if got := string(mustRead(t, sentinel)); got != "managed-preserve\n" {
		t.Fatalf("managed sentinel = %q", got)
	}
	if got := string(mustRead(t, filepath.Join(h.root, "runtime", "data", "unowned-fixture", "sentinel"))); got != "unowned-preserve\n" {
		t.Fatalf("unowned sentinel = %q", got)
	}
	for _, directory := range []string{filepath.Join(h.root, "runtime", "dynamic"), filepath.Join(h.root, "runtime", "managed")} {
		info, statErr := os.Stat(directory)
		if statErr != nil || !info.IsDir() {
			t.Fatalf("retire removed required runtime artifact directory %s: %v", directory, statErr)
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatalf("read retirement artifact directory: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "route-") && strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), "env-") || strings.HasPrefix(entry.Name(), "config-") {
				t.Fatalf("retire left managed artifact %s", filepath.Join(directory, entry.Name()))
			}
		}
	}
	var inventory struct {
		Version int               `json:"version"`
		Objects []json.RawMessage `json:"objects"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "runtime", "managed", "inventory.json")), &inventory); err != nil || inventory.Version != 2 || len(inventory.Objects) != 0 {
		t.Fatalf("retire inventory evidence = %#v, %v; want empty managed-object inventory", inventory, err)
	}
	if info, err := os.Stat(filepath.Join(h.root, "runtime", "managed")); err != nil || !info.IsDir() {
		t.Fatalf("managed artifact directory does not retain required inventory parent: %v", err)
	}
	var docker dockerTrace
	state := runtimeState(t, h)
	networkName := "s2h-net-" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value)
	networkOwner := "s2h1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value, "network", "", "")
	networkLabel := "s2hnet1:" + fixtureToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value)
	ownerSum, labelSum := sha256.Sum256([]byte(networkOwner)), sha256.Sum256([]byte(networkLabel))
	wantNetwork := dockerEffect{Action: "network-rm", Name: networkName, OwnerDigest: hex.EncodeToString(ownerSum[:]), TargetDigest: hex.EncodeToString(labelSum[:])}
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.trace, "docker-state", "state.json")), &docker); err != nil || len(docker.Effects) != len(before)+1 || len(docker.Containers) != 0 || len(docker.Networks) != 0 || !reflect.DeepEqual(docker.Effects[:len(before)], before) || docker.Effects[len(before)] != wantNetwork {
		t.Fatalf("retire Docker model/effects are not exact: %v", err)
	}
	if preRetire == nil || state.Resource != (hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}) || state.Retirement == nil || state.Retirement.Machine != state.Machine || state.Retirement.Ownership != state.Ownership || !state.Retirement.PreserveData || state.LastOperation == nil || !reflect.DeepEqual(*state.LastOperation, *preRetire) || state.LastOperation.Status != "complete" || state.LastOperation.Key.Action != hostcontract.ActionReconcile || state.LastOperation.Result == nil || state.LastOperation.Result.Status != hostprotocol.ResultApplied || state.Journal == nil || state.Journal.Status != "complete" || state.Journal.Key.Action != hostcontract.ActionRetirePreserveData || state.Journal.Key.TargetRevision != state.AppliedRevision || state.Journal.Key.PriorAppliedRevision != state.AppliedRevision || state.Journal.Result == nil || state.Journal.Result.Status != hostprotocol.ResultRetired || state.Journal.Result.Machine == nil || state.Journal.Result.Ownership == nil || *state.Journal.Result.Machine != state.Machine || *state.Journal.Result.Ownership != state.Ownership || state.Journal.Result.Retirement == nil || !state.Journal.Result.Retirement.PreserveData {
		t.Fatal("retirement evidence is not complete")
	}
}

func assertSecretIsolation(t *testing.T, h *providerProcess, created *pulumirpc.CreateResponse) {
	t.Helper()
	checkpoint := unmarshalProperties(t, created.Properties)
	assertCheckpointSecrets(t, checkpoint)
	for _, name := range []string{"machine", "ownership", "appliedRevision", "observation"} {
		value, _ := checkpoint.GetOk(name)
		if strings.Contains(fmt.Sprint(value), ciSecret) {
			t.Fatalf("non-secret output %s leaked canary", name)
		}
	}
	_, _, revision, _, err := checkpointValues(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	allow := filepath.Join(h.root, "runtime", "managed", "env-"+fixtureToken("app", "api")+fixtureToken(revision))
	assertNoSecretCanary(t, h, allow)
	for _, subject := range h.approvals.Subjects() {
		if strings.Contains(fmt.Sprint(subject), ciSecret) {
			t.Fatal("approval record leaked secret")
		}
	}
}

func assertCheckpointSecrets(t *testing.T, checkpoint property.Map) {
	t.Helper()
	secrets, ok := checkpoint.GetOk("secrets")
	if !ok || !secrets.Secret() {
		t.Fatal("checkpoint secrets are not Pulumi-secret")
	}
}

func assertExactUpdateCheckpoint(t *testing.T, checkpoint, news property.Map, revision string) {
	t.Helper()
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		got, ok := checkpoint.GetOk(name)
		want, _ := news.GetOk(name)
		if !ok || !got.Equals(want) {
			t.Fatalf("Update checkpoint %s does not equal News", name)
		}
	}
	assertCheckpointSecrets(t, checkpoint)
	secrets, _ := checkpoint.GetOk("secrets")
	if !strings.Contains(string(mustJSON(t, secrets)), ciSecret) {
		t.Fatal("Update checkpoint lost nested secret canary")
	}
	_, _, applied, observation, err := checkpointValues(checkpoint)
	if err != nil || applied != revision || observation.AppliedRevision != revision || !observation.Ready {
		t.Fatalf("Update checkpoint revision/observation = %q %#v %v", applied, observation, err)
	}
	targetValue, _ := news.GetOk("target")
	var target hostcontract.Target
	if err := json.Unmarshal(mustJSON(t, targetValue), &target); err != nil {
		t.Fatal(err)
	}
	if observation.HostRelease != target.ReleaseArtifact || len(observation.Apps) != len(target.Apps) {
		t.Fatal("Update checkpoint observation does not cover News target")
	}
	for _, app := range target.Apps {
		matched := false
		for _, observed := range observation.Apps {
			if observed.ID == app.ID && observed.ActiveImage == app.Image && observed.Ready {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("Update checkpoint observation omitted app %q", app.ID)
		}
	}
}

func assertNoSecretCanary(t *testing.T, h *providerProcess, allowed string) {
	t.Helper()
	foundAllowed := false
	err := filepath.Walk(h.caseDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), ciSecret) {
			return nil
		}
		if allowed != "" && path == allowed {
			foundAllowed = true
			return nil
		}
		return fmt.Errorf("secret canary leaked to %s", filepath.Base(path))
	})
	if err != nil {
		t.Fatal(err)
	}
	if allowed != "" && !foundAllowed {
		t.Fatal("secret canary was not present in the current owned env file")
	}
}
