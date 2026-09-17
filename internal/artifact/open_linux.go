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
