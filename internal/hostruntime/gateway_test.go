package hostruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

func TestReconcileCreatesSingleGatewayWithFixedRuntimeContract(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "exec" && argv[2] == "wget" {
			return nil
		}
		return nil
	}
	rt.runner = runner

	request := requestFor(state, revisionB())
	request.Target.PaymentGateways = []hostcontract.PaymentGatewayTarget{{
		ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test",
	}}

	if _, err := rt.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("%v calls=%#v", err, runner.calls)
	}

	token := paymentGatewayToken("primary")
	object := findPaymentGateway(mustInventory(t, rt), token)
	if object.Name == "" || object.DataToken == "" {
		t.Fatalf("gateway inventory = %#v", object)
	}
	if !runner.hasCall([]string{"pull", "gmwallet/epusdt:v2.0.0"}) {
		t.Fatalf("gateway pull trace = %#v", runner.calls)
	}
	want := []string{
		"run", "--pull", "never", "-d", "--restart", "unless-stopped",
		"--label", "sub2api.host=" + ownershipLabelFor(state.Resource, state.Ownership, "payment-gateway", token, "live"),
		"--label", "sub2api.host.target=" + targetLabelFor(object),
		"--name", object.Name,
		"--network", networkName(state),
		"--network-alias", paymentGatewayAlias(token),
		"-e", "EPUSDT_CONFIG=/data/.env",
		"-v", rt.dataPath(object.DataToken) + ":/data",
		"gmwallet/epusdt:v2.0.0",
	}
	if !runner.hasCall(want) {
		t.Fatalf("gateway run trace = %#v\nwant = %#v", runner.calls, want)
	}
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "run" && strings.Contains(strings.Join(call, "\x00"), "-p\x00") {
			t.Fatalf("gateway published a host port: %#v", call)
		}
	}
	if got := string(mustRouteArtifact(t, rt, routeName(token))); !strings.Contains(got, "http://"+object.Name+":8000") {
		t.Fatalf("gateway route = %s", got)
	}
	stored := mustState(t, rt)
	if len(stored.Observation.PaymentGateways) != 1 || stored.Observation.PaymentGateways[0] != (hostcontract.PaymentGatewayObservation{ID: "primary", ActiveImage: "gmwallet/epusdt:v2.0.0", Ready: true}) {
		t.Fatalf("gateway observation = %#v", stored.Observation.PaymentGateways)
	}
}

func TestGatewayContractDriftMarksObservationAndFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(*gatewayInspectState, managedObject, State)
	}{
		{name: "image", tamper: func(s *gatewayInspectState, o managedObject, _ State) { s.Image = "gmwallet/epusdt:foreign" }},
		{name: "restart-policy", tamper: func(s *gatewayInspectState, _ managedObject, _ State) { s.RestartPolicy = "always" }},
		{name: "host-port", tamper: func(s *gatewayInspectState, _ managedObject, _ State) {
			s.PortBindings = map[string][]gatewayPortBinding{"8000/tcp": {{HostIP: "0.0.0.0", HostPort: "8000"}}}
		}},
		{name: "publish-all-ports", tamper: func(s *gatewayInspectState, _ managedObject, _ State) { s.PublishAllPorts = true }},
		{name: "data-bind", tamper: func(s *gatewayInspectState, o managedObject, _ State) {
			s.Binds = []string{"/tmp/foreign:/data"}
			_ = o
		}},
		{name: "data-bind-read-only", tamper: func(s *gatewayInspectState, o managedObject, _ State) {
			s.Binds = []string{"/data/" + o.DataToken + ":/data:ro"}
		}},
		{name: "config-missing", tamper: func(s *gatewayInspectState, _ managedObject, _ State) { s.Env = []string{} }},
		{name: "config-duplicate", tamper: func(s *gatewayInspectState, _ managedObject, _ State) {
			s.Env = append(s.Env, "EPUSDT_CONFIG=/data/.env")
		}},
		{name: "config-conflicting", tamper: func(s *gatewayInspectState, _ managedObject, _ State) {
			s.Env = append(s.Env, "EPUSDT_CONFIG=/tmp/foreign.env")
		}},
		{name: "extra-network", tamper: func(s *gatewayInspectState, _ managedObject, state State) {
			s.Networks["foreign-network"] = []string{"gateway"}
			_ = state
		}},
		{name: "wrong-network-alias", tamper: func(s *gatewayInspectState, _ managedObject, state State) {
			s.Networks[networkName(state)] = []string{"gateway", "foreign-alias"}
		}},
		{name: "stopped", tamper: func(s *gatewayInspectState, _ managedObject, _ State) { s.Running = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
			if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})); err != nil {
				t.Fatal(err)
			}
			state = mustState(t, rt)
			object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
			contract := runner.gatewayInspect[object.Name]
			tc.tamper(&contract, object, state)
			runner.gatewayInspect[object.Name] = contract
			inspect, err := rt.Handle(t.Context(), hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource})
			if err != nil {
				t.Fatalf("inspect error: %v", err)
			}
			if inspect.Observation == nil || inspect.Observation.Ready || len(inspect.Observation.PaymentGateways) != 1 || inspect.Observation.PaymentGateways[0].Ready {
				t.Fatalf("drift observation = %#v", inspect.Observation)
			}
			runner.calls = nil
			if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
				t.Fatalf("drift reconcile = %v calls=%#v", err, runner.calls)
			}
			if runner.mutations("pull", "run", "stop", "start", "rename", "rm") != 0 {
				t.Fatalf("drift reconcile mutated container: %#v", runner.calls)
			}
		})
	}
}

