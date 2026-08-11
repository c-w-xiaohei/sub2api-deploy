package hostprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	Magic         = "s2h1:"
	Version       = 1
	MaxFrameSize  = 1 << 20
	MaxHeaderSize = 16
)

type ErrorCategory string

const (
	ErrorValidation       ErrorCategory = "validation"
	ErrorApproval         ErrorCategory = "approval"
	ErrorTransport        ErrorCategory = "transport"
	ErrorHostKey          ErrorCategory = "host-key"
	ErrorProtocol         ErrorCategory = "protocol"
	ErrorRemoteOperation  ErrorCategory = "remote-operation"
	ErrorConflict         ErrorCategory = "conflict"
	ErrorRecoveryRequired ErrorCategory = "recovery-required"
)

type ErrorCode string

const (
	CodeInvalidInput      ErrorCode = "invalid-input"
	CodeApprovalRequired  ErrorCode = "approval-required"
	CodeUnavailable       ErrorCode = "unavailable"
	CodeHostKeyMismatch   ErrorCode = "host-key-mismatch"
	CodeMalformedFrame    ErrorCode = "malformed-frame"
	CodeOperationFailed   ErrorCode = "operation-failed"
	CodeOperationConflict ErrorCode = "operation-conflict"
	CodeRecoveryRequired  ErrorCode = "recovery-required"
)

type ProtocolError struct {
	Category ErrorCategory
	Code     ErrorCode
}

func (e *ProtocolError) Error() string { return string(e.Category) + ": " + string(e.Code) }
func bad() error                       { return &ProtocolError{ErrorProtocol, CodeMalformedFrame} }

type Request struct {
	Version              int                           `json:"version"`
	Action               hostcontract.Action           `json:"action"`
	Server               hostcontract.ServerTarget     `json:"server"`
	Resource             hostcontract.ResourceIdentity `json:"resource"`
	TargetRevision       string                        `json:"targetRevision"`
	PriorAppliedRevision string                        `json:"priorAppliedRevision,omitempty"`
	PriorObservation     string                        `json:"priorObservation,omitempty"`
	Target               *hostcontract.Target          `json:"target,omitempty"`
	Secrets              *hostcontract.Secrets         `json:"secrets,omitempty"`
	Approval             *hostcontract.ApprovalSubject `json:"approval,omitempty"`
}
type ResultStatus string

const (
	ResultApplied   ResultStatus = "applied"
	ResultInspected ResultStatus = "inspected"
	ResultRetired   ResultStatus = "retired"
)
type OperationStatus string

const (
	OperationPending  OperationStatus = "pending"
	OperationComplete OperationStatus = "complete"
)
type OperationEvidence struct {
	Key      hostcontract.OperationKey       `json:"key"`
	Status   OperationStatus                 `json:"status"`
	Approval *hostcontract.ApprovalSubject   `json:"approval,omitempty"`
}

type Result struct {
	Status            ResultStatus                    `json:"status"`
	AppliedRevision   string                          `json:"appliedRevision,omitempty"`
	Observation       *hostcontract.StableObservation `json:"observation,omitempty"`
	Machine           *hostcontract.MachineIdentity   `json:"machine,omitempty"`
	Ownership         *hostcontract.OwnershipIdentity `json:"ownership,omitempty"`
	Retirement        *RetirementEvidence             `json:"retirement,omitempty"`
	OperationEvidence *OperationEvidence              `json:"operationEvidence,omitempty"`
}

type RetirementEvidence struct {
	PreserveData bool `json:"preserveData"`
}
type RemoteError struct {
	Category ErrorCategory `json:"category"`
	Code     ErrorCode     `json:"code"`
}
type Response struct {
	Version int          `json:"version"`
	Result  *Result      `json:"result,omitempty"`
	Error   *RemoteError `json:"error,omitempty"`
}

