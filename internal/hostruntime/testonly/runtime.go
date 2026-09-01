// Package testonly contains CI helper plumbing for the host runtime.
// It is deliberately not a replacement for cmd/sub2api-host.
package testonly

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

// ErrSharedServeSeamUnavailable is returned until hostruntime owns the shared
// stdio serving contract used by both the released command and this CI helper.
var ErrSharedServeSeamUnavailable = errors.New("shared host runtime serve seam is unavailable")

type stdioServer interface {
	Serve(io.Writer, io.Reader) error
}

type bootstrapStdioServer interface {
	ServeBootstrap(io.Writer, io.Reader) error
}

// Serve constructs a real Runtime rooted at root and delegates framing and
// dispatch to the production runtime seam. It intentionally has no protocol
// fallback: duplicating cmd/sub2api-host here would test a second Host.
func Serve(out io.Writer, in io.Reader, root, machinePath string) error {
	runtime := hostruntime.New(root, machinePath)
	server, ok := any(runtime).(stdioServer)
	if !ok {
		return ErrSharedServeSeamUnavailable
	}
	return server.Serve(out, in)
}

// ServeBootstrap delegates Create's fresh-Host protocol to the same runtime
// seam. It remains unavailable until that seam exists rather than copying the
// command's bootstrap framing behavior.
func ServeBootstrap(out io.Writer, in io.Reader, root, machinePath string) error {
	runtime := hostruntime.New(root, machinePath)
	server, ok := any(runtime).(bootstrapStdioServer)
	if !ok {
		return ErrSharedServeSeamUnavailable
	}
	return server.ServeBootstrap(out, in)
}

// ServeBootstrapWithRequestDigest is a test-only protocol oracle. It validates
// one already-framed request and records only its digest before delegating to
// the shared serving seam; it never constructs a response or reimplements it.
func ServeBootstrapWithRequestDigest(out io.Writer, in io.Reader, root, machinePath, digestPath string) error {
	frame, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	if _, err := hostprotocol.DecodeRequest(frame); err != nil {
		return err
	}
	if digestPath != "" {
		sum := sha256.Sum256(frame)
		if err := os.WriteFile(digestPath, []byte(fmt.Sprintf("%x\n", sum)), 0600); err != nil {
			return err
		}
	}
	return ServeBootstrap(out, bytes.NewReader(frame), root, machinePath)
}