func TestGatewayInspectAcceptsExplicitWritableBindMode(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	contract := runner.gatewayInspect[object.Name]
	contract.Binds = []string{rt.dataPath(object.DataToken) + ":/data:rw"}
	runner.gatewayInspect[object.Name] = contract
	result, err := rt.Handle(t.Context(), hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource})
	if err != nil || result.Observation == nil || !result.Observation.Ready || result.Observation.Drifted {
		t.Fatalf("explicit writable bind = %#v %v", result, err)
	}
}

func TestGatewayInspectPropagatesDockerInspectRecoveryFailure(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	rt.runner = gatewayInspectOverrideRunner{recordingRunner: runner, output: []byte("{")}
	_, err := rt.Handle(t.Context(), hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource})
	if !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("malformed gateway inspect = %v", err)
	}
}

func TestDecodeGatewayDockerInspectRejectsNullRequiredFields(t *testing.T) {
	valid := []byte(`{"Config":{"Image":"image","Env":["EPUSDT_CONFIG=/data/.env"]},"HostConfig":{"RestartPolicy":{"Name":"unless-stopped"},"Binds":["/data:/data"],"PortBindings":null,"PublishAllPorts":false},"NetworkSettings":{"Networks":{"managed":{"Aliases":["name","alias"]}}},"State":{"Running":true}}`)
	for _, field := range []string{"Config", "HostConfig", "NetworkSettings", "State"} {
		t.Run(field, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(valid, &document); err != nil {
				t.Fatal(err)
			}
			document[field] = nil
			b, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeGatewayDockerInspect(b, &gatewayDockerInspect{}); err == nil {
				t.Fatalf("null %s was accepted", field)
			}
		})
	}
	for _, field := range []string{"Image", "Env", "RestartPolicy", "Binds", "PublishAllPorts", "Networks", "Running"} {
		t.Run(field, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(valid, &document); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "Image", "Env":
				document["Config"].(map[string]any)[field] = nil
			case "RestartPolicy":
				document["HostConfig"].(map[string]any)[field] = nil
			case "Binds", "PublishAllPorts":
				document["HostConfig"].(map[string]any)[field] = nil
			case "Networks":
				document["NetworkSettings"].(map[string]any)[field] = nil
			case "Running":
				document["State"].(map[string]any)[field] = nil
			}
			b, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeGatewayDockerInspect(b, &gatewayDockerInspect{}); err == nil {
				t.Fatalf("null %s was accepted", field)
			}
		})
	}
}

func TestGatewayInspectAcceptsRealDockerEnvelopeWithUnrelatedFields(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	before := len(runner.calls)
	rt.runner = realisticGatewayInspectRunner{recordingRunner: runner}
	result, err := rt.Handle(t.Context(), hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource})
	if err != nil || result.Observation == nil || !result.Observation.Ready || result.Observation.Drifted {
		t.Fatalf("real Docker inspect envelope = %#v %v", result, err)
	}
	if len(runner.calls) <= before {
		t.Fatal("inspect did not perform its read-only Docker checks")
	}
}

type realisticGatewayInspectRunner struct {
	*recordingRunner
}

func (r realisticGatewayInspectRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	out, err := r.recordingRunner.Run(ctx, argv, stdin)
	if err != nil || len(argv) != 5 || argv[0] != "container" || argv[1] != "inspect" || argv[3] != `{{json .}}` {
		return out, err
	}
	var document map[string]any
	if err := json.Unmarshal(out, &document); err != nil {
		return out, err
	}
	document["Id"] = "sha256:fixture"
	document["Name"] = "/fixture"
	document["Created"] = "2026-09-18T00:00:00Z"
	document["Config"].(map[string]any)["Cmd"] = []any{"/bin/epusdt"}
	document["HostConfig"].(map[string]any)["PublishAllPorts"] = false
	document["NetworkSettings"].(map[string]any)["Ports"] = map[string]any{"8000/tcp": nil}
	return json.Marshal(document)
}

