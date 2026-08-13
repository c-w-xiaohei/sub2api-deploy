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

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

const maxApprovalSubjectSize = 64 * 1024
const maxApprovalResponseSize = 74

func terminalApproval(ctx context.Context, subject hostcontract.ApprovalSubject) bool {
	return terminalApprovalFromPath(ctx, subject, "/dev/tty")
}

func terminalApprovalFromPath(context.Context, hostcontract.ApprovalSubject, string) bool {
	return false
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
