package hostapproval

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

const testTimeout = time.Second

func TestServeLoopsSequentialFramesAndCleanEOF(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	server := NewServer(func(context.Context, hostcontract.ApprovalSubject) bool { return true })
	done := startServe(server, serverConn)
	client := NewClient(clientConn)
	for _, subject := range []hostcontract.ApprovalSubject{dataSubject(), retireSubject()} {
		got, err := client.Approve(testContext(t), subject)
		if err != nil || got == nil || *got != subject { // Checked after Serve has been joined below.
			_ = clientConn.Close()
			serveErr := await(t, done)
			_ = serverConn.Close()
			t.Fatalf("Approve = %#v, %v; Serve = %v", got, err, serveErr)
		}
	}
	if err := clientConn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); err != nil {
		t.Fatalf("Serve after clean EOF: %v", err)
	}
	_ = serverConn.Close()
}

func TestServeReturnsContextCausePromptly(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan error, 1)
	go func() { done <- NewServer(func(context.Context, hostcontract.ApprovalSubject) bool { return true }).Serve(ctx, serverConn) }()
	cancel()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve cancellation = %v", err)
	}
}

func TestServeReturnsNonEOFReadError(t *testing.T) {
	errRead := errors.New("connection read failed")
	err := NewServer(func(context.Context, hostcontract.ApprovalSubject) bool { return true }).Serve(testContext(t), readErrorConn{err: errRead})
	if !errors.Is(err, errRead) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve read error = %v", err)
	}
}

