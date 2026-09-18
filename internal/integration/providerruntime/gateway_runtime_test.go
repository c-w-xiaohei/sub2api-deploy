//go:build linux

package providerruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
)

const gatewayReadinessTimeout = 65 * time.Second

func TestProviderLifecycleWithPaymentGatewayFixture(t *testing.T) {
	provider := buildProviderForPrerequisite(t)
	h := startProviderWithApproval(t, provider, approvalExact)

	inputs := gatewayFixtureInputs("sub2api-gmpay-fixture:v2.0.0")
	created := createProviderResource(t, h, inputs)
	writeGatewaySentinel(t, h)
	createdEffects := dockerEffects(t, h)

	// This fixture models one selected Host. Multi-Host placement remains the
	// Task 3 Program proof; this test is intentionally limited to one Host.
	noOp, err := updateProviderResource(t, h, updateRequest(t, created.Id, created.Properties, inputs, inputs), inputs, hostcontract.ActionInspect)
	if err != nil || noOp == nil || !reflect.DeepEqual(createdEffects, dockerEffects(t, h)) {
		t.Fatalf("same-tag no-op changed the Host runtime: response=%#v err=%v effects=%#v", noOp, err, dockerEffects(t, h))
	}

	next := gatewayFixtureInputs("sub2api-gmpay-fixture:v2.0.1")
	failureMarker := filepath.Join(h.trace, "gateway-readiness-failure")
	if err := os.WriteFile(failureMarker, []byte("forced\n"), 0600); err != nil {
		t.Fatal(err)
	}
	failed, err := updateProviderResourceWithTimeout(t, h, updateRequest(t, created.Id, created.Properties, inputs, next), next, matrixCreateUpdateTimeout+gatewayReadinessTimeout, hostcontract.ActionInspect, hostcontract.ActionReconcile)
	if err == nil || failed != nil {
		t.Fatalf("forced readiness failure response=%#v err=%v", failed, err)
	}
	if err := os.Remove(failureMarker); err != nil {
		t.Fatal(err)
	}
	assertGatewayVersionAndData(t, h, "sub2api-gmpay-fixture:v2.0.0", "keep-gateway-data\n")
	failureEffects := dockerEffects(t, h)

	h.dropHostResponse(t, hostcontract.ActionReconcile)
	_, err = updateProviderResourceWithTimeout(t, h, updateRequest(t, created.Id, created.Properties, inputs, next), next, matrixCreateUpdateTimeout+gatewayReadinessTimeout, hostcontract.ActionInspect, hostcontract.ActionReconcile)
	if err == nil {
		t.Fatal("successful gateway update with lost Provider response unexpectedly returned success")
	}
	assertDroppedHostResponse(t, h, hostcontract.ActionReconcile)
	updatedEffects := dockerEffects(t, h)
	updated, err := updateProviderResource(t, h, updateRequest(t, created.Id, created.Properties, inputs, next), next, hostcontract.ActionInspect)
	if err != nil || updated == nil {
		t.Fatalf("same revision Provider retry: %v", err)
	}
	if !reflect.DeepEqual(updatedEffects, dockerEffects(t, h)) {
		t.Fatalf("same revision retry repeated gateway Docker mutations: before=%#v after=%#v", updatedEffects, dockerEffects(t, h))
	}
	assertGatewayVersionAndData(t, h, "sub2api-gmpay-fixture:v2.0.1", "keep-gateway-data\n")
	assertGatewayMutationTrace(t, h, createdEffects, failureEffects, updatedEffects)
	assertGatewayRunPullPolicy(t, h)

	drained := createInputsWithTarget(hostcontract.Target{ReleaseArtifact: ciRelease})
	drainedResponse, err := updateProviderResource(t, h, updateRequest(t, created.Id, updated.Properties, next, drained), drained, hostcontract.ActionInspect, hostcontract.ActionReconcile, hostcontract.ActionInspect)
	if err != nil || drainedResponse == nil {
		t.Fatalf("gateway drain: %v", err)
	}
	checkpoint := unmarshalProperties(t, drainedResponse.Properties)
	h.approvals.Expect(retireApprovalSubject(t, checkpoint, drained))
	if _, err := deleteProviderResource(t, h, deleteRequest(t, created.Id, drainedResponse.Properties, drained), drained, hostcontract.ActionInspect, hostcontract.ActionRetirePreserveData); err != nil {
		t.Fatalf("gateway removal: %v", err)
	}
	h.approvals.AssertExpectedConsumed(t)
	assertGatewayRemovedPreservingData(t, h)
	assertFixedGatewaySSHRecords(t, h.trace)

	// Keep the response-loss and rollback traces inspectable without retaining
	// any provider secret or external image/network state.
	if strings.Contains(string(mustRead(t, filepath.Join(h.trace, "docker.args"))), ciSecret) {
		t.Fatal("gateway Docker trace leaked the unrelated secret canary")
	}
	checkpoint = unmarshalProperties(t, updated.Properties)
	_, _, _, observation, err := checkpointValues(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.PaymentGateways) != 1 || observation.PaymentGateways[0].ID != "primary" || !observation.PaymentGateways[0].Ready || observation.PaymentGateways[0].ActiveImage != "sub2api-gmpay-fixture:v2.0.1" {
		t.Fatalf("gateway observation = %#v", observation.PaymentGateways)
	}
}

