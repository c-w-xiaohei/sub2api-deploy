//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/pkg/term/termios"
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

func TestValidateCLIUsesFakeSopsAndSSHAndDoesNotPrintSecrets(t *testing.T) {
	root := t.TempDir()
	envDir := filepath.Join(root, "environments", "production")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "config.yaml"), []byte(validConfigForCLI), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "secrets.yaml"), []byte("encrypted-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "sops"), `#!/bin/sh
printf '%s\n' 'revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' 'apps:' '  web-app:' '    initialAdminPassword: CLI_SECRET_SENTINEL' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	sshLog := filepath.Join(root, "ssh.log")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+sshLog+"\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	if err := run(context.Background(), []string{"validate", "production"}, root, nil, &stdout, &stderr); err != nil {
		t.Fatalf("run error = %v, stderr = %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "environment: production") || !strings.Contains(stdout.String(), "servers: 1") || !strings.Contains(stdout.String(), "apps: 1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "CLI_SECRET_SENTINEL") {
		t.Fatal("CLI output exposed a secret")
	}
	sshOutput, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	sshArgs := string(sshOutput)
	if !strings.Contains(sshArgs, "-G -- Edge_Box") || !strings.Contains(sshArgs, "StrictHostKeyChecking=yes") || !strings.Contains(sshArgs, "BatchMode=yes") || !strings.Contains(sshArgs, "ConnectTimeout=10") || !strings.Contains(sshArgs, "-- Edge_Box true") {
		t.Fatalf("fake ssh log = %q", sshOutput)
	}
}