func TestApproveReturnsExactSubjectAndOpaqueDistinctIDs(t *testing.T) {
	subject := dataSubject()
	ids := make(chan string, 2)
	for range 2 {
		clientConn, peerConn := net.Pipe()
		t.Cleanup(func() { _ = clientConn.Close(); _ = peerConn.Close() })
		peer := make(chan error, 1)
		go func() {
			request, err := readRequest(peerConn)
			if err == nil && request.Subject != subject {
				err = fmt.Errorf("subject = %#v", request.Subject)
			}
			if err == nil {
				ids <- request.ID
				err = writeDecision(peerConn, request.ID, request.Subject, true)
			}
			peer <- err
		}()
		got, err := NewClient(clientConn).Approve(testContext(t), subject)
		closeErr := clientConn.Close()
		peerErr := await(t, peer)
		_ = peerConn.Close()
		if closeErr != nil || peerErr != nil || err != nil || got == nil || *got != subject {
			t.Fatalf("Approve = %#v, %v; close = %v; peer = %v", got, err, closeErr, peerErr)
		}
	}
	first, second := <-ids, <-ids
	for _, id := range []string{first, second} {
		if len(id) != 32 || strings.ToLower(id) != id {
			t.Fatalf("request ID = %q, want lowercase 32-hex", id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("request ID = %q: %v", id, err)
		}
	}
	if first == second {
		t.Fatalf("independent IDs matched: %q", first)
	}
}

func TestNewRequestIDUsesExactRandomBytesAndRejectsBadReads(t *testing.T) {
	known := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	id, err := newRequestID(bytes.NewReader(known))
	if err != nil || id != "000102030405060708090a0b0c0d0e0f" {
		t.Fatalf("newRequestID known bytes = %q, %v", id, err)
	}
	for name, reader := range map[string]io.Reader{
		"short": bytes.NewReader(known[:15]),
		"error": errorReader{},
	} {
		t.Run(name, func(t *testing.T) {
			if id, err := newRequestID(reader); err == nil || id != "" {
				t.Fatalf("newRequestID = %q, %v", id, err)
			}
		})
	}
}

func TestApproveFailsClosedWhenRandomIDReadFails(t *testing.T) {
	saved := randomReader
	randomReader = errorReader{}
	t.Cleanup(func() { randomReader = saved })
	conn := &recordingConn{}
	if got, err := NewClient(conn).Approve(testContext(t), dataSubject()); err == nil || got != nil {
		t.Fatalf("Approve = %#v, %v", got, err)
	}
	if conn.writes != 0 {
		t.Fatalf("random failure wrote %d bytes", conn.writes)
	}
}

func TestApproveRejectsInvalidSubjectBeforeWriting(t *testing.T) {
	for name, subject := range map[string]hostcontract.ApprovalSubject{
		"unparsed revision": func() hostcontract.ApprovalSubject { s := dataSubject(); s.TargetRevision = "bad"; return s }(),
		"invalid UTF-8":       func() hostcontract.ApprovalSubject { s := dataSubject(); s.AppID = "\xff"; return s }(),
		"retire old data":     func() hostcontract.ApprovalSubject { s := retireSubject(); s.OldData = dataSubject().OldData; return s }(),
		"retire new data":     func() hostcontract.ApprovalSubject { s := retireSubject(); s.NewData = dataSubject().NewData; return s }(),
		"retire app fields":   func() hostcontract.ApprovalSubject { s := retireSubject(); s.AppID, s.DataKind = "api", "postgres"; return s }(),
		"data link machine":   func() hostcontract.ApprovalSubject { s := dataSubject(); s.Machine.Value = "machine-a"; return s }(),
		"data link ownership": func() hostcontract.ApprovalSubject { s := dataSubject(); s.Ownership.Value = "owner-a"; return s }(),
		"data link preserve":  func() hostcontract.ApprovalSubject { s := dataSubject(); s.PreserveData = true; return s }(),
	} {
		t.Run(name, func(t *testing.T) {
			conn := &recordingConn{}
			if got, err := NewClient(conn).Approve(testContext(t), subject); err == nil || got != nil {
				t.Fatalf("Approve = %#v, %v", got, err)
			}
			if conn.writes != 0 {
				t.Fatalf("invalid subject wrote %d bytes", conn.writes)
			}
		})
	}
}

func TestDecodeDecisionRejectsSemanticallyInvalidCorrelatedSubject(t *testing.T) {
	subject := dataSubject()
	subject.Machine.Value = "forbidden"
	_, err := decodeDecision(bytes.NewReader(frame(decisionBody(t, approvalDecision{Version: 1, ID: "0123456789abcdef0123456789abcdef", Subject: subject, Approved: true}))))
	assertProtocolError(t, err)
}

func TestApproveRejectsMalformedAndMismatchedDecisions(t *testing.T) {
	for name, reply := range map[string]func(approvalRequest) []byte{
		"missing version": func(request approvalRequest) []byte { return []byte(fmt.Sprintf(`{"id":%q,"subject":%s,"approved":true}`, request.ID, subjectJSON(t, request.Subject))) },
		"wrong ID": func(request approvalRequest) []byte { return decisionBody(t, approvalDecision{Version: 1, ID: "00000000000000000000000000000000", Subject: request.Subject, Approved: true}) },
		"wrong length ID": func(request approvalRequest) []byte { return decisionBody(t, approvalDecision{Version: 1, ID: request.ID[:31], Subject: request.Subject, Approved: true}) },
		"wrong subject": func(request approvalRequest) []byte { subject := request.Subject; subject.TargetRevision = otherRevision; return decisionBody(t, approvalDecision{Version: 1, ID: request.ID, Subject: subject, Approved: true}) },
		"wrong version": func(request approvalRequest) []byte { return decisionBody(t, approvalDecision{Version: 2, ID: request.ID, Subject: request.Subject, Approved: true}) },
		"missing approved": func(request approvalRequest) []byte { return []byte(fmt.Sprintf(`{"version":1,"id":%q,"subject":%s}`, request.ID, subjectJSON(t, request.Subject))) },
		"unknown top level": func(request approvalRequest) []byte { return []byte(fmt.Sprintf(`{"version":1,"id":%q,"subject":%s,"approved":true,"unknown":true}`, request.ID, subjectJSON(t, request.Subject))) },
		"duplicate top level": func(request approvalRequest) []byte { return []byte(fmt.Sprintf(`{"version":1,"id":%q,"id":%q,"subject":%s,"approved":true}`, request.ID, request.ID, subjectJSON(t, request.Subject))) },
		"unknown nested": func(request approvalRequest) []byte { return []byte(fmt.Sprintf(`{"version":1,"id":%q,"subject":%s,"approved":true}`, request.ID, strings.Replace(subjectJSON(t, request.Subject), `"serverKey":"edge-a"`, `"serverKey":"edge-a","unknown":true`, 1))) },
		"duplicate nested": func(request approvalRequest) []byte { return []byte(fmt.Sprintf(`{"version":1,"id":%q,"subject":%s,"approved":true}`, request.ID, strings.Replace(subjectJSON(t, request.Subject), `"serverKey":"edge-a"`, `"serverKey":"edge-a","serverKey":"edge-a"`, 1))) },
		"uppercase ID": func(request approvalRequest) []byte { return decisionBody(t, approvalDecision{Version: 1, ID: strings.ToUpper(request.ID), Subject: request.Subject, Approved: true}) },
		"truncated": func(approvalRequest) []byte { return []byte(`{"version":1`) },
		"wire invalid UTF-8": func(request approvalRequest) []byte { return bytes.Replace(decisionBody(t, approvalDecision{Version: 1, ID: request.ID, Subject: request.Subject, Approved: true}), []byte(`"api"`), []byte{'"', 0xff, '"'}, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			clientConn, peerConn := net.Pipe()
			t.Cleanup(func() { _ = clientConn.Close(); _ = peerConn.Close() })
			peer := make(chan error, 1)
			go func() {
				request, err := readRequest(peerConn)
				if err == nil {
					err = writeBody(peerConn, reply(request))
				}
				peer <- err
			}()
			got, err := NewClient(clientConn).Approve(testContext(t), dataSubject())
			_ = clientConn.Close()
			_ = peerConn.Close()
			peerErr := await(t, peer)
			if peerErr != nil {
				t.Fatalf("peer: %v", peerErr)
			}
			assertProtocolFailure(t, got, err)
		})
	}
}

func TestServeRejectsInvalidRequestsBeforeDeciding(t *testing.T) {
	valid := requestBody(t, approvalRequest{Version: 1, ID: "0123456789abcdef0123456789abcdef", Subject: dataSubject()})
	for name, body := range map[string][]byte{
		"missing version":     bytes.Replace(valid, []byte(`"version":1,`), nil, 1),
		"wrong version":       bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":2`), 1),
		"unknown top level":   bytes.Replace(valid, []byte(`"id":`), []byte(`"unknown":true,"id":`), 1),
		"duplicate top level": bytes.Replace(valid, []byte(`"id":`), []byte(`"id":"x","id":`), 1),
		"unknown nested":      bytes.Replace(valid, []byte(`"serverKey":"edge-a"`), []byte(`"serverKey":"edge-a","unknown":true`), 1),
		"uppercase ID":        bytes.Replace(valid, []byte(`0123456789abcdef0123456789abcdef`), []byte(`0123456789ABCDEF0123456789ABCDEF`), 1),
		"wrong length ID":     bytes.Replace(valid, []byte(`0123456789abcdef0123456789abcdef`), []byte(`0123456789abcdef0123456789abcde`), 1),
		"duplicate nested":    bytes.Replace(valid, []byte(`"serverKey":"edge-a"`), []byte(`"serverKey":"edge-a","serverKey":"edge-a"`), 1),
		"semantic invalid subject": bytes.Replace(valid, []byte(`"targetRevision":"`+testRevision+`"`), []byte(`"targetRevision":"bad"`), 1),
		"wire invalid UTF-8":  bytes.Replace(valid, []byte(`"api"`), []byte{'"', 0xff, '"'}, 1),
		"truncated":           []byte(`{"version":1`),
	} {
		t.Run(name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
			var called atomic.Bool
			server := NewServer(func(context.Context, hostcontract.ApprovalSubject) bool { called.Store(true); return true })
			done := startServe(server, serverConn)
			writeErr := writeBody(clientConn, body)
			_ = clientConn.Close()
			serveErr := await(t, done)
			_ = serverConn.Close()
			if writeErr != nil {
				t.Fatalf("write invalid request: %v", writeErr)
			}
			if called.Load() {
				t.Fatal("invalid request reached decider")
			}
			assertProtocolError(t, serveErr)
		})
	}
}

