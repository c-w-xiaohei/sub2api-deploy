package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostresource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/constant"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	environmentConfigKey  = "sub2api-environment:environmentConfig"
	environmentSecretsKey = "sub2api-environment:environmentSecrets"
	secretCanary          = "environment-secret-canary"
	bundleRelease         = "ghcr.io/example/sub2api-deploy@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type callerMocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
	calls     []pulumi.MockCallArgs
}

func TestPulumiPluginDiscovery(t *testing.T) {
	if os.Getenv("SUB2API_ENVIRONMENT_PLUGIN_DISCOVERY_HELPER") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPulumiPluginDiscovery$")
	cmd.Env = append(os.Environ(), "SUB2API_ENVIRONMENT_PLUGIN_DISCOVERY_HELPER=1", "PULUMI_PLUGINS=true")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != constant.ExitStatusLoggedError {
		t.Errorf("plugin discovery exit status = %v, want %d", err, constant.ExitStatusLoggedError)
	}

	var response map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Errorf("plugin discovery stdout is not JSON: %v\nstdout: %s", err, stdout.String())
	} else {
		plugins, ok := response["plugins"]
		if !ok {
			t.Error("plugin discovery JSON has no plugins field")
		} else if trimmedPlugins := bytes.TrimSpace(plugins); bytes.Equal(trimmedPlugins, []byte("null")) {
			// Pulumi emits null when its package registry is empty.
		} else if bytes.HasPrefix(trimmedPlugins, []byte("[")) {
			var providers []json.RawMessage
			if err := json.Unmarshal(plugins, &providers); err != nil {
				t.Errorf("plugin discovery JSON plugins is not an array: %v", err)
			}
		} else {
			t.Error("plugin discovery JSON plugins is neither null nor an array")
		}
	}
	if strings.Contains(stderr.String(), "environment program failed") {
		t.Error("plugin discovery stderr contains generic environment failure")
	}
	if strings.Contains(stderr.String(), secretCanary) {
		t.Error("plugin discovery stderr leaked secret canary")
	}
}

func (m *callerMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, args)
	m.mu.Unlock()
	return args.Name + "-id", args.Inputs, nil
}

func (m *callerMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	m.mu.Lock()
	m.calls = append(m.calls, args)
	m.mu.Unlock()
	return nil, fmt.Errorf("unexpected Pulumi Call %q", args.Token)
}

func TestRegisterReadsConfigAndReleaseFromExecutableBundle(t *testing.T) {
	executable := writeBundle(t, validManifest(bundleRelease))
	mocks := &callerMocks{}
	err := runCaller(t, mocks, executable, map[string]string{
		environmentConfigKey:  environmentConfig,
		environmentSecretsKey: environmentSecrets,
	}, []string{environmentSecretsKey})
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}

	hosts := resourcesOfType(mocks.resources, hostresource.HostToken)
	if len(hosts) != 1 {
		t.Fatalf("Host registrations = %d, want 1", len(hosts))
	}
	target := hosts[0].Inputs["target"].ObjectValue()
	if release := target["releaseArtifact"].StringValue(); release != bundleRelease {
		t.Fatalf("Host target releaseArtifact = %q, want manifest release %q", release, bundleRelease)
	}
	resourceInput := hosts[0].Inputs["resource"].ObjectValue()
	if serverKey := resourceInput["serverKey"].StringValue(); serverKey != "server-a" {
		t.Fatalf("Host resource serverKey = %q, want config server key", serverKey)
	}
	serverInput := hosts[0].Inputs["server"].ObjectValue()
	if alias := serverInput["sshAlias"].StringValue(); alias != "bundle-server-ssh" {
		t.Fatalf("Host server sshAlias = %q, want config SSH alias", alias)
	}
	assertCanaryOnlyInHostSecrets(t, mocks)
}

