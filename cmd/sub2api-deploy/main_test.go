package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestValidateCLIUsesFakeSopsAndSSHAndDoesNotPrintSecrets(t *testing.T) {
	root := t.TempDir()
	envDir := filepath.Join(root, "environments", "production")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "config.yaml"), []byte(validConfigForCLI), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "secrets.yaml"), []byte("encrypted-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "sops"), `#!/bin/sh
printf '%s\n' 'revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' 'apps:' '  web-app:' '    initialAdminPassword: CLI_SECRET_SENTINEL' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	sshLog := filepath.Join(root, "ssh.log")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+sshLog+"\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	if err := run(context.Background(), []string{"validate", "production"}, root, nil, &stdout, &stderr); err != nil {
		t.Fatalf("run error = %v, stderr = %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "environment: production") || !strings.Contains(stdout.String(), "servers: 1") || !strings.Contains(stdout.String(), "apps: 1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "CLI_SECRET_SENTINEL") {
		t.Fatal("CLI output exposed a secret")
	}
	sshOutput, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	sshArgs := string(sshOutput)
	if !strings.Contains(sshArgs, "-G -- Edge_Box") || !strings.Contains(sshArgs, "StrictHostKeyChecking=yes") || !strings.Contains(sshArgs, "BatchMode=yes") || !strings.Contains(sshArgs, "ConnectTimeout=10") || !strings.Contains(sshArgs, "-- Edge_Box true") {
		t.Fatalf("fake ssh log = %q", sshOutput)
	}
}

