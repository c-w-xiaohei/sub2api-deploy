package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/encoding"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"gopkg.in/yaml.v3"
)

func TestWithStagedStackStagesPrivateRenderedFileAndCleansUp(t *testing.T) {
	project, source, manager, _ := stagedStackFixture(t)
	sourcePath := writeStagedStackSource(t, source)
	before := stagedStackSourceIdentity(t, sourcePath)
	sourceStack := loadStagedStack(t, project, source)
	sourceCiphertext := stagedStackConfigText(t, sourceStack, "sub2api-environment:unrelatedSecret", config.NopDecrypter)
	tmpRoot := stagedStackTempRoot(t)

	called := 0
	stagedParent := ""
	err := withStagedStack(context.Background(), project, sourcePath, stagePassphrase, stagedStackValues(), func(stagedPath string) error {
		called++
		if stagedPath == sourcePath {
			t.Fatal("operation received source path")
		}
		parent := filepath.Dir(stagedPath)
		stagedParent = parent
		assertStagedStackDirectoryMode(t, parent)
		assertStagedStackFileMode(t, stagedPath)
		assertStagedStackParentContainsOnly(t, parent, filepath.Base(stagedPath))

		rendered, err := os.ReadFile(stagedPath)
		if err != nil {
			t.Fatal("could not read staged file")
		}
		stack := loadLifecycleStagedStack(t, project, rendered)
		assertStackValue(t, stack, "sub2api-environment:environmentConfig", false, stageEnvironment, config.NopDecrypter)
		assertStagedStackProtectedValue(t, stack, "sub2api-environment:environmentSecrets", stageSecrets, manager.Decrypter())
		assertStagedStackProtectedValue(t, stack, "sub2api-host:revisionKey", stageRevision, manager.Decrypter())
		assertStackValue(t, stack, "sub2api-environment:unrelated", false, stageUnrelatedValue, config.NopDecrypter)
		assertStagedStackProtectedValue(t, stack, "sub2api-environment:unrelatedSecret", stageUnrelatedSecret, manager.Decrypter())
		if got := stagedStackConfigText(t, stack, "sub2api-environment:unrelatedSecret", config.NopDecrypter); got != sourceCiphertext {
			t.Fatal("unrelated secure ciphertext changed")
		}
		if stack.EncryptionSalt != sourceStack.EncryptionSalt || stack.SecretsProvider != "" || stack.EncryptedKey != "" {
			t.Fatal("staged stack secrets metadata changed")
		}
		assertRenderedEnvironment(t, stack)
		unknown := mappingValue(t, documentMapping(t, loadYAMLNode(t, rendered)), "unknownTopLevel")
		if unknown.Kind != yaml.ScalarNode || unknown.Value != stageUnknownTopLevel {
			t.Fatal("unknown top-level value changed")
		}
		for _, canary := range []string{stagePassphrase, stageSecrets, stageRevision} {
			if bytes.Contains(rendered, []byte(canary)) {
				t.Fatal("staged file exposed a protected value")
			}
		}
		return nil
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageSecrets, stageRevision)
	if err != nil {
		t.Fatal("withStagedStack unexpectedly failed")
	}
	if called != 1 {
		t.Fatalf("operation calls = %d, want 1", called)
	}
	assertStagedStackSourceUnchanged(t, sourcePath, source, before)
	assertStagedStackDirectoryAbsent(t, stagedParent)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func TestWithStagedStackReturnsOperationErrorAndCleansUp(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	sourcePath := writeStagedStackSource(t, source)
	before := stagedStackSourceIdentity(t, sourcePath)
	tmpRoot := stagedStackTempRoot(t)
	operationErr := errors.New("operation failed")
	stagedParent := ""

	err := withStagedStack(context.Background(), project, sourcePath, stagePassphrase, stagedStackValues(), func(stagedPath string) error {
		stagedParent = filepath.Dir(stagedPath)
		return operationErr
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageSecrets, stageRevision)
	if !errors.Is(err, operationErr) {
		t.Fatal("withStagedStack did not preserve operation error")
	}
	assertStagedStackSourceUnchanged(t, sourcePath, source, before)
	assertStagedStackDirectoryAbsent(t, stagedParent)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func TestWithStagedStackCleansUpAfterRenderFailure(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	sourcePath := writeStagedStackSource(t, source)
	before := stagedStackSourceIdentity(t, sourcePath)
	tmpRoot := stagedStackTempRoot(t)

	called := false
	err := withStagedStack(context.Background(), project, sourcePath, stageWrongPassphrase, stagedStackValues(), func(string) error {
		called = true
		return nil
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageWrongPassphrase, stageSecrets, stageRevision)
	if !errors.Is(err, errInvalidStagedStack) {
		t.Fatal("withStagedStack did not reject wrong passphrase")
	}
	if called {
		t.Fatal("operation called after render failure")
	}
	assertStagedStackSourceUnchanged(t, sourcePath, source, before)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func TestWithStagedStackRejectsSymlinkSource(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	directory := t.TempDir()
	targetPath := filepath.Join(directory, "real-stack.yaml")
	if err := os.WriteFile(targetPath, source, 0o600); err != nil {
		t.Fatal("could not write symlink target")
	}
	targetBefore := stagedStackSourceIdentity(t, targetPath)
	sourcePath := filepath.Join(directory, "Pulumi.production.yaml")
	if err := os.Symlink(targetPath, sourcePath); err != nil {
		t.Fatal("could not create source symlink")
	}
	tmpRoot := stagedStackTempRoot(t)

	called := false
	err := withStagedStack(context.Background(), project, sourcePath, stagePassphrase, stagedStackValues(), func(string) error {
		called = true
		return nil
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageSecrets, stageRevision)
	if !errors.Is(err, errInvalidStagedStack) {
		t.Fatal("withStagedStack did not reject symlink source")
	}
	if called {
		t.Fatal("operation called for symlink source")
	}
	assertStagedStackSourceUnchanged(t, targetPath, source, targetBefore)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func TestWithStagedStackRejectsWorldWritableNonStickyTempRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux filesystem permissions")
	}
	project, source, _, _ := stagedStackFixture(t)
	sourcePath := writeStagedStackSource(t, source)
	before := stagedStackSourceIdentity(t, sourcePath)
	tmpRoot := t.TempDir()
	if err := os.Chmod(tmpRoot, 0o777); err != nil {
		t.Fatal("could not make temp root world writable")
	}
	t.Cleanup(func() {
		if err := os.Chmod(tmpRoot, 0o700); err != nil {
			t.Fatal("could not restore temp root mode")
		}
	})
	t.Setenv("TMPDIR", tmpRoot)

	called := false
	err := withStagedStack(context.Background(), project, sourcePath, stagePassphrase, stagedStackValues(), func(string) error {
		called = true
		return nil
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageSecrets, stageRevision)
	if !errors.Is(err, errInvalidStagedStack) {
		t.Fatal("withStagedStack did not reject hostile temp root")
	}
	if called {
		t.Fatal("operation called with hostile temp root")
	}
	assertStagedStackSourceUnchanged(t, sourcePath, source, before)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func TestWithStagedStackSourceRejectsChangedSnapshot(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	sourcePath := writeStagedStackSource(t, source)
	before := stagedStackSourceIdentity(t, sourcePath)
	snapshot, err := readStagedStackSource(sourcePath)
	if err != nil {
		t.Fatal("could not snapshot source")
	}
	changed := append(append([]byte(nil), source...), []byte("# changed after snapshot\n")...)
	if err := os.WriteFile(sourcePath, changed, 0o600); err != nil {
		t.Fatal("could not change source after snapshot")
	}
	tmpRoot := stagedStackTempRoot(t)

	called := false
	err = withStagedStackSource(context.Background(), project, snapshot, stagePassphrase, stagedStackValues(), func(string) error {
		called = true
		return nil
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageSecrets, stageRevision)
	if !errors.Is(err, errInvalidStagedStack) {
		t.Fatal("withStagedStackSource did not reject changed snapshot")
	}
	if called {
		t.Fatal("operation called after source changed")
	}
	assertStagedStackSourceChangedOnlyAsTestRequested(t, sourcePath, changed, before)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func TestWithStagedStackHonorsPreCancelledContext(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	sourcePath := writeStagedStackSource(t, source)
	before := stagedStackSourceIdentity(t, sourcePath)
	tmpRoot := stagedStackTempRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	err := withStagedStack(ctx, project, sourcePath, stagePassphrase, stagedStackValues(), func(string) error {
		called = true
		return nil
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageSecrets, stageRevision)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("withStagedStack did not preserve cancelled context")
	}
	if called {
		t.Fatal("operation called with cancelled context")
	}
	assertStagedStackSourceUnchanged(t, sourcePath, source, before)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func TestWithStagedStackCleansUpWhenOperationCancelsContext(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	sourcePath := writeStagedStackSource(t, source)
	before := stagedStackSourceIdentity(t, sourcePath)
	tmpRoot := stagedStackTempRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	called := false
	stagedParent := ""
	err := withStagedStack(ctx, project, sourcePath, stagePassphrase, stagedStackValues(), func(stagedPath string) error {
		called = true
		stagedParent = filepath.Dir(stagedPath)
		assertStagedStackFileMode(t, stagedPath)
		cancel()
		return context.Canceled
	})
	assertStagedStackErrorRedacted(t, err, stagePassphrase, stageSecrets, stageRevision)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("withStagedStack did not preserve operation cancellation")
	}
	if !called {
		t.Fatal("operation was not called")
	}
	assertStagedStackSourceUnchanged(t, sourcePath, source, before)
	assertStagedStackDirectoryAbsent(t, stagedParent)
	assertStagedStackTempRootEmpty(t, tmpRoot)
}

func stagedStackTempRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	return root
}

func writeStagedStackSource(t *testing.T, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Pulumi.production.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal("could not write source")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal("could not set source mode")
	}
	return path
}

func stagedStackSourceIdentity(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal("could not stat source")
	}
	return info
}

func assertStagedStackDirectoryMode(t *testing.T, path string) {
	t.Helper()
	info := stagedStackSourceIdentity(t, path)
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("staged parent mode = %v, want directory mode 0700", info.Mode())
	}
}

func assertStagedStackFileMode(t *testing.T, path string) {
	t.Helper()
	info := stagedStackSourceIdentity(t, path)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged file mode = %v, want regular non-symlink mode 0600", info.Mode())
	}
	if runtime.GOOS == "linux" {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("staged file did not provide Linux file metadata")
		}
		if stat.Nlink != 1 {
			t.Fatalf("staged file link count = %d, want 1", stat.Nlink)
		}
	}
}

func assertStagedStackParentContainsOnly(t *testing.T, parent, want string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal("could not read staged parent")
	}
	if len(entries) != 1 || entries[0].Name() != want {
		t.Fatal("staged parent did not contain only final staged file")
	}
}

func assertStagedStackSourceUnchanged(t *testing.T, path string, want []byte, before os.FileInfo) {
	t.Helper()
	after := stagedStackSourceIdentity(t, path)
	if !os.SameFile(before, after) {
		t.Fatal("source identity changed")
	}
	if after.Mode() != before.Mode() {
		t.Fatal("source mode changed")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not read source")
	}
	if !bytes.Equal(got, want) {
		t.Fatal("source bytes changed")
	}
}

func assertStagedStackSourceChangedOnlyAsTestRequested(t *testing.T, path string, want []byte, before os.FileInfo) {
	t.Helper()
	after := stagedStackSourceIdentity(t, path)
	if !os.SameFile(before, after) || after.Mode() != before.Mode() {
		t.Fatal("source identity or mode changed unexpectedly")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("source changed beyond test mutation")
	}
}

func assertStagedStackTempRootEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal("could not read staging temp root")
	}
	if len(entries) != 0 {
		t.Fatal("staging temp root was not cleaned up")
	}
}

func assertStagedStackDirectoryAbsent(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("operation did not provide a staged parent")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staged parent was not removed")
	}
}

