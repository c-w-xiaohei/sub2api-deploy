package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
printf '%s\n' 'apps:' '  web-app:' '    initialAdminPassword: CLI_SECRET_SENTINEL' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	sshLog := filepath.Join(root, "ssh.log")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+sshLog+"\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	if err := run([]string{"validate", "production"}, root, &stdout, &stderr); err != nil {
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
printf '%s\n' 'apps:' '  web-app:' '    initialAdminPassword: CLI_SECRET_SENTINEL' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' 'fake command output CLI_SECRET_SENTINEL' >&2\nexit 7\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	if err := run([]string{"validate", "production"}, root, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "Edge_Box") || !strings.Contains(err.Error(), "expand") {
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
printf '%s\n' 'apps:' '  web-app:' '    initialAdminPassword: password' '    jwtSecret: jwt' '    totpEncryptionKey: totp' '    postgres:' '      username: app' '      password: pass' '    redis:' '      username: default' '      password: pass' 'postgres:' '  main-db:' '    adminPassword: pass' 'redis:' '  main-cache:' '    adminPassword: pass' 'reverseProxy:' '  dnsChallengeToken: dns'
`)
	sshLog := filepath.Join(root, "ssh.log")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+sshLog+"\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := run([]string{"validate", "production"}, root, io.Discard, io.Discard); err != nil {
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

func TestExecuteReturnsGetwdErrorWithoutPanicking(t *testing.T) {
	var stdout, stderr strings.Builder
	err := execute([]string{"validate", "production"}, func() (string, error) {
		return "", os.ErrNotExist
	}, &stdout, &stderr)
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
