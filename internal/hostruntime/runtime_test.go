package hostruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

const testMachineID = "0123456789abcdef0123456789abcdef"

func TestMachineIdentityStrictInputAndGolden(t *testing.T) {
	rt := testRuntime(t)
	got, err := rt.MachineIdentity()
	if err != nil || got.Value != "mid1:0911601b3b0a5f6fdc51f3661518ee20e26ea0cbadfb4f7283e5b1f288941f54" {
		t.Fatalf("identity = %#v, %v", got, err)
	}
	for _, input := range []string{"", testMachineID + "\n\n", testMachineID + " ", "00000000000000000000000000000000", "0123456789ABCDEF0123456789abcdef"} {
		if err := os.WriteFile(rt.machinePath, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.MachineIdentity(); err == nil || bytes.Contains([]byte(err.Error()), []byte(testMachineID)) {
			t.Fatalf("accepted or leaked %q: %v", input, err)
		}
	}
}

func TestBootstrapBindsHostGeneratedOwnershipThenReconcilesAndReplaysExactly(t *testing.T) {
	rt := testRuntime(t)
	runner := &bootstrapLockRunner{lockPath: rt.lockPath(), discovery: map[string]bool{}}
	rt.runner = runner
	request := hostprotocol.Request{
		Action:               hostcontract.ActionReconcile,
		Server:               hostcontract.ServerTarget{SSHAlias: "edge"},
		Resource:             resource(),
		TargetRevision:       revisionB(),
		PriorAppliedRevision: revision(),
		Target:               &hostcontract.Target{ReleaseArtifact: "release", Apps: []hostcontract.AppTarget{app("one", "image-one")}},
		Secrets:              &hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"one": {JWTSecret: "BOOTSTRAP_SECRET_CANARY"}}},
	}

	result, err := rt.Bootstrap(context.Background(), request)
	if err != nil || result.Status != hostprotocol.ResultApplied || result.AppliedRevision != request.TargetRevision || result.Observation != nil {
		t.Fatalf("bootstrap result = %#v, %v", result, err)
	}
	state := mustState(t, rt)
	stateBytes, readErr := os.ReadFile(rt.statePath())
	resultBytes, marshalErr := json.Marshal(result)
	inventoryBytes := mustArtifact(t, rt, artifactInventory)
	artifacts := runtimeArtifactTree(t, filepath.Join(rt.root, "runtime"))
	if state.Resource != request.Resource || state.Machine != mustMachine(t, rt) || !regexp.MustCompile(`^oid1:[0-9a-f]{64}$`).MatchString(state.Ownership.Value) || !state.Observation.Ready || state.Observation.AppliedRevision != request.TargetRevision || state.Observation.Ownership != state.Ownership || state.Journal == nil || state.Journal.Status != journalComplete || state.Journal.Key != requestKey(request) || state.Journal.Result == nil || !reflect.DeepEqual(*state.Journal.Result, result) || readErr != nil || marshalErr != nil || bytes.Contains(stateBytes, []byte("BOOTSTRAP_SECRET_CANARY")) || bytes.Contains(resultBytes, []byte("BOOTSTRAP_SECRET_CANARY")) || runner.hasSecret("BOOTSTRAP_SECRET_CANARY") || !runner.lockHeld || !runner.discovery["container"] || !runner.discovery["network"] {
		t.Fatalf("bootstrap state = %#v, result = %#v", state, result)
	}
	calls := len(runner.calls)
	ownership := state.Ownership
	retry, err := rt.Bootstrap(context.Background(), request)
	retryStateBytes, retryReadErr := os.ReadFile(rt.statePath())
	retryInventoryBytes := mustArtifact(t, rt, artifactInventory)
	retryArtifacts := runtimeArtifactTree(t, filepath.Join(rt.root, "runtime"))
	if err != nil || !reflect.DeepEqual(retry, result) || retryReadErr != nil || !bytes.Equal(retryStateBytes, stateBytes) || !bytes.Equal(retryInventoryBytes, inventoryBytes) || !reflect.DeepEqual(retryArtifacts, artifacts) || mustState(t, rt).Ownership != ownership || len(runner.calls) != calls {
		t.Fatalf("exact bootstrap retry = %#v, %v; calls %d -> %d", retry, err, calls, len(runner.calls))
	}
	conflict := request
	conflict.TargetRevision = revisionC()
	conflict.PriorAppliedRevision = request.TargetRevision
	if _, err := rt.Bootstrap(context.Background(), conflict); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || len(runner.calls) != calls {
		t.Fatalf("conflicting bootstrap = %v; calls %d -> %d", err, calls, len(runner.calls))
	}
}