func TestValidateCLIFailsAtSSHWithoutPrintingCommandOutputOrSecrets(t *testing.T) {
	root := t.TempDir()
	envDir := filepath.Join(root, "environments", "production")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "config.yaml"), []byte(validConfigForCLI), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "secrets.yaml"), []byte("encrypted-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "sops"), `#!/bin/sh
printf '%s\n' 'revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' 'apps:' '  web-app:' '    initialAdminPassword: CLI_SECRET_SENTINEL' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' 'fake command output CLI_SECRET_SENTINEL' >&2\nexit 7\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	if err := run(context.Background(), []string{"validate", "production"}, root, nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "Edge_Box") || !strings.Contains(err.Error(), "expand") {
		t.Fatalf("run error = %v", err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "CLI_SECRET_SENTINEL") || strings.Contains(stdout.String()+stderr.String(), "fake command output") {
		t.Fatalf("CLI exposed secret or command output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestValidateCLIChecksSSHAliasInsteadOfServerKey(t *testing.T) {
	root := t.TempDir()
	envDir := filepath.Join(root, "environments", "production")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(validConfigForCLI, "  Edge_Box:\n", "  Edge_Box:\n    sshAlias: deploy-target\n", 1)
	if err := os.WriteFile(filepath.Join(envDir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "secrets.yaml"), []byte("encrypted-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "sops"), `#!/bin/sh
printf '%s\n' 'revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' 'apps:' '  web-app:' '    initialAdminPassword: password' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	sshLog := filepath.Join(root, "ssh.log")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+sshLog+"\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := run(context.Background(), []string{"validate", "production"}, root, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	sshOutput, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sshOutput), "Edge_Box") || !strings.Contains(string(sshOutput), "deploy-target") {
		t.Fatalf("SSH checked server key instead of alias: %q", sshOutput)
	}
}

func TestPublicCLIWiresPulumiProviderAndApproval(t *testing.T) {
	workdir, cli, _, _, logs := pulumiRunFixture(t, pulumiRunSecrets(), "success")

	var stdout, stderr strings.Builder
	if err := run(context.Background(), []string{"pulumi", "production", "preview"}, workdir, func() (string, error) { return cli, nil }, &stdout, &stderr); err != nil {
		t.Fatalf("run error = %v, stderr = %s", err, stderr.String())
	}

	pulumi := pulumiRunRead(t, logs.pulumi)
	for _, want := range []string{
		"cwd=" + workdir,
		"args=<preview><--stack=production><--config-file=",
		"fd3=no",
		"approval=\n",
	} {
		if !strings.Contains(pulumi, want) {
			t.Fatalf("Pulumi invocation missing %q: %s", want, pulumi)
		}
	}
	if _, err := os.Stat(logs.providerStarted); err != nil {
		t.Fatalf("provider did not start: %v", err)
	}
}

// This is an acceptance fixture, not a replacement provider.  It crosses the
// public CLI, its attached provider process, and the provider's gRPC surface.
func TestPublicCLIDeniedDangerousUpdateLeavesRemoteAndStateUntouched(t *testing.T) {
	if os.Getenv("SUB2API_TASK2_SSH_HELPER") == "1" {
		task2SSHHelper()
		os.Exit(0)
	}
	if os.Getenv("SUB2API_TASK2_PULUMI_HELPER") == "1" {
		task2PulumiHelper()
		os.Exit(1) // A denied provider Update must make the public CLI fail.
	}

	root, bundle, evidence := t.TempDir(), t.TempDir(), t.TempDir()
	bin := filepath.Join(bundle, "bin")
	if err := os.MkdirAll(filepath.Join(root, "environments", "production"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "environments", "production", "config.yaml"), []byte("apps: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "environments", "production", "secrets.yaml"), []byte("encrypted placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Pulumi.yaml"), []byte("name: sub2api-environment\nruntime: go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := stagedStackFixture(t)
	if err := os.WriteFile(filepath.Join(root, "Pulumi.production.yaml"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "sops"), "#!/bin/sh\nprintf '%s\\n' 'pulumiPassphrase: "+stagePassphrase+"' 'revisionKey: "+task2RevisionKey+"'\n")
	writeExecutable(t, filepath.Join(bin, "pulumi"), "#!/bin/sh\nSUB2API_TASK2_SSH_HELPER= exec \"$SUB2API_TASK2_TEST_BINARY\" -test.run '^TestPublicCLIDeniedDangerousUpdateLeavesRemoteAndStateUntouched$' -- \"$@\"\n")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nSUB2API_TASK2_PULUMI_HELPER= exec \"$SUB2API_TASK2_TEST_BINARY\" -test.run '^TestPublicCLIDeniedDangerousUpdateLeavesRemoteAndStateUntouched$' -- \"$@\"\n")

	build := func(output, directory string) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "go", "build", "-o", output, ".")
		command.Dir = directory
		if result, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build acceptance sibling: %v: %s", err, result)
		}
	}
	build(filepath.Join(bin, "sub2api-deploy"), ".")
	build(filepath.Join(bin, "pulumi-resource-sub2api-host"), "../pulumi-resource-sub2api-host")

	master, slave, err := termios.Pty()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	cli := exec.Command(filepath.Join(bin, "sub2api-deploy"), "pulumi", "production", "up")
	cli.Dir = root
	cli.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SUB2API_TASK2_TEST_BINARY="+os.Args[0],
		"SUB2API_TASK2_EVIDENCE="+evidence,
		"SUB2API_TASK2_PULUMI_HELPER=1",
		"SUB2API_TASK2_SSH_HELPER=1",
	)
	cli.Stdin, cli.Stdout, cli.Stderr = slave, slave, slave
	cli.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cli.Start(); err != nil {
		_ = slave.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cli.Wait(); close(done) }()
	pty := task2PTYOutput(master)
	var closeSlaveOnce sync.Once
	var closeSlaveErr error
	closeSlave := func() { closeSlaveOnce.Do(func() { closeSlaveErr = slave.Close() }) }
	t.Cleanup(func() {
		// This finalizer owns the bounded final diagnostic drain on every path.
		closeSlave()
		_ = syscall.Kill(-cli.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		task2ReapRecordedProcesses(t, evidence, time.Second)
		output := task2DrainPTY(pty, 2*time.Second)
		task2AssertRedacted(t, output, readPublicCLITestFile(filepath.Join(evidence, "failure")), readPublicCLITestFile(filepath.Join(evidence, "trace")))
	})
	closeSlave()
	if closeSlaveErr != nil { t.Fatal(closeSlaveErr) }

	prompt := task2WaitForPrompt(pty, 10*time.Second)
	if !task2PromptMatches(prompt, task2ApprovalSubject(task2Revision("db.example"))) {
		t.Fatal("approval prompt did not contain the exact dangerous data-link subject")
	}
	if _, err := master.Write([]byte("NO\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if waitErr == nil {
			t.Fatal("public CLI accepted denied dangerous update")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("public CLI did not exit after denial")
	}

	output := task2DrainPTY(pty, 2*time.Second)
	failure, trace := readPublicCLITestFile(filepath.Join(evidence, "failure")), readPublicCLITestFile(filepath.Join(evidence, "trace"))
	task2AssertRedacted(t, output, failure, trace)
	if failure != "" { t.Fatal("Pulumi helper failed before denial") }
	for _, name := range []string{"pulumi-started", "provider-rpc", "denied", "trace", "staged", "staged-parent"} {
		if _, err := os.Stat(filepath.Join(evidence, name)); err != nil {
			t.Fatalf("missing acceptance evidence %s: %v", name, err)
		}
	}
	for _, name := range []string{"bootstrap", "reconcile", "retire", "unexpected-action"} {
		if _, err := os.Stat(filepath.Join(evidence, name)); !os.IsNotExist(err) {
			t.Fatalf("denied update produced forbidden %s: %v", name, err)
		}
	}
	if trace != "inspect\n" { t.Fatalf("SSH action trace = %q", trace) }
	processes, err := task2RecordedProcesses(evidence)
	if err != nil { task2AssertRedacted(t, output, readPublicCLITestFile(filepath.Join(evidence, "failure")), readPublicCLITestFile(filepath.Join(evidence, "trace"))); t.Fatalf("read helper process records: %v", err) }
	for _, process := range processes {
		if err := task2WaitForPIDExit(process.pid, 2*time.Second); err != nil {
			t.Fatalf("attached child %d remained after CLI exit: %v", process.pid, err)
		}
	}
	for _, staged := range []string{strings.TrimSpace(readPublicCLITestFile(filepath.Join(evidence, "staged"))), strings.TrimSpace(readPublicCLITestFile(filepath.Join(evidence, "staged-parent")))} {
		if staged == "" { t.Fatal("missing staged path evidence") }
		if _, err := os.Stat(staged); !os.IsNotExist(err) {
			t.Fatalf("staged stack remains: %v", err)
		}
	}
}

func task2PulumiHelper() {
	evidence := os.Getenv("SUB2API_TASK2_EVIDENCE")
	if evidence == "" {
		os.Exit(90)
	}
	_ = os.WriteFile(filepath.Join(evidence, "pulumi-started"), []byte("1"), 0o600)
	if err := task2RecordProcess(evidence, "pulumi"); err != nil { task2HelperFailure(evidence, err) }
	args := task2HelperArgs()
	if len(args) != 3 || args[0] != "up" || args[1] != "--stack=production" || !strings.HasPrefix(args[2], "--config-file=") { task2HelperFailure(evidence, fmt.Errorf("unexpected Pulumi invocation")) }
	for _, arg := range args {
		if strings.HasPrefix(arg, "--config-file=") {
			staged := strings.TrimPrefix(arg, "--config-file=")
			_ = os.WriteFile(filepath.Join(evidence, "staged"), []byte(staged), 0o600)
			_ = os.WriteFile(filepath.Join(evidence, "staged-parent"), []byte(filepath.Dir(staged)), 0o600)
		}
	}
	port := ""
	for _, item := range strings.Split(os.Getenv("PULUMI_DEBUG_PROVIDERS"), ",") {
		if strings.HasPrefix(item, "sub2api-host:") {
			port = strings.TrimPrefix(item, "sub2api-host:")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, "127.0.0.1:"+port, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		task2HelperFailure(evidence, err)
	}
	defer connection.Close()
	_ = os.WriteFile(filepath.Join(evidence, "provider-rpc"), []byte("1"), 0o600)
	for _, pid := range task2ChildPIDs(os.Getppid()) { if err := task2RecordPID(evidence, "cli-child", pid); err != nil { task2HelperFailure(evidence, err) } }
	client := pulumirpc.NewResourceProviderClient(connection)
	config := task2RPCProperties(property.NewMap(map[string]property.Value{"revisionKey": property.New(task2RevisionKey).WithSecret(true)}))
	if _, err := client.Configure(ctx, &pulumirpc.ConfigureRequest{Args: config, AcceptSecrets: true, AcceptResources: true, SendsOldInputs: true, SendsOldInputsToDelete: true}); err != nil {
		task2HelperFailure(evidence, err)
	}
	old, next := task2HostInputs("old-db.example"), task2HostInputs("db.example")
	oldRevision := task2Revision("old-db.example")
	state := task2Checkpoint(old, oldRevision)
	result, err := client.Update(ctx, &pulumirpc.UpdateRequest{Id: task2StableID(), Urn: "urn:pulumi:production::sub2api-environment::sub2api-host:index:Host::edge", Name: "edge", Type: "sub2api-host:index:Host", Olds: task2RPCProperties(state), OldInputs: task2RPCProperties(old), News: task2RPCProperties(next)})
	if err == nil || result != nil || status.Code(err) != codes.Unknown || status.Convert(err).Message() != "approval required" { task2HelperFailure(evidence, fmt.Errorf("unexpected Update error")) }
	_ = os.WriteFile(filepath.Join(evidence, "denied"), []byte("1"), 0o600)
}

func task2HostInputs(endpoint string) property.Map {
	identity := property.New(property.NewMap(map[string]property.Value{"kind": property.New("postgres"), "providerId": property.New("db-1"), "endpoint": property.New(endpoint), "port": property.New(5432.0), "database": property.New("app"), "tlsServerName": property.New(endpoint)}))
	link := property.New(property.NewMap(map[string]property.Value{"name": property.New("main"), "identity": identity}))
	app := property.New(property.NewMap(map[string]property.Value{"id": property.New("api"), "image": property.New("api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "hostname": property.New("api.example"), "readinessPath": property.New("/ready"), "dataLinks": property.New(property.NewArray([]property.Value{link}))}))
	return property.NewMap(map[string]property.Value{
		"resource": property.New(property.NewMap(map[string]property.Value{"environment": property.New("production"), "serverKey": property.New("edge")})),
		"server": property.New(property.NewMap(map[string]property.Value{"sshAlias": property.New("edge")})),
		"target": property.New(property.NewMap(map[string]property.Value{"releaseArtifact": property.New("release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "apps": property.New(property.NewArray([]property.Value{app}))})),
		"secrets": property.New(property.NewMap(map[string]property.Value{"apps": property.New(property.NewMap(map[string]property.Value{"api": property.New(property.NewMap(map[string]property.Value{"jwtSecret": property.New("task2-app-secret")}))}))})).WithSecret(true),
	})
}

const task2RevisionKey = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="

func task2Revision(endpoint string) string {
	key, _ := base64.StdEncoding.DecodeString(task2RevisionKey)
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}, hostcontract.Target{ReleaseArtifact: "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Apps: []hostcontract.AppTarget{{ID: "api", Image: "api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Hostname: "api.example", ReadinessPath: "/ready", DataLinks: []hostcontract.DataLink{{Name: "main", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "db-1", Endpoint: endpoint, Port: 5432, Database: "app", TLSServerName: endpoint}}}}}}, hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{"api": {JWTSecret: "task2-app-secret"}}})
	if err != nil {
		panic(err)
	}
	return revision
}

func task2StableID() string {
	payload := "sub2api-host-resource-id-v1:10:production4:edge"
	sum := sha256.Sum256([]byte(payload))
	return "host-" + hex.EncodeToString(sum[:])
}

func task2ApprovalSubject(revision string) hostcontract.ApprovalSubject {
	return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: "production", Resource: hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}, AppID: "api", DataKind: "postgres", OldData: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "db-1", Endpoint: "old-db.example", Port: 5432, Database: "app", TLSServerName: "old-db.example"}, NewData: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "db-1", Endpoint: "db.example", Port: 5432, Database: "app", TLSServerName: "db.example"}, TargetRevision: revision}
}

func task2Checkpoint(inputs property.Map, revision string) property.Map {
	observation := hostcontract.StableObservation{Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, HostRelease: "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AppliedRevision: revision, Ready: true, Apps: []hostcontract.AppObservation{{ID: "api", ActiveImage: "api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ready: true}}}
	return inputs.Set("machine", task2Property(observation.Machine)).Set("ownership", task2Property(observation.Ownership)).Set("appliedRevision", property.New(revision)).Set("observation", task2Property(observation))
}