type gatewayInspectOverrideRunner struct {
	*recordingRunner
	output []byte
}

func (r gatewayInspectOverrideRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	out, err := r.recordingRunner.Run(ctx, argv, stdin)
	if err != nil || len(argv) != 5 || argv[0] != "container" || argv[1] != "inspect" || argv[3] != `{{json .}}` {
		return out, err
	}
	return r.output, nil
}

func TestGatewaySameTagReconcileIsNoOpAndRemovalPreservesDataAfterRoute(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := requestFor(state, revisionB())
	request.Target.PaymentGateways = []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}}
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	if err := os.WriteFile(filepath.Join(rt.dataPath(object.DataToken), "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionC(), request.Target.PaymentGateways)); err != nil {
		gotState, stateErr := rt.readState()
		gotInventory, inventoryErr := rt.readInventory()
		t.Fatalf("same tag: %v calls=%#v state=%#v/%v inventory=%#v/%v validation=%v", err, runner.calls, gotState, stateErr, gotInventory, inventoryErr, validateInventory(gotInventory))
	}
	if runner.mutations("pull", "run", "stop", "rm") != 0 {
		t.Fatalf("same-tag mutations = %#v", runner.calls)
	}
	state = mustState(t, rt)
	runner.calls = nil
	events := []string{}
	routeRemoveHook = func(token string) error { events = append(events, "route:"+token); return nil }
	runner.event = func(argv []string) {
		if len(argv) > 0 && argv[0] == "rm" {
			events = append(events, "container:"+argv[len(argv)-1])
		}
	}
	t.Cleanup(func() { routeRemoveHook = nil })
	removal := requestForGateway(state, revision(), nil)
	if _, err := rt.Reconcile(t.Context(), removal); err != nil {
		t.Fatalf("removal: %v calls=%#v", err, runner.calls)
	}
	if _, err := os.Stat(filepath.Join(rt.dataPath(object.DataToken), "sentinel")); err != nil {
		t.Fatalf("gateway data = %v", err)
	}
	if !sameStrings(events, []string{"route:" + paymentGatewayToken("primary"), "container:" + object.Name}) {
		t.Fatalf("gateway removal order = %#v calls=%#v", events, runner.calls)
	}
}

func requestForGateway(state State, revision string, gateways []hostcontract.PaymentGatewayTarget) hostprotocol.Request {
	request := requestFor(state, revision)
	request.Target.PaymentGateways = gateways
	return request
}

func countRunning(runner *recordingRunner) int {
	count := 0
	for _, running := range runner.running {
		if running {
			count++
		}
	}
	return count
}

func TestGatewayChangedImagePullsBeforeExclusiveUpgradeAndPublishesAfterReady(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	runner.calls = nil
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "new.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("upgrade: %v calls=%#v", err, runner.calls)
	}
	newObject := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	if newObject.Image != "gmwallet/epusdt:v2.0.1" || runner.mutations("pull") != 1 || runner.mutations("stop") != 1 || runner.mutations("run") != 1 || runner.mutations("rm") != 1 {
		t.Fatalf("upgrade inventory/calls=%#v object=%#v", runner.calls, newObject)
	}
	indices := map[string]int{}
	for i, call := range runner.calls {
		if len(call) == 0 {
			continue
		}
		switch call[0] {
		case "pull":
			indices["pull"] = i
		case "stop":
			indices["stop"] = i
		case "run":
			indices["run"] = i
		case "rm":
			indices["rm"] = i
		}
	}
	if !(indices["pull"] < indices["stop"] && indices["stop"] < indices["run"] && indices["run"] < indices["rm"]) {
		t.Fatalf("upgrade order=%#v calls=%#v old=%s", indices, runner.calls, old.Name)
	}
}

func TestGatewayReadinessFailureRestoresStoppedRollback(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	oldRoute := mustRouteArtifact(t, rt, routeName(old.AppToken))
	newName := objectName(state, "payment-gateway", old.AppToken, "live")
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "exec" && argv[2] == "wget" && argv[1] == newName {
			return errors.New("not ready")
		}
		return nil
	}
	runner.failAfter = false
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "new.example.test"}})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := rt.Reconcile(ctx, second); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("readiness failure = %v calls=%#v", err, runner.calls)
	}
	if got := mustRouteArtifact(t, rt, routeName(old.AppToken)); !bytes.Equal(got, oldRoute) {
		t.Fatalf("route was not restored: %q", got)
	}
	if runner.running[old.Name] != true {
		t.Fatalf("rollback containers running=%#v calls=%#v", runner.running, runner.calls)
	}
	if got := findPaymentGateway(mustInventory(t, rt), old.AppToken); got.Image != old.Image {
		t.Fatalf("inventory advanced after failed upgrade: %#v", got)
	}
}

