package openssh

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

func TestRunUsesFixedSSHArgvAndFramedStdin(t *testing.T) {
	response, err := hostprotocol.EncodeResponse(hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}})
	if err != nil {
		t.Fatal(err)
	}
	var got invocation
	transport := Transport{start: recordingProcess{result: fakeResult{stdout: response}, got: &got}.start}
	request := []byte("s2h1:secret-canary")
	_, err = transport.Run(context.Background(), "edge.prod", Host, request)
	if err == nil {
		t.Fatal("remote error was accepted")
	}
	want := []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "--", "edge.prod", "/usr/local/libexec/sub2api-host stdio"}
	if got.name != "ssh" || !reflect.DeepEqual(got.args, want) {
		t.Fatalf("process = %q %#v, want ssh %#v", got.name, got.args, want)
	}
	if !bytes.Equal(got.stdin, request) {
		t.Fatalf("stdin = %q, want %q", got.stdin, request)
	}
	if strings.Contains(strings.Join(got.args, " "), "secret-canary") {
		t.Fatal("secret reached argv")
	}
}

func TestRunRejectsHostileAliasBeforeStartingProcess(t *testing.T) {
	called := false
	transport := Transport{start: func(context.Context, string, []string, []byte) processResult { called = true; return processResult{} }}
	_, err := transport.Run(context.Background(), "bad;alias", Host, nil)
	if err == nil || called {
		t.Fatalf("err = %v, process started = %v", err, called)
	}
}

