//go:build linux

package artifact

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func readPinned(root, relative string, size int64) ([]byte, error) {
	if !validRelativePath(relative) {
		return nil, fmt.Errorf("artifact escapes bundle root")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("invalid bundle root")
	}
	base, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("invalid bundle root")
	}
	rootFD, err := openDirectories(base, strings.Split(strings.TrimPrefix(root, "/"), "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid bundle root")
	}
	defer syscall.Close(rootFD)
	dirFD := rootFD
	parts := strings.Split(relative, "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("artifact escapes bundle root")
		}
		next, e := syscall.Openat(dirFD, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return nil, fmt.Errorf("artifact path contains symlink")
		}
		defer syscall.Close(next)
		dirFD = next
	}
	name := parts[len(parts)-1]
	if name == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("artifact escapes bundle root")
	}
	fileFD, err := syscall.Openat(dirFD, name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open artifact")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fileFD, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Size != size || size > maxArtifactSize {
		syscall.Close(fileFD)
		return nil, fmt.Errorf("artifact size mismatch")
	}
	file := os.NewFile(uintptr(fileFD), "artifact")
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, size+1))
}

func LoadBundle(root string) (Bundle, error) {
	rootFD, err := openBundleRoot(root)
	if err != nil {
		return Bundle{}, fmt.Errorf("invalid artifact bundle")
	}
	defer syscall.Close(rootFD)

	fileFD, err := syscall.Openat(rootFD, "manifest.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Bundle{}, fmt.Errorf("invalid artifact bundle")
	}
	file := os.NewFile(uintptr(fileFD), "manifest.json")
	defer file.Close()

	var stat syscall.Stat_t
	if err := syscall.Fstat(fileFD, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Size < 0 || stat.Size > maxManifestSize {
		return Bundle{}, fmt.Errorf("invalid artifact bundle")
	}
	manifest, err := LoadManifest(io.LimitReader(file, maxManifestSize+1))
	if err != nil {
		return Bundle{}, fmt.Errorf("invalid artifact bundle")
	}
	return Bundle{Root: root, Manifest: manifest}, nil
}

func openBundleRoot(root string) (int, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return -1, err
	}
	base, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return -1, err
	}
	return openDirectories(base, strings.Split(strings.TrimPrefix(absolute, "/"), "/"))
}

func openDirectories(start int, parts []string) (int, error) {
	fd := start
	for _, part := range parts {
		if part == "" {
			continue
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			syscall.Close(fd)
			return -1, err
		}
		syscall.Close(fd)
		fd = next
	}
	return fd, nil
}