func task2Property(value any) property.Value {
	var raw any
	data, _ := json.Marshal(value)
	_ = json.Unmarshal(data, &raw)
	return task2RawProperty(raw)
}

func task2RawProperty(value any) property.Value {
	switch value := value.(type) {
	case string:
		return property.New(value)
	case float64:
		return property.New(value)
	case []any:
		items := make([]property.Value, len(value))
		for i := range value { items[i] = task2RawProperty(value[i]) }
		return property.New(property.NewArray(items))
	case map[string]any:
		items := map[string]property.Value{}
		for key, item := range value { items[key] = task2RawProperty(item) }
		return property.New(property.NewMap(items))
	default:
		return property.New(property.Null)
	}
}

func task2RPCProperties(values property.Map) *structpb.Struct {
	properties := resource.PropertyMap{}
	values.All(func(key string, value property.Value) bool { properties[resource.PropertyKey(key)] = resource.ToResourcePropertyValue(value); return true })
	encoded, err := plugin.MarshalProperties(properties, plugin.MarshalOptions{KeepUnknowns: true, KeepSecrets: true, KeepResources: true})
	if err != nil { panic(err) }
	return encoded
}

func task2HelperFailure(evidence string, err error) {
	_ = os.WriteFile(filepath.Join(evidence, "failure"), []byte(err.Error()), 0o600)
	os.Exit(91)
}