func TestGatewayCreateRunResponseLossReplaysWithoutDuplicateWorker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{failAfter: true}
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "run" {
			return errors.New("response lost")
		}
		return nil
	}
	rt.runner = runner
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), request); err == nil {
		t.Fatal("response-loss create unexpectedly succeeded")
	}
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatalf("replay: %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("run") != 1 || len(mustInventory(t, rt).Objects) != 1 {
		t.Fatalf("replayed worker count=%d inventory=%#v calls=%#v", runner.mutations("run"), mustInventory(t, rt), runner.calls)
	}
}

func TestGatewayRemovalResponseLossReplaysAfterContainerWasRemoved(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "rm" {
			return errors.New("response lost")
		}
		return nil
	}
	removal := requestForGateway(state, revisionC(), nil)
	if _, err := rt.Reconcile(t.Context(), removal); err == nil {
		t.Fatal("response-loss removal unexpectedly succeeded")
	}
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(t.Context(), removal); err != nil {
		t.Fatalf("removal replay: %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("rm") != 1 || runner.running[object.Name] {
		t.Fatalf("removal replay duplicated/desynchronized worker: calls=%#v running=%#v", runner.calls, runner.running)
	}
}

func TestContainerTargetAcceptsDockerNameAndTargetLabelOutput(t *testing.T) {
	rt := testRuntime(t)
	rt.runner = stdoutRunner{output: []byte("gateway\ttarget\n")}
	got, err := rt.containerTarget(t.Context(), "gateway")
	if err != nil || got != "target" {
		t.Fatalf("container target = %q, %v", got, err)
	}
}

func TestGatewayHostnameChangeRewritesRouteWithoutRestartingWorker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "old.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	runner.calls = nil
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "new.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("hostname update: %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("pull", "run", "stop", "rm") != 0 {
		t.Fatalf("hostname update restarted worker: %#v", runner.calls)
	}
	route := string(mustRouteArtifact(t, rt, routeName(paymentGatewayToken("primary"))))
	if !strings.Contains(route, "new.example.test") || strings.Contains(route, "old.example.test") {
		t.Fatalf("hostname route = %s", route)
	}
	if got := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary")); got.Hostname != "new.example.test" {
		t.Fatalf("hostname inventory = %#v", got)
	}
}

func TestGatewayUpgradeRunFailureRestoresRunningOldWorker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "run" && strings.Contains(strings.Join(argv, "\x00"), "gmwallet/epusdt:v2.0.1") {
			return errors.New("candidate failed to start")
		}
		return nil
	}
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("run failure = %v calls=%#v", err, runner.calls)
	}
	if runner.running[old.Name] != true || runner.inspect[old.Name] == "" {
		t.Fatalf("old worker was not restored: running=%#v inspect=%#v calls=%#v", runner.running, runner.inspect, runner.calls)
	}
	rollback := objectName(state, "payment-gateway-rollback", old.AppToken, old.Revision)
	if runner.inspect[rollback] != "" || runner.running[rollback] {
		t.Fatalf("rollback worker remains: running=%#v inspect=%#v", runner.running, runner.inspect)
	}
	if got := findPaymentGateway(mustInventory(t, rt), old.AppToken); got.Image != old.Image || !rt.routeMatches(mustInventory(t, rt), got) {
		t.Fatalf("failed upgrade advanced state: %#v", got)
	}
}

func TestGatewayCreatePullFailureLeavesRuntimeStateUntouched(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "pull" {
			return errors.New("pull failed")
		}
		return nil
	}
	rt.runner = runner
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	object := paymentGatewayObject(state, request.Target.PaymentGateways[0], request.TargetRevision)
	if _, err := rt.Reconcile(t.Context(), request); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("pull failure = %v", err)
	}
	if _, err := os.Stat(rt.dataPath(object.DataToken)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pull failure created data root: %v", err)
	}
	if _, err := rt.readInventory(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pull failure inventory = %v", err)
	}
	if runner.mutations("run", "stop", "rm") != 0 {
		t.Fatalf("pull failure mutated containers: %#v", runner.calls)
	}
}