func TestRunRejectsMalformedResponsesAndBoundsStderr(t *testing.T) {
	for name, stdout := range map[string][]byte{"short": nil, "extra": append(validRemoteError(t), []byte("junk")...)} {
		t.Run(name, func(t *testing.T) {
			transport := Transport{start: recordingProcess{result: fakeResult{stdout: stdout, stderr: bytes.Repeat([]byte("x"), maxStderr+1)}}.start}
			_, err := transport.Run(context.Background(), "edge", Host, nil)
			if !errors.Is(err, ErrProtocol) || len(err.Error()) > 512 {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestRunDoesNotClassifyRemoteControlledStderrAsHostKey(t *testing.T) {
	transport := Transport{start: recordingProcess{result: fakeResult{stderr: []byte("Host key verification failed"), err: errors.New("failed")}}.start}
	_, err := transport.Run(context.Background(), "edge", Host, nil)
	if !errors.Is(err, ErrTransport) || errors.Is(err, ErrHostKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunDoesNotExposeStderrCanary(t *testing.T) {
	transport := Transport{start: recordingProcess{result: fakeResult{stderr: []byte("SECRET-SSH-STDERR-CANARY"), err: errors.New("failed")}}.start}
	_, err := transport.Run(context.Background(), "edge", Host, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET-SSH-STDERR-CANARY") {
		t.Fatalf("stderr leaked in error: %v", err)
	}
}

func TestProbeAcceptsOnlyStrictBoundedLinuxRecord(t *testing.T) {
	machine := strings.Repeat("a", 64)
	transport := Transport{start: recordingProcess{result: fakeResult{stdout: []byte("s2p1:Linux\namd64\n" + machine + "\nmissing\n")}}.start}
	probe, err := transport.Probe(context.Background(), "edge")
	if err != nil || probe.OS != "Linux" || probe.Arch != "amd64" || probe.Machine != "mid1:"+machine || probe.InstalledDigest != "missing" {
		t.Fatalf("Probe = %#v, %v", probe, err)
	}
	transport = Transport{start: recordingProcess{result: fakeResult{stdout: []byte("s2p1:Darwin\namd64\nmachine\nmissing\n")}}.start}
	if _, err := transport.Probe(context.Background(), "edge"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeCapsParentDeadlineAtTenSeconds(t *testing.T) {
	var deadline time.Time
	transport := Transport{start: func(ctx context.Context, _ string, _ []string, _ []byte) processResult {
		deadline, _ = ctx.Deadline()
		return processResult{err: context.DeadlineExceeded}
	}}
	before := time.Now()
	_, _ = transport.Probe(context.Background(), "edge")
	if deadline.IsZero() || deadline.Before(before.Add(9*time.Second)) || deadline.After(before.Add(11*time.Second)) {
		t.Fatalf("probe deadline = %v", deadline)
	}
}

func TestProbeScriptProducesMachineIdentityAndRejectsInvalidMachineIDs(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is unavailable")
	}
	dir := t.TempDir()
	machine := filepath.Join(dir, "machine-id")
	binary := filepath.Join(dir, "host")
	if err := os.WriteFile(machine, []byte("0123456789abcdef0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("host"), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("/bin/sh", "-c", probeScript(machine, binary)).Output()
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(machineIdentityKey))
	_, _ = mac.Write([]byte("0123456789abcdef0123456789abcdef"))
	digest := sha256.Sum256([]byte("host"))
	want := fmt.Sprintf("s2p1:Linux\n%s\n%x\n%x\n", runtimeArch(), mac.Sum(nil), digest)
	if string(output) != want {
		t.Fatalf("probe output = %q, want %q", output, want)
	}
	for _, invalid := range [][]byte{[]byte(""), []byte(strings.Repeat("0", 32)), []byte(strings.Repeat("A", 32)), []byte(strings.Repeat("a", 32) + "\n\n")} {
		if err := os.WriteFile(machine, invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("/bin/sh", "-c", probeScript(machine, binary)).Run(); err == nil {
			t.Fatalf("invalid machine id %q accepted", invalid)
		}
	}
}

func TestCommandsHaveOnlyFixedRemoteEntrypoints(t *testing.T) {
	want := map[Command]string{
		Probe:             probeCommand,
		BootstrapReceiver: "sudo -n /bin/sh -c " + shellQuote(bootstrapReceiverScript(stagePath, finalPath)) + " fixed-argv0",
		Host:              "/usr/local/libexec/sub2api-host stdio",
	}
	for command, remote := range want {
		got, ok := remoteCommand(command)
		if !ok || got != remote {
			t.Fatalf("remoteCommand(%d) = %q, %v", command, got, ok)
		}
	}
	if _, ok := remoteCommand(Command(99)); ok {
		t.Fatal("unknown command accepted")
	}
	if strings.Contains(want[BootstrapReceiver], "/usr/local/libexec/sub2api-host bootstrap-receiver") || strings.Contains(want[BootstrapReceiver], "release") {
		t.Fatalf("bootstrap receiver depends on final binary or interpolated release: %q", want[BootstrapReceiver])
	}
}

func TestBootstrapReceiverScriptSyntaxAndCandidateHandoff(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	script := bootstrapReceiverScript(stage, final)
	check := exec.Command("/bin/sh", "-n", "-c", script)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("shell syntax: %v: %s", err, output)
	}
	marker := filepath.Join(dir, "marker")
	candidate := "#!/bin/sh\n[ \"$1\" = bootstrap-stdio ] || exit 7\ncat > \"$MARKER\"\n"
	digest := sha256.Sum256([]byte(candidate))
	input := []byte(fmt.Sprintf("s2a1:%d:%x\n%srequest", len(candidate), digest, candidate))
	cmd := exec.Command("/bin/sh", "-c", script, "fixed-argv0")
	cmd.Env = append(os.Environ(), "MARKER="+marker)
	cmd.Stdin = bytes.NewReader(input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("receiver: %v: %s", err, output)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "request" {
		t.Fatalf("candidate request = %q, %v", got, err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("receiver changed final: %v", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage remained after candidate handoff: %v", err)
	}
	if _, err := os.Stat(stage + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock remained after candidate handoff: %v", err)
	}
}

func TestBootstrapReceiverRejectsMatchingShortDigestAndCleansUpEveryRun(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	if err := os.WriteFile(final, []byte("existing"), 0700); err != nil {
		t.Fatal(err)
	}
	short := []byte("x")
	shortDigest := sha256.Sum256(short)
	cmd := exec.Command("/bin/sh", "-c", bootstrapReceiverScript(stage, final), "fixed-argv0")
	cmd.Stdin = bytes.NewReader([]byte(fmt.Sprintf("s2a1:2:%x\n%s", shortDigest, short)))
	if err := cmd.Run(); err == nil {
		t.Fatal("matching digest short body accepted")
	}
	for i := 0; i != 2; i++ {
		candidate := []byte("#!/bin/sh\ncat\n")
		digest := sha256.Sum256(candidate)
		cmd = exec.Command("/bin/sh", "-c", bootstrapReceiverScript(stage, final), "fixed-argv0")
		cmd.Stdin = bytes.NewReader([]byte(fmt.Sprintf("s2a1:%d:%x\n%srequest", len(candidate), digest, candidate)))
		if output, err := cmd.CombinedOutput(); err != nil || string(output) != "request" {
			t.Fatalf("run %d = %q, %v", i, output, err)
		}
		for _, path := range []string{stage, stage + ".lock"} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("run %d retained %s: %v", i, path, err)
			}
		}
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != "existing" {
		t.Fatalf("final = %q, %v", got, err)
	}
}

func TestBootstrapReceiverRejectsInvalidFramesWithoutChangingFinal(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	if err := os.WriteFile(final, []byte("existing"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{"short": "s2a1:10:" + strings.Repeat("a", 64) + "\nx", "oversize": "s2a1:67108865:" + strings.Repeat("a", 64) + "\n", "hash": "s2a1:1:" + strings.Repeat("a", 64) + "\nx"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", bootstrapReceiverScript(stage, final), "fixed-argv0")
			cmd.Stdin = strings.NewReader(input)
			if err := cmd.Run(); err == nil {
				t.Fatal("invalid frame accepted")
			}
			got, err := os.ReadFile(final)
			if err != nil || string(got) != "existing" {
				t.Fatalf("final = %q, %v", got, err)
			}
		})
	}
}

func TestBootstrapReceiverLockContentionAndFixedProductionPaths(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	if err := os.Mkdir(stage+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", bootstrapReceiverScript(stage, final), "fixed-argv0")
	cmd.Stdin = strings.NewReader("s2a1:1:" + strings.Repeat("a", 64) + "\nx")
	if err := cmd.Run(); err == nil {
		t.Fatal("lock contention accepted")
	}
	remote, _ := remoteCommand(BootstrapReceiver)
	want := "sudo -n /bin/sh -c " + shellQuote(bootstrapReceiverScript(stagePath, finalPath)) + " fixed-argv0"
	if remote != want || strings.Contains(remote, dir) {
		t.Fatalf("receiver command is not fixed: %q", remote)
	}
}

func validRemoteError(t *testing.T) []byte {
	t.Helper()
	b, err := hostprotocol.EncodeResponse(hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type invocation struct {
	name  string
	args  []string
	stdin []byte
}
type fakeResult struct {
	stdout, stderr []byte
	err            error
}
type recordingProcess struct {
	result fakeResult
	got    *invocation
}

func (p recordingProcess) start(_ context.Context, name string, args []string, stdin []byte) processResult {
	if p.got != nil {
		*p.got = invocation{name, append([]string(nil), args...), append([]byte(nil), stdin...)}
	}
	return processResult{stdout: p.result.stdout, stderr: p.result.stderr, err: p.result.err}
}
