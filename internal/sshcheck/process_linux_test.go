//go:build linux

package sshcheck

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestSystemRunCancellationKillsProxyCommandProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "proxy.pid")
	config := filepath.Join(dir, "config")
	proxy := filepath.Join(dir, "proxy")
	if err := os.WriteFile(proxy, []byte("#!/bin/sh\nprintf '%s' $$ > \"$1\"\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("Host local\n  HostName 127.0.0.1\n  ProxyCommand "+proxy+" "+pidFile+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- systemRun(ctx, "ssh", []string{"-F", config, "-o", "BatchMode=yes", "local", "true"})
	}()

	var pid int
	readyBy := time.Now().Add(5 * time.Second)
	for pid == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(string(b))
		}
		select {
		case err := <-done:
			t.Fatalf("ssh exited before ProxyCommand started: %v", err)
		default:
		}
		if time.Now().After(readyBy) {
			cancel()
			<-done
			t.Fatal("ProxyCommand did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	until := time.Now().Add(time.Second)
	var err error
	for err = syscall.Kill(pid, 0); err == nil && time.Now().Before(until); err = syscall.Kill(pid, 0) {
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil {
		t.Fatalf("proxy %d survived", pid)
	}
}