func TestGatewayRetirementRemovesRouteAndWorkerButPreservesDataAndMetadata(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	sentinel := filepath.Join(rt.dataPath(object.DataToken), "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	trace := []string{}
	routeRemoveHook = func(token string) error {
		trace = append(trace, "route:"+token)
		return nil
	}
	runner.event = func(argv []string) {
		if len(argv) > 0 && argv[0] == "rm" {
			trace = append(trace, "rm:"+argv[len(argv)-1])
		}
	}
	t.Cleanup(func() { routeRemoveHook, runner.event = nil, nil })
	key, approval := retireKey(state), retireApproval(retireKey(state), state)
	if _, err := rt.Retire(t.Context(), retireRequest(key, approval)); err != nil {
		t.Fatalf("retire: %v calls=%#v", err, runner.calls)
	}
	if !sameStrings(trace, []string{"route:" + object.AppToken, "rm:" + object.Name}) {
		t.Fatalf("retirement order = %#v", trace)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("retirement removed data: %v", err)
	}
	retained := findPaymentGateway(mustInventory(t, rt), object.AppToken)
	if retained.DataToken != object.DataToken || retained.PathToken != object.PathToken || retained.Hostname != object.Hostname {
		t.Fatalf("retirement inventory metadata = %#v", retained)
	}
	if _, err := rt.readArtifactBytes(routeName(object.AppToken)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement route = %v", err)
	}
}

func TestGatewaySameImageMissingManagedContainerRepairsWithoutPull(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	delete(runner.inspect, object.Name)
	delete(runner.targets, object.Name)
	runner.calls = nil
	request = requestForGateway(state, revisionC(), request.Target.PaymentGateways)
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatalf("missing worker repair = %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("pull") != 0 || runner.mutations("run") != 1 || runner.mutations("stop", "rm") != 0 {
		t.Fatalf("missing worker repair trace = %#v", runner.calls)
	}
}

func TestGatewaySameImageRepairFailsWithoutImplicitPullWhenLocalImageMissing(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	delete(runner.inspect, object.Name)
	delete(runner.targets, object.Name)
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "run" && argv[1] == "--pull" && argv[2] == "never" {
			return errors.New("local image missing")
		}
		return nil
	}
	runner.calls = nil
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{target})); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("missing local image repair = %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("pull") != 0 {
		t.Fatalf("same-image repair implicitly pulled image: %#v", runner.calls)
	}
}

func TestGatewayUpgradePullResponseLossLeavesOldWorkerRunningForSafeRetry(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	runner.calls = nil
	pulls := 0
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "pull" {
			pulls++
			return errors.New("pull response lost")
		}
		return nil
	}
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("pull response loss = %v", err)
	}
	if runner.running[old.Name] != true || runner.mutations("stop", "run", "rm") != 0 {
		t.Fatalf("pull response loss changed worker: running=%#v calls=%#v", runner.running, runner.calls)
	}
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("pull retry: %v calls=%#v", err, runner.calls)
	}
	if pulls != 1 || runner.mutations("run") != 1 || countRunning(runner) != 1 {
		t.Fatalf("pull retry duplicated or skipped upgrade: pulls=%d running=%#v calls=%#v", pulls, runner.running, runner.calls)
	}
}

func TestGatewayUpgradeStopResponseLossRetriesWithoutConcurrentWorkers(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "stop" {
			return errors.New("stop response lost")
		}
		return nil
	}
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	firstResult, firstErr := rt.Reconcile(t.Context(), second)
	if firstErr != nil || firstResult.Status != hostprotocol.ResultApplied || runner.mutations("run") != 2 || countRunning(runner) != 1 {
		t.Fatalf("stop response loss left unsafe state: result=%#v err=%v running=%#v calls=%#v", firstResult, firstErr, runner.running, runner.calls)
	}
	calls := len(runner.calls)
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("stop retry after %v: %v calls=%#v", firstErr, err, runner.calls)
	}
	if len(runner.calls) != calls || runner.mutations("run") != 2 || countRunning(runner) != 1 {
		t.Fatalf("stop retry did not resume upgrade: running=%#v calls=%#v", runner.running, runner.calls)
	}
}

func TestGatewayUpgradeRollbackRemovalFailureReplaysAfterCandidateWasPublished(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	rollback := objectName(state, "payment-gateway-rollback", old.AppToken, old.Revision)
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 1 && argv[0] == "rm" && argv[len(argv)-1] == rollback {
			return errors.New("rollback removal failed")
		}
		return nil
	}
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("rollback removal response loss: %v calls=%#v", err, runner.calls)
	}
	calls := len(runner.calls)
	runner.fail = nil
	if _, err := rt.Reconcile(t.Context(), second); err != nil || len(runner.calls) != calls {
		t.Fatalf("rollback removal retry: %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("run") != 2 || runner.running[rollback] {
		t.Fatalf("rollback removal replay = running=%#v calls=%#v", runner.running, runner.calls)
	}
}

func TestGatewayUpgradeRouteWriteFailureRestoresOldWorkerAndRoute(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "old.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	oldRoute := mustRouteArtifact(t, rt, routeName(old.AppToken))
	routeWriteHook = func() error { return errors.New("route response lost") }
	t.Cleanup(func() { routeWriteHook = nil })
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "new.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err == nil {
		t.Fatal("route write failure unexpectedly succeeded")
	}
	if runner.running[old.Name] != true || !bytes.Equal(mustRouteArtifact(t, rt, routeName(old.AppToken)), oldRoute) {
		t.Fatalf("route failure did not restore old state: running=%#v route=%q calls=%#v", runner.running, mustRouteArtifact(t, rt, routeName(old.AppToken)), runner.calls)
	}
}

