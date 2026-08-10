//go:build linux

package openssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoopbackStrictKnownHostAndOptionTerminator(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("sshd is unavailable; loopback host-key evidence is not available")
	}
	dir := t.TempDir()
	port := reservePort(t)
	user, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "client")
	runTool(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	hostKey := filepath.Join(dir, "host")
	runTool(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
	publicClient, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), publicClient, 0600); err != nil {
		t.Fatal(err)
	}
	hostPublic := strings.TrimSpace(string(runTool(t, "ssh-keygen", "-y", "-f", hostKey)))
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(fmt.Sprintf("[127.0.0.1]:%d %s\n", port, hostPublic)), 0600); err != nil {
		t.Fatal(err)
	}
	originalKnownHosts, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "sshd_config")
	contents := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\nPidFile %s\nUsePAM no\nPasswordAuthentication no\nChallengeResponseAuthentication no\nPermitRootLogin no\nStrictModes no\nLogLevel ERROR\n", port, hostKey, filepath.Join(dir, "authorized_keys"), filepath.Join(dir, "sshd.pid"))
	if err := os.WriteFile(config, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	server := exec.Command(sshd, "-D", "-e", "-f", config)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	waitListening(t, port)
	include := filepath.Join(dir, "included")
	if err := os.WriteFile(include, []byte(fmt.Sprintf("Host loopback\n  HostName 127.0.0.1\n  Port %d\n  User %s\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n", port, user.Username, key, knownHosts)), 0600); err != nil {
		t.Fatal(err)
	}
	sshConfig := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(sshConfig, []byte("Include "+include+"\nMatch host loopback\n  BatchMode yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-F", sshConfig}, sshArgs("loopback", Host, "true")...)
	if result := systemStart(context.Background(), "ssh", args, nil); result.err != nil {
		t.Fatalf("known host connection failed: %v (%s)", result.err, result.stderr)
	}
	if after, err := os.ReadFile(knownHosts); err != nil || !bytes.Equal(after, originalKnownHosts) {
		t.Fatalf("known_hosts changed: %q, %v", after, err)
	}
	if err := os.WriteFile(knownHosts, nil, 0600); err != nil {
		t.Fatal(err)
	}
	emptyKnownHosts, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if result := systemStart(context.Background(), "ssh", args, nil); result.err == nil || !result.hostKey {
		t.Fatalf("unknown host key was accepted: %#v", result)
	} else if mapped := processFailure(result); !errors.Is(mapped, ErrHostKey) {
		t.Fatalf("unknown host key mapping = %v", mapped)
	} else {
		var process *ProcessError
		if !errors.As(mapped, &process) || !process.HostKey {
			t.Fatalf("unknown host key process error = %#v", mapped)
		}
	}
	if after, err := os.ReadFile(knownHosts); err != nil || !bytes.Equal(after, emptyKnownHosts) {
		t.Fatalf("unknown host changed known_hosts: %q, %v", after, err)
	}
	if err := os.WriteFile(knownHosts, []byte(fmt.Sprintf("[127.0.0.1]:%d ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n", port)), 0600); err != nil {
		t.Fatal(err)
	}
	wrongKnownHosts, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if result := systemStart(context.Background(), "ssh", args, nil); result.err == nil || !result.hostKey {
		t.Fatalf("changed host key was accepted: %#v", result)
	}
	if after, err := os.ReadFile(knownHosts); err != nil || !bytes.Equal(after, wrongKnownHosts) {
		t.Fatalf("changed host changed known_hosts: %q, %v", after, err)
	}
}

func reservePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitListening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sshd did not listen")
}

func runTool(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, output)
	}
	return output
}
