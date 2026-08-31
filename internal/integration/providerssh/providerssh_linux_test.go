//go:build linux

package providerssh

import (
	"bufio"
	"bytes"
	"context"
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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	providerRevisionKey = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="
	providerRelease     = "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	providerAlias       = "edge"
	providerEnvironment = "prod"
	providerServerKey   = "edge"
	providerSecret      = "PROVIDER_SSH_SECRET_CANARY"
)

func TestProviderProcessUsesScriptedSSHTransport(t *testing.T) {
	oldInputs, oldTarget, secrets := providerInputs(t, "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	nextInputs, nextTarget, _ := providerInputs(t, "api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	key, err := base64.StdEncoding.DecodeString(providerRevisionKey)
	if err != nil {
		t.Fatal(err)
	}
	identity := hostcontract.ResourceIdentity{Environment: providerEnvironment, ServerKey: providerServerKey}
	oldRevision := providerRevision(t, key, identity, oldTarget, secrets)
	desiredRevision := providerRevision(t, key, identity, nextTarget, secrets)
	oldObservation := providerObservation(oldTarget, oldRevision)
	nextObservation := providerObservation(nextTarget, desiredRevision)
	state := providerCheckpoint(t, oldInputs, oldObservation, oldRevision)
	operationEvidence := hostprotocol.OperationEvidence{Key: hostcontract.OperationKey{Resource: identity, Action: hostcontract.ActionReconcile, TargetRevision: desiredRevision, PriorAppliedRevision: oldRevision}, Status: hostprotocol.OperationPending}
	operationEvidenceJSON, err := json.Marshal(operationEvidence)
	if err != nil {
		t.Fatal(err)
	}
	operationKeyJSON, err := json.Marshal(operationEvidence.Key)
	if err != nil {
		t.Fatal(err)
	}
	completeEvidence := operationEvidence
	completeEvidence.Status = hostprotocol.OperationComplete
	completeEvidenceJSON, err := json.Marshal(completeEvidence)
	if err != nil {
		t.Fatal(err)
	}

	provider, traceDir, responseDir := startProvider(t, "normal")
	inspectFrame, err := hostprotocol.EncodeRequest(hostprotocol.Request{Action: hostcontract.ActionInspect, Server: hostcontract.ServerTarget{SSHAlias: providerAlias}, Resource: identity, TargetRevision: desiredRevision})
	if err != nil {
		t.Fatal(err)
	}
	reconcileFrame, err := hostprotocol.EncodeRequest(hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: providerAlias}, Resource: identity, TargetRevision: desiredRevision, PriorAppliedRevision: oldRevision, Target: &nextTarget, Secrets: &secrets})
	if err != nil {
		t.Fatal(err)
	}
	writeProviderFixture(t, traceDir, "expected-inspect-frame", inspectFrame)
	writeProviderFixture(t, traceDir, "expected-reconcile-frame", reconcileFrame)
	writeProviderFixture(t, traceDir, "expected-operation-key", operationKeyJSON)
	writeProviderFixture(t, traceDir, "expected-pending-evidence", operationEvidenceJSON)
	writeProviderFixture(t, traceDir, "expected-complete-evidence", completeEvidenceJSON)
	writeResponse(t, responseDir, "response-1", inspectedResponse(oldObservation))
	writeResponse(t, responseDir, "response-pending", inspectedEvidenceResponse(oldObservation, &operationEvidence))
	writeResponse(t, responseDir, "response-applied", appliedResponse(desiredRevision))
	writeResponse(t, responseDir, "response-complete", inspectedEvidenceResponse(nextObservation, &completeEvidence))
	writeResponse(t, responseDir, "response-conflict", conflictResponse())
	configureProviderProcess(t, provider.client)

	request := pulumirpc.UpdateRequest{
		Id:         providerStableID(identity),
		Urn:        "urn:pulumi:prod::integration::sub2api-host:index:Host::edge",
		Type:       "sub2api-host:index:Host",
		Name:       providerServerKey,
		Olds:       providerRPCProperties(t, state),
		OldInputs:  providerRPCProperties(t, oldInputs),
		News:       providerRPCProperties(t, nextInputs),
	}
	first, err := provider.client.Update(t.Context(), &request)
	if err == nil || first != nil {
		t.Fatal("response-loss update returned a response; expected the lost SSH response to fail the RPC")
	}
	if err := waitForProviderFile(filepath.Join(traceDir, "call-2.stdin")); err != nil {
		t.Fatal(err)
	}
	assertProviderStateDirectorySafe(t, filepath.Join(traceDir, "state.pending"))
	second, err := provider.client.Update(t.Context(), &request)
	if err != nil || second == nil {
		t.Fatal("same-key response-loss resume did not succeed")
	}
	if err := waitForProviderFile(filepath.Join(traceDir, "call-5.stdin")); err != nil {
		t.Fatal(err)
	}

	assertProviderSSHInvocation(t, traceDir, 1, providerAlias, providerSecret)
	assertProviderSSHInvocation(t, traceDir, 2, providerAlias, providerSecret)
	assertProviderSSHInvocation(t, traceDir, 3, providerAlias, providerSecret)
	assertProviderSSHInvocation(t, traceDir, 4, providerAlias, providerSecret)
	assertProviderSSHInvocation(t, traceDir, 5, providerAlias, providerSecret)
	assertProviderFile(t, filepath.Join(traceDir, "call-1.stdin"), inspectFrame)
	assertProviderFile(t, filepath.Join(traceDir, "call-2.stdin"), reconcileFrame)
	assertProviderFile(t, filepath.Join(traceDir, "call-3.stdin"), inspectFrame)
	assertProviderFile(t, filepath.Join(traceDir, "call-4.stdin"), reconcileFrame)
	assertProviderFile(t, filepath.Join(traceDir, "call-5.stdin"), inspectFrame)
	if got := providerSSHCallCount(t, traceDir); got != 5 {
		t.Fatalf("SSH calls after response-loss resume = %d, want 5 including final inspect", got)
	}
	if _, err := os.Stat(filepath.Join(traceDir, "call-6.stdin")); !os.IsNotExist(err) {
		t.Fatalf("response-loss resume issued an unexpected sixth SSH call")
	}
	assertProviderFile(t, filepath.Join(traceDir, "effect-marker"), []byte("reconcile-effect\n"))
	effectLog := mustReadProviderFile(t, filepath.Join(traceDir, "effect.log"))
	if lines := bytes.Count(effectLog, []byte{'\n'}); lines != 1 {
		t.Fatalf("effect log line count = %d, want 1", lines)
	}
	assertProviderFile(t, filepath.Join(traceDir, "effect.log"), append(operationKeyJSON, '\n'))
	assertProviderFile(t, filepath.Join(traceDir, "operation-key-evidence"), operationKeyJSON)
	assertProviderStateDirectorySafe(t, filepath.Join(traceDir, "state.complete"))
	assertProviderFile(t, filepath.Join(traceDir, "state.complete", "key"), operationKeyJSON)
	assertProviderFile(t, filepath.Join(traceDir, "state.complete", "evidence"), completeEvidenceJSON)
	if strings.Contains(string(mustReadProviderFile(t, filepath.Join(traceDir, "call-2.args"))), providerSecret) {
		t.Fatal("secret reached SSH argv")
	}
}