func TestBootstrapAdvancesCompletedManagedHostToNextReleaseWithoutRepeatingRetry(t *testing.T) {
	rt, old := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	oldRequest := requestFor(old, revisionB(), app("one", "image-old"))
	oldResult, err := rt.Reconcile(t.Context(), oldRequest)
	if err != nil || oldResult.Status != hostprotocol.ResultApplied {
		t.Fatalf("trusted prior reconcile = %#v, %v", oldResult, err)
	}
	completed := mustState(t, rt)
	oldJournal := *completed.Journal
	next := requestFor(completed, revisionC(), app("one", "image-next"))
	next.Server = hostcontract.ServerTarget{SSHAlias: "edge"}
	next.Target.ReleaseArtifact = "release-next"

	mutations := runner.mutations()
	result, err := rt.Bootstrap(t.Context(), next)
	after := mustState(t, rt)
	if err != nil || result.Status != hostprotocol.ResultApplied || result.AppliedRevision != next.TargetRevision || runner.mutations() <= mutations || after.Resource != completed.Resource || after.Machine != completed.Machine || after.Ownership != completed.Ownership || after.AppliedRevision != next.TargetRevision || after.Observation.HostRelease != next.Target.ReleaseArtifact || after.Observation.AppliedRevision != next.TargetRevision || len(after.Observation.Apps) != 1 || after.Observation.Apps[0].ID != "one" || after.Observation.Apps[0].ActiveImage != "image-next" || !after.Observation.Apps[0].Ready || after.LastOperation == nil || !reflect.DeepEqual(*after.LastOperation, oldJournal) || after.Journal == nil || after.Journal.Status != journalComplete || after.Journal.Key != requestKey(next) || after.Journal.Result == nil || !reflect.DeepEqual(*after.Journal.Result, result) {
		t.Fatalf("completed Host upgrade = %#v, %#v, %v", result, after, err)
	}
	calls := len(runner.calls)
	retry, err := rt.Bootstrap(t.Context(), next)
	if err != nil || !reflect.DeepEqual(retry, result) || len(runner.calls) != calls {
		t.Fatalf("exact completed Host upgrade retry = %#v, %v; calls %d -> %d", retry, err, calls, len(runner.calls))
	}
}

func TestBootstrapRejectsUnsafeNextOperationWithoutMutation(t *testing.T) {
	for _, scenario := range []func(*State, *hostprotocol.Request){
		func(state *State, request *hostprotocol.Request) { request.Resource.ServerKey = "other" },
		func(state *State, request *hostprotocol.Request) { state.Machine.Value = "mid1:other"; state.Observation.Machine = state.Machine },
		func(_ *State, request *hostprotocol.Request) { request.PriorAppliedRevision = revisionC() },
		func(state *State, _ *hostprotocol.Request) { state.Journal = &Journal{Key: reconcileKey(*state, revision()), Status: journalPending} },
	} {
		rt, state := initialized(t)
		key := reconcileKey(state, revisionB())
		op, err := rt.Begin(key, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Complete(applied(key, state), observation(state, key.TargetRevision)); err != nil {
			t.Fatal(err)
		}
		state = mustState(t, rt)
		next := bootstrapRequest()
		next.TargetRevision, next.PriorAppliedRevision = revisionC(), state.AppliedRevision
		next.Target.ReleaseArtifact = "release-next"
		scenario(&state, &next)
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rt.statePath(), raw, 0600); err != nil {
			t.Fatal(err)
		}
		runner := &recordingRunner{}
		rt.runner = runner
		if _, err := rt.Bootstrap(t.Context(), next); err == nil || runner.mutations() != 0 || len(runner.calls) != 0 || !bytes.Equal(mustFile(t, rt.statePath()), raw) {
			t.Fatal("unsafe Host upgrade bootstrap mutated managed state")
		}
	}
}

func TestBootstrapRejectsInvalidMachineBeforeStateOrMutationWithoutSecretLeak(t *testing.T) {
	rt := testRuntime(t)
	if err := os.WriteFile(rt.machinePath, []byte("00000000000000000000000000000000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	rt.runner = runner
	request := hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource(), TargetRevision: revisionB(), PriorAppliedRevision: revision(), Target: &hostcontract.Target{ReleaseArtifact: "release", Apps: []hostcontract.AppTarget{app("one", "image-one")}}, Secrets: &hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"one": {JWTSecret: "BOOTSTRAP_SECRET_CANARY"}}}}
	_, err := rt.Bootstrap(context.Background(), request)
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("BOOTSTRAP_SECRET_CANARY")) || runner.hasSecret("BOOTSTRAP_SECRET_CANARY") || len(runner.calls) != 0 {
		t.Fatalf("invalid machine bootstrap = %v, calls=%#v", err, runner.calls)
	}
	if _, err := os.Lstat(rt.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid machine bootstrap created state root: %v", err)
	}
}