func task2SSHHelper() {
	evidence := os.Getenv("SUB2API_TASK2_EVIDENCE")
	if err := task2RecordProcess(evidence, "ssh"); err != nil { _ = os.WriteFile(filepath.Join(evidence, "unexpected-action"), []byte("record ssh process"), 0o600); return }
	args := task2HelperArgs()
	separator := -1
	for i, arg := range args { if arg == "--" { separator = i; break } }
	if separator < 2 || args[separator-2] != "-E" || !task2SafeClientLog(args[separator-1]) {
		_ = os.WriteFile(filepath.Join(evidence, "unexpected-action"), []byte("invalid client log"), 0o600)
		return
	}
	args = append(append([]string(nil), args[:separator-2]...), args[separator:]...)
	want := []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "--", "edge", "/usr/local/libexec/sub2api-host stdio"}
	if !reflect.DeepEqual(args, want) {
		_ = os.WriteFile(filepath.Join(evidence, "unexpected-action"), []byte("invalid ssh invocation"), 0o600)
		return
	}
	command := want[len(want)-1]
	if strings.Contains(command, "sub2api-host.stage") {
		_ = os.WriteFile(filepath.Join(evidence, "bootstrap"), []byte("1"), 0o600)
		return
	}
	request, err := hostprotocol.DecodeRequestFrom(os.Stdin)
	if err != nil {
		_ = os.WriteFile(filepath.Join(evidence, "unexpected-action"), []byte("invalid request"), 0o600)
		return
	}
	if request.Action != hostcontract.ActionInspect {
		name := "reconcile"
		if request.Action == hostcontract.ActionRetirePreserveData { name = "retire" }
		_ = os.WriteFile(filepath.Join(evidence, name), []byte("1"), 0o600)
		_ = os.WriteFile(filepath.Join(evidence, "unexpected-action"), []byte("non-inspect request"), 0o600)
		return
	}
	trace, _ := os.OpenFile(filepath.Join(evidence, "trace"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if trace != nil { _, _ = trace.WriteString("inspect\n"); _ = trace.Close() }
	response, err := hostprotocol.EncodeResponse(hostprotocol.Response{Result: &hostprotocol.Result{Status: hostprotocol.ResultInspected, Observation: &hostcontract.StableObservation{Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, HostRelease: "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AppliedRevision: task2Revision("old-db.example"), Ready: true, Apps: []hostcontract.AppObservation{{ID: "api", ActiveImage: "api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ready: true}}}}})
	if err == nil { _, _ = os.Stdout.Write(response) }
}