func TestProviderProcessCancellationReclaimsScriptedSSH(t *testing.T) {
	inputs, target, secrets := providerInputs(t, "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	key, err := base64.StdEncoding.DecodeString(providerRevisionKey)
	if err != nil {
		t.Fatal(err)
	}
	identity := hostcontract.ResourceIdentity{Environment: providerEnvironment, ServerKey: providerServerKey}
	revision := providerRevision(t, key, identity, target, secrets)
	observation := providerObservation(target, revision)
	state := providerCheckpoint(t, inputs, observation, revision)
	provider, traceDir, _ := startProvider(t, "cancel")
	configureProviderProcess(t, provider.client)
	request := &pulumirpc.ReadRequest{
		Id:         providerStableID(identity),
		Urn:        "urn:pulumi:prod::integration::sub2api-host:index:Host::edge",
		Type:       "sub2api-host:index:Host",
		Name:       providerServerKey,
		Inputs:     providerRPCProperties(t, inputs),
		Properties: providerRPCProperties(t, state),
	}
	result := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		_, err := provider.client.Read(ctx, request)
		result <- err
	}()
	if err := waitForProviderFile(filepath.Join(traceDir, "call-1.started")); err != nil {
		t.Fatal(err)
	}
	if err := waitForProviderFile(filepath.Join(traceDir, "call-1.ready")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if status.Code(err) != codes.Canceled && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled provider Read = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled provider Read did not return")
	}
	pid := strings.TrimSpace(string(mustReadProviderFile(t, filepath.Join(traceDir, "call-1.pid"))))
	start := strings.TrimSpace(string(mustReadProviderFile(t, filepath.Join(traceDir, "call-1.start"))))
	if err := waitForProcessExit(pid, start); err != nil {
		t.Fatal(err)
	}
	childPID := strings.TrimSpace(string(mustReadProviderFile(t, filepath.Join(traceDir, "call-1.child.pid"))))
	childStart := strings.TrimSpace(string(mustReadProviderFile(t, filepath.Join(traceDir, "call-1.child.start"))))
	if err := waitForProcessExit(childPID, childStart); err != nil {
		t.Fatal(err)
	}
}