func TestGatewayFixtureRejectsForeignDestructiveIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "runtime", "managed"), 0700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"resource":  map[string]string{"environment": "test", "serverKey": "edge"},
		"ownership": map[string]string{"value": "oid1:" + strings.Repeat("a", 64)},
	}
	writeFixtureJSON(t, filepath.Join(root, "state.json"), state)
	token := fixtureToken("payment-gateway", "primary")
	oldRevision := "tr1:bd0687409e690a17:" + strings.Repeat("1", 64)
	writeFixtureJSON(t, filepath.Join(root, "runtime", "managed", "inventory.json"), map[string]any{
		"objects": []map[string]any{{
			"role": "payment-gateway", "appToken": token, "name": "", "image": "sub2api-gmpay-fixture:v2.0.0", "revision": oldRevision,
			"type": "gmpay", "hostname": "pay.example", "dataToken": fixtureToken("payment-gateway-data", token), "pathToken": fixtureToken("payment-gateway-path", token),
		}},
	})
	trace := filepath.Join(root, "trace")
	if err := os.MkdirAll(trace, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixtureJSON(t, filepath.Join(trace, "target.expectation.json"), targetExpectation{
		Revision:        oldRevision,
		PaymentGateways: []expectedTargetGateway{{ID: "primary", Type: "gmpay", Image: "sub2api-gmpay-fixture:v2.0.0", Hostname: "pay.example"}},
	})
	t.Setenv("PROVIDER_RUNTIME_TRACE", trace)
	owner := "s2h1:" + fixtureToken("test", "edge", "oid1:"+strings.Repeat("a", 64), "payment-gateway", token, "live")
	target := "s2ht1:" + fixtureToken("payment-gateway", token, "live", oldRevision, "sub2api-gmpay-fixture:v2.0.0", "gmpay", "0", "false")
	name := "s2h-" + fixtureToken("test", "edge", "oid1:"+strings.Repeat("a", 64), "payment-gateway", token, "live")
	valid := dockerContainer{Owner: owner, Target: target, Image: "sub2api-gmpay-fixture:v2.0.0", Slot: "live", AppToken: token, Role: "payment-gateway"}
	if !validDestructiveContainer(root, name, valid) {
		t.Fatal("exact persisted gateway identity was rejected")
	}
	for _, foreign := range []dockerContainer{
		{Owner: "s2h1:foreign", Target: target, Image: valid.Image, Slot: "live", AppToken: token, Role: "payment-gateway"},
		{Owner: owner, Target: "s2ht1:foreign", Image: valid.Image, Slot: "live", AppToken: token, Role: "payment-gateway"},
	} {
		if validDestructiveContainer(root, name, foreign) {
			t.Fatalf("foreign gateway identity was accepted: %#v", foreign)
		}
	}
	if validDestructiveContainer(root, "s2h-foreign", valid) {
		t.Fatal("foreign gateway name was accepted")
	}
}

