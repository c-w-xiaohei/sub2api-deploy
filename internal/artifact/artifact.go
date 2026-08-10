// Package artifact validates and prepares pinned Host binaries for bootstrap transport.
package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

const bootstrapMagic = "s2a1:"

type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Release       string `json:"release"`
	LinuxAMD64    Entry  `json:"linux-amd64"`
	LinuxARM64    Entry  `json:"linux-arm64"`
}
type Pinned struct {
	bytes  []byte
	Size   int64
	SHA256 string
}

type ProbeInfo struct{ OS, Arch, Machine, InstalledDigest string }

func LoadManifest(r io.Reader) (Manifest, error) {
	var manifest Manifest
	b, err := io.ReadAll(io.LimitReader(r, maxManifestSize+1))
	if err != nil || len(b) > maxManifestSize {
		return Manifest{}, fmt.Errorf("invalid artifact manifest")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Manifest{}, fmt.Errorf("invalid artifact manifest")
	}
	if manifest.SchemaVersion != 1 || manifest.Release == "" || !validEntry(manifest.LinuxAMD64) || !validEntry(manifest.LinuxARM64) {
		return Manifest{}, fmt.Errorf("invalid artifact manifest")
	}
	return manifest, nil
}

func LoadPinned(root string, manifest Manifest, arch string) (*Pinned, error) {
	if manifest.SchemaVersion != 1 || manifest.Release == "" {
		return nil, fmt.Errorf("invalid artifact manifest")
	}
	platform, err := PlatformFor(arch)
	if err != nil {
		return nil, err
	}
	entry := manifest.LinuxAMD64
	if platform == "linux-arm64" {
		entry = manifest.LinuxARM64
	}
	if !validEntry(entry) {
		return nil, fmt.Errorf("invalid artifact entry")
	}
	binary, err := readPinned(root, entry.Path, entry.Size)
	hash := sha256.Sum256(binary)
	if err != nil || int64(len(binary)) != entry.Size || !bytes.Equal(hash[:], mustHex(entry.SHA256)) {
		return nil, fmt.Errorf("artifact checksum mismatch")
	}
	return &Pinned{bytes: binary, Size: entry.Size, SHA256: entry.SHA256}, nil
}

func PlatformFor(machine string) (string, error) {
	switch machine {
	case "x86_64", "amd64":
		return "linux-amd64", nil
	case "aarch64", "arm64":
		return "linux-arm64", nil
	default:
		return "", fmt.Errorf("unsupported host architecture %q", machine)
	}
}

func BootstrapInput(pinned *Pinned, request []byte) ([]byte, error) {
	if pinned == nil || int64(len(pinned.bytes)) != pinned.Size || pinned.Size < 0 || len(pinned.SHA256) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid pinned artifact")
	}
	binary := append([]byte(nil), pinned.bytes...)
	input := append([]byte(bootstrapMagic+fmt.Sprintf("%d:%s\n", pinned.Size, pinned.SHA256)), binary...)
	return append(input, request...), nil
}

// BootstrapClient is the narrow capability EnsurePinned needs from fixed OpenSSH.
type BootstrapClient interface {
	Probe(context.Context, string) (ProbeInfo, error)
	Bootstrap(context.Context, string, []byte) (hostprotocol.Response, error)
}

// EnsurePinned selects, verifies, and transfers a release-pinned host binary.
// Read and Import callers must not invoke it.
func EnsurePinned(ctx context.Context, client BootstrapClient, alias, bundleRoot string, manifest Manifest, request []byte) (hostprotocol.Response, error) {
	requestValue, err := hostprotocol.DecodeRequest(request)
	if err != nil || requestValue.Target == nil || requestValue.Target.ReleaseArtifact != manifest.Release {
		return hostprotocol.Response{}, fmt.Errorf("artifact release does not match request")
	}
	machine, err := client.Probe(ctx, alias)
	if err != nil {
		return hostprotocol.Response{}, err
	}
	if machine.OS != "Linux" {
		return hostprotocol.Response{}, fmt.Errorf("unsupported host OS")
	}
	pinned, err := LoadPinned(bundleRoot, manifest, machine.Arch)
	if err != nil {
		return hostprotocol.Response{}, err
	}
	input, err := BootstrapInput(pinned, request)
	if err != nil {
		return hostprotocol.Response{}, err
	}
	return client.Bootstrap(ctx, alias, input)
}

func mustHex(value string) []byte { b, _ := hex.DecodeString(value); return b }

func validEntry(entry Entry) bool {
	if entry.Path == "" || entry.Size < 0 || entry.Size > maxArtifactSize || len(entry.SHA256) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(entry.SHA256)
	return err == nil
}

const (
	maxManifestSize = 64 << 10
	maxArtifactSize = 64 << 20
)