func TestProviderProcessRejectsScriptedHostKeyFailure(t *testing.T) {
	inputs, target, secrets := providerInputs(t, "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	key, err := base64.StdEncoding.DecodeString(providerRevisionKey)
	if err != nil {
		t.Fatal(err)
	}
	identity := hostcontract.ResourceIdentity{Environment: providerEnvironment, ServerKey: providerServerKey}
	revision := providerRevision(t, key, identity, target, secrets)
	state := providerCheckpoint(t, inputs, providerObservation(target, revision), revision)
	provider, traceDir, _ := startProvider(t, "host-key")
	configureProviderProcess(t, provider.client)
	_, err = provider.client.Read(t.Context(), &pulumirpc.ReadRequest{
		Id:         providerStableID(identity),
		Urn:        "urn:pulumi:prod::integration::sub2api-host:index:Host::edge",
		Type:       "sub2api-host:index:Host",
		Name:       providerServerKey,
		Inputs:     providerRPCProperties(t, inputs),
		Properties: providerRPCProperties(t, state),
	})
	if err == nil || !strings.Contains(status.Convert(err).Message(), "transport failed") {
		t.Fatal("scripted host-key Read did not fail closed as a transport error")
	}
	if got := providerSSHCallCount(t, traceDir); got != 1 {
		t.Fatalf("host-key failure started %d SSH processes, want 1", got)
	}
}

type providerProcess struct {
	client pulumirpc.ResourceProviderClient
	conn   *grpc.ClientConn
}

func startProvider(t *testing.T, mode string) (*providerProcess, string, string) {
	t.Helper()
	root := repositoryRoot(t)
	providerPath := filepath.Join(t.TempDir(), "pulumi-resource-sub2api-host")
	buildContext, cancelBuild := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildContext, "go", "build", "-o", providerPath, "./cmd/pulumi-resource-sub2api-host")
	build.Dir = root
	if _, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build provider: %v", err)
	}
	traceDir, responseDir := filepath.Join(t.TempDir(), "trace"), filepath.Join(t.TempDir(), "responses")
	if err := os.MkdirAll(traceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(responseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	modeFile := filepath.Join(t.TempDir(), "mode")
	if err := os.WriteFile(modeFile, []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
	sshDir := t.TempDir()
	script, err := os.ReadFile(filepath.Join(root, "internal", "integration", "providerssh", "testdata", "ssh-recorder.sh"))
	if err != nil {
		t.Fatal(err)
	}
	sshPath := filepath.Join(sshDir, "ssh")
	if err := os.WriteFile(sshPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(providerPath)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"PATH="+sshDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SSH_TRACE_DIR="+traceDir,
		"SSH_RESPONSE_DIR="+responseDir,
		"SSH_MODE_FILE="+modeFile,
		"SSH_DROP_RESPONSE_CALL=2",
		"SSH_EXPECTED_INSPECT_FRAME="+filepath.Join(traceDir, "expected-inspect-frame"),
		"SSH_EXPECTED_RECONCILE_FRAME="+filepath.Join(traceDir, "expected-reconcile-frame"),
		"SSH_EXPECTED_OPERATION_KEY="+filepath.Join(traceDir, "expected-operation-key"),
		"SSH_EXPECTED_PENDING_EVIDENCE="+filepath.Join(traceDir, "expected-pending-evidence"),
		"SSH_EXPECTED_COMPLETE_EVIDENCE="+filepath.Join(traceDir, "expected-complete-evidence"),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	providerStart := processStartTime(cmd.Process.Pid)
	providerPGID := processGroupID(cmd.Process.Pid)
	t.Cleanup(func() {
		terminateProvider(t, cmd.Process.Pid, providerStart, providerPGID, providerPath, done)
		cleanupRecordedSSH(t, traceDir, sshPath)
		_ = stdout.Close()
	})
	line, err := readProviderPort(stdout, 5*time.Second)
	if err != nil {
		t.Fatal("provider port was not emitted before the startup deadline")
	}
	port, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal("provider emitted an invalid startup port")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal("provider RPC did not become reachable before the startup deadline")
	}
	process := &providerProcess{client: pulumirpc.NewResourceProviderClient(conn), conn: conn}
	t.Cleanup(func() { _ = process.conn.Close() })
	return process, traceDir, responseDir
}

func configureProviderProcess(t *testing.T, client pulumirpc.ResourceProviderClient) {
	t.Helper()
	key := property.New(providerRevisionKey).WithSecret(true)
	if _, err := client.Configure(t.Context(), &pulumirpc.ConfigureRequest{Args: providerRPCProperties(t, property.NewMap(map[string]property.Value{"revisionKey": key}))}); err != nil {
		t.Fatal(err)
	}
}

func providerInputs(t *testing.T, image string) (property.Map, hostcontract.Target, hostcontract.Secrets) {
	t.Helper()
	target := hostcontract.Target{ReleaseArtifact: providerRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: image, Hostname: "api.example", ReadinessPath: "/ready"}}}
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: providerSecret}}}
	inputs := property.NewMap(map[string]property.Value{"resource": providerJSONValue(t, hostcontract.ResourceIdentity{Environment: providerEnvironment, ServerKey: providerServerKey}), "server": providerJSONValue(t, hostcontract.ServerTarget{SSHAlias: providerAlias}), "target": providerJSONValue(t, target), "secrets": providerJSONValue(t, secrets).WithSecret(true)})
	return inputs, target, secrets
}