func TestFrameBoundsAndCanonicalHeaders(t *testing.T) {
	valid := requestBody(t, approvalRequest{Version: 1, ID: "0123456789abcdef0123456789abcdef", Subject: dataSubject()})
	for name, input := range map[string][]byte{
		"leading zero":    append([]byte(frameMagic+fmt.Sprintf("0%d\n", len(valid))), valid...),
		"non-numeric":     append([]byte(frameMagic+"x\n"), valid...),
		"header too long": append(append(append([]byte(frameMagic), bytes.Repeat([]byte("1"), maxHeaderSize+1)...), '\n'), valid...),
		"declared over max": []byte(frameMagic + fmt.Sprintf("%d\n", maxFrameSize+1)),
	} {
		t.Run(name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
			done := startServe(NewServer(func(context.Context, hostcontract.ApprovalSubject) bool { return true }), serverConn)
			writeErr := writeRaw(clientConn, input)
			_ = clientConn.Close()
			serveErr := await(t, done)
			_ = serverConn.Close()
			if writeErr != nil {
				t.Fatalf("write invalid header: %v", writeErr)
			}
			assertProtocolError(t, serveErr)
		})
	}
	if _, err := encodeFrame(bytes.Repeat([]byte("x"), maxFrameSize+1)); err == nil {
		t.Fatal("oversized body encoded")
	}
	if frame, err := encodeFrame(bytes.Repeat([]byte("x"), maxFrameSize)); err != nil || len(frame) == 0 {
		t.Fatalf("exact maximum body frame = %d bytes, %v", len(frame), err)
	} else if body, err := decodeFrame(bytes.NewReader(frame)); err != nil || len(body) != maxFrameSize {
		t.Fatalf("exact maximum body decode = %d bytes, %v", len(body), err)
	}
	if _, err := decodeFrame(bytes.NewReader([]byte(frameMagic + "0\n"))); !errors.Is(err, errProtocol) {
		t.Fatalf("zero-length frame = %v", err)
	}
}

