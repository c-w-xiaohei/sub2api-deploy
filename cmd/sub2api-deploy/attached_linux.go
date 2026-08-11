//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostapproval"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

const attachedPortLimit = 64

type attachedExecutables struct {
	provider string
	pulumi   string
}

type attachedCompletion struct {
	done chan struct{}
	err  error
}

func resolveAttachedExecutables(cliPath string) (attachedExecutables, error) {
	dir := filepath.Dir(cliPath)
	paths := attachedExecutables{
		provider: filepath.Join(dir, "pulumi-resource-sub2api-host"),
		pulumi:   filepath.Join(dir, "pulumi"),
	}
	for _, path := range []string{paths.provider, paths.pulumi} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return attachedExecutables{}, fmt.Errorf("attached executable is unavailable")
		}
	}
	return paths, nil
}

func attachedSocketpair() (*os.File, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "sub2api-host-approval-parent"), os.NewFile(uintptr(fds[1]), "sub2api-host-approval-child"), nil
}

func runAttached(ctx context.Context, paths attachedExecutables, args, env []string, stdout, stderr io.Writer, decide func(context.Context, hostcontract.ApprovalSubject) bool) error {
	if ctx == nil || decide == nil {
		return fmt.Errorf("attached execution is unavailable")
	}
	parent, child, err := attachedSocketpair()
	if err != nil {
		return fmt.Errorf("attached execution failed")
	}
	defer parent.Close()
	defer child.Close()
	server := &attachedCompletion{done: make(chan struct{})}
	go func() {
		server.err = hostapproval.NewServer(decide).Serve(ctx, parent)
		close(server.done)
	}()

	output, outputWriter, err := os.Pipe()
	if err != nil {
		_ = parent.Close()
		<-server.done
		return fmt.Errorf("attached execution failed")
	}
	defer output.Close()
	provider := exec.Command(paths.provider)
	provider.Env, provider.Stdout, provider.Stderr = attachedProviderEnv(env), outputWriter, io.Discard
	provider.ExtraFiles = []*os.File{child}
	if err := provider.Start(); err != nil {
		_ = outputWriter.Close()
		_ = parent.Close()
		<-server.done
		return fmt.Errorf("attached provider failed to start")
	}
	_ = outputWriter.Close()
	_ = child.Close()
	providerDone := waitAttached(provider)

	reader := bufio.NewReaderSize(output, attachedPortLimit)
	type portResult struct {
		port string
		err  error
	}
	portReady := make(chan portResult, 1)
	readerDone := make(chan struct{})
	go func() {
		port, err := readAttachedPort(reader)
		portReady <- portResult{port, err}
		if err == nil {
			_, _ = io.Copy(io.Discard, reader)
		}
		close(readerDone)
	}()

	var port string
	for {
		if terminal := attachedTerminal(ctx, providerDone, server); terminal != nil {
			cleanupAttached(provider, providerDone, parent, output, readerDone, server)
			return terminal
		}
		select {
		case result := <-portReady:
			if terminal := attachedTerminal(ctx, providerDone, server); terminal != nil {
				cleanupAttached(provider, providerDone, parent, output, readerDone, server)
				return terminal
			}
			if result.err != nil {
				cleanupAttached(provider, providerDone, parent, output, readerDone, server)
				return fmt.Errorf("attached provider failed to start")
			}
			port = result.port
		case <-providerDone.done:
		case <-server.done:
		case <-ctx.Done():
		}
		if port != "" {
			break
		}
	}
	if terminal := attachedTerminal(ctx, providerDone, server); terminal != nil {
		cleanupAttached(provider, providerDone, parent, output, readerDone, server)
		return terminal
	}
	pulumi := exec.Command(paths.pulumi, args...)
	pulumi.Env, pulumi.Stdout, pulumi.Stderr = attachedPulumiEnv(env, port), stdout, stderr
	if err := pulumi.Start(); err != nil {
		cleanupAttached(provider, providerDone, parent, output, readerDone, server)
		return fmt.Errorf("pulumi failed")
	}
	pulumiDone := waitAttached(pulumi)
	select {
	case <-pulumiDone.done:
		if terminal := attachedTerminal(ctx, providerDone, server); terminal != nil {
			cleanupAttached(provider, providerDone, parent, output, readerDone, server)
			return terminal
		}
		cleanupAttached(provider, providerDone, parent, output, readerDone, server)
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if pulumiDone.err != nil {
			return fmt.Errorf("pulumi failed")
		}
		return nil
	case <-providerDone.done:
	case <-server.done:
	case <-ctx.Done():
	}
	stopAttachedProcess(pulumi, pulumiDone)
	cleanupAttached(provider, providerDone, parent, output, readerDone, server)
	return attachedTerminal(ctx, providerDone, server)
}

func attachedTerminal(ctx context.Context, provider, server *attachedCompletion) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	select {
	case <-provider.done:
		return fmt.Errorf("attached provider failed")
	default:
	}
	select {
	case <-server.done:
		return fmt.Errorf("attached approval failed")
	default:
		return nil
	}
}

func waitAttached(command *exec.Cmd) *attachedCompletion {
	result := &attachedCompletion{done: make(chan struct{})}
	go func() {
		result.err = command.Wait()
		close(result.done)
	}()
	return result
}

func cleanupAttached(provider *exec.Cmd, providerDone *attachedCompletion, parent, output *os.File, readerDone <-chan struct{}, server *attachedCompletion) {
	_ = parent.Close()
	stopAttachedProcess(provider, providerDone)
	_ = output.Close()
	<-readerDone
	<-server.done
}

func stopAttachedProcess(command *exec.Cmd, done *attachedCompletion) {
	select {
	case <-done.done:
		return
	case <-time.After(100 * time.Millisecond):
	}
	_ = command.Process.Signal(os.Interrupt)
	select {
	case <-done.done:
		return
	case <-time.After(time.Second):
	}
	select {
	case <-done.done:
		return
	default:
		_ = command.Process.Kill()
	}
	<-done.done
}

func readAttachedPort(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) < 2 || len(line) >= attachedPortLimit {
		return "", fmt.Errorf("invalid provider port")
	}
	port := string(line[:len(line)-1])
	if len(port) > 1 && port[0] == '0' {
		return "", fmt.Errorf("invalid provider port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("invalid provider port")
	}
	return port, nil
}

func attachedProviderEnv(env []string) []string {
	result := make([]string, 0, len(env)+1)
	for _, value := range env {
		if !strings.HasPrefix(value, "SUB2API_HOST_APPROVAL_FD=") {
			result = append(result, value)
		}
	}
	return append(result, "SUB2API_HOST_APPROVAL_FD=3")
}

func attachedPulumiEnv(env []string, port string) []string {
	result := make([]string, 0, len(env)+1)
	lastDebug := ""
	for _, value := range env {
		if strings.HasPrefix(value, "SUB2API_HOST_APPROVAL_FD=") {
			continue
		}
		if strings.HasPrefix(value, "PULUMI_DEBUG_PROVIDERS=") {
			lastDebug = strings.TrimPrefix(value, "PULUMI_DEBUG_PROVIDERS=")
			continue
		}
		result = append(result, value)
	}
	entries := make([]string, 0)
	for _, entry := range strings.Split(lastDebug, ",") {
		if entry != "" && strings.SplitN(entry, ":", 2)[0] != "sub2api-host" {
			entries = append(entries, entry)
		}
	}
	entries = append(entries, "sub2api-host:"+port)
	return append(result, "PULUMI_DEBUG_PROVIDERS="+strings.Join(entries, ","))
}