func TestGatewayRemovalRouteResponseLossReplaysBeforeContainerRemoval(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	object := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	routeRemoveHook = func(string) error { return errors.New("route response lost") }
	t.Cleanup(func() { routeRemoveHook = nil })
	removal := requestForGateway(state, revisionC(), nil)
	if _, err := rt.Reconcile(t.Context(), removal); err == nil {
		t.Fatal("route removal loss unexpectedly succeeded")
	}
	if runner.mutations("rm") != 0 || runner.inspect[object.Name] == "" {
		t.Fatalf("route loss removed worker: %#v", runner.calls)
	}
	routeRemoveHook = nil
	if _, err := rt.Reconcile(t.Context(), removal); err != nil {
		t.Fatalf("route removal retry: %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("rm") != 1 {
		t.Fatalf("route removal retry did not remove worker: %#v", runner.calls)
	}
}

func TestGatewayCreateRouteFailureRemovesWorkerAndLeavesDataForRetry(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	routeWriteHook = func() error { return errors.New("route write failed") }
	t.Cleanup(func() { routeWriteHook = nil })
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})
	object := paymentGatewayObject(state, target, revisionB())
	if _, err := rt.Reconcile(t.Context(), request); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("create route failure = %v calls=%#v", err, runner.calls)
	}
	if runner.inspect[object.Name] != "" || runner.running[object.Name] {
		t.Fatalf("candidate worker was not removed: inspect=%#v running=%#v calls=%#v", runner.inspect, runner.running, runner.calls)
	}
	if _, err := rt.readArtifactBytes(routeName(object.AppToken)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate route remains: %v", err)
	}
	if _, err := os.Stat(rt.dataPath(object.DataToken)); err != nil {
		t.Fatalf("data root was not preserved: %v", err)
	}
	if _, err := rt.readInventory(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed create advanced inventory: %v", err)
	}
}

func TestGatewaySameImageHostnameRouteFailureBeforeEffectiveWriteRestoresOldRoute(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	oldTarget := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "old.example.test"}
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{oldTarget})); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	oldRoute := mustRouteArtifact(t, rt, routeName(old.AppToken))
	routeWriteHook = func() error {
		if err := os.Remove(rt.artifactPath(routeName(old.AppToken))); err != nil {
			return err
		}
		return errors.New("route write failed before effective publication")
	}
	t.Cleanup(func() { routeWriteHook = nil })
	newTarget := oldTarget
	newTarget.Hostname = "new.example.test"
	runner.calls = nil
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{newTarget})); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("hostname route failure = %v calls=%#v", err, runner.calls)
	}
	if !bytes.Equal(mustRouteArtifact(t, rt, routeName(old.AppToken)), oldRoute) || runner.mutations("pull", "run", "stop", "rm") != 0 {
		t.Fatalf("old state was not retained: route=%q calls=%#v", mustRouteArtifact(t, rt, routeName(old.AppToken)), runner.calls)
	}
	if got := findPaymentGateway(mustInventory(t, rt), old.AppToken); got.Hostname != old.Hostname {
		t.Fatalf("inventory advanced after route failure: %#v", got)
	}
}

func TestGatewaySameImageHostnameRouteResponseLossAfterWriteRestoresOldRoute(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	oldTarget := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "old.example.test"}
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{oldTarget})); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	oldRoute := mustRouteArtifact(t, rt, routeName(old.AppToken))
	routeWriteHook = func() error { return errors.New("route response lost") }
	t.Cleanup(func() { routeWriteHook = nil })
	newTarget := oldTarget
	newTarget.Hostname = "new.example.test"
	runner.calls = nil
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{newTarget})); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("hostname route response loss = %v calls=%#v", err, runner.calls)
	}
	if !bytes.Equal(mustRouteArtifact(t, rt, routeName(old.AppToken)), oldRoute) || runner.mutations("pull", "run", "stop", "rm") != 0 {
		t.Fatalf("old state was not restored: route=%q calls=%#v", mustRouteArtifact(t, rt, routeName(old.AppToken)), runner.calls)
	}
}