func TestBootstrapRejectsExistingSecureRootWithoutStateAsNonFresh(t *testing.T) {
	rt := testRuntime(t)
	if err := os.Mkdir(rt.root, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(rt.root, "runtime-evidence")
	if err := os.WriteFile(evidence, []byte("pre-existing-runtime-evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Bootstrap(context.Background(), bootstrapRequest()); !(isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired)) {
		t.Fatalf("existing root bootstrap = %v", err)
	}
	if _, err := os.Lstat(rt.statePath()); !errors.Is(err, os.ErrNotExist) || len(runner.calls) != 0 {
		t.Fatalf("existing root created state or ran docker: %v, %#v", err, runner.calls)
	}
	if got, err := os.ReadFile(evidence); err != nil || !bytes.Equal(got, []byte("pre-existing-runtime-evidence")) {
		t.Fatalf("existing root evidence changed = %q, %v", got, err)
	}
}

func TestBootstrapRejectsExistingDockerOwnershipEvidenceBeforeStateOrMutation(t *testing.T) {
	for _, name := range []string{"container", "network"} {
		t.Run(name, func(t *testing.T) {
			rt := testRuntime(t)
			runner := &bootstrapDiscoveryRunner{kind: name, evidence: true}
			rt.runner = runner
			if _, err := rt.Bootstrap(context.Background(), bootstrapRequest()); !(isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired)) {
				t.Fatalf("existing %s ownership = %v", name, err)
			}
			if _, err := os.Lstat(rt.root); !errors.Is(err, os.ErrNotExist) || runner.dockerMutations() != 0 || !runner.discovery[name] || !onlyBootstrapDiscovery(runner.calls) {
				t.Fatalf("existing %s ownership created state or mutated runtime: %v, %#v", name, err, runner.calls)
			}
		})
	}
}

func TestBootstrapDiscoveryErrorLeavesAbsentRootUntouched(t *testing.T) {
	for _, kind := range []string{"container", "network"} {
		t.Run(kind, func(t *testing.T) {
			rt := testRuntime(t)
			runner := &bootstrapDiscoveryRunner{kind: kind, discoveryErr: errors.New("daemon unavailable")}
			rt.runner = runner
			if _, err := rt.Bootstrap(context.Background(), bootstrapRequest()); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
				t.Fatalf("%s discovery error = %v", kind, err)
			}
			if _, err := os.Lstat(rt.root); !errors.Is(err, os.ErrNotExist) || runner.dockerMutations() != 0 || !runner.discovery[kind] || !onlyBootstrapDiscovery(runner.calls) {
				t.Fatalf("%s discovery error created root or mutated runtime: %v, %#v", kind, err, runner.calls)
			}
		})
	}
}

func TestBootstrapIgnoresEmptyDockerOwnershipLabelDuringDiscovery(t *testing.T) {
	rt := testRuntime(t)
	runner := &bootstrapDiscoveryRunner{kind: "container", emptyLabel: true}
	rt.runner = runner
	result, err := rt.Bootstrap(context.Background(), bootstrapRequest())
	if err != nil || result.Status != hostprotocol.ResultApplied || !runner.discovery["container"] || !runner.discovery["network"] {
		t.Fatalf("empty ownership discovery = %#v, %v, %#v", result, err, runner.calls)
	}
}

func TestBootstrapRejectsRootCreatedAfterCleanDiscovery(t *testing.T) {
	rt := testRuntime(t)
	sentinel := filepath.Join(rt.root, "racer-sentinel")
	runner := &bootstrapDiscoveryRunner{afterNetwork: func() error {
		if err := os.Mkdir(rt.root, 0700); err != nil {
			return err
		}
		return os.WriteFile(sentinel, []byte("foreign-root"), 0600)
	}}
	rt.runner = runner
	if _, err := rt.Bootstrap(context.Background(), bootstrapRequest()); !(isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired)) {
		t.Fatalf("post-discovery root bootstrap = %v", err)
	}
	if _, err := os.Lstat(rt.statePath()); !errors.Is(err, os.ErrNotExist) || len(runner.calls) != 2 || !onlyBootstrapDiscovery(runner.calls) {
		t.Fatalf("post-discovery root created state or ran unexpected command: %v, %#v", err, runner.calls)
	}
	if got, err := os.ReadFile(sentinel); err != nil || !bytes.Equal(got, []byte("foreign-root")) {
		t.Fatalf("post-discovery root sentinel changed = %q, %v", got, err)
	}
}

