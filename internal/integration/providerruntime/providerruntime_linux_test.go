//go:build linux

package providerruntime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
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
)

// TestProviderRuntimeCIHelper is selected only by this integration test binary.
// It is a CI helper, never a released or installed sub2api-host binary.
func TestProviderRuntimeCIHelper(t *testing.T) {
	if os.Getenv(ciHelperGuard) != "1" {
		return
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

// TestProviderProcessReachesSharedTemporaryRuntimeServe is prerequisite 7a.
// It proves the real Provider Create boundary reaches the temporary Runtime
// helper. The only intended RED is the absent shared Runtime serving seam.
func TestProviderProcessReachesSharedTemporaryRuntimeServe(t *testing.T) {
	h := startProvider(t)
	_, err := h.client.Create(t.Context(), &pulumirpc.CreateRequest{
		Urn:        "urn:pulumi:test::runtime::sub2api-host:index:Host::edge",
		Properties: rpcProperties(t, createInputs()),
	})
	if err == nil {
		t.Fatal("Provider Create unexpectedly succeeded without the shared runtime serve seam")
	}
	stderr := strings.TrimSpace(string(mustRead(t, filepath.Join(h.trace, "helper.stderr"))))
	if stderr != testonly.ErrSharedServeSeamUnavailable.Error() {
		t.Fatalf("Provider Create failed before shared runtime serve seam: %v; helper stderr: %q", err, stderr)
	}
	if got, want := strings.TrimSpace(string(mustRead(t, filepath.Join(h.trace, "request.sha256")))), expectedRequestDigest(t); got != want {
		t.Fatalf("bootstrap request digest = %q, want %q", got, want)
	}
	assertSSHRecords(t, h.trace)
	if _, err := os.Stat(filepath.Join(h.trace, "docker.args")); !os.IsNotExist(err) {
		t.Fatalf("missing seam reached runtime Docker discovery: %v", err)
	}
	t.Fatalf("%s", testonly.ErrSharedServeSeamUnavailable)
}

type providerProcess struct {
	client pulumirpc.ResourceProviderClient
	trace  string
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
	writeExpectedSSHCommands(t, workspace, trace)
	clientLogDir := filepath.Join(caseDir, "ssh-client-logs")
	if err := os.MkdirAll(clientLogDir, 0700); err != nil { t.Fatal(err) }
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
		"PROVIDER_RUNTIME_PROBE_COMMAND="+filepath.Join(trace, "probe.command"),
		"PROVIDER_RUNTIME_BOOTSTRAP_COMMAND="+filepath.Join(trace, "bootstrap.command"),
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
	if err != nil { t.Fatal(err) }
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
	return &providerProcess{client: client, trace: trace}
}

func createInputs() property.Map {
	target := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}}
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: "PROVIDER_RUNTIME_SECRET_CANARY"}}}
	return property.NewMap(map[string]property.Value{
		"resource": jsonProperty(hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}),
		"server":   jsonProperty(hostcontract.ServerTarget{SSHAlias: "edge"}),
		"target":   jsonProperty(target),
		"secrets":  jsonProperty(secrets).WithSecret(true),
	})
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
	source := filepath.Join(repositoryRoot(t), "internal", "integration", "providerruntime", "testdata", "docker-bootstrap-discovery.sh")
	b, err := os.ReadFile(source)
	if err != nil { t.Fatal(err) }
	if err := os.WriteFile(destination, b, 0o700); err != nil { t.Fatal(err) }
}

func writeExpectedSSHCommands(t *testing.T, workspace, trace string) {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(workspace, "internal", "openssh", "openssh.go"))
	if err != nil { t.Fatal(err) }
	probeTemplate := sourceTemplate(t, string(source), "func probeScript")
	bootstrapTemplate := sourceTemplate(t, string(source), "func bootstrapReceiverScript")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	probe := fmt.Sprintf(probeTemplate, quote("/etc/machine-id"), quote("/etc/machine-id"), quote("/etc/machine-id"), "sub2api-host-machine-identity-v1", quote("/usr/local/libexec/sub2api-host"), quote("/usr/local/libexec/sub2api-host"))
	bootstrap := "sudo -n /bin/sh -c " + quote(fmt.Sprintf(bootstrapTemplate, quote("/usr/local/libexec/.sub2api-host.stage"), quote("/usr/local/libexec/.sub2api-host.stage.lock"), quote("/usr/local/libexec/sub2api-host"))) + " fixed-argv0"
	if err := os.WriteFile(filepath.Join(trace, "probe.command"), []byte(probe), 0600); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(trace, "bootstrap.command"), []byte(bootstrap), 0600); err != nil { t.Fatal(err) }
}

func sourceTemplate(t *testing.T, source, function string) string {
	t.Helper()
	start := strings.Index(source, function)
	if start < 0 { t.Fatalf("missing %s source", function) }
	start += strings.Index(source[start:], "`")
	end := strings.Index(source[start+1:], "`")
	if end < 0 { t.Fatalf("unterminated %s template", function) }
	return source[start+1 : start+1+end]
}

