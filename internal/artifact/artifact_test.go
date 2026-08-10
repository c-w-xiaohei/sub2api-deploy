package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPinnedRejectsTraversalSymlinkSizeAndHash(t *testing.T) {
	root := t.TempDir()
	payload := []byte("host binary")
	if err := os.WriteFile(filepath.Join(root, "host-amd64"), payload, 0755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	manifest := Manifest{SchemaVersion: 1, Release: "v1", LinuxAMD64: Entry{Path: "host-amd64", Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}, LinuxARM64: Entry{Path: "host-amd64", Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}}
	if _, err := LoadPinned(root, manifest, "amd64"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"traversal": func(m *Manifest) { m.LinuxAMD64.Path = "../host-amd64" },
		"size":      func(m *Manifest) { m.LinuxAMD64.Size++ },
		"hash":      func(m *Manifest) { m.LinuxAMD64.SHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := manifest
			mutate(&bad)
			if _, err := LoadPinned(root, bad, "amd64"); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	if err := os.Symlink(filepath.Join(root, "host-amd64"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	manifest.LinuxAMD64.Path = "link"
	if _, err := LoadPinned(root, manifest, "amd64"); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "linked-dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "linked-dir", "escape")); err != nil {
		t.Fatal(err)
	}
	manifest.LinuxAMD64.Path = "linked-dir/escape/host-amd64"
	if _, err := LoadPinned(root, manifest, "amd64"); err == nil {
		t.Fatal("intermediate symlink accepted")
	}
}

func TestLoadManifestIsStrictAndTyped(t *testing.T) {
	manifest := Manifest{SchemaVersion: 1, Release: "v1", LinuxAMD64: Entry{Path: "amd64", Size: 1, SHA256: strings.Repeat("a", 64)}, LinuxARM64: Entry{Path: "arm64", Size: 1, SHA256: strings.Repeat("b", 64)}}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(bytes.NewReader(append(b[:len(b)-1], []byte(`,"unknown":true}`)...))); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestLoadManifestRejectsBytesBeyondExactBoundIncludingWhitespace(t *testing.T) {
	manifest := Manifest{SchemaVersion: 1, Release: "v1", LinuxAMD64: Entry{Path: "amd64", Size: 1, SHA256: strings.Repeat("a", 64)}, LinuxARM64: Entry{Path: "arm64", Size: 1, SHA256: strings.Repeat("b", 64)}}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) >= maxManifestSize {
		t.Fatal("fixture unexpectedly exceeds bound")
	}
	if _, err := LoadManifest(bytes.NewReader(append(b, bytes.Repeat([]byte(" "), maxManifestSize-len(b)+1)...))); err == nil {
		t.Fatal("oversize whitespace accepted")
	}
}

func TestBootstrapInputIsRawPinnedBytesThenRequest(t *testing.T) {
	payload := []byte{0, 1, 2, 3}
	digest := sha256.Sum256(payload)
	pinned := &Pinned{bytes: append([]byte(nil), payload...), Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}
	request := []byte("s2h1:3\nxyz")
	got, err := BootstrapInput(pinned, request)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte("s2a1:4:"+hex.EncodeToString(digest[:])+"\n"), payload...)
	want = append(want, request...)
	if !bytes.Equal(got, want) {
		t.Fatalf("input = %x, want %x", got, want)
	}
}

func TestLoadPinnedRejectsSymlinkedRootAncestorAndFinal(t *testing.T) {
	root := t.TempDir()
	payload := []byte("binary")
	digest := sha256.Sum256(payload)
	if err := os.Mkdir(filepath.Join(root, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundle", "host"), payload, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{SchemaVersion: 1, Release: "v1", LinuxAMD64: Entry{Path: "host", Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}, LinuxARM64: Entry{Path: "host", Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}}
	if err := os.Symlink(filepath.Join(root, "bundle"), filepath.Join(root, "root-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPinned(filepath.Join(root, "root-link"), manifest, "amd64"); err == nil {
		t.Fatal("symlinked root accepted")
	}
	if err := os.Symlink(filepath.Join(root, "bundle", "host"), filepath.Join(root, "bundle", "final-link")); err != nil {
		t.Fatal(err)
	}
	manifest.LinuxAMD64.Path = "final-link"
	if _, err := LoadPinned(filepath.Join(root, "bundle"), manifest, "amd64"); err == nil {
		t.Fatal("symlinked final accepted")
	}
}

func TestPinnedUsesVerifiedBytesAfterArtifactMutation(t *testing.T) {
	root := t.TempDir()
	payload := []byte("verified")
	digest := sha256.Sum256(payload)
	if err := os.WriteFile(filepath.Join(root, "host"), payload, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{SchemaVersion: 1, Release: "v1", LinuxAMD64: Entry{Path: "host", Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}, LinuxARM64: Entry{Path: "host", Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}}
	pinned, err := LoadPinned(root, manifest, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "host"), []byte("mutated!"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := BootstrapInput(pinned, []byte("request"))
	if err != nil || !bytes.Contains(got, payload) || bytes.Contains(got, []byte("mutated!")) {
		t.Fatalf("input = %q, err = %v", got, err)
	}
}

func TestReadPinnedReusesDescriptorsWhenRootIsSlash(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("pinned")
	path := filepath.Join(dir, "host")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	relativeRoot := strings.TrimPrefix(dir, "/")
	for i := 0; i != 512; i++ {
		got, err := readPinned("/", filepath.Join(relativeRoot, "host"), int64(len(payload)))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("read %d = %q, %v", i, got, err)
		}
	}
}

func TestReadPinnedClosesDescriptorsWhenNestedTraversalFails(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i != 512; i++ {
		if _, err := readPinned(root, "nested/missing/host", 1); err == nil {
			t.Fatal("missing nested path accepted")
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before)+4 {
		t.Fatalf("nested traversal leaked descriptors: before=%d after=%d", len(before), len(after))
	}
}

func TestArchitectureMappingIsStrict(t *testing.T) {
	for _, arch := range []string{"x86_64", "amd64", "aarch64", "arm64"} {
		if _, err := PlatformFor(arch); err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
	}
	if _, err := PlatformFor("riscv64"); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}
