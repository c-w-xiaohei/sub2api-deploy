package hostprotocol

import (
	"bytes"
	"errors"
	"strconv"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

func TestRequestRoundTripAndActionValidation(t *testing.T) {
	target, secrets := protocolTarget()
	resource := hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey([]byte("01234567890123456789012345678901")), resource, target, secrets)
	if err != nil {
		t.Fatal(err)
	}
	base := Request{Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource, TargetRevision: revision}
	valid := []Request{
		{Server: base.Server, Resource: base.Resource, TargetRevision: base.TargetRevision, Action: hostcontract.ActionInspect},
		{Server: base.Server, Resource: base.Resource, TargetRevision: base.TargetRevision, Action: hostcontract.ActionReconcile, PriorAppliedRevision: "tr1:key:old", Target: &target, Secrets: &secrets},
		{Server: base.Server, Resource: base.Resource, TargetRevision: base.TargetRevision, Action: hostcontract.ActionRetirePreserveData, PriorObservation: "ready", Approval: &hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: "production", Resource: base.Resource, Machine: hostcontract.MachineIdentity{Value: "machine"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner"}, TargetRevision: base.TargetRevision, PreserveData: true}},
	}
	for _, request := range valid {
		frame, err := EncodeRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeRequest(frame)
		if err != nil || got.Action != request.Action {
			t.Fatalf("round trip = %#v, %v", got, err)
		}
	}
	invalid := append([]Request{}, valid...)
	invalid[0].Target = &target
	invalid[1].Secrets = nil
	invalid[2].Target = &target
	for _, request := range invalid {
		if _, err := EncodeRequest(request); err == nil {
			t.Fatal("invalid action-specific request encoded")
		}
	}
}

func TestResponseIsARealUnion(t *testing.T) {
	target, secrets := protocolTarget()
	resource := hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey([]byte("01234567890123456789012345678901")), resource, target, secrets)
	if err != nil {
		t.Fatal(err)
	}
	observation := hostcontract.StableObservation{Machine: hostcontract.MachineIdentity{Value: "machine"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner"}, HostRelease: "release", AppliedRevision: revision, Ready: true}
	for _, response := range []Response{{Result: &Result{Status: ResultApplied, AppliedRevision: revision}}, {Result: &Result{Status: ResultInspected, Observation: &observation}}, {Result: &Result{Status: ResultRetired, Machine: &observation.Machine, Ownership: &observation.Ownership, Retirement: &RetirementEvidence{PreserveData: true}}}, {Error: &RemoteError{Category: ErrorApproval, Code: CodeApprovalRequired}}} {
		frame, err := EncodeResponse(response)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(frame, []byte(`"result":{}`)) {
			t.Fatal("error response encoded blank result")
		}
		if _, err := DecodeResponse(frame); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []Response{{}, {Result: &Result{Status: ResultApplied}}, {Result: &Result{Status: ResultApplied, AppliedRevision: revision, Observation: &observation}}, {Result: &Result{Status: ResultInspected, Observation: &observation, AppliedRevision: revision}}, {Result: &Result{Status: ResultRetired, Machine: &hostcontract.MachineIdentity{Value: "\xff"}, Ownership: &observation.Ownership, Retirement: &RetirementEvidence{PreserveData: true}}}, {Result: &Result{Status: ResultRetired, Machine: &observation.Machine, Ownership: &hostcontract.OwnershipIdentity{Value: "\xff"}, Retirement: &RetirementEvidence{PreserveData: true}}}, {Result: &Result{Status: ResultInspected, Observation: &hostcontract.StableObservation{}}}, {Result: &Result{Status: ResultApplied, AppliedRevision: "x"}, Error: &RemoteError{Category: ErrorApproval, Code: CodeApprovalRequired}}, {Error: &RemoteError{Category: ErrorApproval, Code: CodeMalformedFrame}}} {
		if _, err := EncodeResponse(invalid); err == nil {
			t.Fatal("invalid response encoded")
		}
	}
}

func TestInspectOperationEvidenceIsBoundedAndStrict(t *testing.T) {
	resource := hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}
	key := hostcontract.OperationKey{Resource: resource, Action: hostcontract.ActionReconcile, TargetRevision: "tr1:0123456789abcdef:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PriorAppliedRevision: "tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	observation := hostcontract.StableObservation{Machine: hostcontract.MachineIdentity{Value: "machine"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner"}, HostRelease: "release", AppliedRevision: key.PriorAppliedRevision, Ready: true}
	evidence := &OperationEvidence{Key: key, Status: OperationPending}
	valid := Response{Result: &Result{Status: ResultInspected, Observation: &observation, OperationEvidence: evidence}}
	if _, err := EncodeResponse(valid); err != nil { t.Fatalf("bounded inspect evidence rejected: %v", err) }
	for _, response := range []Response{
		{Result: &Result{Status: ResultApplied, AppliedRevision: key.TargetRevision, OperationEvidence: evidence}},
		{Result: &Result{Status: ResultInspected, Observation: &observation, OperationEvidence: &OperationEvidence{Key: key, Status: "unknown"}}},
		{Result: &Result{Status: ResultInspected, Observation: &observation, OperationEvidence: &OperationEvidence{Key: hostcontract.OperationKey{}, Status: OperationPending}}},
		{Result: &Result{Status: ResultInspected, Observation: &observation, OperationEvidence: &OperationEvidence{Key: key, Status: OperationComplete, Approval: &hostcontract.ApprovalSubject{}}}},
	} { if _, err := EncodeResponse(response); err == nil { t.Fatal("invalid operation evidence encoded") } }
	for _, body := range []string{
		`{"version":1,"result":{"status":"inspected","observation":{"machine":{"value":"machine"},"ownership":{"value":"owner"},"hostRelease":"release","appliedRevision":"tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ready":true},"operationEvidence":"pending"}}`,
		`{"version":1,"result":{"status":"inspected","observation":{"machine":{"value":"machine"},"ownership":{"value":"owner"},"hostRelease":"release","appliedRevision":"tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ready":true},"operationEvidence":{"unknown":true}}}`,
		`{"version":1,"result":{"status":"inspected","observation":{"machine":{"value":"machine"},"ownership":{"value":"owner"},"hostRelease":"release","appliedRevision":"tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ready":true},"operationEvidence":{"status":"pending","status":"complete"}}}`,
	} { if _, err := DecodeResponse(appendFrame([]byte(body))); err == nil { t.Fatal("malformed operation evidence accepted") } }
}

func TestDecodeFromReaderIsBoundedAndTyped(t *testing.T) {
	for name, input := range map[string][]byte{"empty": nil, "header too long": append([]byte(Magic), bytes.Repeat([]byte("9"), MaxHeaderSize+1)...), "overflow": []byte(Magic + "999999999999999999999999999999\n"), "leading zero": []byte(Magic + "01\n{}"), "extra": appendFrame([]byte(`{"version":1,"error":{"category":"protocol","code":"malformed-frame"}}`), []byte("x"))} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeResponseFrom(bytes.NewReader(input))
			var local *ProtocolError
			if !errors.As(err, &local) || local.Code != CodeMalformedFrame {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestProtocolFrameBoundaries(t *testing.T) {
	encoded, err := EncodeResponse(Response{Error: &RemoteError{Category: ErrorProtocol, Code: CodeMalformedFrame}})
	if err != nil {
		t.Fatal(err)
	}
	newline := bytes.IndexByte(encoded, '\n')
	if newline < 0 {
		t.Fatal("encoded response has no frame header")
	}
	base := encoded[newline+1:]
	for _, size := range []int{MaxFrameSize - 1, MaxFrameSize} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			body := append(append([]byte(nil), base...), bytes.Repeat([]byte{' '}, size-len(base))...)
			frame := append([]byte(Magic+strconv.Itoa(len(body))+"\n"), body...)
			if _, err := DecodeResponse(frame); err != nil {
				t.Fatalf("frame body size %d rejected: %v", size, err)
			}
		})
	}
	oversized := bytes.Repeat([]byte{' '}, MaxFrameSize+1)
	frame := append([]byte(Magic+strconv.Itoa(len(oversized))+"\n"), oversized...)
	if _, err := DecodeResponse(frame); err == nil {
		t.Fatal("max+1 response frame accepted")
	}
}

func TestDecodeRejectsCaseFoldedAndNestedUnknownJSON(t *testing.T) {
	for _, body := range []string{`{"Version":1,"error":{"category":"protocol","code":"malformed-frame"}}`, `{"version":1,"Version":1,"error":{"category":"protocol","code":"malformed-frame"}}`, `{"version":1,"result":{"status":"inspected","observation":{"machine":{"value":"m","unknown":true},"ownership":{"value":"o"},"appliedRevision":"r","ready":true}}}`} {
		if _, err := DecodeResponse(appendFrame([]byte(body))); err == nil {
			t.Fatal("non-exact JSON accepted")
		}
	}
}

func TestRequestRejectsMalformedRevisionAndInvalidTargetApprovalScope(t *testing.T) {
	target, secrets := protocolTarget()
	resource := hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge"}
	revision, err := hostcontract.TargetRevision(hostcontract.RevisionKey([]byte("01234567890123456789012345678901")), resource, target, secrets)
	if err != nil {
		t.Fatal(err)
	}
	base := Request{Action: hostcontract.ActionReconcile, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Resource: resource, TargetRevision: revision, PriorAppliedRevision: revision, Target: &target, Secrets: &secrets}
	badRevision := base
	badRevision.TargetRevision = "not-a-revision"
	if _, err := EncodeRequest(badRevision); err == nil {
		t.Fatal("malformed revision accepted")
	}
	badTarget := base
	badTarget.Target = &hostcontract.Target{}
	if _, err := EncodeRequest(badTarget); err == nil {
		t.Fatal("invalid target accepted")
	}
	badAlias := base
	badAlias.Server.SSHAlias = "\xff"
	if _, err := EncodeRequest(badAlias); err == nil {
		t.Fatal("invalid UTF-8 alias accepted")
	}
	approval := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: "production", Resource: resource, AppID: "missing", DataKind: "postgres", OldData: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old", Endpoint: "old", Port: 5432, Database: "app", TLSServerName: "old"}, NewData: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new", Endpoint: "new", Port: 5432, Database: "app", TLSServerName: "new"}, TargetRevision: revision}
	withApproval := base
	withApproval.Approval = &approval
	if _, err := EncodeRequest(withApproval); err == nil {
		t.Fatal("approval for missing target app accepted")
	}
}

func protocolTarget() (hostcontract.Target, hostcontract.Secrets) {
	return hostcontract.Target{ReleaseArtifact: "release"}, hostcontract.Secrets{}
}
func appendFrame(body []byte, extra ...[]byte) []byte {
	frame := append([]byte(Magic+strconv.Itoa(len(body))+"\n"), body...)
	for _, value := range extra {
		frame = append(frame, value...)
	}
	return frame
}