func TestRegisterRejectsInvalidConfigContractBeforeRegistration(t *testing.T) {
	executable := writeBundle(t, validManifest(bundleRelease))
	for _, tc := range []struct {
		name    string
		config  map[string]string
		secrets []string
	}{
		{"missing ordinary config", map[string]string{environmentSecretsKey: environmentSecrets}, []string{environmentSecretsKey}},
		{"missing secret config", map[string]string{environmentConfigKey: environmentConfig}, nil},
		{"ordinary secrets", map[string]string{environmentConfigKey: environmentConfig, environmentSecretsKey: environmentSecrets}, nil},
		{"secret ordinary config", map[string]string{environmentConfigKey: environmentConfig, environmentSecretsKey: environmentSecrets}, []string{environmentConfigKey, environmentSecretsKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mocks := &callerMocks{}
			err := runCaller(t, mocks, executable, tc.config, tc.secrets)
			if err == nil {
				t.Fatal("register() error = nil, want config-contract rejection")
			}
			if strings.Contains(err.Error(), secretCanary) {
				t.Fatalf("register() error leaked secret canary: %v", err)
			}
			assertRejectedWithoutSideEffects(t, mocks)
		})
	}
}

func TestRegisterDoesNotLeakMalformedSecretConfigInDiagnostics(t *testing.T) {
	executable := writeBundle(t, validManifest(bundleRelease))
	mocks := &callerMocks{}
	err := runCaller(t, mocks, executable, map[string]string{
		environmentConfigKey:  environmentConfig,
		environmentSecretsKey: "reverseProxy: *" + secretCanary,
	}, []string{environmentSecretsKey})
	if err == nil {
		t.Fatal("register() error = nil, want malformed secret config rejection")
	}
	if strings.Contains(err.Error(), secretCanary) {
		t.Fatalf("register() error leaked secret canary: %v", err)
	}
	assertRejectedWithoutSideEffects(t, mocks)
}

