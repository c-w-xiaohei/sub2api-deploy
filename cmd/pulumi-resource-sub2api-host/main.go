package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostapproval"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprovider"
)

var version = "0.0.0-dev"

func main() {
	approve, closeApproval := approvalFromFD3()
	if closeApproval != nil {
		defer closeApproval()
	}
	if err := hostprovider.NewWithApproval(version, approve).Run(context.Background(), "sub2api-host", version); err != nil {
		log.Fatal(err)
	}
}

func approvalFromFD3() (func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error), func()) {
	unavailable := func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error) {
		return nil, fmt.Errorf("approval channel is unavailable")
	}
	defer os.Unsetenv("SUB2API_HOST_APPROVAL_FD")
	if os.Getenv("SUB2API_HOST_APPROVAL_FD") != "3" {
		return unavailable, nil
	}
	file := os.NewFile(uintptr(3), "sub2api-host-approval")
	if file == nil {
		return unavailable, nil
	}
	rejectApproval := func() (func(context.Context, hostcontract.ApprovalSubject) (*hostcontract.ApprovalSubject, error), func()) {
		_ = file.Close()
		return unavailable, nil
	}
	kind, err := syscall.GetsockoptInt(3, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || kind != syscall.SOCK_STREAM {
		return rejectApproval()
	}
	peer, err := syscall.Getpeername(3)
	if err != nil {
		return rejectApproval()
	}
	if _, ok := peer.(*syscall.SockaddrUnix); !ok {
		return rejectApproval()
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, 3, syscall.F_GETFD, 0)
	if errno != 0 {
		return rejectApproval()
	}
	if _, _, errno = syscall.Syscall(syscall.SYS_FCNTL, 3, syscall.F_SETFD, flags|syscall.FD_CLOEXEC); errno != 0 {
		return rejectApproval()
	}
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return unavailable, nil
	}
	client := hostapproval.NewClient(conn)
	return client.Approve, func() { _ = conn.Close() }
}
