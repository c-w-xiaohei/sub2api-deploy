package hostapproval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

const (
	frameMagic    = "sub2api-host-approval-v1 "
	maxHeaderSize = 32
	maxFrameSize  = 64 * 1024
)

var (
	errProtocol   = errors.New("approval protocol error")
	randomReader io.Reader = rand.Reader
)

type approvalRequest struct {
	Version int                          `json:"version"`
	ID      string                       `json:"id"`
	Subject hostcontract.ApprovalSubject `json:"subject"`
}

type approvalDecision struct {
	Version  int                          `json:"version"`
	ID       string                       `json:"id"`
	Subject  hostcontract.ApprovalSubject `json:"subject"`
	Approved bool                         `json:"approved"`
}

type Client struct {
	conn        io.ReadWriteCloser
	mu          sync.Mutex
	invalidated atomic.Bool
}

func NewClient(conn io.ReadWriteCloser) *Client { return &Client{conn: conn} }

func (c *Client) Approve(ctx context.Context, subject hostcontract.ApprovalSubject) (result *hostcontract.ApprovalSubject, err error) {
	if c == nil || ctx == nil || c.conn == nil {
		return nil, fmt.Errorf("approval channel is unavailable")
	}
	if err := validateSubject(subject); err != nil {
		return nil, err
	}
	if !c.mu.TryLock() {
		return nil, fmt.Errorf("approval request already in flight")
	}
	defer c.mu.Unlock()
	if c.invalidated.Load() {
		return nil, fmt.Errorf("approval channel is invalidated")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	id, err := newRequestID(randomReader)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(approvalRequest{Version: 1, ID: id, Subject: subject})
	if err != nil {
		return nil, err
	}
	frame, err := encodeFrame(body)
	if err != nil {
		return nil, err
	}
	finished := make(chan struct{})
	watcher := watchCancellation(ctx, finished, c.invalidate)
	defer func() {
		close(finished)
		<-watcher
	}()
	written := false
	defer func() {
		if written && err != nil {
			c.invalidate()
		}
	}()
	written = true
	if _, err := c.conn.Write(frame); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		return nil, err
	}
	decision, err := decodeDecision(c.conn)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		return nil, err
	}
	if decision.ID != id || decision.Subject != subject {
		return nil, errProtocol
	}
	if !decision.Approved {
		return nil, nil
	}
	result = &subject
	return result, nil
}

func (c *Client) invalidate() {
	if c.invalidated.CompareAndSwap(false, true) && c.conn != nil {
		_ = c.conn.Close()
	}
}

type Server struct {
	decide   func(context.Context, hostcontract.ApprovalSubject) bool
	mu       sync.Mutex
	consumed map[hostcontract.ApprovalSubject]struct{}
}

func NewServer(decide func(context.Context, hostcontract.ApprovalSubject) bool) *Server {
	return &Server{decide: decide, consumed: make(map[hostcontract.ApprovalSubject]struct{})}
}

func (s *Server) Serve(ctx context.Context, conn io.ReadWriteCloser) error {
	if s == nil || ctx == nil || conn == nil || s.decide == nil {
		return fmt.Errorf("approval server is unavailable")
	}
	finished := make(chan struct{})
	watcher := watchCancellation(ctx, finished, func() { _ = conn.Close() })
	defer func() {
		close(finished)
		<-watcher
	}()
	for {
		request, err := decodeRequest(conn)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		denial, err := decisionFrame(request, false)
		if err != nil {
			return errProtocol
		}
		approval, err := decisionFrame(request, true)
		if err != nil {
			return errProtocol
		}
		approved := false
		s.mu.Lock()
		if _, exists := s.consumed[request.Subject]; !exists {
			s.consumed[request.Subject] = struct{}{}
			s.mu.Unlock()
			approved = s.decide(ctx, request.Subject)
		} else {
			s.mu.Unlock()
		}
		frame := denial
		if approved {
			frame = approval
		}
		if _, err := conn.Write(frame); err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return err
		}
	}
}

func watchCancellation(ctx context.Context, finished <-chan struct{}, invalidate func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-finished:
			return
		default:
		}
		select {
		case <-finished:
			return
		case <-ctx.Done():
			select {
			case <-finished:
				return
			default:
				invalidate()
			}
		}
	}()
	return done
}

