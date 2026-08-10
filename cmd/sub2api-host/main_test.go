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
	if _, err := hostprotocol.DecodeRequest(frame); err != nil {
		t.Fatalf("test request is invalid: %v", err)
	}
	var out bytes.Buffer
	if err := serve(&out, bytes.NewReader(frame), rt); err != nil {
		t.Fatal(err)
	}
	response, err := hostprotocol.DecodeResponse(out.Bytes())
	if err != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorTransport || response.Error.Code != hostprotocol.CodeUnavailable {
		t.Fatalf("missing state response = %#v, %v", response, err)
	}
	write := request
	write.Action = hostcontract.ActionReconcile
	write.PriorAppliedRevision = revision
	write.Target = &hostcontract.Target{ReleaseArtifact: "release"}
	write.Secrets = &hostcontract.Secrets{}
	frame, err = hostprotocol.EncodeRequest(write)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := serve(&out, bytes.NewReader(frame), rt); err != nil {
		t.Fatal(err)
	}
	response, err = hostprotocol.DecodeResponse(out.Bytes())
	if err != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorRecoveryRequired || response.Error.Code != hostprotocol.CodeRecoveryRequired {
		t.Fatalf("write response = %#v, %v", response, err)
	}
	out.Reset()
	if err := serve(&out, bytes.NewReader(append(frame, frame...)), rt); err == nil {
		t.Fatal("two frames accepted")
	}
	if response, err := hostprotocol.DecodeResponse(out.Bytes()); err != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorProtocol {
		t.Fatalf("two frame response = %#v, %v", response, err)
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
	cmd := command(frame)
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := hostprotocol.EncodeResponse(hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorTransport, Code: hostprotocol.CodeUnavailable}})
	if err != nil || !bytes.Equal(output, expected) {
		t.Fatalf("one-frame output = %q, expected %q, %v", output, expected, err)
	}
	bad := command(append(frame, frame...))
	output, err = bad.Output()
	if err == nil {
		t.Fatal("process accepted two frames")
	}
	response, decodeErr := hostprotocol.DecodeResponse(output)
	if decodeErr != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorProtocol || !bytes.Equal(output, mustResponse(t, hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}})) {
		t.Fatalf("two-frame output = %q, %#v, %v", output, response, decodeErr)
	}
}

func mustResponse(t *testing.T, response hostprotocol.Response) []byte {
	t.Helper()
	frame, err := hostprotocol.EncodeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