func providerObservation(target hostcontract.Target, revision string) hostcontract.StableObservation {
	return hostcontract.StableObservation{Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, HostRelease: target.ReleaseArtifact, AppliedRevision: revision, Ready: true, Apps: []hostcontract.AppObservation{{ID: target.Apps[0].ID, ActiveImage: target.Apps[0].Image, Ready: true}}}
}

func providerRevision(t *testing.T, key []byte, identity hostcontract.ResourceIdentity, target hostcontract.Target, secrets hostcontract.Secrets) string {
	t.Helper()
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), identity, target, secrets)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func providerCheckpoint(t *testing.T, inputs property.Map, observation hostcontract.StableObservation, revision string) property.Map {
	t.Helper()
	return inputs.Set("machine", providerJSONValue(t, observation.Machine)).Set("ownership", providerJSONValue(t, observation.Ownership)).Set("appliedRevision", property.New(revision)).Set("observation", providerJSONValue(t, observation))
}

func inspectedResponse(observation hostcontract.StableObservation) hostprotocol.Response {
	return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultInspected, Observation: &observation}}
}

func inspectedEvidenceResponse(observation hostcontract.StableObservation, evidence *hostprotocol.OperationEvidence) hostprotocol.Response {
	return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultInspected, Observation: &observation, OperationEvidence: evidence}}
}

func appliedResponse(revision string) hostprotocol.Response {
	return hostprotocol.Response{Version: hostprotocol.Version, Result: &hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: revision}}
}

func writeResponse(t *testing.T, dir, name string, response hostprotocol.Response) {
	t.Helper()
	frame, err := hostprotocol.EncodeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), frame, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeProviderFixture(t *testing.T, dir, name string, value []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), value, 0o600); err != nil {
		t.Fatal(err)
	}
}

func conflictResponse() hostprotocol.Response {
	return hostprotocol.Response{Version: hostprotocol.Version, Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorConflict, Code: hostprotocol.CodeOperationConflict}}
}

