//go:build linux

package openssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

const maxClientDiagnostic = 16 << 10

func systemStart(ctx context.Context, name string, args []string, stdin []byte) processResult {
	var clientLog *os.File
	if name == "ssh" {
		var err error
		clientLog, err = os.CreateTemp("", "sub2api-ssh-*")
		if err != nil {
			return processResult{err: err}
		}
		if err := clientLog.Chmod(0600); err != nil {
			_ = clientLog.Close()
			_ = os.Remove(clientLog.Name())
			return processResult{err: err}
		}
		defer func() { _ = clientLog.Close(); _ = os.Remove(clientLog.Name()) }()
		args = withClientLog(args, clientLog.Name())
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A failed ssh leader can leave ProxyCommand children holding its pipes.
	// Bound that wait so the process group can always be reclaimed.
	cmd.WaitDelay = 100 * time.Millisecond
	stdout := limitedBuffer{limit: len(hostprotocol.Magic) + hostprotocol.MaxHeaderSize + hostprotocol.MaxFrameSize + 1}
	stderr := limitedBuffer{limit: maxStderr}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return processResult{stderr: stderr.Bytes(), err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return finishResult(err, stdout.Bytes(), stderr.Bytes(), clientLog)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return finishResult(ctx.Err(), stdout.Bytes(), stderr.Bytes(), clientLog)
	}
}

func withClientLog(args []string, path string) []string {
	for i, v := range args {
		if v == "--" {
			out := append([]string(nil), args[:i]...)
			out = append(out, "-E", path)
			return append(out, args[i:]...)
		}
	}
	return append([]string{"-E", path}, args...)
}
func finishResult(err error, stdout, stderr []byte, clientLog *os.File) processResult {
	r := processResult{stdout: stdout, stderr: stderr, err: err, exitCode: -1}
	if clientLog != nil {
		if _, e := clientLog.Seek(0, 0); e == nil {
			b, e := io.ReadAll(io.LimitReader(clientLog, maxClientDiagnostic+1))
			if e != nil {
				return r
			}
			if len(b) > maxClientDiagnostic {
				b = b[:maxClientDiagnostic]
			}
			r.clientDiagnostic = string(b)
			r.hostKey = trustedHostKey(r.clientDiagnostic)
		}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.exitCode = exit.ExitCode()
	}
	return r
}
func trustedHostKey(log string) bool {
	return strings.Contains(log, "Host key verification failed") || strings.Contains(log, "REMOTE HOST IDENTIFICATION HAS CHANGED") || strings.Contains(log, "host key is known")
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.Len(); remaining > 0 {
		if len(p) > remaining {
			_, _ = b.Buffer.Write(p[:remaining])
		} else {
			_, _ = b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
