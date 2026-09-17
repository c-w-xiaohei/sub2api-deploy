package artifact

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestLoadManifestLoadsExactProductionJSONSchema(t *testing.T) {
	bundle, err := LoadManifest(strings.NewReader(productionManifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	if bundle != validProductionManifest() {
		t.Fatalf("manifest = %#v, want %#v", bundle, validProductionManifest())
	}
}

func TestLoadManifestRejectsUnsafeOrAmbiguousEntries(t *testing.T) {
	for name, mutate := range map[string]func(*Entry){
		"uppercase sha256":  func(entry *Entry) { entry.SHA256 = strings.Repeat("A", 64) },
		"absolute path":     func(entry *Entry) { entry.Path = "/sub2api-host" },
		"parent traversal":  func(entry *Entry) { entry.Path = "../sub2api-host" },
		"nested traversal":  func(entry *Entry) { entry.Path = "bin/../sub2api-host" },
		"current directory": func(entry *Entry) { entry.Path = "./sub2api-host" },
		"empty component":   func(entry *Entry) { entry.Path = "bin//host" },
	} {
		for _, architecture := range []string{"amd64", "arm64"} {
			t.Run(name+"/"+architecture, func(t *testing.T) {
				manifest := validProductionManifest()
				entry := &manifest.LinuxAMD64
				if architecture == "arm64" {
					entry = &manifest.LinuxARM64
				}
				mutate(entry)
				if _, err := LoadManifest(bytes.NewReader(mustJSON(t, manifest))); err == nil {
					t.Fatal("unsafe or ambiguous manifest accepted")
				}
			})
		}
	}
	manifest := validProductionManifest()
	manifest.LinuxARM64.Path = manifest.LinuxAMD64.Path
	if _, err := LoadManifest(bytes.NewReader(mustJSON(t, manifest))); err == nil {
		t.Fatal("identical architecture paths accepted")
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

func validProductionManifest() Manifest {
	return Manifest{
		SchemaVersion: 1,
		Release:       "v1.2.3",
		LinuxAMD64: Entry{
			Path:   "sub2api-host-linux-amd64",
			Size:   1024,
			SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		LinuxARM64: Entry{
			Path:   "sub2api-host-linux-arm64",
			Size:   2048,
			SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}
}

const productionManifestJSON = `{
  "schemaVersion": 1,
  "release": "v1.2.3",
  "linux-amd64": {
    "path": "sub2api-host-linux-amd64",
    "size": 1024,
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  },
  "linux-arm64": {
    "path": "sub2api-host-linux-arm64",
    "size": 2048,
    "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  }
}`

func writeManifest(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