func decisionFrame(request approvalRequest, approved bool) ([]byte, error) {
	body, err := json.Marshal(approvalDecision{Version: 1, ID: request.ID, Subject: request.Subject, Approved: approved})
	if err != nil {
		return nil, err
	}
	return encodeFrame(body)
}

func newRequestID(reader io.Reader) (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(reader, bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func encodeFrame(body []byte) ([]byte, error) {
	if len(body) == 0 || len(body) > maxFrameSize {
		return nil, errProtocol
	}
	return []byte(frameMagic + strconv.Itoa(len(body)) + "\n" + string(body)), nil
}

func decodeFrame(reader io.Reader) ([]byte, error) {
	prefix := make([]byte, len(frameMagic))
	if n, err := io.ReadFull(reader, prefix); err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errProtocol
		}
		return nil, err
	}
	if string(prefix) != frameMagic {
		return nil, errProtocol
	}
	header := make([]byte, 0, maxHeaderSize)
	for {
		var b [1]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errProtocol
			}
			return nil, err
		}
		if b[0] == '\n' {
			break
		}
		if len(header) == maxHeaderSize {
			return nil, errProtocol
		}
		header = append(header, b[0])
	}
	if len(header) == 0 || (len(header) > 1 && header[0] == '0') {
		return nil, errProtocol
	}
	for _, digit := range header {
		if digit < '0' || digit > '9' {
			return nil, errProtocol
		}
	}
	length, err := strconv.Atoi(string(header))
	if err != nil || length < 1 || length > maxFrameSize {
		return nil, errProtocol
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errProtocol
		}
		return nil, err
	}
	return body, nil
}

func decodeRequest(reader io.Reader) (approvalRequest, error) {
	body, err := decodeFrame(reader)
	if err != nil {
		return approvalRequest{}, err
	}
	var request approvalRequest
	if err := strictJSON(body, &request, "version", "id", "subject"); err != nil || request.Version != 1 || !validID(request.ID) || validateSubject(request.Subject) != nil {
		return approvalRequest{}, errProtocol
	}
	return request, nil
}

func decodeDecision(reader io.Reader) (approvalDecision, error) {
	body, err := decodeFrame(reader)
	if err != nil {
		return approvalDecision{}, err
	}
	var decision approvalDecision
	if err := strictJSON(body, &decision, "version", "id", "subject", "approved"); err != nil || decision.Version != 1 || !validID(decision.ID) || validateSubject(decision.Subject) != nil {
		return approvalDecision{}, errProtocol
	}
	return decision, nil
}

func strictJSON(body []byte, value any, required ...string) error {
	if !utf8.Valid(body) || hasDuplicateKeys(body) {
		return errProtocol
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return errProtocol
	}
	if _, decision := value.(*approvalDecision); decision {
		if !validEnvelope(fields, []string{"version", "id", "subject", "approved"}) || !validBool(fields["approved"]) {
			return errProtocol
		}
	} else if _, request := value.(*approvalRequest); request {
		if !validEnvelope(fields, []string{"version", "id", "subject"}) {
			return errProtocol
		}
	} else {
		return errProtocol
	}
	_ = required
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errProtocol
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errProtocol
	}
	return nil
}

