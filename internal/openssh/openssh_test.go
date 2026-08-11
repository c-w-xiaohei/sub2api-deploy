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
	"syscall"
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
	var remote *RemoteError
	if !errors.Is(err, ErrRemote) || !errors.As(err, &remote) || remote.Category != hostprotocol.ErrorProtocol || remote.Code != hostprotocol.CodeMalformedFrame {
		t.Fatalf("err = %v, want remote protocol/malformed-frame", err)
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

func TestBootstrapReceiverAttestsThenInstallsBeforeFinalPathBootstrap(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	script := bootstrapReceiverScript(stage, final)
	check := exec.Command("/bin/sh", "-n", "-c", script)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("shell syntax: %v: %s", err, output)
	}
	old := []byte("old final bytes")
	if err := os.WriteFile(final, old, 0700); err != nil {
		t.Fatal(err)
	}
	invoked := filepath.Join(dir, "invoked")
	observed := filepath.Join(dir, "observed")
	request := []byte("exact post-artifact request bytes")
	received := filepath.Join(dir, "received")
	candidate := attestedCandidate(t, fmt.Sprintf("printf %%s \"$0\" > %s\ncat > %s\ncat \"$FINAL\" > %s", shellQuote(invoked), shellQuote(received), shellQuote(observed)))
	stdout, stderr, err := runBootstrapReceiver(stage, final, candidate, request)
	if err != nil {
		t.Fatalf("receiver: %v: stdout %q stderr %q", err, stdout, stderr)
	}
	if !bytes.Equal(stdout, successResponse(t)) || len(stderr) != 0 {
		t.Fatalf("candidate streams = stdout %q stderr %q", stdout, stderr)
	}
	if got, err := os.ReadFile(received); err != nil || !bytes.Equal(got, request) {
		t.Fatalf("candidate request = %q, %v", got, err)
	}
	if got, err := os.ReadFile(invoked); err != nil || string(got) != final {
		t.Fatalf("bootstrap path = %q, want %q: %v", got, final, err)
	}
	if got, err := os.ReadFile(observed); err != nil || !bytes.Equal(got, candidate) {
		t.Fatalf("final observed during bootstrap = %q, %v", got, err)
	}
	assertFinalCandidate(t, final, candidate)
	assertReceiverCleanup(t, stage)
}

func TestBootstrapReceiverAttestationCannotReadBootstrapRequest(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	seen, received := filepath.Join(dir, "attest-seen"), filepath.Join(dir, "received")
	request := []byte("bootstrap request remains secret until final execution")
	candidate := phaseCandidate(t, "if IFS= read -r byte; then printf %s \"$byte\" > "+shellQuote(seen)+"; elif [ -e /proc/self/fd/4 ]; then printf FD4 > "+shellQuote(seen)+"; else printf EOF > "+shellQuote(seen)+"; fi", "cat > "+shellQuote(received))
	stdout, stderr, err := runBootstrapReceiver(stage, final, candidate, request)
	if err != nil || !bytes.Equal(stdout, successResponse(t)) || len(stderr) != 0 {
		t.Fatalf("receiver = stdout %q stderr %q err %v", stdout, stderr, err)
	}
	if got := string(mustReadFile(t, seen)); got != "EOF" {
		t.Fatalf("attestation read request byte %q", got)
	}
	if got := mustReadFile(t, received); !bytes.Equal(got, request) {
		t.Fatalf("bootstrap request = %q", got)
	}
	assertFinalCandidate(t, final, candidate)
}

