package main

import (
	"bytes"
	"io"
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

func TestBootstrapStdioProcessExitsAfterOneRejectedFrame(t *testing.T) {
	if os.Getenv("SUB2API_HOST_BOOTSTRAP_HELPER") == "1" {
		os.Args = []string{"sub2api-host", "bootstrap-stdio"}
		main()
		os.Exit(0)
	}
	request := hostprotocol.Request{Action: hostcontract.ActionInspect, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}, TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	frame, err := hostprotocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestBootstrapStdioProcessExitsAfterOneRejectedFrame")
	cmd.Env = append(os.Environ(), "SUB2API_HOST_BOOTSTRAP_HELPER=1")
	cmd.Stdin = bytes.NewReader(frame)
	attestation, err := os.CreateTemp(t.TempDir(), "attestation")
	if err != nil {
		t.Fatal(err)
	}
	defer attestation.Close()
	cmd.ExtraFiles = []*os.File{attestation}
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	response, err := hostprotocol.DecodeResponse(output)
	if err != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorRemoteOperation || response.Error.Code != hostprotocol.CodeOperationFailed || !bytes.Equal(output, mustResponse(t, hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorRemoteOperation, Code: hostprotocol.CodeOperationFailed}})) {
		t.Fatalf("bootstrap process output = %q, %#v, %v", output, response, err)
	}
}

func TestInstallAttestProcessUsesOnlyFD3(t *testing.T) {
	if os.Getenv("SUB2API_HOST_INSTALL_ATTEST_HELPER") == "1" {
		os.Args = []string{"sub2api-host", "install-attest"}
		main()
		os.Exit(0)
	}
	attestation, err := os.CreateTemp(t.TempDir(), "attestation")
	if err != nil { t.Fatal(err) }
	defer attestation.Close()
	cmd := exec.Command(os.Args[0], "-test.run=TestInstallAttestProcessUsesOnlyFD3")
	cmd.Env = append(os.Environ(), "SUB2API_HOST_INSTALL_ATTEST_HELPER=1")
	cmd.ExtraFiles = []*os.File{attestation}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil || len(stdout) != 0 || stderr.Len() != 0 { t.Fatalf("install attest output=%q stderr=%q err=%v", stdout, stderr.Bytes(), err) }
	if _, err := attestation.Seek(0, io.SeekStart); err != nil { t.Fatal(err) }
	got, err := io.ReadAll(attestation)
	if err != nil || string(got) != bootstrapAttestation { t.Fatalf("attestation=%q, %v", got, err) }
	missing := exec.Command(os.Args[0], "-test.run=TestInstallAttestProcessUsesOnlyFD3")
	missing.Env = append(os.Environ(), "SUB2API_HOST_INSTALL_ATTEST_HELPER=1")
	if err := missing.Run(); err == nil { t.Fatal("install attest accepted missing fd3") }
}

func TestBootstrapStdioServesOneReconcileFrameAndReturnsAppliedResult(t *testing.T) {
	root := t.TempDir()
	fakeDocker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fakeDocker, []byte(`#!/bin/sh
tab=$(printf '\t')
if [ "$1" = container ] && [ "$2" = ls ] && [ "$3" = --all ] && [ "$4" = --filter ] && [ "$5" = label=sub2api.host ] && [ "$6" = --format ] && [ "$7" = "{{.Names}}${tab}{{index .Labels \"sub2api.host\"}}" ] && [ "$#" = 7 ]; then exit 0; fi
if [ "$1" = network ] && [ "$2" = ls ] && [ "$3" = --filter ] && [ "$4" = label=sub2api.host ] && [ "$5" = --format ] && [ "$6" = "{{.Name}}${tab}{{index .Labels \"sub2api.host\"}}" ] && [ "$#" = 6 ]; then exit 0; fi
if [ "$1" = network ] && [ "$2" = ls ] && [ "$3" = --filter ] && [ "$4" != label=sub2api.host ] && [ "$5" = --format ] && [ "$6" = "{{.Name}}${tab}{{index .Labels \"sub2api.host\"}}${tab}{{index .Labels \"sub2api.host.network\"}}" ] && [ "$#" = 6 ]; then exit 0; fi
if [ "$1" = network ] && [ "$2" = create ] && [ "$3" = --label ] && [ "$4" != '' ] && [ "$5" = --label ] && [ "$6" != '' ] && [ "$7" != '' ] && [ "$#" = 7 ]; then exit 0; fi
exit 1
`), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fakeDocker)+":"+os.Getenv("PATH"))
	machine := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machine, []byte("0123456789abcdef0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rt := hostruntime.New(filepath.Join(root, "state"), machine)
	request := hostprotocol.Request{
		Action:               hostcontract.ActionReconcile,
		Server:               hostcontract.ServerTarget{SSHAlias: "edge"},
		Resource:             hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"},
		TargetRevision:       "tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PriorAppliedRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Target:               &hostcontract.Target{ReleaseArtifact: "release"},
		Secrets:              &hostcontract.Secrets{},
	}
	frame, err := hostprotocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	out := orderedBuffer{name: "stdout", events: &events}
	attestation := orderedBuffer{name: "attestation", events: &events}
	if err := bootstrapServe(&out, bytes.NewReader(frame), rt, &attestation); err != nil {
		t.Fatal(err)
	}
	response, err := hostprotocol.DecodeResponse(out.Bytes())
	if err != nil || response.Result == nil || response.Result.Status != hostprotocol.ResultApplied || response.Result.AppliedRevision != request.TargetRevision || response.Result.Observation != nil {
		t.Fatalf("bootstrap response = %#v, %v", response, err)
	}
	if got := attestation.String(); got != "sub2api-bootstrap-attested-v1" {
		t.Fatalf("bootstrap attestation = %q", got)
	}
	if len(events) != 2 || events[0] != "attestation" || events[1] != "stdout" {
		t.Fatalf("bootstrap writes = %v, want attestation then stdout", events)
	}
	if err := bootstrapServe(io.Discard, bytes.NewReader(frame), rt, errWriter{}); err == nil {
		t.Fatal("bootstrap accepted failed attestation write")
	}
	if err := bootstrapServe(errWriter{}, bytes.NewReader(frame), rt, io.Discard); err == nil {
		t.Fatal("bootstrap accepted failed response write")
	}
	_, err = os.ReadFile(filepath.Join(root, "state", "state.json"))
	if err != nil {
		t.Fatalf("bootstrap omitted state: %v", err)
	}

	out.Reset()
	attestation.Reset()
	if err := bootstrapServe(&out, bytes.NewReader(append(frame, frame...)), rt, &attestation); err == nil {
		t.Fatal("bootstrap accepted two frames")
	}
	if response, err := hostprotocol.DecodeResponse(out.Bytes()); err != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorProtocol || response.Error.Code != hostprotocol.CodeMalformedFrame {
		t.Fatalf("two bootstrap frames = %#v, %v", response, err)
	}
	if attestation.Len() != 0 {
		t.Fatalf("malformed bootstrap attestation = %q", attestation.Bytes())
	}
}

