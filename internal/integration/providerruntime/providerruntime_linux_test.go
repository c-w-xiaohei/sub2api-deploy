//go:build linux

package providerruntime

import (
	"bufio"
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
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
		if dockerFixture(os.Args) != nil {
			os.Exit(64)
		}
		os.Exit(0)
	}
	serve := testonly.Serve
	if os.Getenv(ciHelperMode) == "bootstrap" {
		if err := testonly.ServeBootstrapWithRequestDigest(os.Stdout, os.Stdin, os.Getenv(ciHelperRoot), os.Getenv(ciHelperMID), os.Getenv("PROVIDER_RUNTIME_REQUEST_DIGEST")); err != nil {
			_ = os.WriteFile(filepath.Join(os.Getenv("PROVIDER_RUNTIME_TRACE"), "helper.stderr"), []byte(err.Error()+"\n"), 0o600)
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := serve(os.Stdout, os.Stdin, os.Getenv(ciHelperRoot), os.Getenv(ciHelperMID)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestProviderProcessReachesSharedTemporaryRuntimeServe is the permanent 7a
// boundary oracle for Provider Create through the shared temporary Runtime.
func TestProviderProcessReachesSharedTemporaryRuntimeServe(t *testing.T) {
	h := startProvider(t)
	inputs := createInputs()
	response, err := h.client.Create(t.Context(), &pulumirpc.CreateRequest{
		Urn:        "urn:pulumi:test::runtime::sub2api-host:index:Host::edge",
		Properties: rpcProperties(t, inputs),
	})
	if err != nil {
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
	if _, err := os.Stat(filepath.Join(h.trace, "helper.stderr")); !os.IsNotExist(err) {
		t.Fatalf("successful helper wrote stderr: %v", err)
	}
	if got, want := strings.TrimSpace(string(mustRead(t, filepath.Join(h.trace, "request.sha256")))), expectedRequestDigest(t); got != want {
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
	client pulumirpc.ResourceProviderClient
	trace  string
	root   string
}

func startProvider(t *testing.T) *providerProcess {
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
	providerPath := filepath.Join(caseDir, "provider", "pulumi-resource-sub2api-host")
	if err := os.MkdirAll(filepath.Dir(providerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", providerPath, "./cmd/pulumi-resource-sub2api-host")
	build.Dir = workspace
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test Provider: %v: %s", err, output)
	}
	writeArtifactFixture(t, caseDir)
	writeSSHFixture(t, filepath.Join(binDir, "ssh"))
	writeDockerFixture(t, filepath.Join(binDir, "docker"))
	writeExpectedSSHCommands(t, trace)
	clientLogDir := filepath.Join(caseDir, "ssh-client-logs")
	if err := os.MkdirAll(clientLogDir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(providerPath)
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	cleanup := &providerCleanup{cmd: cmd, done: done}
	t.Cleanup(func() { cleanupProvider(t, cleanup) })
	identity, err := providerIdentity(cmd.Process.Pid, providerPath)
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
	client := pulumirpc.NewResourceProviderClient(conn)
	if _, err := client.Configure(t.Context(), &pulumirpc.ConfigureRequest{Args: rpcProperties(t, property.NewMap(map[string]property.Value{"revisionKey": property.New(ciKey).WithSecret(true)}))}); err != nil {
		t.Fatal(err)
	}
	return &providerProcess{client: client, trace: trace, root: root}
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
	binary := []byte("test-only-artifact-not-a-host-binary")
	sum := sha256.Sum256(binary)
	for _, name := range []string{"host-amd64", "host-arm64"} {
		if err := os.WriteFile(filepath.Join(root, name), binary, 0o600); err != nil {
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
	Ownership       string     `json:"ownership"`
	OwnershipLabel  string     `json:"ownershipLabel"`
	ArgumentDigests [][]string `json:"argumentDigests"`
}

// dockerFixture is the only Docker implementation available to this test. It
// validates a closed empty-host Create trace and never starts a real process.
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
	state := dockerTrace{}
	if b, readErr := os.ReadFile(statePath); readErr == nil {
		if json.Unmarshal(b, &state) != nil {
			return errors.New("invalid docker state")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	state.ArgumentDigests = append(state.ArgumentDigests, argumentDigests(args))
	if len(state.ArgumentDigests) == 7 {
		if len(args) != 7 || args[0] != "network" || args[1] != "create" || args[2] != "--label" || !strings.HasPrefix(args[3], "sub2api.host=") || args[4] != "--label" || !strings.HasPrefix(args[5], "sub2api.host.network=") {
			return errors.New("ownership capture position")
		}
		state.OwnershipLabel = strings.TrimPrefix(args[3], "sub2api.host=")
		if !validOwnershipLabel(state.OwnershipLabel) {
			return errors.New("invalid ownership label")
		}
		var runtimeState hostruntime.State
		if err := json.Unmarshal(mustReadFixture(filepath.Join(root, "state.json")), &runtimeState); err != nil || !validOwnership(runtimeState.Ownership.Value) {
			return errors.New("runtime ownership unavailable")
		}
		state.Ownership = runtimeState.Ownership.Value
	}
	if state.Ownership != "" {
		want, err := expectedDockerCommands(root, state.Ownership)
		if err != nil || len(state.ArgumentDigests) > len(want) {
			return errors.New("unexpected docker action")
		}
		for i := range state.ArgumentDigests {
			if !sameArgs(state.ArgumentDigests[i], argumentDigests(want[i].args)) {
				return fmt.Errorf("docker argv mismatch at action %d", i+1)
			}
		}
	}
	if err := writeJSONAtomic(statePath, state); err != nil {
		return err
	}
	if err := appendDockerAction(tracePath, len(state.ArgumentDigests), state.Ownership); err != nil {
		return err
	}
	return dockerOutput(state, root)
}

type dockerCommand struct {
	action string
	args   []string
	output string
}

func expectedDockerCommands(root, ownership string) ([]dockerCommand, error) {
	if !validOwnership(ownership) {
		return nil, errors.New("invalid ownership")
	}
	revision := expectedRevisionNoTest()
	appToken := fixtureToken("app", "api")
	name := "s2h-" + fixtureToken("test", "edge", ownership, "app", appToken, "green")
	network := "s2h-net-" + fixtureToken("test", "edge", ownership)
	ownerLabel := func(role, app, slot string) string {
		return "s2h1:" + fixtureToken("test", "edge", ownership, role, app, slot)
	}
	targetLabel := "s2ht1:" + fixtureToken("app", appToken, "green", revision, "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", "0", "false")
	networkLabel := "s2hnet1:" + fixtureToken("test", "edge", ownership)
	networkFormat := "{{.Name}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.network\"}}"
	containerFormat := "{{.Names}}\t{{index .Labels \"sub2api.host\"}}\t{{index .Labels \"sub2api.host.target\"}}"
	networkList := []string{"network", "ls", "--filter", "name=^" + network + "$", "--format", networkFormat}
	containerList := []string{"container", "ls", "--all", "--filter", "name=^/" + name + "$", "--format", containerFormat}
	env := filepath.Join(root, "runtime", "managed", "env-"+appToken+fixtureToken(revision))
	data := filepath.Join(root, "runtime", "data", fixtureToken("app-data", appToken))
	commands := []dockerCommand{
		{"bootstrap-container-discovery", []string{"container", "ls", "--all", "--filter", "label=sub2api.host", "--format", "{{.Names}}\t{{index .Labels \"sub2api.host\"}}"}, ""},
		{"bootstrap-network-discovery", []string{"network", "ls", "--filter", "label=sub2api.host", "--format", "{{.Name}}\t{{index .Labels \"sub2api.host\"}}"}, ""},
		{"app-candidate-preflight", containerList, ""}, {"network-admit-preflight", networkList, ""}, {"network-ensure-admit", networkList, ""}, {"network-ensure-list", networkList, ""},
		{"network-create", []string{"network", "create", "--label", "sub2api.host=" + ownerLabel("network", "", ""), "--label", "sub2api.host.network=" + networkLabel, network}, ""},
		{"app-candidate-reinspect", containerList, ""}, {"app-candidate-exists", containerList, ""},
		{"app-run", []string{"run", "-d", "--restart", "unless-stopped", "--label", "sub2api.host=" + ownerLabel("app", appToken, "green"), "--label", "sub2api.host.target=" + targetLabel, "--name", name, "--network", network, "--env-file", env, "-v", data + ":/app/data", "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, ""},
		{"app-ready", []string{"exec", name, "wget", "-q", "-O", "/dev/null", "http://localhost:8080/ready"}, ""}, {"app-post-route-ready", []string{"exec", name, "wget", "-q", "-O", "/dev/null", "http://localhost:8080/ready"}, ""},
		{"inspect-network-admit", networkList, network + "\t" + ownerLabel("network", "", "") + "\t" + networkLabel + "\n"}, {"inspect-network-list", networkList, network + "\t" + ownerLabel("network", "", "") + "\t" + networkLabel + "\n"},
		{"inspect-app", containerList, name + "\t" + ownerLabel("app", appToken, "green") + "\t" + targetLabel + "\n"}, {"inspect-app-ready", []string{"exec", name, "wget", "-q", "-O", "/dev/null", "http://localhost:8080/ready"}, ""},
	}
	return commands, nil
}
func dockerOutput(state dockerTrace, root string) error {
	if state.Ownership == "" {
		return nil
	}
	commands, err := expectedDockerCommands(root, state.Ownership)
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, commands[len(state.ArgumentDigests)-1].output)
	return err
}
func appendDockerAction(path string, index int, ownership string) error {
	actions := []string{"bootstrap-container-discovery", "bootstrap-network-discovery", "app-candidate-preflight", "network-admit-preflight", "network-ensure-admit", "network-ensure-list", "network-create", "app-candidate-reinspect", "app-candidate-exists", "app-run", "app-ready", "app-post-route-ready", "inspect-network-admit", "inspect-network-list", "inspect-app", "inspect-app-ready"}
	if index < 1 || index > len(actions) {
		return errors.New("docker action index")
	}
	line := actions[index-1] + "\n"
	if index == 7 {
		sum := sha256.Sum256([]byte(ownership))
		line = actions[index-1] + " ownership-sha256=" + hex.EncodeToString(sum[:]) + "\n"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.WriteString(f, line)
	return err
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
func sameArgs(got, want []string) bool {
	return len(got) == len(want) && func() bool {
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}()
}
func argumentDigests(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		sum := sha256.Sum256([]byte(arg))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
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
	key, _ := base64.StdEncoding.DecodeString(ciKey)
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}, hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}}, hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: ciSecret}}})
	if err != nil {
		panic(err)
	}
	return revision
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
	want, err := expectedDockerCommands(h.root, trace.Ownership)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.ArgumentDigests) != len(want) {
		t.Fatalf("Docker action count = %d, want %d", len(trace.ArgumentDigests), len(want))
	}
	for i := range want {
		if !sameArgs(trace.ArgumentDigests[i], argumentDigests(want[i].args)) {
			t.Fatalf("Docker argv at action %d (%s) differs from strict oracle", i+1, want[i].action)
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(mustRead(t, filepath.Join(h.trace, "docker.args"))), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("sanitized Docker log count = %d, want %d", len(lines), len(want))
	}
	for i, command := range want {
		expected := command.action
		if i == 6 {
			sum := sha256.Sum256([]byte(trace.Ownership))
			expected += " ownership-sha256=" + hex.EncodeToString(sum[:])
		}
		if lines[i] != expected {
			t.Fatalf("sanitized Docker action %d = %q, want %q", i+1, lines[i], expected)
		}
		if strings.Contains(lines[i], ciSecret) {
			t.Fatal("Docker trace leaked secret")
		}
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
	key, err := base64.StdEncoding.DecodeString(ciKey)
	if err != nil {
		t.Fatal(err)
	}
	value, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}, hostcontract.Target{ReleaseArtifact: ciRelease}, hostcontract.Secrets{})
	if err != nil {
		t.Fatal(err)
	}
	return value
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
	key, err := base64.StdEncoding.DecodeString(ciKey)
	if err != nil {
		t.Fatal(err)
	}
	resource := hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}
	target := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}}
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: "PROVIDER_RUNTIME_SECRET_CANARY"}}}
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), resource, target, secrets)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), resource, hostcontract.Target{ReleaseArtifact: ciRelease}, hostcontract.Secrets{})
	if err != nil {
		t.Fatal(err)
	}
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
	artifact := []byte("test-only-artifact-not-a-host-binary")
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

func readProviderPort(t *testing.T, output io.Reader) string {
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