func TestUnsortedGatewayRequestCompletesWithSortedObservationAndReplays(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	gateways := []hostcontract.PaymentGatewayTarget{
		{ID: "zeta", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "zeta.example.test"},
		{ID: "alpha", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "alpha.example.test"},
	}
	request := requestForGateway(state, revisionB(), gateways)
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatalf("unsorted gateway request = %v calls=%#v", err, runner.calls)
	}
	stored := mustState(t, rt)
	if len(stored.Observation.PaymentGateways) != 2 || stored.Observation.PaymentGateways[0].ID != "alpha" || stored.Observation.PaymentGateways[1].ID != "zeta" {
		t.Fatalf("gateway observation order = %#v", stored.Observation.PaymentGateways)
	}
	calls := len(runner.calls)
	if _, err := rt.Reconcile(t.Context(), request); err != nil || len(runner.calls) != calls {
		t.Fatalf("unsorted gateway replay = %v calls %d -> %d", err, calls, len(runner.calls))
	}
}

func TestGatewayOrphanContainerRequiresPendingAdoption(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	candidate := paymentGatewayObject(state, target, revisionB())
	runner.inspect = map[string]string{candidate.Name: ownershipLabelFor(state.Resource, state.Ownership, candidate.Role, candidate.AppToken, candidate.Active)}
	runner.targets = map[string]string{candidate.Name: targetLabelFor(candidate)}
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})
	if _, err := rt.Reconcile(t.Context(), request); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("orphan gateway = %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("pull", "run", "stop", "rm") != 0 {
		t.Fatalf("orphan gateway was adopted: %#v", runner.calls)
	}
}

func TestGatewayInventoryRejectsActiveAndRetainedMetadataDuplicate(t *testing.T) {
	rt, state := initialized(t)
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	active := paymentGatewayObject(state, target, revision())
	metadata := gatewayMetadata(active)
	value, err := json.Marshal(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: revision(), Objects: []managedObject{active, metadata}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.writeArtifact(artifactInventory, value, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.readInventory(); err == nil {
		t.Fatal("active gateway and retained metadata both accepted")
	}
}

func TestPendingGatewayRejectsForeignRouteInsteadOfAdoptingIt(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "old.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	key := requestKey(requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "new.example.test"}}))
	pending := state
	pending.Journal = &Journal{Key: key, Status: journalPending}
	if err := rt.writeState(pending); err != nil {
		t.Fatal(err)
	}
	foreign := old
	foreign.Hostname = "foreign.example.test"
	foreignRoute, err := routeBytesFor(mustInventory(t, rt), foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.writeArtifact(routeName(old.AppToken), foreignRoute, 0600); err != nil {
		t.Fatal(err)
	}
	request := hostprotocol.Request{Action: hostcontract.ActionReconcile, Resource: state.Resource, TargetRevision: key.TargetRevision, PriorAppliedRevision: state.AppliedRevision, Target: &hostcontract.Target{ReleaseArtifact: "release", PaymentGateways: []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "new.example.test"}}}, Secrets: &hostcontract.Secrets{}}
	if _, err := rt.Reconcile(t.Context(), request); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("foreign pending route = %v calls=%#v", err, runner.calls)
	}
}

func TestGatewayUpgradeStopsUnexpectedRunningRollbackBeforeCandidate(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	rollback := objectName(state, "payment-gateway-rollback", old.AppToken, old.Revision)
	delete(runner.inspect, old.Name)
	delete(runner.targets, old.Name)
	delete(runner.running, old.Name)
	runner.inspect[rollback] = ownershipLabelFor(state.Resource, state.Ownership, old.Role, old.AppToken, old.Active)
	runner.targets[rollback] = targetLabelFor(old)
	runner.running[rollback] = true
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("running rollback upgrade: %v calls=%#v", err, runner.calls)
	}
	if countRunning(runner) != 1 || runner.running[rollback] {
		t.Fatalf("concurrent rollback worker: running=%#v calls=%#v", runner.running, runner.calls)
	}
}

func TestGatewayRemovalThenReaddReusesPersistentDataIdentity(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	target := hostcontract.PaymentGatewayTarget{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	if err := os.WriteFile(filepath.Join(rt.dataPath(old.DataToken), "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	removal := requestForGateway(state, revisionC(), nil)
	if _, err := rt.Reconcile(t.Context(), removal); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	runner.calls = nil
	if _, err := rt.Reconcile(t.Context(), requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{target})); err != nil {
		t.Fatalf("re-add: %v calls=%#v", err, runner.calls)
	}
	got := findPaymentGateway(mustInventory(t, rt), old.AppToken)
	if got.DataToken != old.DataToken || got.PathToken != old.PathToken {
		t.Fatalf("re-add changed data identity: old=%#v got=%#v", old, got)
	}
	if _, err := os.Stat(filepath.Join(rt.dataPath(got.DataToken), "sentinel")); err != nil {
		t.Fatalf("re-add lost data: %v", err)
	}
}

func TestGatewayUpgradeRollbackRenameResponseLossRestoresOldWorker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	rollback := objectName(state, "payment-gateway-rollback", old.AppToken, old.Revision)
	routeWriteHook = func() error { return errors.New("route response lost") }
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) == 3 && argv[0] == "rename" {
			return errors.New("rename response lost")
		}
		return nil
	}
	t.Cleanup(func() { routeWriteHook = nil })
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "new.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err == nil {
		t.Fatal("rename response loss unexpectedly succeeded")
	}
	if runner.running[old.Name] != true || runner.inspect[rollback] != "" || countRunning(runner) != 1 {
		t.Fatalf("rename response loss state: running=%#v inspect=%#v calls=%#v", runner.running, runner.inspect, runner.calls)
	}
	routeWriteHook = nil
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("rename response-loss retry: %v calls=%#v", err, runner.calls)
	}
	if countRunning(runner) != 1 || runner.running[rollback] || runner.mutations("run") != 3 {
		t.Fatalf("rename response-loss retry state: running=%#v calls=%#v", runner.running, runner.calls)
	}
}