func providerRPCProperties(t *testing.T, values property.Map) *structpb.Struct {
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

func providerJSONValue(t *testing.T, value any) property.Value {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var raw any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	return providerRawValue(t, raw)
}

func providerRawValue(t *testing.T, raw any) property.Value {
	t.Helper()
	switch value := raw.(type) {
	case nil:
		return property.New(property.Null)
	case string:
		return property.New(value)
	case bool:
		return property.New(value)
	case float64:
		return property.New(value)
	case []any:
		values := make([]property.Value, len(value))
		for i := range value {
			values[i] = providerRawValue(t, value[i])
		}
		return property.New(property.NewArray(values))
	case map[string]any:
		values := make(map[string]property.Value, len(value))
		for key, nested := range value {
			values[key] = providerRawValue(t, nested)
		}
		return property.New(property.NewMap(values))
	default:
		t.Fatalf("unsupported fixture value %T", raw)
		return property.New(property.Null)
	}
}

func providerStableID(identity hostcontract.ResourceIdentity) string {
	payload := fmt.Sprintf("sub2api-host-resource-id-v1:%d:%s%d:%s", len(identity.Environment), identity.Environment, len(identity.ServerKey), identity.ServerKey)
	sum := sha256.Sum256([]byte(payload))
	return "host-" + hex.EncodeToString(sum[:])
}

func assertProviderSSHInvocation(t *testing.T, traceDir string, call int, alias, secret string) {
	t.Helper()
	args := strings.Split(strings.TrimSuffix(string(mustReadProviderFile(t, filepath.Join(traceDir, fmt.Sprintf("call-%d.args", call)))), "\n"), "\n")
	want := []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "-E", "<client-log>", "--", alias, "/usr/local/libexec/sub2api-host stdio"}
	if len(args) != len(want) {
		t.Fatalf("SSH call %d argv length = %d, want %d", call, len(args), len(want))
	}
	for i := range want {
		if want[i] == "<client-log>" {
			if !filepath.IsAbs(args[i]) {
				t.Fatalf("SSH call %d client log path = %q", call, args[i])
			}
			continue
		}
		if args[i] != want[i] {
			t.Fatalf("SSH call %d argv mismatch at index %d", call, i)
		}
	}
	if strings.Contains(strings.Join(args, "\x00"), secret) {
		t.Fatalf("SSH call %d argv contains secret", call)
	}
}

func assertProviderFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got := mustReadProviderFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s frame mismatch: got length %d, want length %d, first differing byte offset %d", filepath.Base(path), len(got), len(want), firstDifferingByte(got, want))
	}
}

func assertProviderStateDirectorySafe(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"key": true, "evidence": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] || entry.IsDir() {
			t.Fatalf("state directory %s contains an unsafe entry", filepath.Base(path))
		}
		value, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(value, []byte(providerSecret)) {
			t.Fatalf("state directory %s contains the secret canary", filepath.Base(path))
		}
	}
}

func firstDifferingByte(got, want []byte) int {
	limit := len(got)
	if len(want) < limit {
		limit = len(want)
	}
	for i := 0; i < limit; i++ {
		if got[i] != want[i] {
			return i
		}
	}
	return limit
}

func mustReadProviderFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func providerSSHCallCount(t *testing.T, traceDir string) int {
	t.Helper()
	value, err := strconv.Atoi(strings.TrimSpace(string(mustReadProviderFile(t, filepath.Join(traceDir, "count")))))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func waitForProviderFile(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func waitForProcessExit(pid, start string) error {
	n, err := strconv.Atoi(pid)
	if err != nil {
		return fmt.Errorf("invalid SSH pid %q", pid)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !sameProcess(n, start) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("SSH process %d survived cancellation", n)
}

type processSnapshot struct {
	state string
	start string
	pgid  int
}

func readProcessSnapshot(pid int) (processSnapshot, error) {
	value, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processSnapshot{}, err
	}
	closeParen := strings.LastIndexByte(string(value), ')')
	if closeParen < 0 {
		return processSnapshot{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(value)[closeParen+1:])
	if len(fields) < 20 {
		return processSnapshot{}, errors.New("invalid process stat")
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid <= 0 || fields[19] == "" {
		return processSnapshot{}, errors.New("invalid process stat")
	}
	return processSnapshot{state: fields[0], start: fields[19], pgid: pgid}, nil
}

func processStartTime(pid int) string {
	snapshot, err := readProcessSnapshot(pid)
	if err != nil {
		return ""
	}
	return snapshot.start
}

func sameProcess(pid int, start string) bool {
	snapshot, err := readProcessSnapshot(pid)
	return err == nil && snapshot.state != "Z" && snapshot.start == start
}

func terminateProvider(t *testing.T, pid int, start string, pgid int, executable string, done <-chan error) {
	t.Helper()
	doneReceived := false
	waitDone := func(timeout time.Duration) bool {
		if doneReceived {
			return true
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
			doneReceived = true
			return true
		case <-timer.C:
			return false
		}
	}
	if sameProcess(pid, start) && processExecutableMatches(pid, executable) {
		terminateRecordedGroup(pid, start, pgid, syscall.SIGINT)
	}
	waitDone(time.Second)
	if sameProcess(pid, start) && processExecutableMatches(pid, executable) {
		terminateRecordedGroup(pid, start, pgid, syscall.SIGKILL)
	}
	waitDone(time.Second)
	if !doneReceived {
		select {
		case <-done:
			doneReceived = true
		default:
		}
	}
	if !doneReceived {
		t.Errorf("provider process teardown did not complete")
	}
	if sameProcess(pid, start) {
		t.Errorf("provider process remained alive after teardown")
	}
}

func terminateRecordedGroup(pid int, start string, recordedPGID int, signal syscall.Signal) bool {
	if recordedPGID <= 0 {
		return false
	}
	first, err := readProcessSnapshot(pid)
	if err != nil || first.state == "Z" || first.start != start || first.pgid != recordedPGID {
		return false
	}
	second, err := readProcessSnapshot(pid)
	if err != nil || second.state == "Z" || second.start != start || second.pgid != recordedPGID {
		return false
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != recordedPGID {
		return false
	}
	if err := syscall.Kill(-pgid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false
	}
	return true
}

func sameProcessWithGroup(pid int, start string, recordedPGID int) bool {
	snapshot, err := readProcessSnapshot(pid)
	return err == nil && snapshot.state != "Z" && snapshot.start == start && snapshot.pgid == recordedPGID
}

func processGroupID(pid int) int {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return 0
	}
	return pgid
}

func cleanupRecordedSSH(t *testing.T, traceDir, executable string) {
	t.Helper()
	discoveryDeadline := time.Now().Add(2500 * time.Millisecond)
	teardownDeadline := time.Now().Add(5 * time.Second)
	discoveredPaths := map[string]bool{}
	completedPaths := map[string]bool{}
	reported := map[string]bool{}
	scan := func() []string {
		paths, _ := filepath.Glob(filepath.Join(traceDir, "call-*.pid"))
		pendingPaths := []string{}
		for _, path := range paths {
			discoveredPaths[path] = true
			if completedPaths[path] {
				continue
			}
			processName := executable
			if strings.Contains(filepath.Base(path), ".child.") {
				processName = "sleep"
			}
			state := cleanupRecordedProcess(t, path, processName, teardownDeadline, reported)
			if state == recordedPendingMetadata {
				pendingPaths = append(pendingPaths, path)
			}
			if state == recordedExited || state == recordedActiveCleaned {
				completedPaths[path] = true
			}
		}
		return pendingPaths
	}
	for time.Now().Before(discoveryDeadline) {
		scan()
		remaining := time.Until(discoveryDeadline)
		if remaining > 25*time.Millisecond {
			remaining = 25 * time.Millisecond
		}
		if remaining > 0 {
			time.Sleep(remaining)
		}
	}
	finalPending := scan()
	for _, path := range finalPending {
		if reported[path] {
			continue
		}
		reported[path] = true
		t.Errorf("recorded SSH process %s has committed PID metadata with missing or invalid companion metadata", strings.TrimSuffix(filepath.Base(path), ".pid"))
	}
	for path := range discoveredPaths {
		if completedPaths[path] || reported[path] {
			continue
		}
		t.Errorf("recorded SSH process %s teardown state was unresolved", strings.TrimSuffix(filepath.Base(path), ".pid"))
	}
}

type recordedProcessState uint8

const (
	recordedExited recordedProcessState = iota
	recordedPendingMetadata
	recordedActiveCleaned
	recordedUnsafe
)

func cleanupRecordedProcess(t *testing.T, path, executable string, overallDeadline time.Time, reported map[string]bool) recordedProcessState {
	t.Helper()
	label := strings.TrimSuffix(filepath.Base(path), ".pid")
	metadataError := func(message string) {
		if !reported[path] {
			reported[path] = true
			t.Errorf("recorded SSH process %s %s", label, message)
		}
	}
	pidText, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return recordedPendingMetadata
		}
		metadataError("metadata could not be read")
		return recordedUnsafe
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil || pid <= 0 {
		metadataError("has invalid PID metadata")
		return recordedUnsafe
	}
	base := strings.TrimSuffix(path, ".pid")
	startText, err := os.ReadFile(base + ".start")
	if err != nil {
		if os.IsNotExist(err) {
			return recordedPendingMetadata
		}
		metadataError("has invalid start metadata")
		return recordedUnsafe
	}
	if strings.TrimSpace(string(startText)) == "" {
		metadataError("has invalid start metadata")
		return recordedUnsafe
	}
	start := strings.TrimSpace(string(startText))
	if _, err := strconv.ParseUint(start, 10, 64); err != nil {
		metadataError("has invalid start metadata")
		return recordedUnsafe
	}
	pgidText, err := os.ReadFile(base + ".pgid")
	if err != nil {
		if os.IsNotExist(err) {
			return recordedPendingMetadata
		}
		metadataError("has invalid group metadata")
		return recordedUnsafe
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(pgidText)))
	if err != nil || pgid <= 0 {
		metadataError("has invalid group metadata")
		return recordedUnsafe
	}
	commandMatches := processCommandLineMatches
	if executable == "sleep" {
		commandMatches = func(pid int, _ string) bool { return processCommandLineMatches(pid, "sleep") }
	}
	snapshot, snapshotErr := readProcessSnapshot(pid)
	if snapshotErr != nil {
		if os.IsNotExist(snapshotErr) {
			return recordedExited
		}
		metadataError("process identity could not be verified")
		return recordedUnsafe
	}
	if snapshot.state == "Z" || snapshot.start != start {
		return recordedExited
	}
	if snapshot.pgid != pgid || !commandMatches(pid, executable) {
		metadataError(fmt.Sprintf("(%d) identity did not match", pid))
		return recordedUnsafe
	}
	if !terminateRecordedGroup(pid, start, pgid, syscall.SIGTERM) {
		metadataError(fmt.Sprintf("(%d) could not be safely signaled", pid))
		return recordedUnsafe
	}
	waitUntil := time.Now().Add(600 * time.Millisecond)
	if waitUntil.After(overallDeadline) {
		waitUntil = overallDeadline
	}
	for sameProcess(pid, start) && time.Now().Before(waitUntil) {
		time.Sleep(10 * time.Millisecond)
	}
	if sameProcessWithGroup(pid, start, pgid) {
		if !terminateRecordedGroup(pid, start, pgid, syscall.SIGKILL) {
			metadataError(fmt.Sprintf("(%d) could not be safely signaled", pid))
			return recordedUnsafe
		}
		waitUntil = time.Now().Add(600 * time.Millisecond)
		if waitUntil.After(overallDeadline) {
			waitUntil = overallDeadline
		}
		for sameProcess(pid, start) && time.Now().Before(waitUntil) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if sameProcess(pid, start) {
		metadataError(fmt.Sprintf("(%d) remained alive", pid))
		return recordedUnsafe
	}
	return recordedActiveCleaned
}

func processExecutableMatches(pid int, executable string) bool {
	path, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return err == nil && filepath.Clean(path) == filepath.Clean(executable)
}

func processCommandLineMatches(pid int, executable string) bool {
	value, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.Contains(value, []byte(executable))
}

func readProviderPort(stdout io.Reader, timeout time.Duration) (string, error) {
	result := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		result <- struct {
			line string
			err  error
		}{line, err}
	}()
	select {
	case value := <-result:
		return value.line, value.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for provider port")
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate integration test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