func EncodeRequest(v Request) ([]byte, error) {
	if v.Version == 0 {
		v.Version = Version
	}
	if err := validRequest(v); err != nil {
		return nil, err
	}
	return encode(v)
}
func DecodeRequest(b []byte) (Request, error) { return DecodeRequestFrom(bytes.NewReader(b)) }
func DecodeRequestFrom(r io.Reader) (Request, error) {
	var v Request
	b, e := frame(r)
	if e != nil {
		return v, e
	}
	if e = strict(b, &v); e != nil {
		return v, e
	}
	return v, validRequest(v)
}
func EncodeResponse(v Response) ([]byte, error) {
	if v.Version == 0 {
		v.Version = Version
	}
	if e := validResponse(v); e != nil {
		return nil, e
	}
	return encode(v)
}
func DecodeResponse(b []byte) (Response, error) { return DecodeResponseFrom(bytes.NewReader(b)) }
func DecodeResponseFrom(r io.Reader) (Response, error) {
	var v Response
	b, e := frame(r)
	if e != nil {
		return v, e
	}
	if e = strict(b, &v); e != nil {
		return v, e
	}
	return v, validResponse(v)
}
func encode(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil || len(b) > MaxFrameSize {
		return nil, bad()
	}
	return append([]byte(Magic+strconv.Itoa(len(b))+"\n"), b...), nil
}
func frame(r io.Reader) ([]byte, error) {
	limit := len(Magic) + MaxHeaderSize + MaxFrameSize + 1
	b, e := io.ReadAll(io.LimitReader(r, int64(limit)))
	if e != nil || len(b) < len(Magic) || !bytes.HasPrefix(b, []byte(Magic)) {
		return nil, bad()
	}
	x := b[len(Magic):]
	n := bytes.IndexByte(x, '\n')
	if n < 1 || n > MaxHeaderSize {
		return nil, bad()
	}
	s := string(x[:n])
	if (len(s) > 1 && s[0] == '0') || strings.Trim(s, "0123456789") != "" {
		return nil, bad()
	}
	l, e := strconv.Atoi(s)
	if e != nil || l < 1 || l > MaxFrameSize || len(x[n+1:]) != l {
		return nil, bad()
	}
	return x[n+1:], nil
}
func strict(b []byte, out any) error {
	if duplicateJSONKey(b) {
		return bad()
	}
	d := json.NewDecoder(bytes.NewReader(b))
	var raw any
	if e := d.Decode(&raw); e != nil {
		return bad()
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return bad()
	}
	if e := exact(raw, reflect.TypeOf(out).Elem()); e != nil {
		return bad()
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return bad()
	}
	return nil
}