func updateProviderResourceWithTimeout(t *testing.T, h *providerProcess, request *pulumirpc.UpdateRequest, inputs property.Map, timeout time.Duration, actions ...hostcontract.Action) (*pulumirpc.UpdateResponse, error) {
	t.Helper()
	writeTargetExpectation(t, h, inputs, request.OldInputs)
	writeHostActionQueue(t, h, actions...)
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	response, err := h.client.Update(ctx, request)
	assertHostActionQueueEmpty(t, h)
	if err == nil && response != nil {
		assertExactUpdateCheckpoint(t, unmarshalProperties(t, response.Properties), inputs, frozenRevisionForInputs(t, inputs))
	}
	return response, err
}

func writeFixtureJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertGatewayMutationTrace(t *testing.T, h *providerProcess, initial, afterFailure, afterSuccess []dockerEffect) {
	t.Helper()
	if len(initial) != 3 || initial[0].Action != "network-create" || initial[1].Action != "image-pull" || initial[2].Action != "container-run" {
		t.Fatalf("gateway create trace = %#v", initial)
	}
	if len(afterFailure) <= len(initial) || !reflect.DeepEqual(afterFailure[:len(initial)], initial) {
		t.Fatalf("gateway failure trace prefix changed: before=%#v after=%#v", initial, afterFailure)
	}
	failureDelta := afterFailure[len(initial):]
	wantFailure := []string{"image-pull", "container-stop", "container-rename", "container-run", "container-rm", "container-rename", "container-start"}
	if !sameEffectActions(failureDelta, wantFailure) {
		t.Fatalf("gateway rollback trace = %#v, want %v", failureDelta, wantFailure)
	}
	if len(afterSuccess) <= len(afterFailure) || !reflect.DeepEqual(afterSuccess[:len(afterFailure)], afterFailure) {
		t.Fatalf("gateway success trace prefix changed: before=%#v after=%#v", afterFailure, afterSuccess)
	}
	successDelta := afterSuccess[len(afterFailure):]
	wantSuccess := []string{"image-pull", "container-stop", "container-rename", "container-run", "container-rm"}
	if !sameEffectActions(successDelta, wantSuccess) {
		t.Fatalf("gateway success trace = %#v, want %v", successDelta, wantSuccess)
	}
}

func sameEffectActions(effects []dockerEffect, actions []string) bool {
	if len(effects) != len(actions) {
		return false
	}
	for index, action := range actions {
		if effects[index].Action != action {
			return false
		}
	}
	return true
}

func assertGatewayRunPullPolicy(t *testing.T, h *providerProcess) {
	t.Helper()
	var trace dockerTrace
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.trace, "docker-state", "state.json")), &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.GatewayRunPullPolicies) == 0 {
		t.Fatal("gateway run pull policy trace is empty")
	}
	for _, policy := range trace.GatewayRunPullPolicies {
		if policy != "--pull never" {
			t.Fatalf("gateway run pull policy = %q, want --pull never", policy)
		}
	}
}

func gatewayFixtureInputs(image string) property.Map {
	target := hostcontract.Target{
		ReleaseArtifact: ciRelease,
		PaymentGateways: []hostcontract.PaymentGatewayTarget{{
			ID: "primary", Type: "gmpay", Image: image, Hostname: "pay.example",
		}},
	}
	return createInputsWithTarget(target)
}

