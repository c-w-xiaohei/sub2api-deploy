//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/pkg/term/termios"
)

func TestTerminalApprovalAdapterRequiresTerminalAndAcceptsExactPTYChallenge(t *testing.T) {
	if terminalApprovalFromPath(context.Background(), approvalTestSubject(), "/dev/null") {
		t.Fatal("non-terminal approval input was accepted")
	}
	master, slave, err := termios.Pty()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	path := slave.Name()
	canonical, err := json.Marshal(approvalTestSubject())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if _, err := master.Write([]byte("APPROVE " + hex.EncodeToString(digest[:]) + "\n")); err != nil {
		t.Fatal(err)
	}
	if !terminalApprovalFromPath(context.Background(), approvalTestSubject(), path) {
		t.Fatal("exact approval challenge from a terminal was denied")
	}
}

func TestTerminalApprovalRequiresExactCanonicalChallenge(t *testing.T) {
	subject := approvalTestSubject()
	canonical, err := json.Marshal(subject)
	if err != nil {
		t.Fatal("could not encode approval fixture")
	}
	digest := sha256.Sum256(canonical)
	token := hex.EncodeToString(digest[:])
	payload := base64.RawURLEncoding.EncodeToString(canonical)

	var output bytes.Buffer
	approved := terminalApprovalDecision(context.Background(), subject, strings.NewReader("APPROVE "+token+"\n"), &output)
	if !approved {
		t.Fatal("exact approval challenge was denied")
	}
	text := output.String()
	if !strings.Contains(text, "subject-base64url: "+payload+"\n") || !strings.Contains(text, "Type APPROVE "+token+" to approve: ") {
		t.Fatal("approval prompt omitted canonical subject or challenge")
	}
	for _, raw := range []string{subject.Environment, subject.Resource.ServerKey, subject.AppID, subject.OldData.Endpoint, subject.NewData.Endpoint} {
		if strings.Contains(text, raw) {
			t.Fatal("approval prompt exposed an unescaped subject field")
		}
	}
}

func TestTerminalApprovalDeniesAnythingExceptExactLine(t *testing.T) {
	subject := approvalTestSubject()
	canonical, err := json.Marshal(subject)
	if err != nil {
		t.Fatal("could not encode approval fixture")
	}
	digest := sha256.Sum256(canonical)
	token := hex.EncodeToString(digest[:])
	for _, input := range []string{
		"",
		"APPROVE " + token,
		"approve " + token + "\n",
		" APPROVE " + token + "\n",
		"APPROVE " + token + " \n",
		"APPROVE " + strings.Repeat("a", 256) + "\n",
		"NO\n",
	} {
		var output bytes.Buffer
		if terminalApprovalDecision(context.Background(), subject, strings.NewReader(input), &output) {
			t.Fatal("non-exact approval input was accepted")
		}
	}
}

func TestTerminalApprovalFailsClosedForInvalidContextSubjectAndWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if terminalApprovalDecision(ctx, approvalTestSubject(), strings.NewReader("anything\n"), &bytes.Buffer{}) {
		t.Fatal("cancelled approval was accepted")
	}
	if terminalApprovalDecision(context.Background(), hostcontract.ApprovalSubject{}, strings.NewReader("anything\n"), &bytes.Buffer{}) {
		t.Fatal("invalid approval subject was accepted")
	}
	if terminalApprovalDecision(context.Background(), approvalTestSubject(), strings.NewReader("anything\n"), failingApprovalWriter{}) {
		t.Fatal("approval with failed prompt output was accepted")
	}
}

func TestTerminalApprovalPromptDoesNotEmitControlCharacterFields(t *testing.T) {
	subject := approvalTestSubject()
	subject.Resource.ServerKey = "server\n\x1b[31mAPPROVE"
	var output bytes.Buffer
	_ = terminalApprovalDecision(context.Background(), subject, strings.NewReader("NO\n"), &output)
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "server\n") {
		t.Fatal("approval prompt emitted terminal control characters")
	}
}

type failingApprovalWriter struct{}

func (failingApprovalWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func approvalTestSubject() hostcontract.ApprovalSubject {
	resource := hostcontract.ResourceIdentity{Environment: "production", ServerKey: "server-a"}
	return hostcontract.ApprovalSubject{
		Kind:           hostcontract.ApprovalDataLink,
		Environment:    "production",
		Resource:       resource,
		AppID:          "api",
		DataKind:       "postgres",
		OldData:        hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old", Endpoint: "old.example", Port: 5432, Database: "app", TLSServerName: "old.example"},
		NewData:        hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new", Endpoint: "new.example", Port: 5432, Database: "app", TLSServerName: "new.example"},
		TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}
