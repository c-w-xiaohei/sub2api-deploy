package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
)

func TestStdioServesOneInspectFrameAndRejectsWrites(t *testing.T) {
	root := t.TempDir()
	machine := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machine, []byte("0123456789abcdef0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rt := hostruntime.New(filepath.Join(root, "state"), machine)
	resource := hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}
	revision := "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	request := hostprotocol.Request{Action: hostcontract.ActionInspect, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource, TargetRevision: revision}
	frame, err := hostprotocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := serve(&out, bytes.NewReader(frame), rt); err != nil {
		t.Fatal(err)
	}
	response, err := hostprotocol.DecodeResponse(out.Bytes())
	if err != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorTransport || response.Error.Code != hostprotocol.CodeUnavailable {
		t.Fatalf("missing state response = %#v, %v", response, err)
	}
	if err := serve(&out, bytes.NewReader(append(frame, frame...)), rt); err == nil {
		t.Fatal("two frames accepted")
	}
}

func TestStdioProcessExitsAfterOneFrameAndRejectsTwo(t *testing.T) {
	if os.Getenv("SUB2API_HOST_HELPER") == "1" {
		os.Args = []string{"sub2api-host", "stdio"}
		main()
		os.Exit(0)
	}
	request := hostprotocol.Request{Action: hostcontract.ActionInspect, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}, TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	frame, err := hostprotocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	command := func(input []byte) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestStdioProcessExitsAfterOneFrameAndRejectsTwo")
		cmd.Env = append(os.Environ(), "SUB2API_HOST_HELPER=1")
		cmd.Stdin = bytes.NewReader(input)
		return cmd
	}
	output, err := command(frame).Output()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := hostprotocol.EncodeResponse(hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorTransport, Code: hostprotocol.CodeUnavailable}})
	if err != nil || !bytes.Equal(output, expected) {
		t.Fatalf("one-frame output = %q, expected %q, %v", output, expected, err)
	}
	output, err = command(append(frame, frame...)).CombinedOutput()
	if err == nil {
		t.Fatal("process accepted two frames")
	}
	response, decodeErr := hostprotocol.DecodeResponse(output)
	if decodeErr != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorProtocol {
		t.Fatalf("two-frame output = %q, %#v, %v", output, response, decodeErr)
	}
}

func TestRemovedBootstrapCommandsAreRejected(t *testing.T) {
	if os.Getenv("SUB2API_HOST_INVALID_COMMAND_HELPER") == "1" {
		os.Args = []string{"sub2api-host", os.Getenv("SUB2API_HOST_INVALID_COMMAND")}
		main()
		os.Exit(0)
	}
	for _, command := range []string{"bootstrap-stdio", "install-attest"} {
		cmd := exec.Command(os.Args[0], "-test.run=TestRemovedBootstrapCommandsAreRejected")
		cmd.Env = append(os.Environ(), "SUB2API_HOST_INVALID_COMMAND_HELPER=1", "SUB2API_HOST_INVALID_COMMAND="+command)
		if err := cmd.Run(); err == nil {
			t.Fatalf("removed command %q was accepted", command)
		}
	}
}