func loadLifecycleStagedStack(t *testing.T, project *workspace.Project, source []byte) *workspace.ProjectStack {
	t.Helper()
	stack, err := workspace.LoadProjectStackBytes(nil, project, source, stageSourcePath, encoding.YAML)
	if err != nil {
		t.Fatal("staged stack could not be parsed")
	}
	return stack
}

func assertStagedStackProtectedValue(t *testing.T, stack *workspace.ProjectStack, key, want string, decrypter config.Decrypter) {
	t.Helper()
	value, ok := stack.Config[config.MustParseKey(key)]
	if !ok || !value.Secure() {
		t.Fatalf("protected stack config %q is missing or not secure", key)
	}
	got, err := value.Value(decrypter)
	if err != nil || got != want {
		t.Fatalf("protected stack config %q did not decrypt as expected", key)
	}
}

func stagedStackConfigText(t *testing.T, stack *workspace.ProjectStack, key string, decrypter config.Decrypter) string {
	t.Helper()
	value, ok := stack.Config[config.MustParseKey(key)]
	if !ok {
		t.Fatalf("stack config missing %q", key)
	}
	text, err := value.Value(decrypter)
	if err != nil {
		t.Fatalf("could not read stack config %q", key)
	}
	return text
}

func assertStagedStackErrorRedacted(t *testing.T, err error, canaries ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, canary := range canaries {
		if strings.Contains(err.Error(), canary) {
			t.Fatal("staging error exposed a protected value")
		}
	}
}