func TestBootstrapReceiverSignalTrapWaitsForBootstrapChildBeforeUnlocking(t *testing.T) {
	script := bootstrapReceiverScript(stagePath, finalPath)
	bootstrap := strings.Index(script, `"$final" bootstrap-stdio`)
	if bootstrap < 0 || !strings.Contains(script, `trap '`) || strings.Contains(script, `trap 'rm -f "$ok"; rmdir "$lock"' EXIT HUP INT TERM`) {
		t.Fatal("receiver signal trap removes install lock without managing installed bootstrap child")
	}
	if !strings.Contains(script[bootstrap:], `wait "$child"`) || !strings.Contains(script[bootstrap:], `[ -z "$interrupted" ] && break`) {
		t.Fatal("receiver does not retry interrupted waits before releasing lock")
	}
	if strings.Contains(script, `kill -TERM "$child"`) || strings.Contains(script, `pid=${child:-${!:-}}`) {
		t.Fatal("receiver signal trap risks signaling a stale bootstrap pid")
	}
	disable := strings.Index(script[bootstrap:], "trap '' HUP INT TERM")
	clear := strings.Index(script[bootstrap:], "child=\n")
	if disable < 0 || clear < 0 || disable > clear {
		t.Fatal("receiver clears the bootstrap pid before disabling signal handlers")
	}
}

func TestBootstrapReceiverCleanSameCandidateReplayIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	candidate := attestedCandidate(t, "")
	for i := 0; i != 2; i++ {
		stdout, stderr, err := runBootstrapReceiver(stage, final, candidate, nil)
		if err != nil || !bytes.Equal(stdout, successResponse(t)) || len(stderr) != 0 {
			t.Fatalf("run %d = stdout %q stderr %q err %v", i, stdout, stderr, err)
		}
		assertFinalCandidate(t, final, candidate)
		assertReceiverCleanup(t, stage)
	}
}

func TestBootstrapReceiverAttestationAndBootstrapPhasesHaveDistinctCommitRules(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	old := []byte("existing final bytes")
	for name, tc := range map[string]struct {
		candidate []byte
		installed bool
		stdout    []byte
	}{
		"bad attest marker": {candidateWithMarker(t, successResponse(t), "wrong", 0), false, nil},
		"bad attest exit": {candidateWithMarker(t, successResponse(t), "sub2api-bootstrap-attested-v1", 1), false, nil},
		"malformed bootstrap": {candidateWithMarker(t, []byte("not a response"), "sub2api-bootstrap-attested-v1", 0), true, []byte("not a response")},
		"remote bootstrap error": {candidateWithMarker(t, validRemoteError(t), "sub2api-bootstrap-attested-v1", 0), true, validRemoteError(t)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(final, old, 0700); err != nil {
				t.Fatal(err)
			}
			gotStdout, gotStderr, err := runBootstrapReceiver(stage, final, tc.candidate, nil)
			if !bytes.Equal(gotStdout, tc.stdout) || len(gotStderr) != 0 {
				t.Fatalf("candidate streams = stdout %q stderr %q", gotStdout, gotStderr)
			}
			if tc.installed {
				assertFinalCandidate(t, final, tc.candidate)
			} else if got, readErr := os.ReadFile(final); readErr != nil || !bytes.Equal(got, old) {
				t.Fatalf("final = %q, %v", got, readErr)
			}
			_ = err // Bootstrap protocol output does not define the receiver shell exit status.
			assertReceiverCleanup(t, stage)
		})
	}
}