func TestBootstrapStdioRejectsInspectWithoutCreatingState(t *testing.T) {
	root := t.TempDir()
	machine := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machine, []byte("0123456789abcdef0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request := hostprotocol.Request{Action: hostcontract.ActionInspect, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}, TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	frame, err := hostprotocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := bootstrapServe(&out, bytes.NewReader(frame), hostruntime.New(filepath.Join(root, "state"), machine), io.Discard); err != nil {
		t.Fatal(err)
	}
	response, err := hostprotocol.DecodeResponse(out.Bytes())
	if err != nil || response.Error == nil || response.Error.Category != hostprotocol.ErrorRemoteOperation || response.Error.Code != hostprotocol.CodeOperationFailed {
		t.Fatalf("inspect bootstrap response = %#v, %v", response, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "state")); !os.IsNotExist(err) {
		t.Fatalf("inspect bootstrap created state: %v", err)
	}
}

func TestBootstrapServeDoesNotAttestErrorsAndServeCannotAttest(t *testing.T) {
	root := t.TempDir()
	machine := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machine, []byte("0123456789abcdef0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string][]byte{
		"operation": mustRequest(t, hostprotocol.Request{Action: hostcontract.ActionInspect, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}, TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}),
		"malformed": []byte("not a request"),
	} {
		t.Run(name, func(t *testing.T) {
			var out, attestation bytes.Buffer
			err := bootstrapServe(&out, bytes.NewReader(input), hostruntime.New(filepath.Join(root, name), machine), &attestation)
			if name == "malformed" && err == nil {
				t.Fatal("malformed request returned nil")
			}
			response, decodeErr := hostprotocol.DecodeResponse(out.Bytes())
			if decodeErr != nil || response.Error == nil || attestation.Len() != 0 {
				t.Fatalf("response = %#v, decode = %v, attestation = %q", response, decodeErr, attestation.Bytes())
			}
		})
	}
	var out bytes.Buffer
	if err := serve(&out, bytes.NewReader(mustRequest(t, bootstrapReconcileRequest())), hostruntime.New(filepath.Join(root, "ordinary"), machine)); err != nil {
		t.Fatal(err)
	}
}

func bootstrapReconcileRequest() hostprotocol.Request {
	return hostprotocol.Request{
		Action:               hostcontract.ActionReconcile,
		Server:               hostcontract.ServerTarget{SSHAlias: "edge"},
		Resource:             hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"},
		TargetRevision:       "tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PriorAppliedRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Target:               &hostcontract.Target{ReleaseArtifact: "release"},
		Secrets:              &hostcontract.Secrets{},
	}
}

func mustRequest(t *testing.T, request hostprotocol.Request) []byte {
	t.Helper()
	frame, err := hostprotocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

type orderedBuffer struct {
	bytes.Buffer
	name   string
	events *[]string
}

func (b *orderedBuffer) Write(p []byte) (int, error) {
	*b.events = append(*b.events, b.name)
	return b.Buffer.Write(p)
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func mustResponse(t *testing.T, response hostprotocol.Response) []byte {
	t.Helper()
	frame, err := hostprotocol.EncodeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