func validEnvelope(fields map[string]json.RawMessage, allowed []string) bool {
	if len(fields) != len(allowed) {
		return false
	}
	for _, name := range allowed {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return validNumber(fields["version"]) && validString(fields["id"]) && validSubjectJSON(fields["subject"])
}

func validSubjectJSON(raw json.RawMessage) bool {
	fields, ok := object(raw, []string{"kind", "environment", "resource", "appId", "dataKind", "oldData", "newData", "machine", "ownership", "targetRevision", "preserveData"})
	if !ok || !requiredFields(fields, "kind", "environment", "resource", "targetRevision") {
		return false
	}
	if !validString(fields["kind"]) || !validString(fields["environment"]) || !validResourceJSON(fields["resource"]) || !validString(fields["targetRevision"]) {
		return false
	}
	return optionalString(fields, "appId") && optionalString(fields, "dataKind") && optionalData(fields, "oldData") && optionalData(fields, "newData") && optionalMachine(fields, "machine") && optionalMachine(fields, "ownership") && optionalBool(fields, "preserveData")
}

func validResourceJSON(raw json.RawMessage) bool {
	fields, ok := object(raw, []string{"environment", "serverKey"})
	return ok && requiredFields(fields, "environment", "serverKey") && validString(fields["environment"]) && validString(fields["serverKey"])
}

func validDataJSON(raw json.RawMessage) bool {
	fields, ok := object(raw, []string{"kind", "providerId", "endpoint", "port", "database", "tlsMode", "tlsServerName"})
	if !ok || !requiredFields(fields, "kind", "tlsMode") || !validString(fields["kind"]) || !validString(fields["tlsMode"]) {
		return false
	}
	return optionalString(fields, "providerId") && optionalString(fields, "endpoint") && optionalNumber(fields, "port") && optionalString(fields, "database") && optionalString(fields, "tlsServerName")
}

func validMachineJSON(raw json.RawMessage) bool {
	fields, ok := object(raw, []string{"value"})
	return ok && requiredFields(fields, "value") && validString(fields["value"])
}

func object(raw json.RawMessage, allowed []string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	for name := range fields {
		matched := false
		for _, allowedName := range allowed {
			if name == allowedName {
				matched = true
				break
			}
		}
		if !matched {
			return nil, false
		}
	}
	return fields, true
}

func requiredFields(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func validString(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && string(raw) != "null"
}

func validNumber(raw json.RawMessage) bool {
	var value int
	return json.Unmarshal(raw, &value) == nil && string(raw) != "null"
}

func validBool(raw json.RawMessage) bool { return string(raw) == "true" || string(raw) == "false" }

func optionalString(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	return !ok || validString(raw)
}

func optionalNumber(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	return !ok || validNumber(raw)
}

func optionalBool(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	return !ok || validBool(raw)
}

func optionalData(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	return !ok || validDataJSON(raw)
}

func optionalMachine(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	return !ok || validMachineJSON(raw)
}

func hasDuplicateKeys(body []byte) bool {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	var scan func() bool
	scan = func() bool {
		token, err := decoder.Token()
		if err != nil {
			return true
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return false
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return true
				}
				name, ok := key.(string)
				if !ok {
					return true
				}
				if _, exists := seen[name]; exists {
					return true
				}
				seen[name] = struct{}{}
				if scan() {
					return true
				}
			}
			_, err = decoder.Token()
			return err != nil
		case '[':
			for decoder.More() {
				if scan() {
					return true
				}
			}
			_, err = decoder.Token()
			return err != nil
		default:
			return true
		}
	}
	return scan() || decoder.Decode(&struct{}{}) != io.EOF
}

func validID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validateSubject(subject hostcontract.ApprovalSubject) error {
	if subject.Validate() != nil {
		return errProtocol
	}
	if _, err := hostcontract.ParseRevision(subject.TargetRevision); err != nil || subject.TargetRevision != strings.ToLower(subject.TargetRevision) || !validSubjectStrings(subject) {
		return errProtocol
	}
	if subject.Kind == hostcontract.ApprovalRetire && (subject.OldData != (hostcontract.DataIdentity{}) || subject.NewData != (hostcontract.DataIdentity{})) {
		return errProtocol
	}
	return nil
}

func validSubjectStrings(subject hostcontract.ApprovalSubject) bool {
	valid := utf8.ValidString
	return valid(string(subject.Kind)) && valid(subject.Environment) && valid(subject.Resource.Environment) && valid(subject.Resource.ServerKey) && valid(subject.AppID) && valid(subject.DataKind) && validDataStrings(subject.OldData) && validDataStrings(subject.NewData) && valid(subject.Machine.Value) && valid(subject.Ownership.Value) && valid(subject.TargetRevision)
}

func validDataStrings(data hostcontract.DataIdentity) bool {
	return utf8.ValidString(data.Kind) && utf8.ValidString(data.ProviderID) && utf8.ValidString(data.Endpoint) && utf8.ValidString(data.Database) && utf8.ValidString(data.TLSServerName)
}
