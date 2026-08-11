//go:build linux

package artifact

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLoadBundleLoadsExactProductionManifestLayout(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, productionManifestJSON)

	bundle, err := LoadBundle(root)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Root != root {
		t.Fatalf("root = %q, want %q", bundle.Root, root)
	}
	if bundle.Manifest != validProductionManifest() {
		t.Fatalf("manifest = %#v, want %#v", bundle.Manifest, validProductionManifest())
	}
}

func TestLoadBundleRejectsUnsafeManifestFile(t *testing.T) {
	for name, write := range map[string]func(t *testing.T, root string){
		"symlink": func(t *testing.T, root string) {
			target := filepath.Join(root, "elsewhere.json")
			if err := os.WriteFile(target, []byte(productionManifestJSON), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "manifest.json")); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "manifest.json"), 0700); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, root string) {
			path := filepath.Join(root, "manifest.json")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err == nil {
					_, _ = file.WriteString(productionManifestJSON)
					file.Close()
				}
			}()
			t.Cleanup(func() {
				file, err := os.OpenFile(path, os.O_RDWR, 0)
				if err == nil {
					file.Close()
				}
				select {
				case <-writerDone:
				case <-time.After(time.Second):
					t.Error("FIFO writer did not exit")
				}
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root)
			if _, err := LoadBundle(root); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
}

func TestLoadBundleRejectsSymlinkedRootAndAncestor(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real", "bundle")
	if err := os.MkdirAll(realRoot, 0700); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, realRoot, productionManifestJSON)

	if err := os.Symlink(realRoot, filepath.Join(base, "root-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(filepath.Join(base, "root-link")); err == nil {
		t.Fatal("symlinked bundle root accepted")
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "ancestor-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(filepath.Join(base, "ancestor-link", "bundle")); err == nil {
		t.Fatal("symlinked bundle-root ancestor accepted")
	}
}