func TestGatewayUpgradeRollbackStartResponseLossRestoresOldWorker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	newGatewayReadinessContext = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, 5*time.Millisecond)
	}
	t.Cleanup(func() { newGatewayReadinessContext = context.WithTimeout })
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "exec" && argv[1] == old.Name {
			return errors.New("candidate not ready")
		}
		if len(argv) > 0 && argv[0] == "start" {
			return errors.New("start response lost")
		}
		return nil
	}
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err == nil {
		t.Fatal("start response loss unexpectedly succeeded")
	}
	if countRunning(runner) != 1 || runner.running[old.Name] != true {
		t.Fatalf("start response loss state: running=%#v calls=%#v", runner.running, runner.calls)
	}
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("start response-loss retry: %v calls=%#v", err, runner.calls)
	}
	if countRunning(runner) != 1 || runner.mutations("run") != 3 {
		t.Fatalf("start response-loss retry state: running=%#v calls=%#v", runner.running, runner.calls)
	}
}

func TestGatewayUpgradeCandidateRemovalResponseLossRestoresOldWorker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	old := findPaymentGateway(mustInventory(t, rt), paymentGatewayToken("primary"))
	newGatewayReadinessContext = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, 5*time.Millisecond)
	}
	t.Cleanup(func() { newGatewayReadinessContext = context.WithTimeout })
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "exec" && argv[1] == old.Name {
			return errors.New("candidate not ready")
		}
		if len(argv) > 1 && argv[0] == "rm" {
			return errors.New("candidate removal response lost")
		}
		return nil
	}
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err == nil {
		t.Fatal("candidate removal response loss unexpectedly succeeded")
	}
	if countRunning(runner) != 1 || runner.running[old.Name] != true {
		t.Fatalf("candidate removal response loss state: running=%#v inspect=%#v calls=%#v", runner.running, runner.inspect, runner.calls)
	}
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("candidate removal response-loss retry: %v calls=%#v", err, runner.calls)
	}
	if countRunning(runner) != 1 || runner.mutations("run") != 3 {
		t.Fatalf("candidate removal response-loss retry state: running=%#v calls=%#v", runner.running, runner.calls)
	}
}

func TestGatewayUpgradeCandidateRunResponseLossContinuesWithoutDuplicateWorker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "run" && strings.Contains(strings.Join(argv, "\x00"), "gmwallet/epusdt:v2.0.1") {
			return errors.New("candidate run response lost")
		}
		return nil
	}
	second := requestForGateway(state, revisionC(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.1", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("candidate run response loss: %v calls=%#v", err, runner.calls)
	}
	if runner.mutations("run") != 2 || countRunning(runner) != 1 {
		t.Fatalf("candidate run response loss duplicated worker: running=%#v calls=%#v", runner.running, runner.calls)
	}
}

func TestInspectReportsExactGatewayObservationAndLegacyNoGatewayObservation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	noGateway := hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource}
	if result, err := rt.Handle(t.Context(), noGateway); err != nil || result.Observation == nil || len(result.Observation.PaymentGateways) != 0 {
		t.Fatalf("legacy inspect = %#v %v", result, err)
	}
	request := requestForGateway(state, revisionB(), []hostcontract.PaymentGatewayTarget{{ID: "primary", Type: "gmpay", Image: "gmwallet/epusdt:v2.0.0", Hostname: "pay.example.test"}})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	result, err := rt.Handle(t.Context(), hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource})
	if err != nil || result.Observation == nil || len(result.Observation.PaymentGateways) != 1 || result.Observation.PaymentGateways[0].ActiveImage != "gmwallet/epusdt:v2.0.0" || !result.Observation.PaymentGateways[0].Ready || !result.Observation.Ready {
		t.Fatalf("gateway inspect = %#v %v", result, err)
	}
}
