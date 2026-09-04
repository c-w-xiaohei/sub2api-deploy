//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

const attachedCanary = "ATTACHED_RUN_SECRET_CANARY"

func TestResolveAttachedExecutablesUsesOnlyCLIReleaseSiblings(t *testing.T) {
	bundle := t.TempDir()
	bin := filepath.Join(bundle, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(bin, "sub2api-deploy")
	for _, path := range []string{cli, filepath.Join(bin, "pulumi-resource-sub2api-host"), filepath.Join(bin, "pulumi")} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	decoy := t.TempDir()
	for _, name := range []string{"pulumi-resource-sub2api-host", "pulumi"} {
		if err := os.WriteFile(filepath.Join(decoy, name), []byte("decoy"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", decoy)

	paths, err := resolveAttachedExecutables(cli)
	if err != nil {
		t.Fatal(err)
	}
	if paths.provider != filepath.Join(bin, "pulumi-resource-sub2api-host") || paths.pulumi != filepath.Join(bin, "pulumi") {
		t.Fatalf("resolved paths = %#v, want exact executable siblings", paths)
	}
}

func TestResolveAttachedExecutablesRejectsUnsafeSiblings(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		setup func(t *testing.T, bin string)
	}{
		{"missing", func(t *testing.T, bin string) { _ = os.Remove(filepath.Join(bin, "pulumi")) }},
		{"non-executable", func(t *testing.T, bin string) { if err := os.Chmod(filepath.Join(bin, "pulumi"), 0o600); err != nil { t.Fatal(err) } }},
		{"symlink", func(t *testing.T, bin string) { path := filepath.Join(bin, "pulumi"); if err := os.Remove(path); err != nil { t.Fatal(err) }; if err := os.Symlink(filepath.Join(t.TempDir(), "pulumi"), path); err != nil { t.Fatal(err) } }},
		{"directory", func(t *testing.T, bin string) { path := filepath.Join(bin, "pulumi"); if err := os.Remove(path); err != nil { t.Fatal(err) }; if err := os.Mkdir(path, 0o700); err != nil { t.Fatal(err) } }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			bundle := t.TempDir()
			bin := filepath.Join(bundle, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil { t.Fatal(err) }
			for _, name := range []string{"sub2api-deploy", "pulumi-resource-sub2api-host", "pulumi"} { writeAttachedExecutable(t, filepath.Join(bin, name), "#!/bin/sh\nexit 0\n") }
			scenario.setup(t, bin)
			if _, err := resolveAttachedExecutables(filepath.Join(bin, "sub2api-deploy")); err == nil {
				t.Fatal("unsafe attached executable was accepted")
			}
		})
	}
}

func TestAttachedSocketpairUsesAtomicCloseOnExec(t *testing.T) {
	parent, child, err := attachedSocketpair()
	if err != nil { t.Fatal(err) }
	defer parent.Close()
	defer child.Close()
	for _, file := range []*os.File{parent, child} {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), uintptr(syscall.F_GETFD), 0)
		if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
			t.Fatalf("socketpair FD %d CLOEXEC flags=%#x errno=%v", file.Fd(), flags, errno)
		}
	}
}

