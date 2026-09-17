package openssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

func TestRunUsesOnlyTheFixedHostCommand(t *testing.T) {
	response, err := hostprotocol.EncodeResponse(hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}})
	if err != nil {
		t.Fatal(err)
	}
	var got invocation
	transport := Transport{start: recordingProcess{result: fakeResult{stdout: response}, got: &got}.start}
	request := []byte("s2h1:secret-canary")
	_, err = transport.Run(context.Background(), "edge.prod", Host, request)
	var remote *RemoteError
	if !errors.Is(err, ErrRemote) || !errors.As(err, &remote) || remote.Category != hostprotocol.ErrorProtocol || remote.Code != hostprotocol.CodeMalformedFrame {
		t.Fatalf("err = %v, want remote protocol/malformed-frame", err)
	}
	want := append([]string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "--", "edge.prod"}, hostCommand)
	if got.name != "ssh" || !reflect.DeepEqual(got.args, want) || !bytes.Equal(got.stdin, request) {
		t.Fatalf("process = %q %#v stdin=%q, want ssh %#v stdin=%q", got.name, got.args, got.stdin, want, request)
	}
	if strings.Contains(strings.Join(got.args, " "), "secret-canary") {
		t.Fatal("secret reached argv")
	}
}

func TestFixedRemoteCommandInventoryHasOnlyProbeAndStdio(t *testing.T) {
	for _, command := range []Command{Probe, Host} {
		remote, ok := remoteCommand(command)
		if !ok || remote == "" || !strings.HasPrefix(remote, "sudo -n -- "+hostExecutable+" ") {
			t.Fatalf("remoteCommand(%d) = %q, %v", command, remote, ok)
		}
	}
	if got, ok := remoteCommand(Command(99)); ok || got != "" {
		t.Fatalf("unknown command = %q, %v", got, ok)
	}
	if got, ok := remoteCommand(Command(3)); ok || got != "" {
		t.Fatalf("deleted bootstrap command = %q, %v", got, ok)
	}
	if probeCommand != "sudo -n -- /nix/var/nix/profiles/sub2api-host/bin/sub2api-host probe" || hostCommand != "sudo -n -- /nix/var/nix/profiles/sub2api-host/bin/sub2api-host stdio" {
		t.Fatalf("fixed commands changed: probe=%q host=%q", probeCommand, hostCommand)
	}
}

func TestRunRejectsMalformedResponsesAndBoundsStderr(t *testing.T) {
	response, err := hostprotocol.EncodeResponse(hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}})
	if err != nil {
		t.Fatal(err)
	}
	for name, stdout := range map[string][]byte{"short": nil, "extra": append(response, []byte("junk")...)} {
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

func TestProbeAcceptsOnlyStrictBoundedVersionedRecord(t *testing.T) {
	machine := "mid1:" + strings.Repeat("a", 64)
	digest := strings.Repeat("b", 64)
	record := []byte(fmt.Sprintf("s2p2:Linux\namd64\n%s\n%s\nrelease-v1\n", machine, digest))
	got, err := ParseProbeRecord(record)
	if err != nil || got != (ProbeInfo{OS: "Linux", Arch: "amd64", Machine: machine, InstalledDigest: digest, Release: "release-v1"}) {
		t.Fatalf("ProbeInfo = %#v, %v", got, err)
	}
	for _, invalid := range [][]byte{
		[]byte("s2p1:Linux\namd64\n" + machine + "\nmissing\n"),
		[]byte("s2p2:Darwin\namd64\n" + machine + "\n" + digest + "\nrelease-v1\n"),
		[]byte("s2p2:Linux\nmips\n" + machine + "\n" + digest + "\nrelease-v1\n"),
		[]byte("s2p2:Linux\namd64\nnot-machine\n" + digest + "\nrelease-v1\n"),
		[]byte("s2p2:Linux\namd64\n" + machine + "\nmissing\nrelease-v1\n"),
		[]byte("s2p2:Linux\namd64\n" + machine + "\n" + digest + "\n\n"),
		append(record, []byte("junk")...),
	} {
		if _, err := ParseProbeRecord(invalid); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted malformed probe record %q: %v", invalid, err)
		}
	}
}

func TestProbeRejectsUppercaseMachineAndDigest(t *testing.T) {
	lowerMachine := "mid1:" + strings.Repeat("a", 64)
	lowerDigest := strings.Repeat("b", 64)
	valid := []byte(fmt.Sprintf("s2p2:Linux\namd64\n%s\n%s\nrelease-v1\n", lowerMachine, lowerDigest))
	if _, err := ParseProbeRecord(valid); err != nil {
		t.Fatalf("lowercase probe record rejected: %v", err)
	}
	uppercaseMachine := []byte(fmt.Sprintf("s2p2:Linux\namd64\nmid1:%s\n%s\nrelease-v1\n", strings.Repeat("A", 64), lowerDigest))
	if _, err := ParseProbeRecord(uppercaseMachine); !errors.Is(err, ErrProtocol) {
		t.Fatalf("uppercase machine accepted: %v", err)
	}
	uppercaseDigest := []byte(fmt.Sprintf("s2p2:Linux\namd64\n%s\n%s\nrelease-v1\n", lowerMachine, strings.Repeat("B", 64)))
	if _, err := ParseProbeRecord(uppercaseDigest); !errors.Is(err, ErrProtocol) {
		t.Fatalf("uppercase digest accepted: %v", err)
	}
}

func TestProbeUsesTheFixedProbeCommand(t *testing.T) {
	var got invocation
	record := []byte("s2p2:Linux\namd64\nmid1:" + strings.Repeat("a", 64) + "\n" + strings.Repeat("b", 64) + "\nrelease-v1\n")
	transport := Transport{start: recordingProcess{result: fakeResult{stdout: record}, got: &got}.start}
	if _, err := transport.Probe(context.Background(), "edge"); err != nil {
		t.Fatal(err)
	}
	if got.name != "ssh" || got.args[len(got.args)-1] != probeCommand || len(got.stdin) != 0 {
		t.Fatalf("probe process = %#v, want fixed command %q", got, probeCommand)
	}
}

func TestLocalProbeRecordUsesExactExecutableAndSiblingRelease(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	share := filepath.Join(root, "share", "sub2api-host")
	if err := os.MkdirAll(share, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "sub2api-host")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	exact := []byte("exact executable bytes")
	if err := os.WriteFile(executable, exact, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "release"), []byte("release-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	machine := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machine, []byte("0123456789abcdef0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := localProbeRecord(machine, executable, executable, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(exact)
	if !bytes.Contains(record, []byte(fmt.Sprintf("\n%x\nrelease-v1\n", digest))) || !bytes.HasPrefix(record, []byte("s2p2:Linux\namd64\nmid1:")) {
		t.Fatalf("probe record = %q", record)
	}
	for _, release := range [][]byte{nil, []byte("release-v1\n\n"), []byte("release\x00v1\n")} {
		if err := os.WriteFile(filepath.Join(share, "release"), release, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := localProbeRecord(machine, executable, executable, "amd64"); err == nil {
			t.Fatalf("release metadata %q accepted", release)
		}
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

func TestRunRejectsHostileAliasBeforeStartingProcess(t *testing.T) {
	called := false
	transport := Transport{start: func(context.Context, string, []string, []byte) processResult { called = true; return processResult{} }}
	_, err := transport.Run(context.Background(), "bad;alias", Host, nil)
	if err == nil || called {
		t.Fatalf("err = %v, process started = %v", err, called)
	}
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
