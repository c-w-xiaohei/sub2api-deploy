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
	"path/filepath"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
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
	return serveWithRequestDigest(out, in, root, machinePath, digestPath, ServeBootstrap)
}

// ServeWithRequestDigest records a digest and non-sensitive action metadata for
// one framed request before routing it through Runtime.Serve.
func ServeWithRequestDigest(out io.Writer, in io.Reader, root, machinePath, digestPath string) error {
	return serveWithRequestDigest(out, in, root, machinePath, digestPath, Serve)
}

func serveWithRequestDigest(out io.Writer, in io.Reader, root, machinePath, digestPath string, serve func(io.Writer, io.Reader, string, string) error) error {
	frame, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	request, err := hostprotocol.DecodeRequest(frame)
	if err != nil {
		return err
	}
	if digestPath != "" {
		sum := sha256.Sum256(frame)
		metadata := fmt.Sprintf("operationDigest=%x\naction=%s\n", sum, request.Action)
		temporary, err := os.CreateTemp(filepath.Dir(digestPath), ".request-metadata-")
		if err != nil {
			return err
		}
		name := temporary.Name()
		defer os.Remove(name)
		if err := temporary.Chmod(0600); err != nil {
			_ = temporary.Close()
			return err
		}
		if _, err := temporary.WriteString(metadata); err != nil {
			_ = temporary.Close()
			return err
		}
		if err := temporary.Sync(); err != nil {
			_ = temporary.Close()
			return err
		}
		if err := temporary.Close(); err != nil {
			return err
		}
		if err := os.Rename(name, digestPath); err != nil {
			return err
		}
	}
	return serve(out, bytes.NewReader(frame), root, machinePath)
}