func TestBootstrapPreservesConflictingExistingStateWithoutTakeover(t *testing.T) {
	for _, name := range []string{"wrong resource", "wrong machine"} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			if name == "wrong resource" {
				state.Resource.ServerKey = "other"
			} else {
				state.Machine.Value = "mid1:other"
				state.Observation.Machine = state.Machine
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rt.statePath(), raw, 0600); err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{}
			rt.runner = runner
			request := hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource(), TargetRevision: revisionB(), PriorAppliedRevision: revision(), Target: &hostcontract.Target{ReleaseArtifact: "release"}, Secrets: &hostcontract.Secrets{}}
			if _, err := rt.Bootstrap(context.Background(), request); !(isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired)) || len(runner.calls) != 0 {
				t.Fatalf("conflicting bootstrap = %v, calls=%#v", err, runner.calls)
			}
			after, err := os.ReadFile(rt.statePath())
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatalf("conflicting state changed = %q, %v", after, err)
			}
		})
	}
}

type bootstrapLockRunner struct {
	recordingRunner
	lockPath  string
	checked   bool
	lockHeld  bool
	discovery map[string]bool
}

func (r *bootstrapLockRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	for _, kind := range []string{"container", "network"} {
		if bootstrapDiscovery(argv, kind) {
			r.discovery[kind] = true
			r.calls = append(r.calls, append([]string(nil), argv...))
			return nil, nil
		}
	}
	if !r.checked && bootstrapMutation(argv) {
		r.checked = true
		lock, err := os.OpenFile(r.lockPath, os.O_RDWR, 0)
		if err == nil {
			err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			r.lockHeld = errors.Is(err, syscall.EWOULDBLOCK)
			if err == nil {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			}
			_ = lock.Close()
		}
	}
	return r.recordingRunner.Run(ctx, argv, stdin)
}

func bootstrapMutation(argv []string) bool {
	return len(argv) > 0 && (argv[0] == "run" || argv[0] == "rm" || len(argv) > 1 && argv[0] == "network" && (argv[1] == "create" || argv[1] == "rm"))
}

type bootstrapDiscoveryRunner struct {
	recordingRunner
	kind            string
	evidence        bool
	emptyLabel      bool
	discoveryErr    error
	afterNetwork    func() error
	discovery       map[string]bool
}

func (r *bootstrapDiscoveryRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	for _, kind := range []string{"container", "network"} {
		if !bootstrapDiscovery(argv, kind) {
			continue
		}
		if r.discovery == nil {
			r.discovery = map[string]bool{}
		}
		r.discovery[kind] = true
		r.calls = append(r.calls, append([]string(nil), argv...))
		if r.discoveryErr != nil && kind == r.kind {
			return nil, r.discoveryErr
		}
		if r.evidence && kind == r.kind {
			return []byte("existing\towned\n"), nil
		}
		if r.emptyLabel && kind == r.kind {
			return []byte("existing\t\n"), nil
		}
		if kind == "network" && r.afterNetwork != nil {
			if err := r.afterNetwork(); err != nil {
				return nil, err
			}
			r.afterNetwork = nil
		}
		return nil, nil
	}
	return r.recordingRunner.Run(ctx, argv, stdin)
}

func bootstrapDiscovery(argv []string, kind string) bool {
	if kind == "container" {
		return reflect.DeepEqual(argv, []string{"container", "ls", "--all", "--filter", "label=sub2api.host", "--format", "{{.Names}}\t{{index .Labels \"sub2api.host\"}}"})
	}
	return kind == "network" && reflect.DeepEqual(argv, []string{"network", "ls", "--filter", "label=sub2api.host", "--format", "{{.Name}}\t{{index .Labels \"sub2api.host\"}}"})
}

func onlyBootstrapDiscovery(calls [][]string) bool {
	for _, call := range calls {
		if !bootstrapDiscovery(call, "container") && !bootstrapDiscovery(call, "network") {
			return false
		}
	}
	return true
}

func bootstrapRequest() hostprotocol.Request {
	return hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource(), TargetRevision: revisionB(), PriorAppliedRevision: revision(), Target: &hostcontract.Target{ReleaseArtifact: "release"}, Secrets: &hostcontract.Secrets{}}
}

func runtimeArtifactTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	var walk func(string)
	walk = func(path string) {
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := filepath.Join(path, entry.Name())
			if entry.IsDir() {
				walk(name)
				continue
			}
			contents, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			files[name] = contents
		}
	}
	walk(root)
	return files
}

func TestBootstrapRejectsNonReconcileWithoutCreatingUsableState(t *testing.T) {
	rt := testRuntime(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := hostprotocol.Request{
		Action:         hostcontract.ActionInspect,
		Server:         hostcontract.ServerTarget{SSHAlias: "edge"},
		Resource:       resource(),
		TargetRevision: revision(),
	}
	if _, err := rt.Bootstrap(context.Background(), request); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("inspect bootstrap = %v", err)
	}
	if _, err := os.Lstat(rt.root); !errors.Is(err, os.ErrNotExist) || runner.mutations() != 0 {
		t.Fatalf("rejected bootstrap created state root or mutated runtime: %v, %#v", err, runner.calls)
	}
}

func TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry(t *testing.T) {
	rt, state := initialized(t)
	key := reconcileKey(state, revisionB())
	started := make(chan struct{})
	release := make(chan struct{})
	var effects atomic.Int32
	firstDone := make(chan error, 1)
	go func() {
		_, err := rt.RunOperation(key, nil, func(*Operation) (hostprotocol.Result, hostcontract.StableObservation, error) {
			effects.Add(1)
			close(started)
			<-release
			return applied(key, state), observation(state, key.TargetRevision), nil
		})
		firstDone <- err
	}()
	<-started
	if _, err := rt.Begin(key, nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("concurrent begin = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	result, err := rt.RunOperation(key, nil, func(*Operation) (hostprotocol.Result, hostcontract.StableObservation, error) {
		effects.Add(1)
		return hostprotocol.Result{}, hostcontract.StableObservation{}, nil
	})
	if err != nil || result.Status != hostprotocol.ResultApplied || effects.Load() != 1 {
		t.Fatalf("retry = %#v, %v, effects %d", result, err, effects.Load())
	}
}

func TestPendingResumeConflictAndCompletionAdvancesState(t *testing.T) {
	rt, state := initialized(t)
	key := reconcileKey(state, revisionB())
	op, err := rt.Begin(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	} // Simulate response/process loss after intent persistence.
	other := reconcileKey(state, revisionC())
	if _, err := rt.Begin(other, nil); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("different pending = %v", err)
	}
	op, err = rt.Begin(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Complete(applied(key, state), observation(state, key.TargetRevision)); err != nil {
		t.Fatal(err)
	}
	got, err := rt.readState()
	if err != nil || got.AppliedRevision != key.TargetRevision || got.Observation.AppliedRevision != key.TargetRevision || got.Journal == nil || got.Journal.Status != journalComplete {
		t.Fatalf("completion state = %#v, %v", got, err)
	}
}

func TestNextOperationReplacesTerminalAndRetireIsDurable(t *testing.T) {
	rt, state := initialized(t)
	key := reconcileKey(state, revisionB())
	op, err := rt.Begin(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Complete(applied(key, state), observation(state, key.TargetRevision)); err != nil {
		t.Fatal(err)
	}
	nextState, _ := rt.readState()
	next := reconcileKey(nextState, revisionC())
	op, err = rt.Begin(next, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateState(op.state); err != nil {
		t.Fatalf("pending next state invalid: %v: %#v", err, op.state)
	}
	completeResult, completeObservation := applied(next, nextState), observation(nextState, next.TargetRevision)
	if err := op.Complete(completeResult, completeObservation); err != nil {
		t.Fatal(err)
	}
	retry, err := rt.Begin(key, nil)
	if err != nil || !retry.closed || retry.state.Journal.Result.Status != hostprotocol.ResultApplied {
		t.Fatalf("previous terminal retry = %#v, %v", retry, err)
	}
	retireState, _ := rt.readState()
	retire := retireKey(retireState)
	approval := retireApproval(retire, retireState)
	op, err = rt.Begin(retire, &approval)
	if err != nil {
		t.Fatal(err)
	}
	result := hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &retireState.Machine, Ownership: &retireState.Ownership, Retirement: &hostprotocol.RetirementEvidence{PreserveData: true}}
	if err := op.Complete(result, hostcontract.StableObservation{}); err != nil {
		t.Fatal(err)
	}
	got, err := rt.readState()
	if err != nil || got.Retirement == nil || got.Journal == nil || got.Journal.Result.Status != hostprotocol.ResultRetired {
		t.Fatalf("retire state = %#v, %v", got, err)
	}
}

func TestApprovalAndPreconditionsAreExact(t *testing.T) {
	rt, state := initialized(t)
	key := reconcileKey(state, revisionB())
	bad := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: state.Resource.Environment, Resource: state.Resource, Machine: state.Machine, Ownership: state.Ownership, TargetRevision: revisionC(), PreserveData: true}
	if _, err := rt.Begin(key, &bad); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("bad approval = %v", err)
	}
	key.PriorAppliedRevision = revisionC()
	if _, err := rt.Begin(key, nil); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("prior revision = %v", err)
	}
	key = reconcileObservationKey(state, revisionB())
	key.PriorObservation = "wrong"
	if _, err := rt.Begin(key, nil); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("prior observation = %v", err)
	}
}

