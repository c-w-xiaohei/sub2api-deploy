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

func TestSystemRunKillsProxyCommandProcessGroupOnDeadline(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := systemRun(ctx, "ssh", []string{"-F", config, "-o", "BatchMode=yes", "local", "true"})
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v", err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Second)
	for err = syscall.Kill(pid, 0); err == nil && time.Now().Before(until); err = syscall.Kill(pid, 0) {
		time.Sleep(10 * time.Millisecond)
	}
	if err == nil {
		t.Fatalf("proxy %d survived", pid)
	}
}