func TestValidateCLIFailsAtSSHWithoutPrintingCommandOutputOrSecrets(t *testing.T) {
	root := t.TempDir()
	envDir := filepath.Join(root, "environments", "production")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "config.yaml"), []byte(validConfigForCLI), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "secrets.yaml"), []byte("encrypted-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "sops"), `#!/bin/sh
printf '%s\n' 'revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' 'apps:' '  web-app:' '    initialAdminPassword: CLI_SECRET_SENTINEL' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' 'fake command output CLI_SECRET_SENTINEL' >&2\nexit 7\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	if err := run(context.Background(), []string{"validate", "production"}, root, nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "Edge_Box") || !strings.Contains(err.Error(), "expand") {
		t.Fatalf("run error = %v", err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "CLI_SECRET_SENTINEL") || strings.Contains(stdout.String()+stderr.String(), "fake command output") {
		t.Fatalf("CLI exposed secret or command output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestValidateCLIChecksSSHAliasInsteadOfServerKey(t *testing.T) {
	root := t.TempDir()
	envDir := filepath.Join(root, "environments", "production")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(validConfigForCLI, "  Edge_Box:\n", "  Edge_Box:\n    sshAlias: deploy-target\n", 1)
	if err := os.WriteFile(filepath.Join(envDir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "secrets.yaml"), []byte("encrypted-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "sops"), `#!/bin/sh
printf '%s\n' 'revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' 'apps:' '  web-app:' '    initialAdminPassword: password' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	sshLog := filepath.Join(root, "ssh.log")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+sshLog+"\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := run(context.Background(), []string{"validate", "production"}, root, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	sshOutput, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sshOutput), "Edge_Box") || !strings.Contains(string(sshOutput), "deploy-target") {
		t.Fatalf("SSH checked server key instead of alias: %q", sshOutput)
	}
}

func TestPublicCLIWiresPulumiProviderAndApproval(t *testing.T) {
	workdir, cli, _, _, logs := pulumiRunFixture(t, pulumiRunSecrets(), "success")

	var stdout, stderr strings.Builder
	if err := run(context.Background(), []string{"pulumi", "production", "preview"}, workdir, func() (string, error) { return cli, nil }, &stdout, &stderr); err != nil {
		t.Fatalf("run error = %v, stderr = %s", err, stderr.String())
	}

	pulumi := pulumiRunRead(t, logs.pulumi)
	for _, want := range []string{
		"cwd=" + workdir,
		"args=<preview><--stack=production><--config-file=",
		"fd3=no",
		"approval=\n",
	} {
		if !strings.Contains(pulumi, want) {
			t.Fatalf("Pulumi invocation missing %q: %s", want, pulumi)
		}
	}
	if _, err := os.Stat(logs.providerStarted); err != nil {
		t.Fatalf("provider did not start: %v", err)
	}
}

func TestPublicCLICancellationReapsAttachedProcessesAndStagedStack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("attached process cancellation is Linux-specific")
	}

	workdir, cli, _, _, logs := pulumiRunFixture(t, pulumiRunSecrets(), "success")
	bin := filepath.Dir(cli)
	providerPID := filepath.Join(filepath.Dir(logs.pulumi), "provider.pid")
	pulumiPID := filepath.Join(filepath.Dir(logs.pulumi), "pulumi.pid")
	providerClosed := filepath.Join(filepath.Dir(logs.pulumi), "provider-closed")
	providerInterrupted := filepath.Join(filepath.Dir(logs.pulumi), "provider-interrupted")
	pulumiCleanup := filepath.Join(filepath.Dir(logs.pulumi), "pulumi-cleanup")
	pulumiReady := filepath.Join(filepath.Dir(logs.pulumi), "pulumi-ready")
	blockingFIFO := filepath.Join(filepath.Dir(logs.pulumi), "pulumi-block")
	stdoutPath := filepath.Join(filepath.Dir(logs.pulumi), "cli.stdout")
	stderrPath := filepath.Join(filepath.Dir(logs.pulumi), "cli.stderr")
	if err := syscall.Mkfifo(blockingFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"SUB2API_TEST_PROVIDER_PID":         providerPID,
		"SUB2API_TEST_PROVIDER_CLOSED":      providerClosed,
		"SUB2API_TEST_PROVIDER_INTERRUPTED": providerInterrupted,
		"SUB2API_TEST_PULUMI_PID":           pulumiPID,
		"SUB2API_TEST_PULUMI_CLEANUP":       pulumiCleanup,
		"SUB2API_TEST_PULUMI_READY":         pulumiReady,
		"SUB2API_TEST_STAGED_PARENT":        logs.stagedParent,
		"SUB2API_TEST_BLOCKING_FIFO":        blockingFIFO,
	} {
		t.Setenv(key, value)
	}

	// The cleanup handler makes the RED version safe: it leaves the attached
	// processes alive after the public CLI dies directly from SIGINT.
	var command *exec.Cmd
	var commandDone <-chan struct{}
	t.Cleanup(func() {
		if command != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		if commandDone != nil {
			select {
			case <-commandDone:
			case <-time.After(time.Second):
			}
		}
		for _, path := range []string{pulumiPID, providerPID} {
			killPublicCLITestProcess(path)
			_ = waitForPublicCLITestProcessExit(path, time.Second)
		}
		if parent := strings.TrimSpace(readPublicCLITestFile(logs.stagedParent)); parent != "" {
			_ = os.RemoveAll(parent)
		}
	})

	writeExecutable(t, filepath.Join(bin, "pulumi-resource-sub2api-host"), `#!/bin/sh
trap 'printf interrupted > "$SUB2API_TEST_PROVIDER_INTERRUPTED"; exit 0' INT TERM
printf '%s\n' "$$" > "$SUB2API_TEST_PROVIDER_PID"
printf '%s\n' 43123
read -r _ <&3
printf closed > "$SUB2API_TEST_PROVIDER_CLOSED"
`)
	writeExecutable(t, filepath.Join(bin, "pulumi"), `#!/bin/sh
trap 'printf interrupted > "$SUB2API_TEST_PULUMI_CLEANUP"; exit 0' INT TERM
for arg; do case "$arg" in --config-file=*) config=${arg#--config-file=};; esac; done
[ -n "$config" ] || exit 91
printf '%s\n' "${config%/*}" > "$SUB2API_TEST_STAGED_PARENT"
printf '%s\n' "$$" > "$SUB2API_TEST_PULUMI_PID"
printf ready > "$SUB2API_TEST_PULUMI_READY"
exec 9<> "$SUB2API_TEST_BLOCKING_FIFO"
read -r _ <&9
`)

	buildContext, cancelBuild := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildContext, "go", "build", "-o", cli, ".")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public CLI: %v\n%s", err, output)
	}

	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close(); _ = stdout.Close() })
	command = exec.Command(cli, "pulumi", "production", "preview")
	command.Dir = workdir
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start public CLI: %v", err)
	}
	done := make(chan struct{})
	commandDone = done
	go func() { _ = command.Wait(); close(done) }()

	if err := waitForPublicCLITestFile(pulumiReady, 5*time.Second); err != nil {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("Pulumi did not reach blocked state: %v; stderr=%s", err, readPublicCLITestFile(stderrPath))
	}
	stagedParent := strings.TrimSpace(pulumiRunRead(t, logs.stagedParent))
	if stagedParent == "" {
		t.Fatal("Pulumi did not record staged stack parent")
	}

	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt public CLI: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("public CLI did not exit after SIGINT; stderr=%s", readPublicCLITestFile(stderrPath))
	}

	for _, path := range []string{pulumiCleanup, providerClosed} {
		if err := waitForPublicCLITestFile(path, 2*time.Second); err != nil {
			t.Fatalf("attached child did not clean up %s: %v; stderr=%s", filepath.Base(path), err, readPublicCLITestFile(stderrPath))
		}
	}
	if _, err := os.Stat(providerInterrupted); !os.IsNotExist(err) {
		t.Fatalf("provider exited from signal instead of approval fd EOF; stderr=%s", readPublicCLITestFile(stderrPath))
	}
	for _, path := range []string{pulumiPID, providerPID} {
		if err := waitForPublicCLITestProcessExit(path, 2*time.Second); err != nil {
			t.Fatalf("attached child did not exit %s: %v; stderr=%s", filepath.Base(path), err, readPublicCLITestFile(stderrPath))
		}
	}
	if _, err := os.Stat(stagedParent); !os.IsNotExist(err) {
		t.Fatalf("staged stack parent remains after SIGINT: %v; stderr=%s", err, readPublicCLITestFile(stderrPath))
	}
}

func waitForPublicCLITestProcessExit(path string, timeout time.Duration) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 1 {
		return os.ErrInvalid
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return os.ErrProcessDone
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func killPublicCLITestProcess(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil && pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func readPublicCLITestFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func waitForPublicCLITestFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return os.ErrNotExist
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecuteReturnsGetwdErrorWithoutPanicking(t *testing.T) {
	var stdout, stderr strings.Builder
	err := execute(context.Background(), []string{"validate", "production"}, func() (string, error) {
		return "", os.ErrNotExist
	}, os.Executable, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("execute error = %v", err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

const validConfigForCLI = `version: 1
reverseProxy:
  image: traefik:v3.3.3
  acmeEmail: ops@example.com
servers:
  Edge_Box:
    addresses:
      internal:
        ipv4: 10.0.0.10
postgres:
  main-db:
    type: docker
    server: Edge_Box
redis:
  main-cache:
    type: docker
    server: Edge_Box
apps:
  web-app:
    hostname: app.example.com
    image: ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    initialAdminEmail: admin@example.com
    readinessPath: /ready
    servers: [Edge_Box]
    postgres:
      name: main-db
      database: sub2api
    redis:
      name: main-cache
      database: 0
    publicAccess:
      type: none
`