func TestInspectNeverCreatesAndCorruptStateIsPreserved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	machine := filepath.Join(t.TempDir(), "machine")
	if err := os.WriteFile(machine, []byte(testMachineID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rt := New(root, machine)
	if _, err := rt.Inspect(resource()); !isRemote(err, hostprotocol.ErrorTransport, hostprotocol.CodeUnavailable) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect created root: %v", err)
	}
	rt, state := initialized(t)
	valid, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Replace(valid, []byte(`"version"`), []byte(`"Version"`), 1)
	if err := os.WriteFile(rt.statePath(), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Inspect(state.Resource); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("case fold state = %v", err)
	}
	got, _ := os.ReadFile(rt.statePath())
	if !bytes.Equal(got, bad) {
		t.Fatal("inspect modified corrupt state")
	}
}

func TestPersistedPendingRevalidatesPreconditionAndApproval(t *testing.T) {
	rt, state := initialized(t)
	key := reconcileKey(state, revisionB())
	op, err := rt.Begin(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := rt.readState()
	if err != nil {
		t.Fatal(err)
	}
	persisted.AppliedRevision = revisionB()
	persisted.Observation.AppliedRevision = revisionB()
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rt.statePath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Begin(key, nil); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("stale resumed precondition = %v", err)
	}
}

func TestStatePathHardeningAndAdoption(t *testing.T) {
	rt := testRuntime(t)
	if err := os.MkdirAll(rt.root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := rt.Initialize(resource(), ownership(), observationFor(rt, revision())); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("permissive root = %v", err)
	}
	if err := os.Chmod(rt.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := rt.Initialize(resource(), ownership(), observationFor(rt, revision())); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rt.statePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", rt.statePath()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Inspect(resource()); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("state symlink = %v", err)
	}
}

func TestStateRejectsContradictionsAndSecrets(t *testing.T) {
	rt, state := initialized(t)
	state.Observation.Ownership = hostcontract.OwnershipIdentity{Value: "other"}
	if err := rt.writeState(state); err == nil {
		t.Fatal("contradiction accepted")
	}
	key := reconcileKey(testState(t, rt), revisionB())
	approval := hostcontract.ApprovalSubject{AppID: "do-not-persist-secret"}
	_, _ = rt.Begin(key, &approval)
	b, _ := os.ReadFile(rt.statePath())
	if bytes.Contains(b, []byte("do-not-persist-secret")) {
		t.Fatal("secret persisted")
	}
}

func TestStateRejectsInvalidJournalAndRetirementCombinations(t *testing.T) {
	_, state := initialized(t)
	key := reconcileKey(state, revisionB())
	appliedResult := applied(key, state)
	validTerminal := &Journal{Key: key, Status: journalComplete, Result: &appliedResult}
	retire := retireKey(state)
	retireResult := hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &state.Machine, Ownership: &state.Ownership, Retirement: &hostprotocol.RetirementEvidence{PreserveData: true}}
	for name, mutate := range map[string]func(*State){
		"applied revision differs from key": func(s *State) {
			r := appliedResult
			r.AppliedRevision = revisionC()
			s.Journal = &Journal{Key: key, Status: journalComplete, Result: &r}
		},
		"applied result on retire": func(s *State) {
			s.Journal = &Journal{Key: retire, Status: journalComplete, Approval: ptr(retireApproval(retire, state)), Result: &appliedResult}
		},
		"retired result on reconcile": func(s *State) { s.Journal = &Journal{Key: key, Status: journalComplete, Result: &retireResult} },
		"pending stale prior": func(s *State) {
			stale := key
			stale.PriorAppliedRevision = revisionC()
			s.Journal = &Journal{Key: stale, Status: journalPending}
		},
		"orphan retirement": func(s *State) {
			s.Retirement = &Retirement{Machine: s.Machine, Ownership: s.Ownership, PreserveData: true}
		},
		"last operation pending": func(s *State) { s.LastOperation = &Journal{Key: key, Status: journalPending} },
		"journal last contradict": func(s *State) {
			s.Journal = validTerminal
			other := *validTerminal
			other.Key.TargetRevision = revisionC()
			s.LastOperation = &other
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := state
			mutate(&candidate)
			if err := validateState(candidate); err == nil {
				t.Fatalf("accepted %#v", candidate)
			}
		})
	}
}

func TestInspectIdentityMismatchRequiresRecovery(t *testing.T) {
	rt, state := initialized(t)
	if _, err := rt.Inspect(hostcontract.ResourceIdentity{Environment: "other", ServerKey: state.Resource.ServerKey}); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("resource mismatch = %v", err)
	}
	if err := os.WriteFile(rt.machinePath, []byte("fedcba9876543210fedcba9876543210\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Inspect(state.Resource); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("machine mismatch = %v", err)
	}
}