type task2PTY struct {
	chunks chan string
	done chan struct{}
	output bytes.Buffer
	drainOnce sync.Once
	drained string
}

func task2PTYOutput(file *os.File) *task2PTY {
	p := &task2PTY{chunks: make(chan string, 1), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		buf := make([]byte, 1024)
		for {
			n, err := file.Read(buf)
			if n > 0 { p.chunks <- string(buf[:n]) }
			if err != nil { return }
		}
	}()
	return p
}

func task2WaitForPrompt(p *task2PTY, timeout time.Duration) string {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case chunk := <-p.chunks:
		p.output.WriteString(chunk)
		for !strings.Contains(p.output.String(), "Type APPROVE ") {
			select { case chunk := <-p.chunks: p.output.WriteString(chunk); case <-p.done: return p.output.String(); case <-timer.C: return p.output.String() }
		}
		return p.output.String()
	case <-p.done:
		return p.output.String()
	case <-timer.C:
		return ""
	}
}

func task2ChildPIDs(parent int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil { return nil }
	var children []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid < 1 { continue }
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		fields := strings.Fields(string(data))
		if err == nil && len(fields) > 3 && fields[3] == strconv.Itoa(parent) { children = append(children, pid) }
	}
	return children
}

type task2Process struct{ pid, pgid int }