func TestRunAttachedStartsDirectProviderAndPassesPulumiArgumentsAndDebugProvider(t *testing.T) {
	paths := attachedHelperPaths(t)
	pulumiLog := filepath.Join(t.TempDir(), "pulumi.log")
	cleanupLog := filepath.Join(t.TempDir(), "provider-cleanup.log")
	env := append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE=ready", "SUB2API_ATTACHED_PULUMI_LOG="+pulumiLog, "SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog, "PULUMI_DEBUG_PROVIDERS=aws:123,sub2api-host:stale,random:456")
	args := []string{"up", "--stack", "org/project dev", "--yes", "--diff"}
	var stdout, stderr strings.Builder
	if err := runAttached(t.Context(), paths, args, env, &stdout, &stderr, func(context.Context, hostcontract.ApprovalSubject) bool { return false }); err != nil {
		t.Fatalf("runAttached = %v, stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(pulumiLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "<up><--stack><org/project dev><--yes><--diff>" || lines[1] != "aws:123,random:456,sub2api-host:43123" || lines[2] != "no-fd3" {
		t.Fatalf("pulumi invocation = %q", data)
	}
	if strings.Contains(stdout.String()+stderr.String(), attachedCanary) {
		t.Fatalf("attached runner exposed provider output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if data, err := os.ReadFile(cleanupLog); err != nil || string(data) != "closed\n" {
		t.Fatalf("provider was not terminated and reaped after Pulumi exit: %q, %v", data, err)
	}
}

func TestRunAttachedFailsBeforePulumiForProviderStartupProtocolFailures(t *testing.T) {
	for _, mode := range []string{"failure", "malformed", "oversize", "oversize-no-newline"} {
		t.Run(mode, func(t *testing.T) {
			paths := attachedHelperPaths(t)
			pulumiLog := filepath.Join(t.TempDir(), "pulumi.log")
			env := append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE="+mode, "SUB2API_ATTACHED_PULUMI_LOG="+pulumiLog)
			var stdout, stderr strings.Builder
			err := runAttached(t.Context(), paths, []string{"preview"}, env, &stdout, &stderr, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
			if err == nil || errors.Is(err, context.Canceled) || strings.Contains(err.Error(), attachedCanary) {
				t.Fatalf("provider %s error = %v", mode, err)
			}
			if _, statErr := os.Stat(pulumiLog); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Pulumi started after provider %s: %v", mode, statErr)
			}
			if strings.Contains(stdout.String()+stderr.String(), attachedCanary) {
				t.Fatalf("provider %s output leaked: stdout=%q stderr=%q", mode, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunAttachedCancellationReclaimsProviderBlockedBeforePortAndDoesNotStartPulumi(t *testing.T) {
	paths := attachedHelperPaths(t)
	pulumiLog, cleanupLog := filepath.Join(t.TempDir(), "pulumi.log"), filepath.Join(t.TempDir(), "cleanup.log")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := runAttached(ctx, paths, []string{"preview"}, append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE=blocked", "SUB2API_ATTACHED_PULUMI_LOG="+pulumiLog, "SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked provider error = %v", err)
	}
	if _, err := os.Stat(pulumiLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pulumi started before blocked provider port: %v", err)
	}
	if data, err := os.ReadFile(cleanupLog); err != nil || string(data) != "closed\n" {
		t.Fatalf("blocked provider was not cleaned up: %q, %v", data, err)
	}
}

func TestRunAttachedFailsPromptlyWhenApprovalClosesBeforeProviderPort(t *testing.T) {
	paths := attachedHelperPaths(t)
	pulumiLog, cleanupLog := filepath.Join(t.TempDir(), "pulumi.log"), filepath.Join(t.TempDir(), "cleanup.log")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := runAttached(ctx, paths, []string{"preview"}, append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE=close-approval-before-port", "SUB2API_ATTACHED_PULUMI_LOG="+pulumiLog, "SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("approval EOF before port error = %v", err)
	}
	if _, err := os.Stat(pulumiLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pulumi started after approval EOF before provider port: %v", err)
	}
	if data, err := os.ReadFile(cleanupLog); err != nil || string(data) != "closed\n" {
		t.Fatalf("provider was not cleaned up after approval EOF before port: %q, %v", data, err)
	}
}

func TestAttachedPulumiEnvUsesOnlyLastEffectiveDebugProviderValue(t *testing.T) {
	env := attachedPulumiEnv([]string{"PULUMI_DEBUG_PROVIDERS=old:1,sub2api-host:old", "OTHER=value", "PULUMI_DEBUG_PROVIDERS=aws:123,sub2api-host,sub2api-host:bad,random:456"}, "43123")
	var debug []string
	for _, value := range env { if strings.HasPrefix(value, "PULUMI_DEBUG_PROVIDERS=") { debug = append(debug, value) } }
	if len(debug) != 1 || debug[0] != "PULUMI_DEBUG_PROVIDERS=aws:123,random:456,sub2api-host:43123" {
		t.Fatalf("canonical debug providers = %#v", debug)
	}
}

func TestAttachedProviderEnvRemovesAllPulumiVariablesAndPreservesUnrelatedVariables(t *testing.T) {
	env := attachedProviderEnv([]string{
		"PULUMI_DEBUG_PROVIDERS=sub2api-host:1234",
		"PULUMI_CONFIG_PASSPHRASE=secret",
		"PULUMI_CONFIG_PASSPHRASE_FILE=/secret",
		"PULUMI_ARBITRARY_CANARY=must-not-reach-provider",
		"SUB2API_HOST_APPROVAL_FD=9",
		"UNRELATED=value",
	})
	got := strings.Join(env, "\n")
	for _, forbidden := range []string{"PULUMI_DEBUG_PROVIDERS=", "PULUMI_CONFIG_PASSPHRASE=", "PULUMI_CONFIG_PASSPHRASE_FILE=", "PULUMI_ARBITRARY_CANARY="} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("provider environment retained %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "SUB2API_HOST_APPROVAL_FD=3") || !strings.Contains(got, "UNRELATED=value") {
		t.Fatalf("provider environment lost approval or unrelated value: %q", got)
	}
}

func TestRunAttachedFailsWhenProviderExitsAfterPublishingPortAndStopsPulumi(t *testing.T) {
	paths := attachedHelperPaths(t)
	directory := t.TempDir()
	pulumiLog, cleanupLog := filepath.Join(directory, "pulumi.log"), filepath.Join(directory, "cleanup.log")
	pulumiReady := attachedFIFO(t, filepath.Join(directory, "pulumi-ready"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := runAttached(ctx, paths, []string{"up"}, append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE=exit-after-pulumi-ready", "SUB2API_ATTACHED_PULUMI_MODE=blocked", "SUB2API_ATTACHED_PULUMI_LOG="+pulumiLog, "SUB2API_ATTACHED_PULUMI_READY="+pulumiReady.name, "SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
	if err == nil || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), attachedCanary) {
		t.Fatalf("provider early exit error = %v", err)
	}
	if data, err := os.ReadFile(cleanupLog); err != nil || string(data) != "pulumi-closed\n" {
		t.Fatalf("Pulumi was not stopped after provider exit: %q, %v", data, err)
	}
}

func TestRunAttachedFailsWhenApprovalChannelEndsAroundPulumiStartup(t *testing.T) {
	paths := attachedHelperPaths(t)
	pulumiLog, cleanupLog := filepath.Join(t.TempDir(), "pulumi.log"), filepath.Join(t.TempDir(), "cleanup.log")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := runAttached(ctx, paths, []string{"up"}, append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE=close-approval", "SUB2API_ATTACHED_PULUMI_MODE=blocked", "SUB2API_ATTACHED_PULUMI_LOG="+pulumiLog, "SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
	if err == nil || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), attachedCanary) {
		t.Fatalf("approval channel early EOF error = %v", err)
	}
	if _, err := os.Stat(pulumiLog); err == nil {
		if data, readErr := os.ReadFile(cleanupLog); readErr != nil || !strings.Contains(string(data), "pulumi-closed\n") {
			t.Fatalf("started Pulumi was not stopped after approval EOF: %q, %v", data, readErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect Pulumi startup: %v", err)
	}
}

func TestRunAttachedPrefersKnownProviderFailureOverReleasedSuccessfulPulumiExit(t *testing.T) {
	paths := attachedHelperPaths(t)
	dir := t.TempDir()
	pulumiReady := attachedFIFO(t, filepath.Join(dir, "pulumi-ready"))
	pulumiRelease := attachedFIFO(t, filepath.Join(dir, "pulumi-release"))
	providerRelease := attachedFIFO(t, filepath.Join(dir, "provider-release"))
	providerExited := attachedFIFO(t, filepath.Join(dir, "provider-exited"))
	cleanupLog := filepath.Join(dir, "cleanup.log")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runAttached(ctx, paths, []string{"up"}, append(os.Environ(),
			"SUB2API_ATTACHED_PROVIDER_MODE=close-approval-after-port",
			"SUB2API_ATTACHED_PULUMI_MODE=release-success",
			"SUB2API_ATTACHED_PULUMI_READY="+pulumiReady.name,
			"SUB2API_ATTACHED_PULUMI_RELEASE="+pulumiRelease.name,
			"SUB2API_ATTACHED_PROVIDER_RELEASE="+providerRelease.name,
			"SUB2API_ATTACHED_PROVIDER_EXITED="+providerExited.name,
			"SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog,
		), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
	}()
	attachedReceive(t, pulumiReady)
	attachedSend(t, providerRelease)
	attachedReceive(t, providerExited)
	attachedSend(t, pulumiRelease)
	err := <-done
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("provider/approval failure lost to successful Pulumi exit: %v", err)
	}
}

func TestRunAttachedCancellationDuringProviderCleanupWinsOverSuccessfulPulumi(t *testing.T) {
	paths := attachedHelperPaths(t)
	dir := t.TempDir()
	pulumiExited := attachedFIFO(t, filepath.Join(dir, "pulumi-exited"))
	providerHolding := attachedFIFO(t, filepath.Join(dir, "provider-holding"))
	providerRelease := attachedFIFO(t, filepath.Join(dir, "provider-release"))
	cleanupLog := filepath.Join(dir, "cleanup.log")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runAttached(ctx, paths, []string{"up"}, append(os.Environ(),
			"SUB2API_ATTACHED_PROVIDER_MODE=cleanup-hold",
			"SUB2API_ATTACHED_PULUMI_MODE=success-ready",
			"SUB2API_ATTACHED_PULUMI_EXITED="+pulumiExited.name,
			"SUB2API_ATTACHED_PROVIDER_HOLDING="+providerHolding.name,
			"SUB2API_ATTACHED_PROVIDER_RELEASE="+providerRelease.name,
			"SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog,
		), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
	}()
	attachedReceive(t, pulumiExited)
	attachedReceive(t, providerHolding)
	cancel()
	attachedSend(t, providerRelease)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during cleanup = %v, want context.Canceled", err)
	}
	if data, err := os.ReadFile(cleanupLog); err != nil || string(data) != "closed\n" {
		t.Fatalf("provider cleanup after cancellation = %q, %v", data, err)
	}
}

func TestAttachedTerminalPriority(t *testing.T) {
	closed := func() *attachedCompletion {
		completion := &attachedCompletion{done: make(chan struct{})}
		close(completion.done)
		return completion
	}
	open := func() *attachedCompletion { return &attachedCompletion{done: make(chan struct{})} }
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("operator cancelled")
	cancel(cause)
	if got := attachedTerminal(ctx, closed(), closed()); !errors.Is(got, cause) {
		t.Fatalf("context priority = %v, want %v", got, cause)
	}
	if got := attachedTerminal(t.Context(), closed(), closed()); got == nil || got.Error() != "attached provider failed" {
		t.Fatalf("provider priority = %v", got)
	}
	if got := attachedTerminal(t.Context(), open(), closed()); got == nil || got.Error() != "attached approval failed" {
		t.Fatalf("server-only terminal = %v", got)
	}
	if got := attachedTerminal(t.Context(), open(), open()); got != nil {
		t.Fatalf("no terminal completion = %v", got)
	}
}

func TestRunAttachedReturnsPulumiFailureAfterProviderCleanup(t *testing.T) {
	for _, scenario := range []struct{ name, pulumiMode string }{
		{"nonzero", "failure"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			paths := attachedHelperPaths(t)
			cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
			err := runAttached(t.Context(), paths, []string{"up"}, append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE=ready", "SUB2API_ATTACHED_PULUMI_MODE="+scenario.pulumiMode, "SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
			if err == nil {
				t.Fatalf("Pulumi %s error = %v", scenario.name, err)
			}
			if data, err := os.ReadFile(cleanupLog); err != nil || string(data) != "closed\n" {
				t.Fatalf("provider cleanup after Pulumi %s = %q, %v", scenario.name, data, err)
			}
		})
	}
}

func TestRunAttachedCancellationStopsPulumiAndProviderAfterPulumiStarts(t *testing.T) {
	paths := attachedHelperPaths(t)
	ready := filepath.Join(t.TempDir(), "pulumi-ready")
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	if err := syscall.Mkfifo(ready, 0o600); err != nil {
		t.Fatal(err)
	}
	readyRead, err := os.OpenFile(ready, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runAttached(ctx, paths, []string{"up"}, append(os.Environ(), "SUB2API_ATTACHED_PROVIDER_MODE=ready", "SUB2API_ATTACHED_PULUMI_MODE=blocked", "SUB2API_ATTACHED_PULUMI_READY="+ready, "SUB2API_ATTACHED_CLEANUP_LOG="+cleanupLog), io.Discard, io.Discard, func(context.Context, hostcontract.ApprovalSubject) bool { return false })
	}()
	if one := make([]byte, 1); func() int { n, err := readyRead.Read(one); if err != nil { t.Fatal(err) }; return n }() != 1 {
		t.Fatal("Pulumi did not signal startup")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled attached run = %v", err)
	}
	if data, err := os.ReadFile(cleanupLog); err != nil || string(data) != "pulumi-closed\nclosed\n" && string(data) != "closed\npulumi-closed\n" {
		t.Fatalf("cancellation cleanup = %q, %v", data, err)
	}
}

func attachedHelperPaths(t *testing.T) attachedExecutables {
	t.Helper()
	dir := t.TempDir()
	provider := filepath.Join(dir, "pulumi-resource-sub2api-host")
	pulumi := filepath.Join(dir, "pulumi")
	writeAttachedExecutable(t, provider, `#!/bin/sh
if [ "$SUB2API_HOST_APPROVAL_FD" != 3 ] || [ ! -e /proc/self/fd/3 ]; then
  printf '%s\n' '`+attachedCanary+`' >&2
  exit 20
fi
case "$SUB2API_ATTACHED_PROVIDER_MODE" in
  failure) printf '%s\n' '`+attachedCanary+`' >&2; exit 22 ;;
  malformed) printf '%s\n' 'not-a-port'; exit 0 ;;
  oversize) i=0; while [ "$i" -lt 1025 ]; do printf 9; i=$((i + 1)); done; printf '\n'; exit 0 ;;
  oversize-no-newline) i=0; while [ "$i" -lt 1025 ]; do printf 9; i=$((i + 1)); done; cat <&3 >/dev/null; printf '%s\n' closed >> "$SUB2API_ATTACHED_CLEANUP_LOG"; exit 0 ;;
  blocked) cat <&3 >/dev/null; printf '%s\n' closed >> "$SUB2API_ATTACHED_CLEANUP_LOG"; exit 0 ;;
  exit-after-port) printf '%s\n' 43123; exit 0 ;;
  exit-after-pulumi-ready) printf '%s\n' 43123; dd if="$SUB2API_ATTACHED_PULUMI_READY" of=/dev/null bs=1 count=1 2>/dev/null; exit 0 ;;
  close-approval) printf '%s\n' 43123; exec 3>&-; while :; do :; done ;;
  close-approval-before-port) exec 3>&-; trap 'printf "%s\\n" closed >> "$SUB2API_ATTACHED_CLEANUP_LOG"; exit 0' INT TERM; while :; do :; done ;;
  close-approval-after-port) printf '%s\n' 43123; read _ < "$SUB2API_ATTACHED_PROVIDER_RELEASE"; exec 3>&-; trap 'printf x > "$SUB2API_ATTACHED_PROVIDER_EXITED"' EXIT; exit 0 ;;
  cleanup-hold) printf '%s\n' 43123; cat <&3 >/dev/null; printf x > "$SUB2API_ATTACHED_PROVIDER_HOLDING"; trap '' INT; read _ < "$SUB2API_ATTACHED_PROVIDER_RELEASE"; printf '%s\n' closed >> "$SUB2API_ATTACHED_CLEANUP_LOG"; exit 0 ;;
esac
printf '%s\n' 43123
cat <&3 >/dev/null
printf '%s\n' closed >> "$SUB2API_ATTACHED_CLEANUP_LOG"
`)
	writeAttachedExecutable(t, pulumi, `#!/bin/sh
fd3=no-fd3
if [ -e /proc/self/fd/3 ]; then fd3=fd3-inherited; fi
for arg; do printf '<%s>' "$arg"; done > "$SUB2API_ATTACHED_PULUMI_LOG"
printf '\n' >> "$SUB2API_ATTACHED_PULUMI_LOG"
printf '%s\n' "$PULUMI_DEBUG_PROVIDERS" >> "$SUB2API_ATTACHED_PULUMI_LOG"
printf '%s\n' "$fd3" >> "$SUB2API_ATTACHED_PULUMI_LOG"
case "$SUB2API_ATTACHED_PULUMI_MODE" in
  failure) exit 23 ;;
  blocked) if [ -n "$SUB2API_ATTACHED_PULUMI_READY" ]; then printf x > "$SUB2API_ATTACHED_PULUMI_READY"; fi; trap 'printf "%s\\n" pulumi-closed >> "$SUB2API_ATTACHED_CLEANUP_LOG"; exit 0' INT TERM; while :; do :; done ;;
  release-success) printf x > "$SUB2API_ATTACHED_PULUMI_READY"; read _ < "$SUB2API_ATTACHED_PULUMI_RELEASE"; exit 0 ;;
  success-ready) printf x > "$SUB2API_ATTACHED_PULUMI_EXITED"; exit 0 ;;
esac
`)
	return attachedExecutables{provider: provider, pulumi: pulumi}
}

func writeAttachedExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

type attachedPipe struct {
	name string
	file *os.File
}

func attachedFIFO(t *testing.T, name string) attachedPipe {
	t.Helper()
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return attachedPipe{name: name, file: file}
}

func attachedReceive(t *testing.T, pipe attachedPipe) {
	t.Helper()
	var byte [1]byte
	if _, err := pipe.file.Read(byte[:]); err != nil {
		t.Fatal(err)
	}
}

func attachedSend(t *testing.T, pipe attachedPipe) {
	t.Helper()
	if _, err := pipe.file.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
}