func TestAtomicWriteFailureDoesNotReplaceState(t *testing.T) {
	rt, state := initialized(t)
	before, err := os.ReadFile(rt.statePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"chmod", "write", "fsync", "close", "rename", "dirsync"} {
		t.Run(step, func(t *testing.T) {
			stateWriteHook = func(got string) error {
				if got == step {
					return errors.New("injected")
				}
				return nil
			}
			t.Cleanup(func() { stateWriteHook = nil })
			candidate := state
			candidate.Observation.HostRelease = "release2"
			if err := rt.writeState(candidate); err == nil {
				t.Fatal("write succeeded")
			}
			after, err := os.ReadFile(rt.statePath())
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("old state changed: %v", err)
			}
		})
	}
}

func TestWriterReclaimsOnlySafeStaleStateTemp(t *testing.T) {
	rt, state := initialized(t)
	stale := filepath.Join(rt.root, ".state-tmp")
	if err := os.WriteFile(stale, []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	key := reconcileKey(state, revisionB())
	op, err := rt.Begin(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = op.Complete(applied(key, state), observation(state, key.TargetRevision)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temp remains: %v", err)
	}

	before := mustFile(t, rt.statePath())
	if err = os.Symlink("state.json", stale); err != nil {
		t.Fatal(err)
	}
	next := reconcileKey(mustState(t, rt), revisionC())
	if _, err = rt.Begin(next, nil); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("unsafe temp = %v", err)
	}
	if !bytes.Equal(before, mustFile(t, rt.statePath())) {
		t.Fatal("unsafe temp changed state")
	}
}

func TestWriterRejectsStaleFIFOWithoutBlockingOrUnlinking(t *testing.T) {
	rt, state := initialized(t)
	temp := filepath.Join(rt.root, ".state-tmp")
	if err := syscall.Mkfifo(temp, 0600); err != nil {
		t.Fatal(err)
	}
	before := mustFile(t, rt.statePath())
	key := reconcileKey(state, revisionB())
	if _, err := rt.Begin(key, nil); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("fifo temp = %v", err)
	}
	info, err := os.Lstat(temp)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
		t.Fatalf("fifo changed: info=%#v err=%v", info, err)
	}
}

func TestPostRenameDirectorySyncFailureLeavesReadableNewState(t *testing.T) {
	rt, state := initialized(t)
	candidate := state
	candidate.Observation.HostRelease = "release2"
	syncDirHook = func() error { return errors.New("injected") }
	t.Cleanup(func() { syncDirHook = nil })
	if err := rt.writeState(candidate); err == nil {
		t.Fatal("write succeeded")
	}
	got, err := rt.readState()
	if err != nil || got.Observation.HostRelease != "release2" {
		t.Fatalf("new state is not strictly readable: %#v, %v", got, err)
	}
}

func TestJournalRejectsMalformedKeyRevision(t *testing.T) {
	_, state := initialized(t)
	for name, journal := range map[string]Journal{
		"pending":          {Key: hostcontract.OperationKey{Resource: state.Resource, Action: hostcontract.ActionReconcile, TargetRevision: "not-a-revision", PriorAppliedRevision: state.AppliedRevision}, Status: journalPending},
		"completed retire": {Key: hostcontract.OperationKey{Resource: state.Resource, Action: hostcontract.ActionRetirePreserveData, TargetRevision: "not-a-revision", PriorAppliedRevision: state.AppliedRevision}, Status: journalComplete, Approval: ptr(hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: state.Resource.Environment, Resource: state.Resource, Machine: state.Machine, Ownership: state.Ownership, TargetRevision: "not-a-revision", PreserveData: true}), Result: &hostprotocol.Result{Status: hostprotocol.ResultRetired, Machine: &state.Machine, Ownership: &state.Ownership, Retirement: &hostprotocol.RetirementEvidence{PreserveData: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := state
			candidate.Journal = &journal
			if err := validateState(candidate); err == nil {
				t.Fatalf("accepted %#v", candidate.Journal)
			}
		})
	}
}