func duplicateJSONKey(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	var value func() bool
	value = func() bool {
		token, err := d.Token()
		if err != nil {
			return true
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return false
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return true
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return true
				}
				seen[name] = true
				if value() {
					return true
				}
			}
			_, err := d.Token()
			return err != nil
		case '[':
			for d.More() {
				if value() {
					return true
				}
			}
			_, err := d.Token()
			return err != nil
		}
		return true
	}
	if value() {
		return true
	}
	_, err := d.Token()
	return err != io.EOF
}
func exact(v any, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		if v == nil {
			return nil
		}
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("type")
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
		for k, x := range m {
			ft, ok := fields[k]
			if !ok {
				return errors.New("key")
			}
			if e := exact(x, ft); e != nil {
				return e
			}
		}
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return errors.New("type")
		}
		for _, x := range a {
			if e := exact(x, t.Elem()); e != nil {
				return e
			}
		}
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("type")
		}
		for _, x := range m {
			if e := exact(x, t.Elem()); e != nil {
				return e
			}
		}
	}
	return nil
}
func validRequest(v Request) error {
	if v.Version != Version || v.Server.SSHAlias == "" || v.Resource.Environment == "" || v.Resource.ServerKey == "" || v.TargetRevision == "" {
		return bad()
	}
	if !utf8.ValidString(v.Server.SSHAlias) {
		return bad()
	}
	if _, err := hostcontract.ParseRevision(v.TargetRevision); err != nil {
		return bad()
	}
	k := hostcontract.OperationKey{Resource: v.Resource, Action: v.Action, TargetRevision: v.TargetRevision, PriorAppliedRevision: v.PriorAppliedRevision, PriorObservation: v.PriorObservation}
	switch v.Action {
	case hostcontract.ActionInspect:
		if v.Target != nil || v.Secrets != nil || v.Approval != nil || v.PriorAppliedRevision != "" || v.PriorObservation != "" {
			return bad()
		}
	case hostcontract.ActionReconcile:
		if v.Target == nil || v.Secrets == nil || k.Validate() != nil || hostcontract.ValidateTarget(*v.Target, *v.Secrets) != nil {
			return bad()
		}
		if v.Approval != nil && !v.Approval.MatchesReconcileTarget(k, *v.Target) {
			return bad()
		}
	case hostcontract.ActionRetirePreserveData:
		if v.Target != nil || v.Secrets != nil || v.Approval == nil || k.Validate() != nil || !v.Approval.Matches(k, "") || v.Approval.Machine.Value == "" || v.Approval.Ownership.Value == "" {
			return bad()
		}
	default:
		return bad()
	}
	return nil
}
func validResponse(v Response) error {
	if v.Version != Version || (v.Result == nil) == (v.Error == nil) {
		return bad()
	}
	if v.Error != nil {
		if !pair(v.Error.Category, v.Error.Code) {
			return bad()
		}
		return nil
	}
	switch v.Result.Status {
	case ResultApplied:
		if v.Result.AppliedRevision == "" || v.Result.Observation != nil || v.Result.Machine != nil || v.Result.Ownership != nil || v.Result.Retirement != nil || v.Result.OperationEvidence != nil {
			if v.Result.AppliedRevision == "" {
				return bad()
			}
			return bad()
		}
		if _, e := hostcontract.ParseRevision(v.Result.AppliedRevision); e != nil {
			return bad()
		}
	case ResultInspected:
		if v.Result.Observation == nil || v.Result.AppliedRevision != "" || v.Result.Machine != nil || v.Result.Ownership != nil || v.Result.Retirement != nil || v.Result.Observation.Validate() != nil {
			return bad()
		}
		if e := validEvidence(v.Result.OperationEvidence); e != nil {
			return e
		}
	case ResultRetired:
		if v.Result.Machine == nil || v.Result.Ownership == nil || v.Result.Retirement == nil || !v.Result.Retirement.PreserveData || v.Result.AppliedRevision != "" || v.Result.Observation != nil || v.Result.OperationEvidence != nil || v.Result.Machine.Value == "" || v.Result.Ownership.Value == "" || !utf8.ValidString(v.Result.Machine.Value) || !utf8.ValidString(v.Result.Ownership.Value) {
			return bad()
		}
	default:
		return bad()
	}
	return nil
}
func validEvidence(e *OperationEvidence) error {
	if e == nil {
		return nil
	}
	if e.Key.Action != hostcontract.ActionReconcile || e.Key.Validate() != nil || (e.Status != OperationPending && e.Status != OperationComplete) {
		return bad()
	}
	if e.Approval != nil && (e.Approval.Validate() != nil || !e.Approval.Matches(e.Key, e.Approval.AppID)) {
		return bad()
	}
	return nil
}
func pair(c ErrorCategory, k ErrorCode) bool {
	return (c == ErrorValidation && k == CodeInvalidInput) || (c == ErrorApproval && k == CodeApprovalRequired) || (c == ErrorTransport && k == CodeUnavailable) || (c == ErrorHostKey && k == CodeHostKeyMismatch) || (c == ErrorProtocol && k == CodeMalformedFrame) || (c == ErrorRemoteOperation && k == CodeOperationFailed) || (c == ErrorConflict && k == CodeOperationConflict) || (c == ErrorRecoveryRequired && k == CodeRecoveryRequired)
}

var _ = fmt.Sprintf