func TestDecisionConsumesBeforeResponseDelivery(t *testing.T) {
	for name, approved := range map[string]bool{"approved": true, "denied": false} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			clientConn, serverConn := net.Pipe()
			t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
			server := NewServer(func(context.Context, hostcontract.ApprovalSubject) bool {
				calls.Add(1)
				_ = clientConn.Close() // Disconnect after deciding, before response delivery.
				return approved
			})
			done := startServe(server, serverConn)
			_, _ = NewClient(clientConn).Approve(testContext(t), dataSubject())
			if err := await(t, done); err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failed response Serve = %v", err)
			}

			retryClient, retryServer := net.Pipe()
			t.Cleanup(func() { _ = retryClient.Close(); _ = retryServer.Close() })
			retryDone := startServe(server, retryServer)
			got, err := NewClient(retryClient).Approve(testContext(t), dataSubject())
			if got != nil || err != nil {
				t.Fatalf("consumed retry = %#v, %v", got, err)
			}
			_ = retryClient.Close()
			if err := await(t, retryDone); err != nil {
				t.Fatalf("retry Serve: %v", err)
			}
			_ = retryServer.Close()
			if calls.Load() != 1 {
				t.Fatalf("decider calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestConcurrentApproveIsAtomicAndOneIsApproved(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	var winnerConn, winnerServer, loserConn, loserServer net.Conn
	var winnerDone, loserDone <-chan error
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if winnerConn != nil { _ = winnerConn.Close() }
		if winnerServer != nil { _ = winnerServer.Close() }
		if loserConn != nil { _ = loserConn.Close() }
		if loserServer != nil { _ = loserServer.Close() }
		if winnerDone != nil { _, _ = awaitError(winnerDone, testTimeout) }
		if loserDone != nil { _, _ = awaitError(loserDone, testTimeout) }
	})
	var calls atomic.Int32
	server := NewServer(func(context.Context, hostcontract.ApprovalSubject) bool {
		calls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		return true
	})
	winnerConn, winnerServer = net.Pipe()
	winnerDone = startServe(server, winnerServer)
	winnerResult := make(chan *hostcontract.ApprovalSubject, 1)
	winnerError := make(chan error, 1)
	go approveAsync(winnerConn, testContext(t), winnerResult, winnerError)
	awaitSignal(t, entered)

	loserConn, loserServer = net.Pipe()
	loserDone = startServe(server, loserServer)
	loserResult := make(chan *hostcontract.ApprovalSubject, 1)
	loserError := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	go approveAsync(loserConn, ctx, loserResult, loserError)
	if got, err := receiveApproval(t, loserResult, loserError); got != nil || err != nil {
		t.Fatalf("loser while winner is in flight = %#v, %v", got, err)
	}
	releaseOnce.Do(func() { close(release) })
	if got, err := receiveApproval(t, winnerResult, winnerError); got == nil || err != nil {
		t.Fatalf("winner = %#v, %v", got, err)
	}
	if err := await(t, winnerDone); err != nil {
		t.Fatalf("winner Serve: %v", err)
	}
	if err := await(t, loserDone); err != nil {
		t.Fatalf("loser Serve: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestApproveCancellationClosesAndInvalidatesChannel(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = peerConn.Close() })
	client := NewClient(clientConn)
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan error, 1)
	go func() { _, err := client.Approve(ctx, dataSubject()); done <- err }()
	if _, err := readRequest(peerConn); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Approve = %v", err)
	}
	if got, err := client.Approve(testContext(t), dataSubject()); got != nil || err == nil {
		t.Fatalf("invalidated client = %#v, %v", got, err)
	}
	_ = peerConn.Close()
}