func TestBootstrapReceiverNonzeroBootstrapKeepsInstalledFinalAndForwardsStreams(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	if err := os.WriteFile(final, []byte("existing final bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	candidate := []byte("#!/bin/sh\ncase \"$1\" in\ninstall-attest) printf %s sub2api-bootstrap-attested-v1 >&3; exit 0 ;;\nbootstrap-stdio) printf 'candidate stdout'; printf 'candidate stderr' >&2; exit 1 ;;\n*) exit 7 ;; esac\n")
	stdout, stderr, err := runBootstrapReceiver(stage, final, candidate, nil)
	if err == nil || string(stdout) != "candidate stdout" || string(stderr) != "candidate stderr" {
		t.Fatalf("receiver = stdout %q stderr %q err %v", stdout, stderr, err)
	}
	assertFinalCandidate(t, final, candidate)
	assertReceiverCleanup(t, stage)
}

func TestBootstrapReceiverRejectsCandidateModifiedAfterAttestation(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	old := []byte("existing final bytes")
	if err := os.WriteFile(final, old, 0700); err != nil {
		t.Fatal(err)
	}
	candidate := phaseCandidate(t, `printf '# mutation' >> "$0"`, "")
	_, _, err := runBootstrapReceiver(stage, final, candidate, nil)
	if err == nil {
		t.Fatal("modified candidate installed")
	}
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("final = %q, %v", got, err)
	}
	assertReceiverCleanup(t, stage)
}

func TestBootstrapReceiverRechecksFinalBeforeRename(t *testing.T) {
	dir := t.TempDir()
	stage, final, target := filepath.Join(dir, "stage"), filepath.Join(dir, "final"), filepath.Join(dir, "target")
	if err := os.WriteFile(final, []byte("old final bytes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("protected target"), 0600); err != nil {
		t.Fatal(err)
	}
	candidate := phaseCandidate(t, "rm -f \"$FINAL\"\nln -s "+shellQuote(target)+" \"$FINAL\"", "")
	_, _, err := runBootstrapReceiver(stage, final, candidate, nil)
	if err == nil {
		t.Fatal("final replacement raced through an unsafe final")
	}
	link, err := os.Readlink(final)
	if err != nil || link != target {
		t.Fatalf("final symlink = %q, %v", link, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "protected target" {
		t.Fatalf("protected target = %q, %v", got, err)
	}
	assertReceiverCleanup(t, stage)
}

func TestBootstrapReceiverRejectsMatchingShortDigestAndInvalidFrames(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	if err := os.WriteFile(final, []byte("existing"), 0700); err != nil {
		t.Fatal(err)
	}
	short := []byte("x")
	shortDigest := sha256.Sum256(short)
	inputs := map[string]string{
		"matching short digest": fmt.Sprintf("s2a1:2:%x\n%s", shortDigest, short),
		"short":                "s2a1:10:" + strings.Repeat("a", 64) + "\nx",
		"oversize":             "s2a1:67108865:" + strings.Repeat("a", 64) + "\n",
		"hash":                 "s2a1:1:" + strings.Repeat("a", 64) + "\nx",
	}
	for name, input := range inputs {
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
			assertReceiverCleanup(t, stage)
		})
	}
}

func TestBootstrapReceiverAcceptsAbsentOrRegularFinalAndRejectsNonregularFinal(t *testing.T) {
	for name, create := range map[string]func(t *testing.T, final, preserved string){
		"absent": func(*testing.T, string, string) {},
		"regular": func(t *testing.T, final, _ string) {
			if err := os.WriteFile(final, []byte("regular final"), 0600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
			create(t, final, "")
			candidate := attestedCandidate(t, "")
			stdout, stderr, err := runBootstrapReceiver(stage, final, candidate, nil)
			if err != nil || !bytes.Equal(stdout, successResponse(t)) || len(stderr) != 0 {
				t.Fatalf("receiver = stdout %q stderr %q err %v", stdout, stderr, err)
			}
			assertFinalCandidate(t, final, candidate)
			assertReceiverCleanup(t, stage)
		})
	}

	for name, create := range map[string]func(t *testing.T, final, preserved string){
		"symlink": func(t *testing.T, final, preserved string) {
			if err := os.WriteFile(preserved, []byte("protected target"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(preserved, final); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, final, _ string) {
			if err := os.Mkdir(final, 0700); err != nil {
				t.Fatal(err)
			}
		},
		"FIFO": func(t *testing.T, final, _ string) {
			if err := syscall.Mkfifo(final, 0600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stage, final, preserved := filepath.Join(dir, "stage"), filepath.Join(dir, "final"), filepath.Join(dir, "preserved")
			create(t, final, preserved)
			before, err := os.Lstat(final)
			if err != nil {
				t.Fatal(err)
			}
			ran := filepath.Join(dir, "ran")
			_, _, err = runBootstrapReceiver(stage, final, attestedCandidate(t, "printf ran > "+shellQuote(ran)), nil)
			if err == nil {
				t.Fatal("unsafe final accepted")
			}
			if _, err := os.Lstat(ran); !os.IsNotExist(err) {
				t.Fatalf("candidate ran before unsafe-final rejection: %v", err)
			}
			after, err := os.Lstat(final)
			if err != nil {
				t.Fatalf("stat final after rejection: %v", err)
			}
			if after.Mode() != before.Mode() {
				t.Fatalf("final changed: before %v, after %v", before.Mode(), after.Mode())
			}
			if name == "symlink" {
				link, err := os.Readlink(final)
				if err != nil || link != preserved {
					t.Fatalf("symlink = %q, %v", link, err)
				}
				got, err := os.ReadFile(preserved)
				if err != nil || string(got) != "protected target" {
					t.Fatalf("symlink target = %q, %v", got, err)
				}
			}
			assertReceiverCleanup(t, stage)
		})
	}
}

func TestBootstrapReceiverAttestsThenAtomicallyInstallsBeforeBootstrap(t *testing.T) {
	script := bootstrapReceiverScript(stagePath, finalPath)
	attest := strings.Index(script, `"$stage" install-attest`)
	rename := strings.Index(script, `mv -T -- "$stage" "$final"`)
	invoke := strings.Index(script, `"$final" bootstrap-stdio`)
	if attest < 0 || rename < attest || invoke < rename || strings.Count(script, `mv -T -- "$stage" "$final"`) != 1 {
		t.Fatal("receiver must attest candidate, atomically install once, then bootstrap final")
	}
	if strings.Contains(script, `"$stage" bootstrap-stdio`) {
		t.Fatal("receiver bootstraps the uninstalled candidate")
	}
	for _, line := range strings.Split(script[:rename], "\n") {
		if strings.Contains(line, `"$final"`) && (strings.Contains(line, "rm ") || strings.Contains(line, "unlink ") || strings.Contains(line, "cp ") || strings.Contains(line, "install ") || strings.Contains(line, `> "$final"`) || strings.Contains(line, `>"$final"`)) {
			t.Fatalf("receiver mutates final before atomic replacement: %q", line)
		}
	}
}

func TestBootstrapReceiverBootstrapsOnlyInstalledFinalAndKeepsItAfterResponseLoss(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	marker, invocations := filepath.Join(dir, "installed"), filepath.Join(dir, "invocations")
	candidate := []byte("#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"install-attest) printf %s sub2api-bootstrap-attested-v1 >&3; exit 0 ;;\n" +
		"bootstrap-stdio) [ \"$0\" = \"$FINAL\" ] || exit 71; printf installed > " + shellQuote(marker) + "; n=0; [ -f " + shellQuote(invocations) + " ] && n=$(cat " + shellQuote(invocations) + "); printf %s $((n+1)) > " + shellQuote(invocations) + "; exit 72 ;;\n" +
		"*) exit 7 ;; esac\n")
	_, _, err := runBootstrapReceiver(stage, final, candidate, successResponse(t))
	if string(mustReadFile(t, marker)) != "installed" || string(mustReadFile(t, invocations)) != "1" {
		t.Fatalf("post-rename response loss = %v, marker=%q invocations=%q", err, mustReadFile(t, marker), mustReadFile(t, invocations))
	}
	assertFinalCandidate(t, final, candidate)
	_, _, err = runBootstrapReceiver(stage, final, candidate, successResponse(t))
	if string(mustReadFile(t, invocations)) != "2" {
		t.Fatalf("both attempts did not execute installed bytes: %v, invocations=%q", err, mustReadFile(t, invocations))
	}
}

func TestBootstrapReceiverRejectsDirectoryRaceAtAtomicCommit(t *testing.T) {
	dir := t.TempDir()
	stage, final := filepath.Join(dir, "stage"), filepath.Join(dir, "final")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	fakeMV := filepath.Join(bin, "mv")
	if err := os.WriteFile(fakeMV, []byte(`#!/bin/sh
case "$#:$1:$2:$3:$4" in
  2:"$STAGE":"$FINAL"::|4:-T:--:"$STAGE":"$FINAL") ;;
  *) exit 99 ;;
esac
mkdir "$FINAL"
exec /bin/mv "$@"
`), 0700); err != nil {
		t.Fatal(err)
	}
	_, _, err := runBootstrapReceiverWithEnv(stage, final, attestedCandidate(t, ""), nil, []string{"STAGE=" + stage, "PATH=" + bin + ":" + os.Getenv("PATH")})
	if err == nil {
		t.Fatal("receiver accepted directory race at final commit")
	}
	info, err := os.Stat(final)
	if err != nil {
		t.Fatalf("stat raced final: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("raced final mode = %v, want directory", info.Mode())
	}
	if _, err := os.Lstat(filepath.Join(final, filepath.Base(stage))); !os.IsNotExist(err) {
		t.Fatalf("stage was moved into raced final: %v", err)
	}
	assertReceiverCleanup(t, stage)
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

func runBootstrapReceiver(stage, final string, candidate []byte, request []byte) ([]byte, []byte, error) {
	return runBootstrapReceiverWithEnv(stage, final, candidate, request, nil)
}

func runBootstrapReceiverWithEnv(stage, final string, candidate []byte, request []byte, extraEnv []string) ([]byte, []byte, error) {
	digest := sha256.Sum256(candidate)
	input := []byte(fmt.Sprintf("s2a1:%d:%x\n%s", len(candidate), digest, candidate))
	input = append(input, request...)
	cmd := exec.Command("/bin/sh", "-c", bootstrapReceiverScript(stage, final), "fixed-argv0")
	cmd.Env = append(append(os.Environ(), "FINAL="+final), extraEnv...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func attestedCandidate(t *testing.T, body string) []byte {
	t.Helper()
	return phaseCandidate(t, "", body)
}

func phaseCandidate(t *testing.T, attest, bootstrap string) []byte {
	t.Helper()
	return []byte("#!/bin/sh\ncase \"$1\" in\ninstall-attest) " + attest + "\nprintf %s 'sub2api-bootstrap-attested-v1' >&3; exit 0 ;;\nbootstrap-stdio) " + bootstrap + "\nprintf %s " + shellQuote(string(successResponse(t))) + "; exit 0 ;;\n*) exit 7 ;; esac\n")
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil { t.Fatalf("read %s: %v", path, err) }
	return b
}

func candidateWithMarker(t *testing.T, stdout []byte, marker string, status int) []byte {
	t.Helper()
	markerWrite := ""
	if marker != "" {
		markerWrite = "\nprintf %s " + shellQuote(marker) + " >&3"
	}
	return []byte("#!/bin/sh\ncase \"$1\" in\ninstall-attest)" + markerWrite + fmt.Sprintf("\nexit %d ;;\n", status) + "bootstrap-stdio) printf %s " + shellQuote(string(stdout)) + "; exit 0 ;;\n*) exit 7 ;; esac\n")
}

func successResponse(t *testing.T) []byte {
	t.Helper()
	response, err := hostprotocol.EncodeResponse(hostprotocol.Response{
		Result: &hostprotocol.Result{
			Status:          hostprotocol.ResultApplied,
			AppliedRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func assertFinalCandidate(t *testing.T, final string, candidate []byte) {
	t.Helper()
	info, err := os.Lstat(final)
	if err != nil {
		t.Fatalf("stat final: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("final mode = %v, want executable regular file", info.Mode())
	}
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, candidate) {
		t.Fatalf("final bytes = %q, %v", got, err)
	}
}

func assertReceiverCleanup(t *testing.T, stage string) {
	t.Helper()
	for _, path := range []string{stage, stage + ".lock", stage + ".ok"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("receiver retained %s: %v", path, err)
		}
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
