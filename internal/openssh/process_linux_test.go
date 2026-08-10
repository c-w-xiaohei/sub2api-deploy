//go:build linux

package openssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestFinishResultBoundsClientDiagnosticAndClassifiesHostKey(t *testing.T) {
	log, err := os.CreateTemp(t.TempDir(), "ssh-log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if err := log.Chmod(0600); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write(append([]byte("Host key verification failed\n"), append(bytes.Repeat([]byte("x"), maxClientDiagnostic), []byte("CLIENT-LOG-CANARY")...)...)); err != nil {
		t.Fatal(err)
	}
	result := finishResult(errors.New("ssh failed"), nil, nil, log)
	if !result.hostKey || len(result.clientDiagnostic) != maxClientDiagnostic || bytes.Contains([]byte(result.clientDiagnostic), []byte("CLIENT-LOG-CANARY")) {
		t.Fatalf("result = %#v", result)
	}
}

func TestSystemStartCancellationKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := systemStart(ctx, "sh", []string{"-c", "sleep 30 & child=$!; printf '%s' $child > \"$1\"; wait", "sh", pidFile}, nil)
	if result.err != context.DeadlineExceeded {
		t.Fatalf("err = %v", result.err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for err = syscall.Kill(pid, 0); err == nil && time.Now().Before(deadline); err = syscall.Kill(pid, 0) {
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil {
		t.Fatalf("child process %d survived cancellation", pid)
	}
}