func task2RecordProcess(evidence, name string) error { return task2RecordPID(evidence, name, os.Getpid()) }
func task2RecordPID(evidence, name string, pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil { return err }
	file, err := os.CreateTemp(evidence, ".pid-")
	if err != nil { return err }
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil { _ = file.Close(); return err }
	if _, err := fmt.Fprintf(file, "%d %d", pid, pgid); err != nil { _ = file.Close(); return err }
	if err := file.Close(); err != nil { return err }
	return os.Rename(temporary, filepath.Join(evidence, "pid-"+name+"-"+strconv.Itoa(pid)))
}
func task2RecordedProcesses(evidence string) ([]task2Process, error) {
	entries, err := os.ReadDir(evidence)
	if err != nil { return nil, err }
	var processes []task2Process
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "pid-") { continue }
		data, err := os.ReadFile(filepath.Join(evidence, entry.Name()))
		if err != nil { return nil, err }
		fields := strings.Fields(string(data))
		if len(fields) != 2 { return nil, fmt.Errorf("malformed process record %q", entry.Name()) }
		pid, first := strconv.Atoi(fields[0]); pgid, second := strconv.Atoi(fields[1])
		if first != nil || second != nil || pid < 1 || pgid < 1 { return nil, fmt.Errorf("malformed process record %q", entry.Name()) }
		processes = append(processes, task2Process{pid, pgid})
	}
	return processes, nil
}

func task2ReapRecordedProcesses(t *testing.T, evidence string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	seen := map[task2Process]bool{}
	reportedReadError := false
	for time.Now().Before(deadline) {
		processes, err := task2RecordedProcesses(evidence)
		if err != nil {
			if !reportedReadError { t.Errorf("read helper process records: %v", err); reportedReadError = true }
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for _, process := range processes {
			if !seen[process] {
				seen[process] = true
				_ = syscall.Kill(-process.pgid, syscall.SIGKILL)
				_ = syscall.Kill(process.pid, syscall.SIGKILL)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	processes, err := task2RecordedProcesses(evidence)
	if err != nil { t.Errorf("read final helper process records: %v", err) } else {
		for _, process := range processes {
			seen[process] = true
			_ = syscall.Kill(-process.pgid, syscall.SIGKILL)
			_ = syscall.Kill(process.pid, syscall.SIGKILL)
		}
	}
	for process := range seen {
		if err := task2WaitForPIDExit(process.pid, time.Second); err != nil { t.Errorf("reap helper process %d: %v", process.pid, err) }
	}
}

func task2DrainPTY(p *task2PTY, timeout time.Duration) string {
	p.drainOnce.Do(func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case chunk := <-p.chunks:
				p.output.WriteString(chunk)
			case <-p.done:
				for { select { case chunk := <-p.chunks: p.output.WriteString(chunk); default: p.drained = p.output.String(); return } }
			case <-timer.C:
				p.drained = p.output.String()
				return
			}
		}
	})
	return p.drained
}

func task2SafeClientLog(path string) bool {
	info, err := os.Stat(path)
	return err == nil && filepath.IsAbs(path) && filepath.Dir(path) == os.TempDir() && info.Mode().IsRegular() && info.Mode().Perm() == 0o600
}

func task2HelperArgs() []string {
	for i, arg := range os.Args {
		if arg == "--" { return os.Args[i+1:] }
	}
	return nil
}

func task2PromptMatches(prompt string, want hostcontract.ApprovalSubject) bool {
	canonical, err := json.Marshal(want)
	return err == nil && strings.Contains(prompt, "subject-base64url: "+base64.RawURLEncoding.EncodeToString(canonical))
}

func task2AssertRedacted(t *testing.T, values ...string) {
	t.Helper()
	for _, value := range values { for _, secret := range []string{stagePassphrase, task2RevisionKey, "task2-app-secret"} { if strings.Contains(value, secret) { t.Fatal("acceptance diagnostic exposed a fixture secret") } } }
}

func task2WaitForPIDExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) { return nil }
		time.Sleep(10 * time.Millisecond)
	}
	return os.ErrProcessDone
}

