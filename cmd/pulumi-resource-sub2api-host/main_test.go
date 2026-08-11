//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostapproval"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

func TestApprovalFromFD3ReturnsUnavailableWithoutFixedMarker(t *testing.T) {
	t.Setenv("SUB2API_HOST_APPROVAL_FD", "")
	approve, closeApproval := approvalFromFD3()
	if closeApproval != nil {
		t.Fatal("absent marker returned a process-scoped channel cleanup")
	}
	if got, err := approve(t.Context(), approvalSubject()); got != nil || err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unmarked approval = %#v, %v", got, err)
	}

	t.Setenv("SUB2API_HOST_APPROVAL_FD", "4")
	approve, closeApproval = approvalFromFD3()
	if closeApproval != nil {
		t.Fatal("arbitrary descriptor marker was accepted")
	}
	if got, err := approve(t.Context(), approvalSubject()); got != nil || err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("non-fixed approval marker = %#v, %v", got, err)
	}
}

func TestApprovalFromFD3UsesExactSubjectAndClosesWithProviderProcess(t *testing.T) {
	if os.Getenv("SUB2API_PROVIDER_APPROVAL_HELPER") == "1" {
		approve, closeApproval := approvalFromFD3()
		switch os.Getenv("SUB2API_PROVIDER_APPROVAL_MODE") {
		case "non-socket":
			if closeApproval != nil || !fdClosed(3) {
				fmt.Fprintln(os.Stderr, "non-socket FD 3 was accepted or left open")
				os.Exit(5)
			}
			return
		case "exec-child":
			if closeApproval == nil {
				fmt.Fprintln(os.Stderr, "missing approval channel")
				os.Exit(6)
			}
			defer closeApproval()
			child := exec.Command("/bin/sh", "-c", `test -z "$SUB2API_HOST_APPROVAL_FD" && test ! -e /proc/self/fd/3`)
			child.Env = os.Environ()
			if output, err := child.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "provider child inherited approval state: %v %q\n", err, output)
				os.Exit(7)
			}
			return
		}
		if os.Getenv("SUB2API_PROVIDER_APPROVAL_MODE") == "missing" {
			got, err := approve(context.Background(), approvalSubject())
			if closeApproval != nil || got != nil || err == nil || !strings.Contains(err.Error(), "unavailable") {
				fmt.Fprintln(os.Stderr, "fixed marker accepted missing descriptor")
				os.Exit(4)
			}
			return
		}
		if closeApproval == nil {
			fmt.Fprintln(os.Stderr, "missing fixed approval channel")
			os.Exit(2)
		}
		defer closeApproval()
		got, err := approve(context.Background(), approvalSubject())
		if err != nil || got == nil || *got != approvalSubject() {
			fmt.Fprintln(os.Stderr, "approval did not round-trip exactly")
			os.Exit(3)
		}
		return
	}

	parent, child := approvalSocketpair(t)
	defer parent.Close()
	serverDone := make(chan error, 1)
	server := hostapproval.NewServer(func(_ context.Context, got hostcontract.ApprovalSubject) bool {
		if got != approvalSubject() {
			t.Errorf("server subject = %#v, want %#v", got, approvalSubject())
			return false
		}
		return true
	})
	go func() { serverDone <- server.Serve(t.Context(), parent) }()

	cmd := exec.Command(os.Args[0], "-test.run=TestApprovalFromFD3UsesExactSubjectAndClosesWithProviderProcess")
	cmd.Env = append(os.Environ(), "SUB2API_PROVIDER_APPROVAL_HELPER=1", "SUB2API_HOST_APPROVAL_FD=3")
	cmd.ExtraFiles = []*os.File{child}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("provider helper = %v, output=%q", err, output)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("approval server did not observe process-scoped EOF: %v", err)
	}
}

func fdClosed(fd int) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	return errno != 0
}

func TestApprovalFromFD3RejectsFixedMarkerWithoutDescriptor(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestApprovalFromFD3UsesExactSubjectAndClosesWithProviderProcess")
	cmd.Env = append(os.Environ(), "SUB2API_PROVIDER_APPROVAL_HELPER=1", "SUB2API_PROVIDER_APPROVAL_MODE=missing", "SUB2API_HOST_APPROVAL_FD=3")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provider helper accepted missing FD 3: %v, output=%q", err, output)
	}
}

func TestApprovalFromFD3RejectsAndClosesNonSocketFD3(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-a-socket")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.Command(os.Args[0], "-test.run=TestApprovalFromFD3UsesExactSubjectAndClosesWithProviderProcess")
	cmd.Env = append(os.Environ(), "SUB2API_PROVIDER_APPROVAL_HELPER=1", "SUB2API_PROVIDER_APPROVAL_MODE=non-socket", "SUB2API_HOST_APPROVAL_FD=3")
	cmd.ExtraFiles = []*os.File{file}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provider helper accepted non-socket FD 3: %v, output=%q", err, output)
	}
}

func TestApprovalFromFD3DoesNotLeakMarkerOrFD3AcrossProviderChildExec(t *testing.T) {
	parent, child := approvalSocketpair(t)
	defer parent.Close()
	defer child.Close()
	cmd := exec.Command(os.Args[0], "-test.run=TestApprovalFromFD3UsesExactSubjectAndClosesWithProviderProcess")
	cmd.Env = append(os.Environ(), "SUB2API_PROVIDER_APPROVAL_HELPER=1", "SUB2API_PROVIDER_APPROVAL_MODE=exec-child", "SUB2API_HOST_APPROVAL_FD=3")
	cmd.ExtraFiles = []*os.File{child}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provider child inherited approval state: %v, output=%q", err, output)
	}
}

func approvalSocketpair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(fds[0]), "approval-parent"), os.NewFile(uintptr(fds[1]), "approval-child")
}

func approvalSubject() hostcontract.ApprovalSubject {
	return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: "prod", Resource: hostcontract.ResourceIdentity{Environment: "prod", ServerKey: "edge"}, Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PreserveData: true}
}
