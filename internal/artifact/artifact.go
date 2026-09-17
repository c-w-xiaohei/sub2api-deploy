// Package artifact validates release manifest identity and bundle provenance.
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

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
type Bundle struct {
	Root     string
	Manifest Manifest
}

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
	if manifest.SchemaVersion != 1 || manifest.Release == "" || !validEntry(manifest.LinuxAMD64) || !validEntry(manifest.LinuxARM64) || manifest.LinuxAMD64.Path == manifest.LinuxARM64.Path {
		return Manifest{}, fmt.Errorf("invalid artifact manifest")
	}
	return manifest, nil
}

func validEntry(entry Entry) bool {
	if !validRelativePath(entry.Path) || entry.Size < 0 || entry.Size > maxArtifactSize || len(entry.SHA256) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(entry.SHA256)
	return err == nil && strings.ToLower(entry.SHA256) == entry.SHA256 && len(decoded) == sha256.Size
}

func validRelativePath(value string) bool {
	if value == "" || path.IsAbs(value) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

const (
	maxManifestSize = 64 << 10
	maxArtifactSize = 64 << 20
)