func expectedRequestDigest(t *testing.T) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(ciKey)
	if err != nil { t.Fatal(err) }
	resource := hostcontract.ResourceIdentity{Environment: "test", ServerKey: "edge"}
	target := hostcontract.Target{ReleaseArtifact: ciRelease, Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Hostname: "api.example", ReadinessPath: "/ready"}}}
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: "PROVIDER_RUNTIME_SECRET_CANARY"}}}
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), resource, target, secrets)
	if err != nil { t.Fatal(err) }
	prior, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), resource, hostcontract.Target{ReleaseArtifact: ciRelease}, hostcontract.Secrets{})
	if err != nil { t.Fatal(err) }
	frame, err := hostprotocol.EncodeRequest(hostprotocol.Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource, TargetRevision: revision, PriorAppliedRevision: prior, Target: &target, Secrets: &secrets})
	if err != nil { t.Fatal(err) }
	sum := sha256.Sum256(frame)
	return hex.EncodeToString(sum[:])
}

func assertSSHRecords(t *testing.T, trace string) {
	t.Helper()
	for _, name := range []string{"ssh.probe.args", "ssh.bootstrap.args", "bootstrap.meta"} {
		if len(mustRead(t, filepath.Join(trace, name))) == 0 { t.Fatalf("missing scripted SSH record %s", name) }
	}
	for _, name := range []string{"ssh.probe.args", "ssh.bootstrap.args"} {
		lines := strings.Split(strings.TrimSuffix(string(mustRead(t, filepath.Join(trace, name))), "\n"), "\n")
		if len(lines) != 48 {
			t.Fatalf("scripted SSH argv record %s is not the fixed transport shape", name)
		}
		for index, line := range lines {
			parts := strings.Split(line, " ")
			if len(parts) != 2 || parts[0] != strconv.Itoa(index+1) || len(parts[1]) != 64 { t.Fatalf("invalid sanitized SSH argv record %s", name) }
			if index != 44 && parts[1] != sshArgumentDigest(t, index, name, trace) { t.Fatalf("scripted SSH argv digest mismatch at index %d", index+1) }
		}
	}
}

func sshArgumentDigest(t *testing.T, index int, record, trace string) string {
	t.Helper()
	fixed := []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "-E", "", "--", "edge"}
	if index == 47 {
		name := "bootstrap.command"
		if record == "ssh.probe.args" { name = "probe.command" }
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

type processIdentity struct { pid, pgid int; start, executable string }
type providerCleanup struct { cmd *exec.Cmd; done <-chan error; identity *processIdentity }

func providerIdentity(pid int, executable string) (processIdentity, error) {
	state, start, pgid, err := processState(pid)
	if err != nil || state == "Z" || pgid <= 0 { return processIdentity{}, fmt.Errorf("invalid Provider process identity") }
	actual, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || actual != executable { return processIdentity{}, fmt.Errorf("Provider executable identity mismatch") }
	return processIdentity{pid: pid, pgid: pgid, start: start, executable: executable}, nil
}

func processState(pid int) (state, start string, pgid int, err error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil { return "", "", 0, err }
	closeParen := strings.LastIndexByte(string(b), ')')
	if closeParen < 0 { return "", "", 0, fmt.Errorf("invalid process stat") }
	fields := strings.Fields(string(b)[closeParen+1:])
	if len(fields) < 20 { return "", "", 0, fmt.Errorf("invalid process stat") }
	pgid, err = strconv.Atoi(fields[2])
	if err != nil { return "", "", 0, err }
	return fields[0], fields[19], pgid, nil
}

func sameProvider(identity processIdentity) bool {
	state, start, pgid, err := processState(identity.pid)
	if err != nil || state == "Z" || start != identity.start || pgid != identity.pgid { return false }
	actual, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(identity.pid), "exe"))
	return err == nil && actual == identity.executable
}

func cleanupProvider(t *testing.T, cleanup *providerCleanup) {
	t.Helper()
	if cleanup.identity == nil {
		if cleanup.cmd.Process != nil { _ = cleanup.cmd.Process.Kill() }
		select { case <-cleanup.done: case <-time.After(time.Second): t.Error("Provider process did not exit") }
		return
	}
	identity := *cleanup.identity
	if sameProvider(identity) { _ = syscall.Kill(-identity.pgid, syscall.SIGTERM) }
	select {
	case <-cleanup.done:
		return
	case <-time.After(time.Second):
	}
	if sameProvider(identity) { _ = syscall.Kill(-identity.pgid, syscall.SIGKILL) }
	select {
	case <-cleanup.done:
	case <-time.After(time.Second):
		t.Error("Provider process did not exit")
	}
	if sameProvider(identity) { t.Error("Provider process survived cleanup") }
}

func cleanupScriptedSSH(t *testing.T, trace string) {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(trace, "ssh.identity"))
	if err != nil { return }
	fields := strings.Fields(string(value))
	if len(fields) != 3 { t.Error("invalid scripted SSH identity"); return }
	pid, err := strconv.Atoi(fields[0]); if err != nil { t.Error("invalid scripted SSH pid"); return }
	state, start, observedPGID, err := processState(pid)
	if err != nil || state == "Z" || start != fields[1] { return }
	pgid, err := strconv.Atoi(fields[2]); if err != nil || pgid != pid { return }
	if observedPGID == pgid { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, start, _, err := processState(pid)
		if err != nil || state == "Z" || start != fields[1] { return }
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