func TestDescriptorPathRejectsUnsafeLockAndWrongOwner(t *testing.T) {
	rt, state := initialized(t)
	if err := os.Remove(rt.lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("state.json", rt.lockPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.lock(); err == nil {
		t.Fatal("lock symlink accepted")
	}
	if err := os.Remove(rt.lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(rt.statePath(), rt.lockPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.lock(); err == nil {
		t.Fatal("lock hardlink accepted")
	}
	if err := os.Remove(rt.lockPath()); err != nil {
		t.Fatal(err)
	}
	lock, err := rt.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	assertSafeSingleLinkFixture(t, rt.statePath())
	assertSafeSingleLinkFixture(t, rt.lockPath())
	rt.expectedRootUID = os.Geteuid()
	rt.expectedUID++
	if _, err := rt.Inspect(state.Resource); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("state wrong owner = %v", err)
	}
	if _, err := rt.lock(); err == nil {
		t.Fatal("lock wrong owner accepted")
	}
}

func assertSafeSingleLinkFixture(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 {
		t.Fatalf("unsafe fixture %q: mode=%v stat=%#v", path, info.Mode(), info.Sys())
	}
}

func TestReadStateTransfersDescriptorOwnershipToFile(t *testing.T) {
	rt, state := initialized(t)
	for i := 0; i < 512; i++ {
		got, err := rt.readState()
		if err != nil || got.Resource != state.Resource {
			t.Fatalf("read %d = %#v, %v", i, got, err)
		}
	}
}

func TestDescriptorPathRejectsParentSymlinkAndHardlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	machine := filepath.Join(base, "machine")
	if err := os.WriteFile(machine, []byte(testMachineID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rt := New(filepath.Join(link, "state"), machine)
	if err := rt.Initialize(resource(), ownership(), observationFor(rt, revision())); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("parent symlink = %v", err)
	}
	rt, state := initialized(t)
	copy := filepath.Join(filepath.Dir(rt.statePath()), "state-copy")
	if err := os.Link(rt.statePath(), copy); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rt.statePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(copy, rt.statePath()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Inspect(state.Resource); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("hardlink state = %v", err)
	}
}

func initialized(t *testing.T) (*Runtime, State) {
	t.Helper()
	rt := testRuntime(t)
	state := testState(t, rt)
	if err := rt.Initialize(state.Resource, state.Ownership, state.Observation); err != nil {
		t.Fatal(err)
	}
	return rt, state
}
func testRuntime(t *testing.T) *Runtime {
	t.Helper()
	root := t.TempDir()
	machine := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machine, []byte(testMachineID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return New(filepath.Join(root, "state"), machine)
}
func resource() hostcontract.ResourceIdentity {
	return hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}
}
func ownership() hostcontract.OwnershipIdentity {
	return hostcontract.OwnershipIdentity{Value: "owner1"}
}
func testState(t *testing.T, rt *Runtime) State {
	t.Helper()
	return State{Version: stateVersion, Resource: resource(), Machine: mustMachine(t, rt), Ownership: ownership(), AppliedRevision: revision(), Observation: observationFor(rt, revision())}
}
func mustMachine(t *testing.T, rt *Runtime) hostcontract.MachineIdentity {
	t.Helper()
	v, err := rt.MachineIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func observationFor(rt *Runtime, rev string) hostcontract.StableObservation {
	return hostcontract.StableObservation{Machine: mustMachineNoTest(rt), Ownership: ownership(), HostRelease: "release1", AppliedRevision: rev, Ready: true}
}
func mustMachineNoTest(rt *Runtime) hostcontract.MachineIdentity {
	v, err := rt.MachineIdentity()
	if err != nil {
		panic(err)
	}
	return v
}
func observation(state State, rev string) hostcontract.StableObservation {
	o := state.Observation
	o.AppliedRevision = rev
	return o
}
func reconcileKey(s State, target string) hostcontract.OperationKey {
	return hostcontract.OperationKey{Resource: s.Resource, Action: hostcontract.ActionReconcile, TargetRevision: target, PriorAppliedRevision: s.AppliedRevision}
}
func reconcileObservationKey(s State, target string) hostcontract.OperationKey {
	return hostcontract.OperationKey{Resource: s.Resource, Action: hostcontract.ActionReconcile, TargetRevision: target, PriorObservation: observationFingerprint(s.Observation)}
}
func retireKey(s State) hostcontract.OperationKey {
	return hostcontract.OperationKey{Resource: s.Resource, Action: hostcontract.ActionRetirePreserveData, TargetRevision: s.AppliedRevision, PriorAppliedRevision: s.AppliedRevision}
}
func retireApproval(k hostcontract.OperationKey, s State) hostcontract.ApprovalSubject {
	return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: s.Resource.Environment, Resource: s.Resource, Machine: s.Machine, Ownership: s.Ownership, TargetRevision: k.TargetRevision, PreserveData: true}
}
func applied(k hostcontract.OperationKey, _ State) hostprotocol.Result {
	return hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: k.TargetRevision}
}
func ptr[T any](value T) *T { return &value }
func revision() string {
	return "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
func revisionB() string { return "tr1:0123456789abcdef:" + strings.Repeat("a", 64) }
func revisionC() string { return "tr1:0123456789abcdef:" + strings.Repeat("b", 64) }
func isRemote(err error, category hostprotocol.ErrorCategory, code hostprotocol.ErrorCode) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && remote.Category == category && remote.Code == code
}
func mustState(t *testing.T, rt *Runtime) State {
	t.Helper()
	state, err := rt.readState()
	if err != nil {
		t.Fatal(err)
	}
	return state
}