func TestRegisterRejectsMalformedExecutableRelativeManifestBeforeRegistration(t *testing.T) {
	installCWDAndPATHDecoy(t, "ghcr.io/example/decoy@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	executable := writeBundle(t, fmt.Sprintf(`{"schemaVersion":1,"release":%q,"linux-amd64":{"path":"bin/sub2api-host-amd64","size":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, bundleRelease))
	mocks := &callerMocks{}
	err := runCaller(t, mocks, executable, map[string]string{
		environmentConfigKey:  environmentConfig,
		environmentSecretsKey: environmentSecrets,
	}, []string{environmentSecretsKey})
	if err == nil {
		t.Fatal("register() error = nil, want malformed executable-relative bundle rejection")
	}
	if strings.Contains(err.Error(), secretCanary) {
		t.Fatalf("register() error leaked secret canary: %v", err)
	}
	assertRejectedWithoutSideEffects(t, mocks)
}

func runCaller(t *testing.T, mocks *callerMocks, executable string, values map[string]string, secretKeys []string) error {
	t.Helper()
	return pulumi.RunErr(func(ctx *pulumi.Context) error {
		return register(ctx, executable)
	}, pulumi.WithMocks("sub2api-environment", "test", mocks), withConfig(values, secretKeys))
}

func withConfig(values map[string]string, secretKeys []string) pulumi.RunOption {
	return func(info *pulumi.RunInfo) {
		info.Config = values
		info.ConfigSecretKeys = secretKeys
	}
}

func writeBundle(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	return writeBundleAt(t, root, manifest)
}

func writeBundleAt(t *testing.T, root, manifest string) string {
	t.Helper()
	manifestPath := filepath.Join(root, "artifacts", "sub2api-host", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "bin", "pulumi-program")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("unused"), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable
}

func installCWDAndPATHDecoy(t *testing.T, release string) {
	t.Helper()
	root := t.TempDir()
	decoyExecutable := writeBundleAt(t, root, validManifest(release))
	t.Chdir(root)
	t.Setenv("PATH", filepath.Dir(decoyExecutable))
}

func validManifest(release string) string {
	return fmt.Sprintf(`{"schemaVersion":1,"release":%q,"linux-amd64":{"path":"bin/sub2api-host-amd64","size":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"linux-arm64":{"path":"bin/sub2api-host-arm64","size":1,"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`, release)
}

func resourcesOfType(resources []pulumi.MockResourceArgs, token string) []pulumi.MockResourceArgs {
	var matches []pulumi.MockResourceArgs
	for _, resource := range resources {
		if resource.TypeToken == token {
			matches = append(matches, resource)
		}
	}
	return matches
}

func assertRejectedWithoutSideEffects(t *testing.T, mocks *callerMocks) {
	t.Helper()
	for _, resource := range mocks.resources {
		assertNoCanary(t, resource.Inputs)
		if resource.TypeToken != "pulumi:pulumi:Stack" {
			t.Fatalf("rejected caller registered %q", resource.TypeToken)
		}
	}
	for _, call := range mocks.calls {
		assertNoCanary(t, call.Args)
	}
	if len(mocks.calls) != 0 {
		t.Fatalf("rejected caller made %d Pulumi Calls: %#v", len(mocks.calls), mocks.calls)
	}
}

func assertCanaryOnlyInHostSecrets(t *testing.T, mocks *callerMocks) {
	t.Helper()
	for _, resource := range mocks.resources {
		if resource.TypeToken != hostresource.HostToken {
			assertNoCanary(t, resource.Inputs)
			continue
		}
		secrets, ok := resource.Inputs["secrets"]
		if !ok || !secrets.IsSecret() || !containsProperty(secrets, secretCanary) {
			t.Fatalf("Host secrets must be secret-marked and contain the canary: %v", secrets)
		}
		for key, value := range resource.Inputs {
			if key != "secrets" && containsProperty(value, secretCanary) {
				t.Fatalf("secret canary leaked into ordinary Host input %q: %v", key, value)
			}
		}
	}
	for _, call := range mocks.calls {
		assertNoCanary(t, call.Args)
	}
}

func assertNoCanary(t *testing.T, values resource.PropertyMap) {
	t.Helper()
	if containsProperty(resource.NewObjectProperty(values), secretCanary) {
		t.Fatalf("secret canary leaked into Pulumi mock input: %v", values)
	}
}

func containsProperty(value resource.PropertyValue, want string) bool {
	if value.IsString() {
		return strings.Contains(value.StringValue(), want)
	}
	if value.IsSecret() {
		return containsProperty(value.SecretValue().Element, want)
	}
	if value.IsComputed() {
		return false
	}
	if value.IsArray() {
		for _, child := range value.ArrayValue() {
			if containsProperty(child, want) {
				return true
			}
		}
	}
	if value.IsObject() {
		for _, child := range value.ObjectValue() {
			if containsProperty(child, want) {
				return true
			}
		}
	}
	return false
}

const environmentConfig = `version: 1
reverseProxy:
  image: traefik:v3.3.3
  acmeEmail: ops@example.com
servers:
  server-a:
    sshAlias: bundle-server-ssh
postgres:
  app-postgres:
    type: docker
    server: server-a
redis:
  app-redis:
    type: docker
    server: server-a
apps:
  app:
    hostname: app.example.com
    image: ghcr.io/example/app@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    initialAdminEmail: admin@example.com
    readinessPath: /ready
    servers: [server-a]
    postgres:
      name: app-postgres
      database: sub2api
    redis:
      name: app-redis
      database: 0
    publicAccess:
      type: none
`

const environmentSecrets = `revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
reverseProxy:
  dnsChallengeToken: ` + secretCanary + `
apps:
  app:
    initialAdminPassword: ` + secretCanary + `
    jwtSecret: ` + secretCanary + `
    totpEncryptionKey: ` + secretCanary + `
    postgres:
      username: sub2api
      password: ` + secretCanary + `
    redis:
      username: default
      password: ` + secretCanary + `
postgres:
  app-postgres:
    adminPassword: ` + secretCanary + `
redis:
  app-redis:
    adminPassword: ` + secretCanary + `
`