func writeGatewaySentinel(t *testing.T, h *providerProcess) {
	t.Helper()
	path := filepath.Join(h.root, "runtime", "data", fixtureToken("payment-gateway-data", fixtureToken("payment-gateway", "primary")))
	if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("keep-gateway-data\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertGatewayVersionAndData(t *testing.T, h *providerProcess, image, sentinel string) {
	t.Helper()
	var inventory struct {
		Objects []struct {
			Role, AppToken, Image, Hostname string
		} `json:"objects"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "runtime", "managed", "inventory.json")), &inventory); err != nil {
		t.Fatal(err)
	}
	gateways := 0
	for _, object := range inventory.Objects {
		if object.Role == "payment-gateway" {
			gateways++
			if object.AppToken != fixtureToken("payment-gateway", "primary") || object.Image != image || object.Hostname != "pay.example" {
				t.Fatalf("gateway placement/runtime inventory = %#v", object)
			}
		}
	}
	if gateways != 1 {
		t.Fatalf("gateway inventory count = %d, want one", gateways)
	}
	dataPath := filepath.Join(h.root, "runtime", "data", fixtureToken("payment-gateway-data", fixtureToken("payment-gateway", "primary")), "sentinel")
	if got := string(mustRead(t, dataPath)); got != sentinel {
		t.Fatalf("gateway persistent sentinel = %q, want %q", got, sentinel)
	}
	var docker dockerTrace
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.trace, "docker-state", "state.json")), &docker); err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, container := range docker.Containers {
		if container.Role == "payment-gateway" && container.Running {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running gateway count = %d, want one; containers=%#v", running, docker.Containers)
	}
}

func assertGatewayRemovedPreservingData(t *testing.T, h *providerProcess) {
	t.Helper()
	var inventory struct {
		Objects []json.RawMessage `json:"objects"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.root, "runtime", "managed", "inventory.json")), &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Objects) != 1 {
		t.Fatalf("gateway removal inventory = %#v", inventory.Objects)
	}
	var metadata struct {
		Role, AppToken, Type, DataToken, PathToken, Hostname string
	}
	if err := json.Unmarshal(inventory.Objects[0], &metadata); err != nil || metadata.Role != "payment-gateway-meta" || metadata.AppToken != fixtureToken("payment-gateway", "primary") || metadata.Type != "gmpay" || metadata.Hostname != "pay.example" || metadata.DataToken != fixtureToken("payment-gateway-data", metadata.AppToken) || metadata.PathToken != fixtureToken("payment-gateway-path", metadata.AppToken) {
		t.Fatalf("gateway removal metadata = %#v", metadata)
	}
	var docker dockerTrace
	if err := json.Unmarshal(mustRead(t, filepath.Join(h.trace, "docker-state", "state.json")), &docker); err != nil {
		t.Fatal(err)
	}
	if len(docker.Containers) != 0 || len(docker.Networks) != 0 {
		t.Fatalf("gateway removal left Docker state: %#v", docker)
	}
	dataPath := filepath.Join(h.root, "runtime", "data", fixtureToken("payment-gateway-data", fixtureToken("payment-gateway", "primary")), "sentinel")
	if got := string(mustRead(t, dataPath)); got != "keep-gateway-data\n" {
		t.Fatalf("gateway data after removal = %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.root, "runtime", "dynamic", "route-"+fixtureToken("payment-gateway", "primary")+".json")); !os.IsNotExist(err) {
		t.Fatalf("gateway route after removal = %v", err)
	}
}

func assertFixedGatewaySSHRecords(t *testing.T, trace string) {
	t.Helper()
	for _, name := range []string{"probe.command", "host.command"} {
		if got := string(mustRead(t, filepath.Join(trace, name))); got != map[string]string{"probe.command": goldenProbeCommand, "host.command": goldenHostCommand}[name] {
			t.Fatalf("golden SSH command %s changed: %q", name, got)
		}
	}
	for _, prefix := range []string{"ssh.probe.", "ssh.host."} {
		entries, err := os.ReadDir(trace)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".args") {
				count++
				lines := strings.Split(strings.TrimSuffix(string(mustRead(t, filepath.Join(trace, entry.Name()))), "\n"), "\n")
				if len(lines) != 48 {
					t.Fatalf("fixed SSH record %s has %d arguments", entry.Name(), len(lines))
				}
				for index, line := range lines {
					parts := strings.Split(line, " ")
					if len(parts) != 2 || parts[0] != strconv.Itoa(index+1) || index != 44 && parts[1] != sshArgumentDigest(t, index, entry.Name(), trace) {
						t.Fatalf("fixed SSH record %s is not the approved transport shape", entry.Name())
					}
				}
			}
		}
		if count == 0 {
			t.Fatalf("no fixed SSH %s records", prefix)
		}
	}
}
