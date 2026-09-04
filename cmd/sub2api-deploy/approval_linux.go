//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"golang.org/x/sys/unix"
)

const maxApprovalSubjectSize = 64 * 1024
const maxApprovalResponseSize = 74

func terminalApproval(ctx context.Context, subject hostcontract.ApprovalSubject) bool {
	return terminalApprovalFromPath(ctx, subject, "/dev/tty")
}

func terminalApprovalFromPath(ctx context.Context, subject hostcontract.ApprovalSubject, path string) bool {
	if ctx == nil || path == "" || ctx.Err() != nil {
		return false
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return false
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		return false
	}
	if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err != nil {
		return false
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = file.Close() })
	defer stopCancellation()
	return terminalApprovalDecision(ctx, subject, file, file)
}

func terminalApprovalDecision(ctx context.Context, subject hostcontract.ApprovalSubject, input io.Reader, output io.Writer) bool {
	if ctx == nil || input == nil || output == nil || ctx.Err() != nil || subject.Validate() != nil {
		return false
	}
	canonical, err := json.Marshal(subject)
	if err != nil || len(canonical) == 0 || len(canonical) > maxApprovalSubjectSize {
		return false
	}
	digest := sha256.Sum256(canonical)
	token := hex.EncodeToString(digest[:])
	prompt := "subject-base64url: " + base64.RawURLEncoding.EncodeToString(canonical) + "\nType APPROVE " + token + " to approve: "
	if _, err := io.WriteString(output, prompt); err != nil || ctx.Err() != nil {
		return false
	}
	line, err := bufio.NewReaderSize(input, maxApprovalResponseSize).ReadSlice('\n')
	if err != nil || ctx.Err() != nil {
		return false
	}
	return string(line) == "APPROVE "+token+"\n"
}