func dataSubject() hostcontract.ApprovalSubject {
	return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: "production", Resource: hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge-a"}, AppID: "api", DataKind: "postgres", OldData: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old", Endpoint: "old.example", Port: 5432, Database: "app", TLSServerName: "old.example"}, NewData: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new", Endpoint: "new.example", Port: 5432, Database: "app", TLSServerName: "new.example"}, TargetRevision: testRevision}
}

func retireSubject() hostcontract.ApprovalSubject {
	return hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalRetire, Environment: "production", Resource: hostcontract.ResourceIdentity{Environment: "production", ServerKey: "edge-a"}, Machine: hostcontract.MachineIdentity{Value: "machine-a"}, Ownership: hostcontract.OwnershipIdentity{Value: "owner-a"}, TargetRevision: testRevision, PreserveData: true}
}

const testRevision = "tr1:0000000000000000:0000000000000000000000000000000000000000000000000000000000000000"
const otherRevision = "tr1:0000000000000000:1111111111111111111111111111111111111111111111111111111111111111"

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func startServe(server *Server, conn io.ReadWriteCloser) <-chan error {
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	go func() { defer cancel(); done <- server.Serve(ctx, conn) }()
	return done
}

func readRequest(conn net.Conn) (approvalRequest, error) {
	_ = conn.SetReadDeadline(time.Now().Add(testTimeout))
	return decodeRequest(conn)
}

func writeDecision(conn net.Conn, id string, subject hostcontract.ApprovalSubject, approved bool) error {
	return writeBody(conn, decisionBody(nil, approvalDecision{Version: 1, ID: id, Subject: subject, Approved: approved}))
}

func requestBody(t *testing.T, request approvalRequest) []byte { return marshal(t, request) }
func decisionBody(t *testing.T, decision approvalDecision) []byte { return marshal(t, decision) }
func subjectJSON(t *testing.T, subject hostcontract.ApprovalSubject) string { return string(marshal(t, subject)) }
func marshal(t *testing.T, value any) []byte {
	if t != nil {
		t.Helper()
	}
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}
func writeBody(conn net.Conn, body []byte) error { return writeRaw(conn, frame(body)) }
func writeRaw(conn net.Conn, input []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(testTimeout))
	_, err := conn.Write(input)
	return err
}
func await(t *testing.T, done <-chan error) error {
	t.Helper()
	err, ok := awaitError(done, testTimeout)
	if !ok {
		t.Error("operation did not complete")
	}
	return err
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testTimeout):
		t.Error("signal did not arrive")
	}
}

func assertProtocolFailure(t *testing.T, got *hostcontract.ApprovalSubject, err error) {
	t.Helper()
	if got != nil || err == nil {
		t.Fatalf("invalid decision accepted: %#v, %v", got, err)
	}
	assertProtocolError(t, err)
}

func assertProtocolError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errProtocol) {
		t.Fatalf("expected errProtocol, got %v", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("protocol error was cancellation: %v", err)
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		t.Fatalf("protocol error timed out: %v", err)
	}
}

func awaitError(done <-chan error, timeout time.Duration) (error, bool) {
	select {
	case err := <-done:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func approveAsync(conn net.Conn, ctx context.Context, results chan<- *hostcontract.ApprovalSubject, errs chan<- error) {
	got, err := NewClient(conn).Approve(ctx, dataSubject())
	_ = conn.Close()
	results <- got
	errs <- err
}

func receiveApproval(t *testing.T, results <-chan *hostcontract.ApprovalSubject, errs <-chan error) (*hostcontract.ApprovalSubject, error) {
	t.Helper()
	select {
	case got := <-results:
		select {
		case err := <-errs:
			return got, err
		case <-time.After(testTimeout):
			t.Fatal("approval error did not complete")
			return nil, nil
		}
	case <-time.After(testTimeout):
		t.Fatal("approval did not complete")
		return nil, nil
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("random source failed") }

type recordingConn struct{ writes int }

func (*recordingConn) Read([]byte) (int, error) { return 0, errors.New("unexpected read") }
func (c *recordingConn) Write(p []byte) (int, error) { c.writes++; return len(p), nil }
func (*recordingConn) Close() error { return nil }

type readErrorConn struct{ err error }

func (c readErrorConn) Read([]byte) (int, error) { return 0, c.err }
func (readErrorConn) Write([]byte) (int, error) { return 0, errors.New("unexpected write") }
func (readErrorConn) Close() error { return nil }
