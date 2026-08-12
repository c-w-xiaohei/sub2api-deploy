//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

const (
	maxStagedStackSize = 16 << 20 // Stack configuration should remain small and bounded.
	stagedStackName    = "Pulumi.staged.yaml"
)

type stagedStackSource struct {
	path  string
	bytes []byte
	dev   uint64
	ino   uint64
	size  int64
	mode  uint32
	mtime syscall.Timespec
	ctime syscall.Timespec
}

func readStagedStackSource(path string) (stagedStackSource, error) {
	file, stat, err := openStagedSource(path)
	if err != nil {
		return stagedStackSource{}, errInvalidStagedStack
	}
	defer file.Close()

	contents, err := io.ReadAll(io.LimitReader(file, maxStagedStackSize+1))
	if err != nil || len(contents) > maxStagedStackSize {
		return stagedStackSource{}, errInvalidStagedStack
	}
	var after syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &after); err != nil || !sameStagedSourceStat(stat, &after) || int64(len(contents)) != stat.Size {
		return stagedStackSource{}, errInvalidStagedStack
	}
	return stagedStackSource{
		path: path, bytes: append([]byte(nil), contents...), dev: uint64(stat.Dev), ino: uint64(stat.Ino),
		size: stat.Size, mode: stat.Mode, mtime: stat.Mtim, ctime: stat.Ctim,
	}, nil
}