func TestPublicCLICancellationReapsAttachedProcessesAndStagedStack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("attached process cancellation is Linux-specific")
	}

	workdir, cli, _, _, logs := pulumiRunFixture(t, pulumiRunSecrets(), "success")
	bin := filepath.Dir(cli)
	providerPID := filepath.Join(filepath.Dir(logs.pulumi), "provider.pid")
	pulumiPID := filepath.Join(filepath.Dir(logs.pulumi), "pulumi.pid")
	providerClosed := filepath.Join(filepath.Dir(logs.pulumi), "provider-closed")
	providerInterrupted := filepath.Join(filepath.Dir(logs.pulumi), "provider-interrupted")
	pulumiCleanup := filepath.Join(filepath.Dir(logs.pulumi), "pulumi-cleanup")
	pulumiReady := filepath.Join(filepath.Dir(logs.pulumi), "pulumi-ready")
	blockingFIFO := filepath.Join(filepath.Dir(logs.pulumi), "pulumi-block")
	stdoutPath := filepath.Join(filepath.Dir(logs.pulumi), "cli.stdout")
	stderrPath := filepath.Join(filepath.Dir(logs.pulumi), "cli.stderr")
	if err := syscall.Mkfifo(blockingFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"SUB2API_TEST_PROVIDER_PID":         providerPID,
		"SUB2API_TEST_PROVIDER_CLOSED":      providerClosed,
		"SUB2API_TEST_PROVIDER_INTERRUPTED": providerInterrupted,
		"SUB2API_TEST_PULUMI_PID":           pulumiPID,
		"SUB2API_TEST_PULUMI_CLEANUP":       pulumiCleanup,
		"SUB2API_TEST_PULUMI_READY":         pulumiReady,
		"SUB2API_TEST_STAGED_PARENT":        logs.stagedParent,
		"SUB2API_TEST_BLOCKING_FIFO":        blockingFIFO,
	} {
		t.Setenv(key, value)
	}

	// The cleanup handler makes the RED version safe: it leaves the attached
	// processes alive after the public CLI dies directly from SIGINT.
	var command *exec.Cmd
	var commandDone <-chan struct{}
	t.Cleanup(func() {
		if command != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		if commandDone != nil {
			select {
			case <-commandDone:
			case <-time.After(time.Second):
			}
		}
		for _, path := range []string{pulumiPID, providerPID} {
			killPublicCLITestProcess(path)
			_ = waitForPublicCLITestProcessExit(path, time.Second)
		}
		if parent := strings.TrimSpace(readPublicCLITestFile(logs.stagedParent)); parent != "" {
			_ = os.RemoveAll(parent)
		}
	})

	writeExecutable(t, filepath.Join(bin, "pulumi-resource-sub2api-host"), `#!/bin/sh
trap 'printf interrupted > "$SUB2API_TEST_PROVIDER_INTERRUPTED"; exit 0' INT TERM
printf '%s\n' "$$" > "$SUB2API_TEST_PROVIDER_PID"
printf '%s\n' 43123
read -r _ <&3
printf closed > "$SUB2API_TEST_PROVIDER_CLOSED"
`)
	writeExecutable(t, filepath.Join(bin, "pulumi"), `#!/bin/sh
trap 'printf interrupted > "$SUB2API_TEST_PULUMI_CLEANUP"; exit 0' INT TERM
for arg; do case "$arg" in --config-file=*) config=${arg#--config-file=};; esac; done
[ -n "$config" ] || exit 91
printf '%s\n' "${config%/*}" > "$SUB2API_TEST_STAGED_PARENT"
printf '%s\n' "$$" > "$SUB2API_TEST_PULUMI_PID"
printf ready > "$SUB2API_TEST_PULUMI_READY"
exec 9<> "$SUB2API_TEST_BLOCKING_FIFO"
read -r _ <&9
`)

	buildContext, cancelBuild := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildContext, "go", "build", "-o", cli, ".")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public CLI: %v\n%s", err, output)
	}

	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close(); _ = stdout.Close() })
	command = exec.Command(cli, "pulumi", "production", "preview")
	command.Dir = workdir
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start public CLI: %v", err)
	}
	done := make(chan struct{})
	commandDone = done
	go func() { _ = command.Wait(); close(done) }()

	if err := waitForPublicCLITestFile(pulumiReady, 5*time.Second); err != nil {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("Pulumi did not reach blocked state: %v; stderr=%s", err, readPublicCLITestFile(stderrPath))
	}
	stagedParent := strings.TrimSpace(pulumiRunRead(t, logs.stagedParent))
	if stagedParent == "" {
		t.Fatal("Pulumi did not record staged stack parent")
	}

	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt public CLI: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("public CLI did not exit after SIGINT; stderr=%s", readPublicCLITestFile(stderrPath))
	}

	for _, path := range []string{pulumiCleanup, providerClosed} {
		if err := waitForPublicCLITestFile(path, 2*time.Second); err != nil {
			t.Fatalf("attached child did not clean up %s: %v; stderr=%s", filepath.Base(path), err, readPublicCLITestFile(stderrPath))
		}
	}
	if _, err := os.Stat(providerInterrupted); !os.IsNotExist(err) {
		t.Fatalf("provider exited from signal instead of approval fd EOF; stderr=%s", readPublicCLITestFile(stderrPath))
	}
	for _, path := range []string{pulumiPID, providerPID} {
		if err := waitForPublicCLITestProcessExit(path, 2*time.Second); err != nil {
			t.Fatalf("attached child did not exit %s: %v; stderr=%s", filepath.Base(path), err, readPublicCLITestFile(stderrPath))
		}
	}
	if _, err := os.Stat(stagedParent); !os.IsNotExist(err) {
		t.Fatalf("staged stack parent remains after SIGINT: %v; stderr=%s", err, readPublicCLITestFile(stderrPath))
	}
}

func waitForPublicCLITestProcessExit(path string, timeout time.Duration) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 1 {
		return os.ErrInvalid
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return os.ErrProcessDone
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func killPublicCLITestProcess(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil && pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func readPublicCLITestFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func waitForPublicCLITestFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return os.ErrNotExist
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecuteReturnsGetwdErrorWithoutPanicking(t *testing.T) {
	var stdout, stderr strings.Builder
	err := execute(context.Background(), []string{"validate", "production"}, func() (string, error) {
		return "", os.ErrNotExist
	}, os.Executable, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("execute error = %v", err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

const validConfigForCLI = `version: 1
reverseProxy:
  image: traefik:v3.3.3
  acmeEmail: ops@example.com
servers:
  Edge_Box:
    addresses:
      internal:
        ipv4: 10.0.0.10
postgres:
  main-db:
    type: docker
    server: Edge_Box
redis:
  main-cache:
    type: docker
    server: Edge_Box
apps:
  web-app:
    hostname: app.example.com
    image: ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    initialAdminEmail: admin@example.com
    readinessPath: /ready
    servers: [Edge_Box]
    postgres:
      name: main-db
      database: sub2api
    redis:
      name: main-cache
      database: 0
    publicAccess:
      type: none
`