func withStagedStack(ctx context.Context, project *workspace.Project, sourcePath, passphrase string, values stackConfigValues, operation func(string) error) error {
	if ctx == nil || project == nil || operation == nil {
		return errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := readStagedStackSource(sourcePath)
	if err != nil {
		return errInvalidStagedStack
	}
	return withStagedStackSource(ctx, project, source, passphrase, values, operation)
}

func withStagedStackSource(ctx context.Context, project *workspace.Project, source stagedStackSource, passphrase string, values stackConfigValues, operation func(string) error) (result error) {
	if ctx == nil || project == nil || operation == nil {
		return errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rendered, err := renderStagedStack(ctx, project, source.bytes, source.path, passphrase, values)
	if err != nil {
		return stageContextError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	temporaryRoot, err := validatedStagedTempRoot()
	if err != nil {
		return errInvalidStagedStack
	}
	directory, err := os.MkdirTemp(temporaryRoot, "sub2api-pulumi-stack-")
	if err != nil {
		return errInvalidStagedStack
	}
	defer func() {
		// RemoveAll unlinks symlink entries rather than traversing them.
		if err := os.RemoveAll(directory); result == nil && err != nil {
			result = errInvalidStagedStack
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil || !validStagedDirectory(directory) {
		return errInvalidStagedStack
	}

	finalPath, err := publishStagedStack(directory, rendered)
	if err != nil {
		return errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := revalidateStagedStackSource(source); err != nil {
		return errInvalidStagedStack
	}
	return operation(finalPath)
}

func openStagedSource(path string) (*os.File, *syscall.Stat_t, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || !validStagedSourceStat(&stat) {
		file.Close()
		return nil, nil, errInvalidStagedStack
	}
	return file, &stat, nil
}

func validStagedSourceStat(stat *syscall.Stat_t) bool {
	return stat.Mode&syscall.S_IFMT == syscall.S_IFREG && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid()) && stat.Mode&0o077 == 0 && stat.Size >= 0 && stat.Size <= maxStagedStackSize
}

func sameStagedSourceStat(left, right *syscall.Stat_t) bool {
	return validStagedSourceStat(right) && left.Dev == right.Dev && left.Ino == right.Ino && left.Size == right.Size && left.Mode == right.Mode && left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func revalidateStagedStackSource(source stagedStackSource) error {
	current, err := readStagedStackSource(source.path)
	if err != nil || current.dev != source.dev || current.ino != source.ino || current.size != source.size || current.mode != source.mode || current.mtime != source.mtime || current.ctime != source.ctime || !bytes.Equal(current.bytes, source.bytes) {
		return errInvalidStagedStack
	}
	return nil
}

func validStagedDirectory(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o700 && stat.Uid == uint32(os.Geteuid())
}

func validatedStagedTempRoot() (string, error) {
	temporaryRoot := os.TempDir()
	if temporaryRoot == "" {
		return "", errInvalidStagedStack
	}
	temporaryRoot, err := filepath.Abs(temporaryRoot)
	if err != nil || !filepath.IsAbs(temporaryRoot) {
		return "", errInvalidStagedStack
	}
	temporaryRoot = filepath.Clean(temporaryRoot)

	components := []string{temporaryRoot}
	for parent := filepath.Dir(components[len(components)-1]); parent != components[len(components)-1]; parent = filepath.Dir(parent) {
		components = append(components, parent)
	}
	for index := len(components) - 1; index >= 0; index-- {
		info, err := os.Lstat(components[index])
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errInvalidStagedStack
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || !validStagedTempDirectoryMode(stat) {
			return "", errInvalidStagedStack
		}
	}

	info, err := os.Lstat(temporaryRoot)
	if err != nil {
		return "", errInvalidStagedStack
	}
	want, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errInvalidStagedStack
	}
	fd, err := syscall.Open(temporaryRoot, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", errInvalidStagedStack
	}
	file := os.NewFile(uintptr(fd), temporaryRoot)
	defer file.Close()
	var got syscall.Stat_t
	if err := syscall.Fstat(fd, &got); err != nil || got.Dev != want.Dev || got.Ino != want.Ino {
		return "", errInvalidStagedStack
	}
	return temporaryRoot, nil
}

func validStagedTempDirectoryMode(stat *syscall.Stat_t) bool {
	writableByOthers := stat.Mode&0o022 != 0
	return !writableByOthers || (stat.Uid == 0 && stat.Mode&syscall.S_ISVTX != 0)
}

func publishStagedStack(directory string, rendered []byte) (string, error) {
	temporaryPath := filepath.Join(directory, ".Pulumi.staged.tmp")
	fd, err := syscall.Open(temporaryPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return "", errInvalidStagedStack
	}
	file := os.NewFile(uintptr(fd), temporaryPath)
	temporaryExists := true
	defer func() {
		file.Close()
		if temporaryExists {
			os.Remove(temporaryPath)
		}
	}()
	if err := syscall.Fchmod(fd, 0o600); err != nil {
		return "", errInvalidStagedStack
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || !validStagedOutputStat(&stat, 0) {
		return "", errInvalidStagedStack
	}
	if err := writeStagedStack(file, rendered); err != nil || file.Sync() != nil || file.Close() != nil {
		return "", errInvalidStagedStack
	}
	if err := validateStagedOutput(temporaryPath, &stat, int64(len(rendered))); err != nil {
		return "", errInvalidStagedStack
	}
	finalPath := filepath.Join(directory, stagedStackName)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return "", errInvalidStagedStack
	}
	temporaryExists = false
	if err := validateStagedOutput(finalPath, &stat, int64(len(rendered))); err != nil {
		return "", errInvalidStagedStack
	}
	return finalPath, nil
}

func writeStagedStack(file *os.File, contents []byte) error {
	for len(contents) > 0 {
		written, err := file.Write(contents)
		if err != nil || written == 0 {
			return errInvalidStagedStack
		}
		contents = contents[written:]
	}
	return nil
}

func validateStagedOutput(path string, expected *syscall.Stat_t, size int64) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errInvalidStagedStack
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errInvalidStagedStack
	}
	file := os.NewFile(uintptr(fd), path)
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		file.Close()
		return errInvalidStagedStack
	}
	defer file.Close()
	if !validStagedOutputStat(&stat, size) || stat.Dev != expected.Dev || stat.Ino != expected.Ino {
		return errInvalidStagedStack
	}
	return nil
}

func validStagedOutputStat(stat *syscall.Stat_t, size int64) bool {
	return stat.Mode&syscall.S_IFMT == syscall.S_IFREG && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid()) && stat.Mode&0o777 == 0o600 && stat.Size == size
}

func stageContextError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return errInvalidStagedStack
}
