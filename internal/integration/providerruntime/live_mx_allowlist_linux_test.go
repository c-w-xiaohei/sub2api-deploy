//go:build linux

package providerruntime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/openssh"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	providerRuntimeLiveGate   = "SUB2API_PROVIDER_RUNTIME_LIVE"
	providerRuntimeLiveHelper = "SUB2API_PROVIDER_RUNTIME_LIVE_HELPER"
	liveCommandTimeout        = 15 * time.Second
	liveFinalMilestone        = "redis-ready"
)

var liveMilestones = [...]string{"postgres-owned-container", "postgres-ready", "redis-owned-container", liveFinalMilestone}

// TestProviderRuntimeCrossHostDataAdmissionLive is CI-only. Its live body is
// re-executed in one private mount and network namespace; testonly.Serve and
// ssh-runtime.sh are never used at this boundary.
func TestProviderRuntimeCrossHostDataAdmissionLive(t *testing.T) {
	if os.Getenv(providerRuntimeLiveHelper) == "1" {
		runProviderRuntimeLiveNamespace(t)
		return
	}
	if os.Getenv(providerRuntimeLiveGate) != "1" {
		t.Skip("CI-only live Provider Runtime test")
	}
	artifacts := requireLiveArtifacts(t)
	trace := requireLiveTraceDirectory(t)
	root := t.TempDir()
	fixture := newLiveFixture(root, trace, artifacts)
	prepareLiveSSH(t, &fixture)
	t.Cleanup(func() { fixture.cleanupOuter(t) })

	script := filepath.Join(repositoryRoot(t), "internal", "integration", "providerruntime", "testdata", "live-runtime.sh")
	ctx, cancel := context.WithTimeout(t.Context(), 9*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "--mount", "--net", "--propagation", "private", script, os.Args[0], "-test.run", "^TestProviderRuntimeCrossHostDataAdmissionLive$", "-test.count=1")
	cmd.Env = append(os.Environ(), fixture.environment()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 4 * time.Minute
	stdout, stderr := newLiveStdoutCapture(), newLiveRecordCapture()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	recordsOK := stderr.forward(os.Stderr)
	if err != nil {
		reportLiveNamespaceFailure(liveFailureCategory(ctx, stderr.failureBytes(stdout.failureBytes(nil))))
		t.Fatal("live namespace fixture failed")
	}
	if !recordsOK {
		t.Fatal("live namespace observer records invalid")
	}
	if ctx.Err() != nil {
		t.Fatal("live namespace fixture timed out")
	}
}

// runProviderRuntimeLiveNamespace runs only after live-runtime.sh has entered
// the private namespaces, mounted Host paths, loaded images, and started sshd.
func runProviderRuntimeLiveNamespace(t *testing.T) {
	artifacts := requireLiveArtifacts(t)
	trace := requireLiveTraceDirectory(t)
	fixture := newLiveFixture(os.Getenv("SUB2API_PROVIDER_RUNTIME_LIVE_ROOT"), trace, artifacts)
	if fixture.root == "" || !filepath.IsAbs(fixture.root) {
		t.Fatal("isolated live root is required")
	}
	t.Cleanup(func() { fixture.cleanupNamespace(t) })
	reportLiveStage("namespace-prerequisites")
	fixture.requireImages(t)
	fixture.addForeignSentinel(t)
	foreignBefore := fixture.nftDigest(t, fixture.foreignTable)

	reportLiveStage("provider-start")
	provider := startLiveProvider(t, artifacts.provider)
	t.Cleanup(func() { provider.close(t) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	reportLiveStage("provider-configure")
	if _, err := provider.client.Configure(ctx, &pulumirpc.ConfigureRequest{Args: rpcProperties(t, property.NewMap(map[string]property.Value{"revisionKey": property.New(ciKey).WithSecret(true)}))}); err != nil {
		t.Fatal("released Provider configure failed")
	}
	dataInput := liveDataInputs(artifacts.release, fixture.dataIP, fixture.appIP)
	reportLiveStage("data-ssh-probe")
	if _, err := openssh.New().Probe(ctx, "live-data"); err != nil {
		reportLiveStage(liveSSHProbeFailureStage(err))
		t.Fatal("live data SSH probe failed")
	}
	if !liveSSHDockerPreflight(t, ctx, "live-data") {
		t.Fatal("live SSH Docker preflight failed")
	}
	reportLiveStage("data-create")
	resource, target, secrets, ok := liveCheckpointInputs(dataInput)
	key, keyErr := base64.StdEncoding.DecodeString(ciKey)
	revision, revisionErr := hostcontract.TargetRevision(hostcontract.RevisionKey(key), resource, target, secrets)
	prior, priorErr := hostcontract.TargetRevision(hostcontract.RevisionKey(key), resource, hostcontract.Target{ReleaseArtifact: target.ReleaseArtifact}, hostcontract.Secrets{})
	if !ok || keyErr != nil || revisionErr != nil || priorErr != nil || resource != (hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}) {
		t.Fatal("invalid non-secret data observer facts")
	}
	observerFacts := liveDataObserverFacts{resource: resource, revision: revision, priorRevision: prior, machine: liveMachineIdentity("data"), release: target.ReleaseArtifact, services: target.DataServices}
	observer := fixture.startLiveDataMilestoneObserver(ctx, observerFacts)
	dataCreated, err := provider.client.Create(ctx, &pulumirpc.CreateRequest{Urn: "urn:pulumi:live::mx-allowlist::sub2api-host:index:Host::data", Properties: rpcProperties(t, dataInput)})
	observerResult := observer.stop()
	if err != nil || dataCreated == nil || dataCreated.Id == "" {
		reportLivePostCreateSnapshot(ctx, &fixture, observerFacts)
		reportLiveStage(liveDataCreateFailureStage(err))
		reportLiveObserverStatus(liveCreateObserverStatus(false, observerResult))
		t.Fatal("released Provider Create failed")
	}
	ownership, ownershipOK := liveCreateOwnership(unmarshalProperties(t, dataCreated.Properties))
	observerResult = livePostCreateCompletion(true, ownership, ownershipOK, observerResult, func(ownership string, result liveMilestoneObserverResult) liveMilestoneObserverResult {
		return fixture.liveDataCompletionObserved(ctx, observerFacts, ownership, result)
	})
	if !liveDataCreateObserved(observerResult) {
		reportLiveObserverStatus(liveCreateObserverStatus(true, observerResult))
		t.Fatal("live milestone observer insufficient evidence")
	}
	reportLiveStage("data-ready-check")
	dataHostPass := fixture.createReady(t, dataCreated, dataInput, liveMachineIdentity("data"), artifacts.release, []hostcontract.AppObservation{}, true)
	fixture.recordOwnedTable(t, "data")
	appInput := liveAppInputs(artifacts.release, fixture.dataIP)
	reportLiveStage("app-create")
	appCreated, err := provider.client.Create(ctx, &pulumirpc.CreateRequest{Urn: "urn:pulumi:live::mx-allowlist::sub2api-host:index:Host::app", Properties: rpcProperties(t, appInput)})
	if err != nil || appCreated == nil || appCreated.Id == "" {
		t.Fatal("released App Host Create failed")
	}
	reportLiveStage("app-ready-check")
	appHostPass := fixture.createReady(t, appCreated, appInput, liveMachineIdentity("app"), artifacts.release, []hostcontract.AppObservation{{ID: "api", ActiveImage: "sub2api-live-app:mx-allowlist", Ready: true}}, false)

	reportLiveStage("post-create-assertions")
	postgresPass := fixture.postgresClient(t, fixture.appNS, "LivePgClient_123")
	postgresWrongPasswordDenied := fixture.postgresClientFails(t, fixture.appNS, "wrong-password")
	postgresCatalog := fixture.postgresCatalog(t)
	redisPass := fixture.redisClient(t, fixture.appNS, "LiveRedisClient_123")
	redisWrongPasswordDenied := fixture.redisClientFails(t, fixture.appNS, "wrong-password")
	redisDefaultDenied := fixture.redisDefaultDenied(t, fixture.appNS)
	redisACL := fixture.redisACL(t)
	postgresDrop := fixture.socketDropped(t, fixture.badNS, "5432") && fixture.exactDataAdmissionPolicy(t, "5432")
	redisDrop := fixture.socketDropped(t, fixture.badNS, "6379") && fixture.exactDataAdmissionPolicy(t, "6379")
	foreignAfter := fixture.nftDigest(t, fixture.foreignTable)
	foreignUnchanged := foreignBefore == foreignAfter
	appEnvironmentAuthenticated := appHostPass && fixture.appContainerReady(t)
	allChecksPass := dataHostPass &&
		appHostPass &&
		appEnvironmentAuthenticated &&
		postgresPass &&
		postgresWrongPasswordDenied &&
		postgresCatalog &&
		redisPass &&
		redisWrongPasswordDenied &&
		redisDefaultDenied &&
		redisACL &&
		postgresDrop &&
		redisDrop &&
		foreignUnchanged
	writeLiveEvidence(t, trace, liveEvidence{
		Test:                            t.Name(),
		ProviderSHA256:                  artifacts.providerSHA256,
		HostAMD64SHA256:                 artifacts.hostSHA256,
		ReleasedBoundary:                artifacts.release,
		DataHostPass:                    dataHostPass,
		AppHostPass:                     appHostPass,
		AppDataEnvironmentAuthenticated: appEnvironmentAuthenticated,
		AppReadyAfterData:               appHostPass,
		PostgresPass:                    postgresPass,
		PostgresWrongPasswordDenied:     postgresWrongPasswordDenied,
		PostgresCatalog:                 postgresCatalog,
		RedisPass:                       redisPass,
		RedisWrongPasswordDenied:        redisWrongPasswordDenied,
		RedisDefaultDenied:              redisDefaultDenied,
		RedisACL:                        redisACL,
		PostgresDrop:                    postgresDrop,
		RedisDrop:                       redisDrop,
		ForeignTableUnchanged:           foreignUnchanged,
		ForeignTableSHA256:              foreignAfter,
	})
	if !allChecksPass {
		t.Fatal("live MX-ALLOWLIST-01 assertion failed")
	}
	reportLiveStage("complete")
}

type liveArtifacts struct {
	provider       string
	host           string
	images         string
	release        string
	providerSHA256 string
	hostSHA256     string
}

func requireLiveArtifacts(t *testing.T) liveArtifacts {
	t.Helper()
	provider, root, expected := os.Getenv("SUB2API_TEST_PROVIDER_BINARY"), os.Getenv("SUB2API_TEST_RELEASE_ROOT"), os.Getenv("SUB2API_TEST_PROVIDER_SHA256")
	if provider == "" || root == "" || expected == "" || !filepath.IsAbs(provider) || !filepath.IsAbs(root) || len(expected) != 64 || strings.Trim(expected, "0123456789abcdef") != "" {
		t.Fatal("candidate Provider path, release root, and SUB2API_TEST_PROVIDER_SHA256 are required")
	}
	if provider != filepath.Join(root, "bin", "pulumi-resource-sub2api-host") {
		t.Fatal("candidate Provider path is not the exact release Provider")
	}
	manifestPath, host := filepath.Join(root, "artifacts", "sub2api-host", "manifest.json"), filepath.Join(root, "artifacts", "sub2api-host", "sub2api-host-linux-amd64")
	for _, path := range []string{provider, manifestPath, host} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || (path != manifestPath && info.Mode()&0o111 == 0) {
			t.Fatal("candidate release artifact is unavailable")
		}
	}
	providerSum, hostSum := fileSHA256(t, provider), fileSHA256(t, host)
	if providerSum != expected {
		t.Fatal("candidate Provider hash does not match CI provenance")
	}
	if b := mustRead(t, host); len(b) < 4 || string(b[:4]) != "\x7fELF" {
		t.Fatal("candidate Host is not an ELF executable")
	}
	var manifest struct {
		Release    string `json:"release"`
		LinuxAMD64 struct {
			Path   string `json:"path"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"linux-amd64"`
	}
	if err := json.Unmarshal(mustRead(t, manifestPath), &manifest); err != nil || manifest.Release == "" || manifest.LinuxAMD64.Path != "sub2api-host-linux-amd64" {
		t.Fatal("candidate Host manifest is invalid")
	}
	info, err := os.Stat(host)
	if err != nil || info.Size() != manifest.LinuxAMD64.Size || hostSum != manifest.LinuxAMD64.SHA256 {
		t.Fatal("candidate Host does not match release manifest")
	}
	images := os.Getenv("SUB2API_PROVIDER_RUNTIME_LIVE_IMAGE_ARCHIVE")
	if info, err := os.Lstat(images); images == "" || !filepath.IsAbs(images) || err != nil || !info.Mode().IsRegular() {
		t.Fatal("SUB2API_PROVIDER_RUNTIME_LIVE_IMAGE_ARCHIVE is required")
	}
	return liveArtifacts{provider: provider, host: host, images: images, release: manifest.Release, providerSHA256: providerSum, hostSHA256: hostSum}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(mustRead(t, path))
	return hex.EncodeToString(sum[:])
}
func requireLiveTraceDirectory(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("PROVIDER_RUNTIME_LIVE_TRACE_DIR")
	info, err := os.Stat(dir)
	if dir == "" || !filepath.IsAbs(dir) || err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatal("PROVIDER_RUNTIME_LIVE_TRACE_DIR must be private and absolute")
	}
	return dir
}

type liveEvidence struct {
	Test                            string `json:"test"`
	ProviderSHA256                  string `json:"providerSHA256"`
	HostAMD64SHA256                 string `json:"hostAMD64SHA256"`
	ReleasedBoundary                string `json:"releasedBoundary"`
	DataHostPass                    bool   `json:"dataHostPass"`
	AppHostPass                     bool   `json:"appHostPass"`
	AppDataEnvironmentAuthenticated bool   `json:"appDataEnvironmentAuthenticated"`
	AppReadyAfterData               bool   `json:"appReadyAfterData"`
	PostgresPass                    bool   `json:"postgresPass"`
	PostgresWrongPasswordDenied     bool   `json:"postgresWrongPasswordDenied"`
	PostgresCatalog                 bool   `json:"postgresCatalog"`
	RedisPass                       bool   `json:"redisPass"`
	RedisWrongPasswordDenied        bool   `json:"redisWrongPasswordDenied"`
	RedisDefaultDenied              bool   `json:"redisDefaultDenied"`
	RedisACL                        bool   `json:"redisACL"`
	PostgresDrop                    bool   `json:"postgresDrop"`
	RedisDrop                       bool   `json:"redisDrop"`
	ForeignTableUnchanged           bool   `json:"foreignTableUnchanged"`
	ForeignTableSHA256              string `json:"foreignTableSHA256"`
}

func writeLiveEvidence(t *testing.T, dir string, value liveEvidence) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mx-allowlist-live.json")
	if err = os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("live evidence mode is invalid")
	}
}

type liveFixture struct {
	root, trace                                        string
	artifacts                                          liveArtifacts
	dataIP, appIP, badIP, bridge, dataNS, appNS, badNS string
	foreignTable, ownedTable                           string
}

func newLiveFixture(root, trace string, artifacts liveArtifacts) liveFixture {
	token := liveToken("mx-allowlist")
	return liveFixture{
		root:      root,
		trace:     trace,
		artifacts: artifacts,
		dataIP:    "10.252.0.2",
		appIP:     "10.252.0.3",
		badIP:     "10.252.0.4",
		bridge:    "s2lb" + token,
		dataNS:    "s2ld" + token,
		appNS:     "s2la" + token,
		badNS:     "s2lu" + token,
	}
}

func (f *liveFixture) environment() []string {
	return []string{
		providerRuntimeLiveHelper + "=1",
		"SUB2API_PROVIDER_RUNTIME_LIVE_ROOT=" + f.root,
		"LIVE_ROOT=" + f.root,
		"LIVE_HOST_BINARY=" + f.artifacts.host,
		"LIVE_IMAGE_ARCHIVE=" + f.artifacts.images,
		"LIVE_TRACE=" + f.trace,
		"LIVE_BRIDGE=" + f.bridge,
		"LIVE_DATA_NS=" + f.dataNS,
		"LIVE_APP_NS=" + f.appNS,
		"LIVE_UNAUTHORIZED_NS=" + f.badNS,
		"LIVE_DATA_IP=" + f.dataIP,
		"LIVE_APP_IP=" + f.appIP,
		"LIVE_BAD_IP=" + f.badIP,
		"LIVE_DATA_VETH_OUT=s2do" + f.bridge[4:],
		"LIVE_DATA_VETH_IN=s2di" + f.bridge[4:],
		"LIVE_APP_VETH_OUT=s2ao" + f.bridge[4:],
		"LIVE_APP_VETH_IN=s2ai" + f.bridge[4:],
		"LIVE_BAD_VETH_OUT=s2uo" + f.bridge[4:],
		"LIVE_BAD_VETH_IN=s2ui" + f.bridge[4:],
	}
}

func prepareLiveSSH(t *testing.T, f *liveFixture) {
	t.Helper()
	root := f.root
	sandbox := filepath.Join(repositoryRoot(t), "internal", "integration", "providerruntime", "testdata", "live-host-sandbox.sh")
	writePrivate(t, filepath.Join(root, "host-sandbox.sh"), mustRead(t, sandbox))
	if err := os.Chmod(filepath.Join(root, "host-sandbox.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home/.ssh", "sshd"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command(t, 10*time.Second, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(root, "client-key"))
	for _, host := range []string{"data", "app"} {
		if err := os.MkdirAll(filepath.Join(root, host), 0o700); err != nil {
			t.Fatal(err)
		}
		command(t, 10*time.Second, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(root, host+".host-key"))
		writePrivate(t, filepath.Join(root, host+".machine-id"), []byte(liveMachineID(host)+"\n"))
	}
	pub := strings.TrimSpace(string(mustRead(t, filepath.Join(root, "client-key.pub"))))
	writePrivate(t, filepath.Join(root, "home", ".ssh", "authorized_keys"), []byte(pub+"\n"))
	var config, known strings.Builder
	for _, item := range []struct{ name, ip string }{
		{"data", f.dataIP},
		{"app", f.appIP},
	} {
		host := strings.TrimSpace(string(commandOutput(t, 10*time.Second, "ssh-keygen", "-y", "-f", filepath.Join(root, item.name+".host-key"))))
		writePrivate(t, filepath.Join(root, item.name, "sshd_config"), []byte("Port 2222\nListenAddress "+item.ip+"\nHostKey "+filepath.Join(root, item.name+".host-key")+"\nAuthorizedKeysFile /root/.ssh/authorized_keys\nPidFile /var/run/sshd.pid\nUsePAM no\nPasswordAuthentication no\nChallengeResponseAuthentication no\nPermitRootLogin prohibit-password\nStrictModes yes\nLogLevel QUIET\n"))
		config.WriteString("Host live-" + item.name + "\n HostName " + item.ip + "\n Port 2222\n User root\n IdentityFile " + filepath.Join(root, "client-key") + "\n IdentitiesOnly yes\n UserKnownHostsFile " + filepath.Join(root, "home", ".ssh", "known_hosts") + "\n")
		known.WriteString("[" + item.ip + "]:2222 " + host + "\n")
	}
	writePrivate(t, filepath.Join(root, "home", ".ssh", "config"), []byte(config.String()))
	writePrivate(t, filepath.Join(root, "home", ".ssh", "known_hosts"), []byte(known.String()))
}
func liveMachineID(host string) string {
	sum := sha256.Sum256([]byte("live-machine-" + host))
	return hex.EncodeToString(sum[:])[:32]
}
func writePrivate(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *liveFixture) requireImages(t *testing.T) {
	f.sandboxRun(t, "data", liveCommandTimeout, "docker", "image", "inspect", "postgres:18-alpine", "redis:8-alpine", "sub2api-live-app:mx-allowlist")
	f.sandboxRun(t, "app", liveCommandTimeout, "docker", "image", "inspect", "postgres:18-alpine", "redis:8-alpine", "sub2api-live-app:mx-allowlist")
}
func (f *liveFixture) addForeignSentinel(t *testing.T) {
	f.foreignTable = "s2live_foreign_" + liveToken(t.Name())
	f.sandboxRun(t, "data", 10*time.Second, "nft", "add", "table", "inet", f.foreignTable)
	f.sandboxRun(t, "data", 10*time.Second, "nft", "add", "chain", "inet", f.foreignTable, "sentinel", "{", "type", "filter", "hook", "input", "priority", "0;", "policy", "accept;", "}")
	f.sandboxRun(t, "data", 10*time.Second, "nft", "add", "rule", "inet", f.foreignTable, "sentinel", "meta", "l4proto", "tcp", "accept")
}
func (f *liveFixture) recordOwnedTable(t *testing.T, host string) {
	b := f.sandboxOutput(t, host, 8*time.Second, "cat", "/var/lib/sub2api-host/state.json")
	var state struct {
		Resource  hostcontract.ResourceIdentity  `json:"resource"`
		Ownership hostcontract.OwnershipIdentity `json:"ownership"`
	}
	if json.Unmarshal(b, &state) != nil || state.Resource != (hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}) || state.Ownership.Value == "" {
		t.Fatal("test-owned Host state unavailable")
	}
	f.ownedTable = "s2h_" + liveRuntimeToken(state.Resource.Environment, state.Resource.ServerKey, state.Ownership.Value)
}
func liveRuntimeToken(values ...string) string {
	h := sha256.New()
	for _, v := range values {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func (f *liveFixture) postgresClient(t *testing.T, ns, password string) bool {
	return f.netnsOK(t, 8*time.Second, ns, "env", "PGPASSWORD="+password, "psql", "host="+f.dataIP+" port=5432 dbname=api_db user=api_user sslmode=disable connect_timeout=5", "-X", "-tAc", "SELECT 1")
}
func (f *liveFixture) postgresClientFails(t *testing.T, ns, password string) bool {
	return !f.netnsOK(t, 8*time.Second, ns, "env", "PGPASSWORD="+password, "psql", "host="+f.dataIP+" port=5432 dbname=api_db user=api_user sslmode=disable connect_timeout=5", "-X", "-tAc", "SELECT 1")
}
func (f *liveFixture) redisClient(t *testing.T, ns, password string) bool {
	return f.netnsShellOK(t, 8*time.Second, ns, "redis-cli --user api_user --pass "+password+" -h "+f.dataIP+" -p 6379 PING | grep -qx PONG")
}
func (f *liveFixture) redisClientFails(t *testing.T, ns, password string) bool {
	return !f.netnsShellOK(t, 8*time.Second, ns, "redis-cli --user api_user --pass "+password+" -h "+f.dataIP+" -p 6379 PING | grep -qx PONG")
}
func (f *liveFixture) redisDefaultDenied(t *testing.T, ns string) bool {
	return !f.netnsShellOK(t, 8*time.Second, ns, "redis-cli -h "+f.dataIP+" -p 6379 PING | grep -qx PONG")
}
func (f *liveFixture) socketDropped(t *testing.T, ns, port string) bool {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, "bash", "-ceu", "exec 3<>/dev/tcp/"+f.dataIP+"/"+port)
	var out boundedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	return ctx.Err() == context.DeadlineExceeded
}
func (f *liveFixture) exactDataAdmissionPolicy(t *testing.T, port string) bool {
	b := f.sandboxOutput(t, "data", 8*time.Second, "nft", "-j", "list", "table", "inet", f.ownedTable)
	var document struct {
		NFTables []struct {
			Rule *struct {
				Family string            `json:"family"`
				Table  string            `json:"table"`
				Chain  string            `json:"chain"`
				Expr   []json.RawMessage `json:"expr"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if json.Unmarshal(b, &document) != nil {
		return false
	}
	portValue, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	accepts, drops := 0, 0
	for _, entry := range document.NFTables {
		rule := entry.Rule
		if rule == nil || rule.Family != "inet" || rule.Table != f.ownedTable || rule.Chain != "prerouting" {
			continue
		}
		if len(rule.Expr) == 4 && nftMatch(rule.Expr[0], "ip", "saddr", f.appIP) && nftMatch(rule.Expr[1], "ip", "daddr", f.dataIP) && nftPortMatch(rule.Expr[2], portValue) && nftAction(rule.Expr[3], "accept") {
			accepts++
		}
		if len(rule.Expr) == 3 && nftMatch(rule.Expr[0], "ip", "daddr", f.dataIP) && nftPortMatch(rule.Expr[1], portValue) && nftAction(rule.Expr[2], "drop") {
			drops++
		}
	}
	return accepts == 1 && drops == 1
}
func nftMatch(raw json.RawMessage, protocol, field, value string) bool {
	var v struct {
		Match struct {
			Op   string `json:"op"`
			Left struct {
				Payload struct {
					Protocol string `json:"protocol"`
					Field    string `json:"field"`
				} `json:"payload"`
			} `json:"left"`
			Right string `json:"right"`
		} `json:"match"`
	}
	return json.Unmarshal(raw, &v) == nil && v.Match.Op == "==" && v.Match.Left.Payload.Protocol == protocol && v.Match.Left.Payload.Field == field && v.Match.Right == value
}
func nftPortMatch(raw json.RawMessage, port int) bool {
	var v struct {
		Match struct {
			Op   string `json:"op"`
			Left struct {
				Payload struct {
					Protocol string `json:"protocol"`
					Field    string `json:"field"`
				} `json:"payload"`
			} `json:"left"`
			Right int `json:"right"`
		} `json:"match"`
	}
	return json.Unmarshal(raw, &v) == nil && v.Match.Op == "==" && v.Match.Left.Payload.Protocol == "tcp" && v.Match.Left.Payload.Field == "dport" && v.Match.Right == port
}
func nftAction(raw json.RawMessage, action string) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil && len(v) == 1 && v[action] != nil
}
func (f *liveFixture) postgresCatalog(t *testing.T) bool {
	return f.sandboxShellOK(t, "data", 10*time.Second, `id=$(docker ps --filter ancestor=postgres:18-alpine -q); [ -n "$id" ]; docker exec --user postgres "$id" psql -X -U s2h_admin -d postgres -tAc "SELECT bool_and(v) FROM (VALUES ((SELECT rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls FROM pg_roles WHERE rolname='api_user')), ((SELECT NOT pg_has_role('api_user', 's2h_admin', 'member'))), ((SELECT pg_has_role('api_user', datdba, 'member') FROM pg_database WHERE datname='api_db')), ((SELECT datdba=(SELECT oid FROM pg_roles WHERE rolname LIKE 's2h_%') FROM pg_database WHERE datname='api_db')), ((SELECT (SELECT nspowner=(SELECT datdba FROM pg_database WHERE datname='api_db') FROM pg_namespace WHERE nspname='public'))), ((SELECT has_schema_privilege('api_user','public','USAGE') AND NOT has_schema_privilege('api_user','public','CREATE'))), ((SELECT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname='api_user'))), ((SELECT NOT EXISTS (SELECT 1 FROM pg_hba_file_rules WHERE type='host' AND auth_method='trust'))) ) x(v)" | grep -qx t`)
}
func (f *liveFixture) redisACL(t *testing.T) bool {
	id := strings.TrimSpace(string(f.sandboxOutput(t, "data", 8*time.Second, "docker", "ps", "--filter", "ancestor=redis:8-alpine", "-q")))
	if id == "" || strings.ContainsAny(id, " \t\r\n") {
		return false
	}
	acl := string(f.sandboxOutput(t, "data", 8*time.Second, "docker", "exec", id, "redis-cli", "--user", "api_user", "--pass", "LiveRedisClient_123", "ACL", "GETUSER", "api_user"))
	return aclFieldEquals(acl, "flags", "on") && aclFieldEquals(acl, "keys", "~*") && aclFieldEquals(acl, "channels", "&*") && aclFieldEquals(acl, "commands", "+@all")
}

func aclFieldEquals(acl, field, value string) bool {
	lines := strings.Split(acl, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == field {
			return lines[i+1] == value
		}
	}
	return false
}
func (f *liveFixture) nftDigest(t *testing.T, table string) string {
	out := f.sandboxOutput(t, "data", 8*time.Second, "nft", "-j", "list", "table", "inet", table)
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:])
}

func (f *liveFixture) cleanupNamespace(t *testing.T) {
	t.Helper()
	var failed bool
	for _, table := range []string{f.ownedTable, f.foreignTable} {
		if table != "" && !f.sandboxBestEffort("data", 8*time.Second, "nft", "delete", "table", "inet", table) {
			failed = true
		}
	}
	if failed {
		t.Error("live namespace cleanup failed")
	}
}
func (f *liveFixture) cleanupOuter(t *testing.T) {
	t.Helper()
	// Namespace exit removes mount/network state; outer cleanup owns no privileged resources.
}

func (f *liveFixture) netnsOK(t *testing.T, timeout time.Duration, ns, name string, args ...string) bool {
	t.Helper()
	return f.commandOK(t, timeout, "ip", append([]string{"netns", "exec", ns, name}, args...)...)
}
func (f *liveFixture) netnsShellOK(t *testing.T, timeout time.Duration, ns, script string) bool {
	t.Helper()
	return f.netnsOK(t, timeout, ns, "sh", "-ceu", script)
}
func (f *liveFixture) output(t *testing.T, timeout time.Duration, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out boundedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if cmd.Run() != nil || ctx.Err() != nil {
		t.Fatal("live command failed")
	}
	return out.Bytes()
}
func (f *liveFixture) commandOK(t *testing.T, timeout time.Duration, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out boundedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return cmd.Run() == nil && ctx.Err() == nil
}
func (f *liveFixture) bestEffort(timeout time.Duration, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out boundedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return cmd.Run() == nil && ctx.Err() == nil
}

func (f *liveFixture) sandboxPID(t *testing.T, host string) string {
	t.Helper()
	b := mustRead(t, filepath.Join(f.root, host, "supervisor"))
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		t.Fatal("sandbox supervisor identity is invalid")
	}
	stat := mustRead(t, filepath.Join("/proc", fields[0], "stat"))
	if supervisorStartTime(stat) != fields[1] {
		t.Fatal("sandbox supervisor identity changed")
	}
	return fields[0]
}
func (f *liveFixture) sandboxArgs(t *testing.T, host string, args ...string) []string {
	pid := f.sandboxPID(t, host)
	return append([]string{"nsenter", "--mount=/proc/" + pid + "/ns/mnt", "--net=/proc/" + pid + "/ns/net", "--"}, args...)
}
func (f *liveFixture) sandboxRun(t *testing.T, host string, timeout time.Duration, name string, args ...string) {
	t.Helper()
	argv := f.sandboxArgs(t, host, append([]string{name}, args...)...)
	if !f.commandOK(t, timeout, argv[0], argv[1:]...) {
		t.Fatal("sandbox command failed")
	}
}
func (f *liveFixture) sandboxOutput(t *testing.T, host string, timeout time.Duration, name string, args ...string) []byte {
	t.Helper()
	argv := f.sandboxArgs(t, host, append([]string{name}, args...)...)
	return f.output(t, timeout, argv[0], argv[1:]...)
}
func (f *liveFixture) sandboxBestEffort(host string, timeout time.Duration, name string, args ...string) bool {
	pidPath := filepath.Join(f.root, host, "supervisor")
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", fields[0], "stat"))
	if err != nil || supervisorStartTime(stat) != fields[1] {
		return false
	}
	argv := append([]string{"nsenter", "--mount=/proc/" + fields[0] + "/ns/mnt", "--net=/proc/" + fields[0] + "/ns/net", "--", name}, args...)
	return f.bestEffort(timeout, argv[0], argv[1:]...)
}

type liveDataContainerExpectation struct {
	kind, name, image, owner, target, ownership string
	port                                        int
}

type liveContainerInspection struct {
	name, image, owner, target string
	running                    bool
}

type liveMilestoneObserverResult struct {
	lastMilestone string
	observerError bool
	stateSeen     bool
	ownership     string
	milestones    int
}

func liveDataCreateObserved(result liveMilestoneObserverResult) bool {
	return !result.observerError && result.lastMilestone == liveFinalMilestone
}

type liveMilestoneObserver struct {
	expectations func(context.Context) ([]liveDataContainerExpectation, bool, error)
	inspect      func(context.Context, liveDataContainerExpectation) (liveContainerInspection, bool, error)
	ready        func(context.Context, liveDataContainerExpectation) (bool, error)
	emit         func(string)
	poll         time.Duration
}

func (o liveMilestoneObserver) run(ctx context.Context) liveMilestoneObserverResult {
	result := liveMilestoneObserverResult{lastMilestone: "none"}
	next := 0
	ticker := time.NewTicker(o.poll)
	defer ticker.Stop()
	var expectedState []liveDataContainerExpectation
	for {
		expectations, present, err := o.expectations(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return result
			}
			result.observerError = true
			return result
		}
		if !present {
			if result.stateSeen {
				result.observerError = true
				return result
			}
		} else if len(expectations) != 2 {
			result.observerError = true
			return result
		} else if result.stateSeen && !slices.Equal(expectations, expectedState) {
			result.observerError = true
			return result
		} else {
			result.stateSeen = true
			if result.ownership == "" {
				result.ownership = expectations[0].ownership
			}
			expectedState = expectations
		}
		for expectations != nil && next < len(liveMilestones) {
			expected := expectations[next/2]
			if next%2 == 0 {
				inspection, present, err := o.inspect(ctx, expected)
				if err != nil {
					result.observerError = ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded)
					return result
				}
				if !present {
					break
				}
				if !exactLiveContainer(inspection, expected) {
					result.observerError = true
					return result
				}
			} else {
				ready, err := o.ready(ctx, expected)
				if err != nil {
					result.observerError = ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded)
					return result
				}
				if !ready {
					break
				}
			}
			result.lastMilestone = liveMilestones[next]
			if o.emit != nil {
				o.emit(result.lastMilestone)
			}
			next++
			result.milestones = next
		}
		if next == len(liveMilestones) {
			return result
		}
		select {
		case <-ctx.Done():
			result.observerError = errors.Is(ctx.Err(), context.DeadlineExceeded)
			return result
		case <-ticker.C:
		}
	}
}

func exactLiveContainer(got liveContainerInspection, want liveDataContainerExpectation) bool {
	return got.name == want.name && got.image == want.image && got.owner == want.owner && got.target == want.target && got.running
}

type liveDataMilestoneObserver struct {
	cancel context.CancelFunc
	done   <-chan liveMilestoneObserverResult
}

func (o liveDataMilestoneObserver) stop() liveMilestoneObserverResult {
	o.cancel()
	return <-o.done
}

func liveCreateObserverStatus(createSucceeded bool, result liveMilestoneObserverResult) string {
	if createSucceeded && liveDataCreateObserved(result) {
		return ""
	}
	if result.observerError {
		return "observer-error"
	}
	return "observer-inconclusive"
}

func reportLiveObserverStatus(status string) {
	if status != "" {
		_, _ = os.Stderr.WriteString("live observer: " + status + "\n")
	}
}

type liveDataObserverFacts struct {
	resource      hostcontract.ResourceIdentity
	revision      string
	priorRevision string
	machine       hostcontract.MachineIdentity
	release       string
	services      []hostcontract.LocalDataServiceTarget
}

func (f *liveFixture) startLiveDataMilestoneObserver(parent context.Context, facts liveDataObserverFacts) liveDataMilestoneObserver {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	done := make(chan liveMilestoneObserverResult, 1)
	observer := liveMilestoneObserver{expectations: func(ctx context.Context) ([]liveDataContainerExpectation, bool, error) {
		return f.liveDataContainerExpectations(ctx, facts)
	}, inspect: f.inspectLiveDataContainer, ready: f.liveDataReady, emit: reportLiveMilestone, poll: 250 * time.Millisecond}
	go func() { done <- observer.run(ctx) }()
	return liveDataMilestoneObserver{cancel: cancel, done: done}
}

func (f *liveFixture) liveDataContainerExpectations(ctx context.Context, facts liveDataObserverFacts) ([]liveDataContainerExpectation, bool, error) {
	state, present, err := f.liveDataObserverState(ctx, facts)
	if err != nil || !present {
		return nil, present, err
	}
	expectations, err := liveDataContainerExpectationsForState(facts, state)
	return expectations, true, err
}

func liveDataContainerExpectationsForState(facts liveDataObserverFacts, state liveObserverState) ([]liveDataContainerExpectation, error) {
	expectations := make([]liveDataContainerExpectation, 0, 2)
	for _, service := range facts.services {
		image := ""
		switch service.Type {
		case "postgres":
			image = "postgres:18-alpine"
		case "redis":
			image = "redis:8-alpine"
		default:
			return nil, errors.New("invalid observer service")
		}
		id := liveRuntimeToken("local-data", service.ID)
		containerToken := liveRuntimeToken(facts.resource.Environment, facts.resource.ServerKey, state.ownership, "local-data", id, "live")
		ownerToken := liveRuntimeToken(facts.resource.Environment, facts.resource.ServerKey, state.ownership, "local-data", id, "")
		expectations = append(expectations, liveDataContainerExpectation{
			kind: service.Type, name: "s2h-" + containerToken, image: image,
			owner:     "s2h1:" + ownerToken,
			ownership: state.ownership,
			target:    "s2ht1:" + liveRuntimeToken("local-data", id, "", state.revision, image, service.Type, strconv.Itoa(service.Port), strconv.FormatBool(service.Persistence)), port: service.Port,
		})
	}
	if len(expectations) != 2 || expectations[0].kind != "postgres" || expectations[1].kind != "redis" {
		return nil, errors.New("invalid observer expectations")
	}
	return expectations, nil
}

type liveObserverState struct{ ownership, revision string }

func (f *liveFixture) liveDataObserverState(ctx context.Context, facts liveDataObserverFacts) (liveObserverState, bool, error) {
	b, err := f.sandboxOutputForObserver(ctx, "data", 3*time.Second, "sh", "-c", "test -e /var/lib/sub2api-host/state.json || exit 42; cat /var/lib/sub2api-host/state.json")
	if err != nil {
		if liveObserverStateAbsent(err) {
			return liveObserverState{}, false, nil
		}
		return liveObserverState{}, false, err
	}
	return parseLiveDataObserverState(b, facts)
}

func parseLiveDataObserverState(b []byte, facts liveDataObserverFacts) (liveObserverState, bool, error) {
	state, err := decodeLiveObserverHostState(b)
	if err != nil || !livePendingStateMatches(state, facts) {
		return liveObserverState{}, false, errors.New("invalid observer state")
	}
	return liveObserverState{ownership: state.Ownership.Value, revision: state.Journal.Key.TargetRevision}, true, nil
}

func decodeLiveObserverHostState(b []byte) (hostruntime.State, error) {
	if len(b) == 0 || len(b) > 1<<20 || liveJSONHasDuplicateKey(b) {
		return hostruntime.State{}, errors.New("invalid observer state")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var state hostruntime.State
	if err := decoder.Decode(&state); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return hostruntime.State{}, errors.New("invalid observer state")
	}
	return state, nil
}

func liveJSONHasDuplicateKey(b []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(b))
	var value func() bool
	value = func() bool {
		token, err := decoder.Token()
		if err != nil {
			return true
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return false
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return true
				}
				seen[name] = true
				if value() {
					return true
				}
			}
			_, err := decoder.Token()
			return err != nil
		case '[':
			for decoder.More() && !value() {
			}
			_, err := decoder.Token()
			return err != nil
		default:
			return true
		}
	}
	if value() {
		return true
	}
	_, err := decoder.Token()
	return err != io.EOF
}

func livePendingStateMatches(state hostruntime.State, facts liveDataObserverFacts) bool {
	ownership := state.Ownership.Value
	if ownership == "" || !liveObserverHostStateMatches(state, facts, ownership, facts.priorRevision) || len(state.Observation.Apps) != 0 || len(state.Observation.Data) != 0 {
		return false
	}
	journal := state.Journal
	return journal.Status == "pending" && journal.Result == nil && journal.Approval == nil && liveReconcileKeyMatches(journal.Key, facts)
}

func (f *liveFixture) liveDataCompletionObserved(parent context.Context, facts liveDataObserverFacts, ownership string, result liveMilestoneObserverResult) liveMilestoneObserverResult {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	b, err := f.sandboxOutputForObserver(ctx, "data", 3*time.Second, "sh", "-c", "cat /var/lib/sub2api-host/state.json")
	if err != nil {
		result.observerError = true
		return result
	}
	return liveDataCompletionResult(facts, ownership, result, b, liveDataContainerExpectationsForState, f.inspectLiveDataContainer, f.liveDataReady, reportLiveMilestone, ctx)
}

func parseLiveCompletedObserverState(b []byte, facts liveDataObserverFacts, ownership string) (liveObserverState, bool, error) {
	state, err := decodeLiveObserverHostState(b)
	if err != nil || !liveCompletedHostStateMatches(state, facts, ownership) {
		return liveObserverState{}, false, errors.New("invalid completed observer state")
	}
	return liveObserverState{ownership: state.Ownership.Value, revision: state.Journal.Key.TargetRevision}, true, nil
}

func liveCompletedHostStateMatches(state hostruntime.State, facts liveDataObserverFacts, ownership string) bool {
	if !liveObserverHostStateMatches(state, facts, ownership, facts.revision) || len(state.Observation.Apps) != 0 || !exactLiveDataObservations(facts, ownership, state.Observation.Data) {
		return false
	}
	journal := state.Journal
	result := journal.Result
	return journal.Status == "complete" && journal.Approval == nil && liveReconcileKeyMatches(journal.Key, facts) && result != nil && result.Status == hostprotocol.ResultApplied && result.AppliedRevision == facts.revision && result.Observation == nil && result.Machine == nil && result.Ownership == nil && result.Retirement == nil && result.OperationEvidence == nil
}

func liveObserverHostStateMatches(state hostruntime.State, facts liveDataObserverFacts, ownership, appliedRevision string) bool {
	observation := state.Observation
	return ownership != "" &&
		state.Version == 1 &&
		state.Resource == facts.resource &&
		state.Machine == facts.machine &&
		state.Ownership.Value == ownership &&
		state.AppliedRevision == appliedRevision &&
		state.Journal != nil &&
		state.LastOperation == nil &&
		state.Retirement == nil &&
		observation.Validate() == nil &&
		observation.Ready &&
		!observation.Drifted &&
		observation.Machine == facts.machine &&
		observation.Ownership == state.Ownership &&
		observation.HostRelease == facts.release &&
		observation.AppliedRevision == appliedRevision
}

func liveReconcileKeyMatches(key hostcontract.OperationKey, facts liveDataObserverFacts) bool {
	return facts.priorRevision != "" &&
		key.Resource == facts.resource &&
		key.Action == hostcontract.ActionReconcile &&
		key.TargetRevision == facts.revision &&
		key.PriorAppliedRevision == facts.priorRevision &&
		key.PriorObservation == "" &&
		key.Validate() == nil
}

func livePostCreateCompletion(createSucceeded bool, ownership string, ownershipOK bool, result liveMilestoneObserverResult, completion func(string, liveMilestoneObserverResult) liveMilestoneObserverResult) liveMilestoneObserverResult {
	if !createSucceeded {
		return result
	}
	if !ownershipOK {
		result.observerError = true
		return result
	}
	if liveDataCreateObserved(result) {
		if !result.stateSeen || result.ownership != ownership {
			result.observerError = true
		}
		return result
	}
	return completion(ownership, result)
}

func liveDataCompletionResult(facts liveDataObserverFacts, ownership string, result liveMilestoneObserverResult, stateBytes []byte, expectations func(liveDataObserverFacts, liveObserverState) ([]liveDataContainerExpectation, error), inspect func(context.Context, liveDataContainerExpectation) (liveContainerInspection, bool, error), ready func(context.Context, liveDataContainerExpectation) (bool, error), emit func(string), ctx context.Context) liveMilestoneObserverResult {
	state, err := decodeLiveObserverHostState(stateBytes)
	if err != nil || !liveCompletedHostStateMatches(state, facts, ownership) {
		result.observerError = true
		return result
	}
	want, err := expectations(facts, liveObserverState{ownership: state.Ownership.Value, revision: state.Journal.Key.TargetRevision})
	if err != nil {
		result.observerError = true
		return result
	}
	for i, expected := range want {
		got, present, err := inspect(ctx, expected)
		if err != nil || !present || !exactLiveContainer(got, expected) {
			result.observerError = true
			return result
		}
		ok, err := ready(ctx, expected)
		if err != nil || !ok {
			result.observerError = true
			return result
		}
		for result.milestones < i*2+2 {
			result.lastMilestone = liveMilestones[result.milestones]
			emit(result.lastMilestone)
			result.milestones++
		}
	}
	result.observerError, result.stateSeen, result.ownership = false, true, ownership
	return result
}

func (f *liveFixture) inspectLiveDataContainer(ctx context.Context, expected liveDataContainerExpectation) (liveContainerInspection, bool, error) {
	names, err := f.sandboxOutputForObserver(ctx, "data", liveCommandTimeout, "docker", "container", "ls", "--filter", "name=^/"+expected.name+"$", "--format", "{{.Names}}")
	if err != nil {
		return liveContainerInspection{}, false, err
	}
	if len(names) == 0 {
		return liveContainerInspection{}, false, nil
	}
	if string(names) != expected.name+"\n" {
		return liveContainerInspection{}, false, errors.New("malformed container listing")
	}
	out, err := f.sandboxOutputForObserver(ctx, "data", liveCommandTimeout, "docker", "container", "inspect", "--format", "{{.Name}}\t{{.Config.Image}}\t{{index .Config.Labels \"sub2api.host\"}}\t{{index .Config.Labels \"sub2api.host.target\"}}\t{{.State.Running}}", expected.name)
	if err != nil {
		return liveContainerInspection{}, false, err
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\t")
	if len(fields) != 5 || fields[0] != "/"+expected.name || fields[4] != "true" {
		return liveContainerInspection{}, false, errors.New("malformed container inspection")
	}
	got := liveContainerInspection{name: expected.name, image: fields[1], owner: fields[2], target: fields[3], running: true}
	return got, true, nil
}

func (f *liveFixture) liveDataReady(ctx context.Context, expected liveDataContainerExpectation) (bool, error) {
	args := liveDataReadinessArgs(expected)
	out, err := f.sandboxOutputForObserver(ctx, "data", liveCommandTimeout, args[0], args[1:]...)
	if err != nil {
		return false, err
	}
	if expected.kind == "postgres" {
		return true, nil
	}
	return bytes.Equal(out, []byte("PONG\n")), nil
}

const livePostCreateSnapshotMarker = "live post-create snapshot:"

const (
	livePostCreateReportTimeout  = 14 * time.Second
	livePostCreateStateTimeout   = 3 * time.Second
	livePostCreateCommandTimeout = time.Second
	livePostgresLifecycleTimeout = 9 * time.Second
)

var livePostCreateStates = map[string]bool{"root-absent": true, "state-absent": true, "invalid": true, "unavailable": true, "not-exact": true, "pending-exact": true}
var livePostCreateContainers = map[string]bool{"not-inspected": true, "absent": true, "identity-mismatch": true, "exited": true, "not-running": true, "running-probe-failed": true, "running-unready": true, "ready": true, "unavailable": true, "ambiguous": true}

func reportLivePostCreateSnapshot(parent context.Context, f *liveFixture, facts liveDataObserverFacts) {
	ctx, cancel := context.WithTimeout(parent, livePostCreateReportTimeout)
	defer cancel()
	state, parsed := livePostCreateState(ctx, func(ctx context.Context) ([]byte, error) {
		return f.sandboxOutputForObserver(ctx, "data", livePostCreateStateTimeout, "sh", "-c", livePostCreateStateReadScript())
	}, facts)
	postgres, redis := "not-inspected", "not-inspected"
	if state == "pending-exact" {
		expected, err := liveDataContainerExpectationsForState(facts, parsed)
		if err != nil {
			state = "invalid"
		} else {
			command := func(ctx context.Context, args ...string) ([]byte, error) {
				return f.sandboxOutputForObserver(ctx, "data", livePostCreateCommandTimeout, args[0], args[1:]...)
			}
			probe := func(ctx context.Context, expected liveDataContainerExpectation) ([]byte, error) {
				args := liveDataReadinessArgs(expected)
				return command(ctx, args...)
			}
			postgres = livePostCreateContainer(ctx, expected[0], command, probe, func(ctx context.Context, expected liveDataContainerExpectation, command func(context.Context, ...string) ([]byte, error)) {
				diagnosticCtx, cancel := livePostgresLifecycleContext(ctx)
				defer cancel()
				reportLivePostgresLifecycle(diagnosticCtx, expected, command, f.livePostgresRootfsObservation)
			})
			redis = livePostCreateContainer(ctx, expected[1], command, probe, nil)
		}
	}
	_, _ = os.Stderr.WriteString(fmt.Sprintf("%s state=%s postgres=%s redis=%s\n", livePostCreateSnapshotMarker, state, postgres, redis))
}

func livePostCreateStateReadScript() string {
	return livePostCreateStateReadScriptForPaths("/var/lib", "/var/lib/sub2api-host")
}

func livePostCreateStateReadScriptForPaths(parent, root string) string {
	return `parent='` + liveShellQuote(parent) + `'; root='` + liveShellQuote(root) + `'; result=$(find "$parent"/. -mindepth 1 -maxdepth 1 -name sub2api-host -print); status=$?; if test "$status" -ne 0; then exit "$status"; fi; if test -z "$result"; then printf 'root-absent\n'; exit 0; elif test "$result" != "$parent/./sub2api-host" || ! test -d "$root" || test -L "$root"; then exit 1; fi; result=$(find "$root"/. -mindepth 1 -maxdepth 1 -name state.json -print); status=$?; if test "$status" -ne 0; then exit "$status"; fi; if test -z "$result"; then printf 'state-absent\n'; elif test "$result" = "$root/./state.json" && test -f "$root/state.json" && ! test -L "$root/state.json"; then printf 'present\n'; cat "$root/state.json"; else exit 1; fi`
}

func liveShellQuote(value string) string { return strings.ReplaceAll(value, "'", "'\"'\"'") }

func livePostCreateState(ctx context.Context, read func(context.Context) ([]byte, error), facts liveDataObserverFacts) (string, liveObserverState) {
	b, err := read(ctx)
	if err != nil {
		return "unavailable", liveObserverState{}
	}
	if bytes.Equal(b, []byte("root-absent\n")) {
		return "root-absent", liveObserverState{}
	}
	if bytes.Equal(b, []byte("state-absent\n")) {
		return "state-absent", liveObserverState{}
	}
	if !bytes.HasPrefix(b, []byte("present\n")) {
		return "invalid", liveObserverState{}
	}
	b = b[len("present\n"):]
	state, err := decodeLiveObserverHostState(b)
	if err != nil {
		return "invalid", liveObserverState{}
	}
	if !livePendingStateMatches(state, facts) {
		return "not-exact", liveObserverState{}
	}
	return "pending-exact", liveObserverState{ownership: state.Ownership.Value, revision: state.Journal.Key.TargetRevision}
}

func livePostCreateContainer(ctx context.Context, expected liveDataContainerExpectation, command func(context.Context, ...string) ([]byte, error), probe func(context.Context, liveDataContainerExpectation) ([]byte, error), running func(context.Context, liveDataContainerExpectation, func(context.Context, ...string) ([]byte, error))) string {
	list, err := command(ctx, "docker", "container", "ls", "--all", "--filter", "name=^/"+expected.name+"$", "--format", "{{.Names}}")
	if err != nil {
		return "unavailable"
	}
	if len(list) == 0 {
		return "absent"
	}
	if string(list) != expected.name+"\n" {
		return "ambiguous"
	}
	out, err := command(ctx, "docker", "container", "inspect", "--format", "{{.Name}}\t{{.Config.Image}}\t{{index .Config.Labels \"sub2api.host\"}}\t{{index .Config.Labels \"sub2api.host.target\"}}\t{{.State.Status}}", expected.name)
	if err != nil {
		return "unavailable"
	}
	if !strings.HasSuffix(string(out), "\n") {
		return "ambiguous"
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\t")
	if len(fields) != 5 || fields[0] != "/"+expected.name {
		return "ambiguous"
	}
	if fields[1] != expected.image || fields[2] != expected.owner || fields[3] != expected.target {
		return "identity-mismatch"
	}
	switch fields[4] {
	case "exited":
		return "exited"
	case "running":
		if running != nil {
			running(ctx, expected, command)
		}
		return livePostCreateReadiness(ctx, expected, probe, 10, 225*time.Millisecond)
	default:
		return "not-running"
	}
}

func livePostCreateReadiness(ctx context.Context, expected liveDataContainerExpectation, probe func(context.Context, liveDataContainerExpectation) ([]byte, error), attempts int, pause time.Duration) string {
	for i := 0; i < attempts; i++ {
		out, err := probe(ctx, expected)
		if err == nil {
			if expected.kind == "postgres" || bytes.Equal(out, []byte("PONG\n")) {
				return "ready"
			}
			return "running-unready"
		}
		if i+1 < attempts && pause > 0 {
			select {
			case <-ctx.Done():
				return "running-probe-failed"
			case <-time.After(pause):
			}
		}
	}
	return "running-probe-failed"
}

const livePostgresReadinessMarker = "live postgres readiness:"

const livePostgresLifecycleMarker = "live postgres container:"

var livePostgresPGDataCategories = map[string]bool{"present": true, "absent": true, "failed": true}
var livePostgresServerCategories = map[string]bool{"accepting": true, "rejecting": true, "no-response": true, "failed": true}
var livePostgresPSQLCategories = map[string]bool{"ok": true, "failed": true}
var livePostgresLifecycleExecCategories = map[string]bool{"ok": true, "timeout": true, "exit-1": true, "exit-126": true, "exit-127": true, "exit-other": true, "failed": true}
var livePostgresRootfsCategories = map[string]bool{"present": true, "absent": true, "nonregular": true, "nonexecutable": true, "unavailable": true}

func livePostgresLifecycleContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, livePostgresLifecycleTimeout)
}

func reportLivePostgresLifecycle(ctx context.Context, expected liveDataContainerExpectation, command func(context.Context, ...string) ([]byte, error), observeRootfs func(context.Context, string) (string, string, string)) {
	lifecycle := livePostgresLifecycleDiagnostic(ctx, expected, command, observeRootfs, livePostgresProcessStartTime)
	if livePostgresLifecycleRecordValid(lifecycle) {
		_, _ = os.Stderr.WriteString(lifecycle)
	}
}

func livePostgresLifecycleDiagnostic(ctx context.Context, expected liveDataContainerExpectation, command func(context.Context, ...string) ([]byte, error), observeRootfs func(context.Context, string) (string, string, string), readStartTime func(string) (string, bool)) string {
	execErr := livePostgresReadinessCommand(ctx, command, livePostgresNeutralArgs(expected)...)
	absoluteErr := livePostgresReadinessCommand(ctx, command, livePostgresAbsoluteArgs(expected)...)
	pgdata := livePostgresPGDataCategory(livePostgresReadinessCommand(ctx, command, livePostgresPGDataArgs(expected)...))
	server := livePostgresServerCategory(livePostgresReadinessCommand(ctx, command, livePostgresServerArgs(expected)...))
	psql := livePostgresPSQLCategory(livePostgresReadinessCommand(ctx, command, liveDataReadinessArgs(expected)...))
	record := fmt.Sprintf("%s pgdata=%s server=%s psql=%s\n", livePostgresReadinessMarker, pgdata, server, psql)
	if livePostgresReadinessRecord(record) {
		_, _ = os.Stderr.WriteString(record)
	}
	initialOutput, inspectErr := command(ctx, livePostgresLifecycleInspectArgs(expected)...)
	initial, parsedOK := parseLivePostgresLifecycleInspection(expected, initialOutput)
	failedRecord := livePostgresLifecycleFailedRecord(execErr, absoluteErr)
	if inspectErr != nil || !parsedOK {
		return failedRecord
	}

	rootfs, direct, directError := "unavailable", "failed", "unknown"
	observationValid := true
	if initial.state == "running" && observeRootfs != nil {
		if readStartTime == nil {
			return failedRecord
		}
		observationValid = false
		firstStart, firstOK := readStartTime(initial.pid)
		if firstOK {
			rootfs, direct, directError = observeRootfs(ctx, initial.pid)
			secondStart, secondOK := readStartTime(initial.pid)
			observationValid = secondOK && firstStart == secondStart && livePostgresObservationValid(rootfs, direct, directError)
		}
	}

	stableOutput, stableErr := command(ctx, livePostgresLifecycleInspectArgs(expected)...)
	if !observationValid || stableErr != nil || !bytes.Equal(initialOutput, stableOutput) {
		return failedRecord
	}
	if _, stableOK := parseLivePostgresLifecycleInspection(expected, stableOutput); !stableOK {
		return failedRecord
	}
	return livePostgresLifecycleRecordForInspection(execErr, absoluteErr, initial, rootfs, direct, directError)
}

func livePostgresNeutralArgs(expected liveDataContainerExpectation) []string {
	return []string{"docker", "exec", expected.name, "true"}
}

func livePostgresAbsoluteArgs(expected liveDataContainerExpectation) []string {
	return []string{"docker", "exec", expected.name, "/bin/busybox", "true"}
}

func livePostgresLifecycleInspectArgs(expected liveDataContainerExpectation) []string {
	return []string{"docker", "container", "inspect", "--format", "{{.Name}}\t{{.Config.Image}}\t{{index .Config.Labels \"sub2api.host\"}}\t{{index .Config.Labels \"sub2api.host.target\"}}\t{{.State.Status}}\t{{.RestartCount}}\t{{.State.OOMKilled}}\t{{if .State.Error}}true{{else}}false{{end}}\t{{.Id}}\t{{.State.Pid}}", expected.name}
}

func livePostgresLifecycleFailedRecord(execErr, absoluteErr error) string {
	return fmt.Sprintf("%s exec=%s absolute=%s absolute-error=%s rootfs=unavailable direct=failed direct-error=unknown state=failed restarts=unknown oom=unknown error=unknown\n", livePostgresLifecycleMarker, livePostgresLifecycleExecCategory(execErr), livePostgresLifecycleExecCategory(absoluteErr), livePostgresAbsoluteErrorCategory(absoluteErr))
}

func livePostgresLifecycleRecord(execErr, absoluteErr error, expected liveDataContainerExpectation, out []byte) string {
	inspection, ok := parseLivePostgresLifecycleInspection(expected, out)
	if !ok {
		return livePostgresLifecycleFailedRecord(execErr, absoluteErr)
	}
	return livePostgresLifecycleRecordForInspection(execErr, absoluteErr, inspection, "unavailable", "failed", "unknown")
}

type livePostgresLifecycleInspection struct {
	state, restarts, oom, errorValue, pid string
}

func parseLivePostgresLifecycleInspection(expected liveDataContainerExpectation, out []byte) (livePostgresLifecycleInspection, bool) {
	if len(out) >= 256 || !utf8.Valid(out) || bytes.IndexByte(out, '\r') >= 0 || !strings.HasSuffix(string(out), "\n") {
		return livePostgresLifecycleInspection{}, false
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\t")
	if len(fields) != 10 || fields[0] != "/"+expected.name || fields[1] != expected.image || fields[2] != expected.owner || fields[3] != expected.target {
		return livePostgresLifecycleInspection{}, false
	}
	states := map[string]bool{"running": true, "restarting": true, "exited": true, "created": true, "paused": true, "dead": true, "removing": true}
	if !states[fields[4]] || !livePostgresRestartCategory(fields[5]) || (fields[6] != "true" && fields[6] != "false") || (fields[7] != "true" && fields[7] != "false") || !livePostgresContainerID(fields[8]) || !livePostgresCanonicalPID(fields[9]) {
		return livePostgresLifecycleInspection{}, false
	}
	return livePostgresLifecycleInspection{state: fields[4], restarts: fields[5], oom: fields[6], errorValue: fields[7], pid: fields[9]}, true
}

func livePostgresContainerID(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func livePostgresCanonicalPID(value string) bool {
	if len(value) == 0 || value[0] == '0' {
		return false
	}
	for _, b := range value {
		if b < '0' || b > '9' {
			return false
		}
	}
	pid, err := strconv.ParseUint(value, 10, 64)
	return err == nil && pid <= uint64(^uint(0)>>1)
}

func livePostgresLifecycleRecordForInspection(execErr, absoluteErr error, inspection livePostgresLifecycleInspection, rootfs, direct, directError string) string {
	restarts := "nonzero"
	if inspection.restarts == "0" {
		restarts = "zero"
	}
	oom, errorPresent := "yes", "present"
	if inspection.oom == "false" {
		oom = "no"
	}
	if inspection.errorValue == "false" {
		errorPresent = "absent"
	}
	return fmt.Sprintf("%s exec=%s absolute=%s absolute-error=%s rootfs=%s direct=%s direct-error=%s state=%s restarts=%s oom=%s error=%s\n", livePostgresLifecycleMarker, livePostgresLifecycleExecCategory(execErr), livePostgresLifecycleExecCategory(absoluteErr), livePostgresAbsoluteErrorCategory(absoluteErr), rootfs, direct, directError, inspection.state, restarts, oom, errorPresent)
}

func (f *liveFixture) livePostgresRootfsObservation(ctx context.Context, pid string) (string, string, string) {
	info, err := os.Stat(filepath.Join("/proc", pid, "root", "bin", "busybox"))
	rootfs := livePostgresRootfsCategory(info, err)
	if rootfs != "present" {
		return rootfs, "failed", "unknown"
	}
	args := livePostgresDirectArgs(pid)
	_, err = f.sandboxOutputForObserver(ctx, "data", livePostCreateCommandTimeout, args[0], args[1:]...)
	if err == nil {
		return rootfs, "ok", "none"
	}
	return rootfs, livePostgresLifecycleExecCategory(err), livePostgresAbsoluteErrorCategory(err)
}

func livePostgresObservationValid(rootfs, direct, directError string) bool {
	return livePostgresRootfsCategories[rootfs] && livePostgresLifecycleExecCategories[direct] && livePostgresAbsoluteErrorCategories[directError]
}

func livePostgresProcessStartTime(pid string) (string, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return "", false
	}
	return liveProcessStartTime(b)
}

func liveProcessStartTime(stat []byte) (string, bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 3 || end+2 >= len(stat) || stat[end+1] != ' ' {
		return "", false
	}
	fields := strings.Fields(string(stat[end+2:]))
	if len(fields) < 20 {
		return "", false
	}
	for _, b := range fields[19] {
		if b < '0' || b > '9' {
			return "", false
		}
	}
	return fields[19], true
}

func livePostgresDirectArgs(pid string) []string {
	return []string{"nsenter", "--target", pid, "--mount", "--root", "--wd", "--", "/bin/busybox", "true"}
}

func livePostgresRootfsCategory(info os.FileInfo, err error) string {
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "absent"
		}
		return "unavailable"
	}
	if !info.Mode().IsRegular() {
		return "nonregular"
	}
	if info.Mode()&0o111 == 0 {
		return "nonexecutable"
	}
	return "present"
}

func livePostgresLifecycleExecCategory(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 1:
			return "exit-1"
		case 126:
			return "exit-126"
		case 127:
			return "exit-127"
		default:
			return "exit-other"
		}
	}
	return "failed"
}

func livePostgresAbsoluteErrorCategory(err error) string {
	if err == nil {
		return "none"
	}
	var observerErr *liveObserverCommandError
	if errors.As(err, &observerErr) {
		return observerErr.category
	}
	return "unknown"
}

func livePostgresRestartCategory(value string) bool {
	if value == "0" {
		return true
	}
	if len(value) == 0 || len(value) > 19 || value[0] == '0' {
		return false
	}
	for _, b := range value {
		if b < '0' || b > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func livePostgresPGDataArgs(expected liveDataContainerExpectation) []string {
	return []string{"docker", "exec", expected.name, "test", "-s", "/var/lib/postgresql/data/PG_VERSION"}
}

func livePostgresServerArgs(expected liveDataContainerExpectation) []string {
	return []string{"docker", "exec", expected.name, "pg_isready", "-h", "/var/run/postgresql", "-p", strconv.Itoa(expected.port), "-d", "postgres"}
}

func livePostgresReadinessCommand(ctx context.Context, command func(context.Context, ...string) ([]byte, error), args ...string) error {
	_, err := command(ctx, args...)
	return err
}

func livePostgresPGDataCategory(err error) string {
	if err == nil {
		return "present"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "absent"
	}
	return "failed"
}

func livePostgresServerCategory(err error) string {
	if err == nil {
		return "accepting"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 1:
			return "rejecting"
		case 2:
			return "no-response"
		}
	}
	return "failed"
}

func livePostgresPSQLCategory(err error) string {
	if err == nil {
		return "ok"
	}
	return "failed"
}

func liveDataReadinessArgs(expected liveDataContainerExpectation) []string {
	if expected.kind == "postgres" {
		return []string{"docker", "exec", expected.name, "psql", "-X", "-U", "s2h_admin", "-d", "postgres", "-p", strconv.Itoa(expected.port), "-v", "ON_ERROR_STOP=1", "-c", "SELECT 1"}
	}
	return []string{"docker", "exec", expected.name, "redis-cli", "--raw", "-h", "127.0.0.1", "-p", strconv.Itoa(expected.port), "ping"}
}

func (f *liveFixture) sandboxOutputForObserver(parent context.Context, host string, timeout time.Duration, name string, args ...string) ([]byte, error) {
	pidPath := filepath.Join(f.root, host, "supervisor")
	b, err := os.ReadFile(pidPath)
	fields := strings.Fields(string(b))
	if err != nil || len(fields) != 2 {
		return nil, errors.New("observer namespace unavailable")
	}
	stat, err := os.ReadFile(filepath.Join("/proc", fields[0], "stat"))
	if err != nil || supervisorStartTime(stat) != fields[1] {
		return nil, errors.New("observer namespace identity mismatch")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	argv := append([]string{"nsenter", "--mount=/proc/" + fields[0] + "/ns/mnt", "--net=/proc/" + fields[0] + "/ns/net", "--", name}, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, &liveObserverCommandError{err: err, category: liveObserverStderrCategory(stderr)}
	}
	return out.Bytes(), nil
}

type liveObserverCommandError struct {
	err      error
	category string
}

func (e *liveObserverCommandError) Error() string { return "live observer command failed" }

func (e *liveObserverCommandError) Unwrap() error { return e.err }

var livePostgresAbsoluteErrorCategories = map[string]bool{"none": true, "empty": true, "not-found": true, "permission": true, "cgroup": true, "namespace": true, "rootfs": true, "runtime": true, "daemon": true, "unknown": true}

func liveObserverStderrCategory(stderr boundedBuffer) string {
	if stderr.overflow {
		return "unknown"
	}
	text := strings.ToLower(string(stderr.data))
	if text == "" {
		return "empty"
	}
	for _, match := range []struct {
		category string
		phrases  []string
	}{
		{"permission", []string{"permission denied", "operation not permitted"}},
		{"cgroup", []string{"cgroup"}},
		{"namespace", []string{"setns", "namespace"}},
		{"rootfs", []string{"rootfs", "root filesystem", "mount"}},
		{"not-found", []string{"executable file not found", "executable not found", "file not found", "file-not-found", "enoent", "no such file"}},
		{"runtime", []string{"runc", "oci runtime", "containerd", "shim", "unable to start container process"}},
		{"daemon", []string{"docker daemon", "api response", "error response from daemon"}},
	} {
		for _, phrase := range match.phrases {
			if strings.Contains(text, phrase) {
				return match.category
			}
		}
	}
	return "unknown"
}

func liveObserverStateAbsent(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 42
}
func supervisorStartTime(stat []byte) string {
	fields := strings.Fields(string(stat))
	if len(fields) < 22 {
		return ""
	}
	return fields[21]
}
func (f *liveFixture) createReady(t *testing.T, created *pulumirpc.CreateResponse, inputs property.Map, machine hostcontract.MachineIdentity, release string, apps []hostcontract.AppObservation, dataHost bool) bool {
	checkpoint := unmarshalProperties(t, created.Properties)
	for _, name := range []string{"resource", "server", "target", "secrets"} {
		got, ok := checkpoint.GetOk(name)
		want, _ := inputs.GetOk(name)
		if !ok || !got.Equals(want) {
			return false
		}
	}
	secrets, _ := checkpoint.GetOk("secrets")
	if !secrets.Secret() {
		return false
	}
	allowed := map[string]bool{
		"resource":        true,
		"server":          true,
		"target":          true,
		"secrets":         true,
		"machine":         true,
		"ownership":       true,
		"appliedRevision": true,
		"observation":     true,
	}
	propertyCount := 0
	checkpoint.All(func(name string, value property.Value) bool {
		if !allowed[name] {
			propertyCount = -1
			return false
		}
		propertyCount++
		if (name == "machine" || name == "ownership" || name == "appliedRevision" || name == "observation") && value.Secret() {
			propertyCount = -1
			return false
		}
		if !value.Secret() && (name == "machine" || name == "ownership" || name == "appliedRevision" || name == "observation") {
			raw, err := propertyRaw(value)
			encoded, marshalErr := json.Marshal(raw)
			if err != nil || marshalErr != nil || containsLiveCredential(encoded) {
				propertyCount = -1
				return false
			}
		}
		return true
	})
	if propertyCount != len(allowed) {
		return false
	}
	for _, name := range []string{"machine", "ownership", "appliedRevision", "observation"} {
		value, ok := checkpoint.GetOk(name)
		if !ok || value.HasComputed() || value.IsNull() {
			return false
		}
	}
	gotMachine, ownership, revision, observation, err := checkpointValues(checkpoint)
	if err != nil || gotMachine != machine || ownership.Value == "" {
		return false
	}
	resource, target, secretValues, ok := liveCheckpointInputs(inputs)
	if !ok {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(ciKey)
	if err != nil {
		return false
	}
	wantRevision, err := hostcontract.TargetRevision(hostcontract.RevisionKey(key), resource, target, secretValues)
	if err != nil || revision != wantRevision {
		return false
	}
	if observation.Machine != machine || observation.Ownership != ownership || observation.AppliedRevision != revision || observation.HostRelease != release || !observation.Ready || observation.Drifted || !equalAppObservations(observation.Apps, apps) {
		return false
	}
	if dataHost {
		return exactDataObservations(observation.Data, target.DataServices)
	}
	return len(observation.Data) == 0
}

func liveCreateOwnership(checkpoint property.Map) (string, bool) {
	value, ok := checkpoint.GetOk("ownership")
	if !ok || value.Secret() || value.HasComputed() || value.IsNull() || !value.IsMap() {
		return "", false
	}
	object := value.AsMap()
	ownership, ok := object.GetOk("value")
	if !ok || ownership.Secret() || ownership.HasComputed() || ownership.IsNull() || !ownership.IsString() || ownership.AsString() == "" {
		return "", false
	}
	fields := 0
	object.All(func(_ string, _ property.Value) bool { fields++; return true })
	if fields != 1 {
		return "", false
	}
	return ownership.AsString(), true
}

func liveCheckpointInputs(inputs property.Map) (hostcontract.ResourceIdentity, hostcontract.Target, hostcontract.Secrets, bool) {
	var resource hostcontract.ResourceIdentity
	var target hostcontract.Target
	var secrets hostcontract.Secrets
	resourceValue, resourceOK := inputs.GetOk("resource")
	targetValue, targetOK := inputs.GetOk("target")
	secretsValue, secretsOK := inputs.GetOk("secrets")
	if !resourceOK || !targetOK || !secretsOK || decodeProperty(resourceValue, &resource) != nil || decodeProperty(targetValue, &target) != nil || decodeProperty(secretsValue, &secrets) != nil {
		return resource, target, secrets, false
	}
	return resource, target, secrets, true
}
func equalAppObservations(got, want []hostcontract.AppObservation) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
func exactDataObservations(got []hostcontract.DataObservation, target []hostcontract.LocalDataServiceTarget) bool {
	if len(got) != len(target) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, observation := range got {
		if !observation.Ready || observation.Identity.ProviderID == "" || observation.Identity.ProviderID != observation.Identity.Endpoint || observation.Identity.TLSMode != "" || seen[observation.Identity.Kind] {
			return false
		}
		seen[observation.Identity.Kind] = true
		matched := false
		for _, service := range target {
			database, tlsServerName := "sub2api", observation.Identity.Endpoint
			if service.Type == "redis" {
				database, tlsServerName = "0", ""
			}
			if service.Type == observation.Identity.Kind && service.Port == observation.Identity.Port && observation.Identity.Database == database && observation.Identity.TLSServerName == tlsServerName {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	return seen["postgres"] && seen["redis"]
}

func exactLiveDataObservations(facts liveDataObserverFacts, ownership string, got []hostcontract.DataObservation) bool {
	expected, err := liveDataContainerExpectationsForState(facts, liveObserverState{ownership: ownership, revision: facts.revision})
	if err != nil || len(got) != len(expected) {
		return false
	}
	for i, observation := range got {
		want := expected[i]
		database, tlsServerName := "sub2api", want.name
		if want.kind == "redis" {
			database, tlsServerName = "0", ""
		}
		if !observation.Ready || observation.Identity.Kind != want.kind || observation.Identity.ProviderID != want.name || observation.Identity.Endpoint != want.name || observation.Identity.Port != want.port || observation.Identity.Database != database || observation.Identity.TLSMode != "" || observation.Identity.TLSServerName != tlsServerName {
			return false
		}
	}
	return true
}
func containsLiveCredential(value []byte) bool {
	for _, canary := range []string{"LivePgAdmin_123", "LivePgClient_123", "LiveRedisAdmin_123", "LiveRedisClient_123"} {
		if bytes.Contains(value, []byte(canary)) {
			return true
		}
	}
	return false
}
func liveMachineIdentity(host string) hostcontract.MachineIdentity {
	return hostcontract.MachineIdentity{Value: "mid1:" + machineIdentityDigest(liveMachineID(host))}
}
func machineIdentityDigest(machine string) string {
	key := []byte("sub2api-host-machine-identity-v1")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(machine))
	return hex.EncodeToString(mac.Sum(nil))
}
func (f *liveFixture) appContainerReady(t *testing.T) bool {
	return f.sandboxShellOK(t, "app", 10*time.Second, `id=$(docker ps --filter ancestor=sub2api-live-app:mx-allowlist -q); [ -n "$id" ]; docker exec "$id" wget -q -O /dev/null http://127.0.0.1:8080/ready`)
}
func (f *liveFixture) sandboxShellOK(t *testing.T, host string, timeout time.Duration, script string) bool {
	argv := f.sandboxArgs(t, host, "sh", "-ceu", script)
	return f.commandOK(t, timeout, argv[0], argv[1:]...)
}

const liveBoundedBufferLimit = 4096

type boundedBuffer struct {
	data     []byte
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	available := liveBoundedBufferLimit - len(b.data)
	if available > 0 {
		n := available
		if n > len(p) {
			n = len(p)
		}
		b.data = append(b.data, p[:n]...)
	}
	if len(p) > available {
		b.overflow = true
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return append([]byte(nil), b.data...) }

const liveRecordLineLimit = 256

type liveRecordCapture struct {
	mu            sync.Mutex
	fallback      boundedBuffer
	line          [liveRecordLineLimit]byte
	lineLen       int
	overflow      bool
	marker        bool
	rolling       [32]byte
	rollLen       int
	stage         string
	records       []string
	next          int
	observerSeen  bool
	snapshotSeen  bool
	readinessSeen bool
	lifecycleSeen bool
	preflightSeen bool
	invalid       bool
	acceptRecords bool
}

func newLiveRecordCapture() *liveRecordCapture { return &liveRecordCapture{acceptRecords: true} }

func newLiveStdoutCapture() *liveRecordCapture { return &liveRecordCapture{} }

func (c *liveRecordCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.fallback.Write(p)
	for _, b := range p {
		if b == '\n' {
			c.finishLine()
			continue
		}
		if c.lineLen < len(c.line) {
			c.line[c.lineLen] = b
			c.lineLen++
		} else {
			c.overflow = true
		}
		if c.rollLen < len(c.rolling) {
			c.rolling[c.rollLen] = b
			c.rollLen++
		} else {
			copy(c.rolling[:], c.rolling[1:])
			c.rolling[len(c.rolling)-1] = b
		}
		c.marker = c.marker || c.hasMarker()
	}
	return len(p), nil
}

func (c *liveRecordCapture) hasMarker() bool {
	b := c.rolling[:c.rollLen]
	return bytes.Contains(b, []byte("SUB2API_LIVE_STAGE=")) || bytes.Contains(b, []byte("live milestone:")) || bytes.Contains(b, []byte("live observer:")) || bytes.Contains(b, []byte(livePostCreateSnapshotMarker)) || bytes.Contains(b, []byte(livePostgresReadinessMarker)) || bytes.Contains(b, []byte(livePostgresLifecycleMarker)) || bytes.Contains(b, []byte(liveSSHDockerPreflightMarker))
}

func (c *liveRecordCapture) finishLine() {
	record := string(c.line[:c.lineLen]) + "\n"
	marker := c.marker
	if marker && !c.acceptRecords {
		c.invalid = true
	}
	if c.overflow && marker {
		c.invalid = true
	} else if !c.overflow {
		if strings.Contains(record, "SUB2API_LIVE_STAGE=") {
			stage, ok := strings.CutPrefix(record, "SUB2API_LIVE_STAGE=")
			if !ok || liveFailureCategory(context.Background(), []byte(record)) != strings.TrimSuffix(stage, "\n") {
				c.invalid = true
			} else {
				c.stage = strings.TrimSuffix(stage, "\n")
			}
		}
		if strings.Contains(record, "live milestone:") {
			expected := ""
			if c.next < len(liveMilestones) {
				expected = "live milestone: " + liveMilestones[c.next] + "\n"
			}
			if record != expected {
				c.invalid = true
			} else {
				c.records = append(c.records, record)
				c.next++
			}
		}
		if strings.Contains(record, "live observer:") {
			if (record != "live observer: observer-error\n" && record != "live observer: observer-inconclusive\n") || c.observerSeen {
				c.invalid = true
			} else {
				c.observerSeen = true
				c.records = append(c.records, record)
			}
		}
		if strings.Contains(record, livePostCreateSnapshotMarker) {
			if !livePostCreateSnapshotRecord(record) || c.snapshotSeen {
				c.invalid = true
			} else {
				c.snapshotSeen = true
				c.records = append(c.records, record)
			}
		}
		if strings.Contains(record, livePostgresReadinessMarker) {
			if !livePostgresReadinessRecord(record) || c.readinessSeen {
				c.invalid = true
			} else {
				c.readinessSeen = true
				c.records = append(c.records, record)
			}
		}
		if strings.Contains(record, livePostgresLifecycleMarker) {
			if !livePostgresLifecycleRecordValid(record) || c.lifecycleSeen {
				c.invalid = true
			} else {
				c.lifecycleSeen = true
				c.records = append(c.records, record)
			}
		}
		if strings.Contains(record, liveSSHDockerPreflightMarker) {
			if !liveSSHDockerPreflightRecord(record) || c.preflightSeen {
				c.invalid = true
			} else {
				c.preflightSeen = true
				c.records = append(c.records, record)
			}
		}
	}
	c.lineLen, c.overflow, c.marker, c.rollLen = 0, false, false, 0
}

const liveSSHDockerPreflightMarker = "live ssh docker preflight:"

var liveSSHDockerPreflightSockets = map[string]bool{"present": true, "missing": true, "not-socket": true}
var liveSSHDockerPreflightDocker = map[string]bool{"ok": true, "failed": true}
var liveSSHDockerPreflightOverride = map[string]bool{"set": true, "unset": true}
var liveBootstrapDiscoveryCategories = map[string]bool{"empty": true, "unowned": true, "owned": true, "malformed": true, "failed": true}

const liveSSHDockerPreflightScript = `if test -S /var/run/docker.sock; then socket=present; elif test -e /var/run/docker.sock; then socket=not-socket; else socket=missing; fi
if test "${DOCKER_HOST+x}" = x; then docker_host=set; else docker_host=unset; fi
if test "${DOCKER_CONTEXT+x}" = x; then docker_context=set; else docker_context=unset; fi
if test "${DOCKER_CONFIG+x}" = x; then docker_config=set; else docker_config=unset; fi
printf 'socket=%s docker-host=%s docker-context=%s docker-config=%s\n' "$socket" "$docker_host" "$docker_context" "$docker_config"`

var liveBootstrapDiscoveryCommands = [...]string{
	`docker container ls --all --filter label=sub2api.host --format '{{.Names}}\t{{.Label "sub2api.host"}}'`,
	`docker network ls --filter label=sub2api.host --format '{{.Name}}\t{{.Label "sub2api.host"}}'`,
}

func liveSSHDockerPreflightArgs(alias string) []string {
	remote := "/bin/sh -c '" + liveShellQuote(liveSSHDockerPreflightScript) + "' fixed-argv0"
	return append([]string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "--", alias}, remote)
}

func liveSSHDockerPreflight(t *testing.T, parent context.Context, alias string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, liveCommandTimeout)
	defer cancel()
	output, err := liveSSHPreflightCommand(ctx, alias, liveSSHDockerPreflightScript)
	if err != nil || ctx.Err() != nil {
		return false
	}
	fields := strings.Fields(strings.TrimSuffix(string(output), "\n"))
	if len(fields) != 4 {
		return false
	}
	socket, socketOK := strings.CutPrefix(fields[0], "socket=")
	host, hostOK := strings.CutPrefix(fields[1], "docker-host=")
	dockerContext, contextOK := strings.CutPrefix(fields[2], "docker-context=")
	config, configOK := strings.CutPrefix(fields[3], "docker-config=")
	if !socketOK || !hostOK || !contextOK || !configOK || !liveSSHDockerPreflightSockets[socket] || !liveSSHDockerPreflightOverride[host] || !liveSSHDockerPreflightOverride[dockerContext] || !liveSSHDockerPreflightOverride[config] || string(output) != fmt.Sprintf("socket=%s docker-host=%s docker-context=%s docker-config=%s\n", socket, host, dockerContext, config) {
		return false
	}
	containerOutput, containerErr := liveSSHPreflightCommand(ctx, alias, liveBootstrapDiscoveryCommands[0])
	networkOutput, networkErr := liveSSHPreflightCommand(ctx, alias, liveBootstrapDiscoveryCommands[1])
	container := liveBootstrapDiscoveryClassification(containerOutput, containerErr)
	network := liveBootstrapDiscoveryClassification(networkOutput, networkErr)
	record := fmt.Sprintf("%s socket=%s container=%s network=%s container-discovery=%s network-discovery=%s docker-host=%s docker-context=%s docker-config=%s\n", liveSSHDockerPreflightMarker, socket, liveDockerCommandCategory(containerErr), liveDockerCommandCategory(networkErr), container, network, host, dockerContext, config)
	if !liveSSHDockerPreflightRecord(record) {
		return false
	}
	_, _ = os.Stderr.WriteString(record)
	return true
}

func liveSSHPreflightCommand(ctx context.Context, alias, remote string) ([]byte, error) {
	args := liveSSHDockerPreflightArgs(alias)
	args[len(args)-1] = "/bin/sh -c '" + liveShellQuote(remote) + "' fixed-argv0"
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var output liveDiscoveryOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.Bytes(), output.commandError(err)
}

const liveDiscoveryOutputLimit = 64 * 1024

type liveDiscoveryOutput struct {
	data []byte
}

func (b *liveDiscoveryOutput) Write(p []byte) (int, error) {
	const captureLimit = liveDiscoveryOutputLimit + 1
	if len(b.data) < captureLimit {
		n := min(len(p), captureLimit-len(b.data))
		b.data = append(b.data, p[:n]...)
	}
	return len(p), nil
}

func (b *liveDiscoveryOutput) Bytes() []byte { return append([]byte(nil), b.data...) }

func (b *liveDiscoveryOutput) commandError(err error) error {
	if len(b.data) > liveDiscoveryOutputLimit {
		return errors.New("live discovery output limit")
	}
	return err
}

func liveDockerCommandCategory(err error) string {
	if err != nil {
		return "failed"
	}
	return "ok"
}

func liveBootstrapDiscoveryClassification(out []byte, commandErr error) string {
	if commandErr != nil {
		return "failed"
	}
	if len(out) > liveDiscoveryOutputLimit || bytes.IndexByte(out, '\r') >= 0 {
		return "malformed"
	}
	if len(out) == 0 {
		return "empty"
	}
	lines := strings.Split(string(out), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || len(lines) > 1024 {
		return "malformed"
	}
	seen := map[string]bool{}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || fields[0] == "" || !utf8.ValidString(fields[0]) || !utf8.ValidString(fields[1]) || seen[fields[0]] {
			return "malformed"
		}
		seen[fields[0]] = true
		if fields[1] != "" {
			return "owned"
		}
	}
	return "unowned"
}

func liveSSHDockerPreflightRecord(record string) bool {
	fields := strings.Fields(strings.TrimSuffix(record, "\n"))
	if len(fields) != 12 || strings.Join(fields[:4], " ") != liveSSHDockerPreflightMarker {
		return false
	}
	socket, socketOK := strings.CutPrefix(fields[4], "socket=")
	container, containerOK := strings.CutPrefix(fields[5], "container=")
	network, networkOK := strings.CutPrefix(fields[6], "network=")
	containerDiscovery, containerDiscoveryOK := strings.CutPrefix(fields[7], "container-discovery=")
	networkDiscovery, networkDiscoveryOK := strings.CutPrefix(fields[8], "network-discovery=")
	host, hostOK := strings.CutPrefix(fields[9], "docker-host=")
	context, contextOK := strings.CutPrefix(fields[10], "docker-context=")
	config, configOK := strings.CutPrefix(fields[11], "docker-config=")
	return socketOK && containerOK && networkOK && containerDiscoveryOK && networkDiscoveryOK && hostOK && contextOK && configOK && liveSSHDockerPreflightSockets[socket] && liveSSHDockerPreflightDocker[container] && liveSSHDockerPreflightDocker[network] && liveBootstrapDiscoveryCategories[containerDiscovery] && liveBootstrapDiscoveryCategories[networkDiscovery] && liveSSHDockerPreflightOverride[host] && liveSSHDockerPreflightOverride[context] && liveSSHDockerPreflightOverride[config] && record == fmt.Sprintf("%s socket=%s container=%s network=%s container-discovery=%s network-discovery=%s docker-host=%s docker-context=%s docker-config=%s\n", liveSSHDockerPreflightMarker, socket, container, network, containerDiscovery, networkDiscovery, host, context, config)
}

func livePostCreateSnapshotRecord(record string) bool {
	fields := strings.Fields(strings.TrimSuffix(record, "\n"))
	if len(fields) != 6 || strings.Join(fields[:3], " ") != livePostCreateSnapshotMarker {
		return false
	}
	state, stateOK := strings.CutPrefix(fields[3], "state=")
	postgres, postgresOK := strings.CutPrefix(fields[4], "postgres=")
	redis, redisOK := strings.CutPrefix(fields[5], "redis=")
	if !stateOK || !postgresOK || !redisOK || !livePostCreateStates[state] || !livePostCreateContainers[postgres] || !livePostCreateContainers[redis] {
		return false
	}
	if record != fmt.Sprintf("%s state=%s postgres=%s redis=%s\n", livePostCreateSnapshotMarker, state, postgres, redis) {
		return false
	}
	if state == "pending-exact" {
		return postgres != "not-inspected" && redis != "not-inspected"
	}
	return postgres == "not-inspected" && redis == "not-inspected"
}

func livePostgresReadinessRecord(record string) bool {
	fields := strings.Fields(strings.TrimSuffix(record, "\n"))
	if len(fields) != 6 || strings.Join(fields[:3], " ") != livePostgresReadinessMarker {
		return false
	}
	pgdata, pgdataOK := strings.CutPrefix(fields[3], "pgdata=")
	server, serverOK := strings.CutPrefix(fields[4], "server=")
	psql, psqlOK := strings.CutPrefix(fields[5], "psql=")
	return pgdataOK && serverOK && psqlOK && livePostgresPGDataCategories[pgdata] && livePostgresServerCategories[server] && livePostgresPSQLCategories[psql] && record == fmt.Sprintf("%s pgdata=%s server=%s psql=%s\n", livePostgresReadinessMarker, pgdata, server, psql)
}

func livePostgresLifecycleRecordValid(record string) bool {
	if len(record) >= liveRecordLineLimit {
		return false
	}
	fields := strings.Fields(strings.TrimSuffix(record, "\n"))
	if len(fields) != 13 || strings.Join(fields[:3], " ") != livePostgresLifecycleMarker {
		return false
	}
	execResult, execOK := strings.CutPrefix(fields[3], "exec=")
	absoluteResult, absoluteOK := strings.CutPrefix(fields[4], "absolute=")
	absoluteError, absoluteErrorOK := strings.CutPrefix(fields[5], "absolute-error=")
	rootfs, rootfsOK := strings.CutPrefix(fields[6], "rootfs=")
	direct, directOK := strings.CutPrefix(fields[7], "direct=")
	directError, directErrorOK := strings.CutPrefix(fields[8], "direct-error=")
	state, stateOK := strings.CutPrefix(fields[9], "state=")
	restarts, restartsOK := strings.CutPrefix(fields[10], "restarts=")
	oom, oomOK := strings.CutPrefix(fields[11], "oom=")
	errorValue, errorOK := strings.CutPrefix(fields[12], "error=")
	if !execOK || !absoluteOK || !absoluteErrorOK || !rootfsOK || !directOK || !directErrorOK ||
		!stateOK || !restartsOK || !oomOK || !errorOK {
		return false
	}
	if !livePostgresLifecycleExecCategories[execResult] ||
		!livePostgresLifecycleExecCategories[absoluteResult] ||
		!livePostgresAbsoluteErrorCategories[absoluteError] {
		return false
	}
	if !livePostgresRootfsCategories[rootfs] ||
		!livePostgresLifecycleExecCategories[direct] ||
		!livePostgresAbsoluteErrorCategories[directError] {
		return false
	}
	if !map[string]bool{"running": true, "restarting": true, "exited": true, "created": true, "paused": true, "dead": true, "removing": true, "failed": true}[state] ||
		!map[string]bool{"zero": true, "nonzero": true, "unknown": true}[restarts] ||
		!map[string]bool{"yes": true, "no": true, "unknown": true}[oom] ||
		!map[string]bool{"present": true, "absent": true, "unknown": true}[errorValue] {
		return false
	}
	return record == fmt.Sprintf("%s exec=%s absolute=%s absolute-error=%s rootfs=%s direct=%s direct-error=%s state=%s restarts=%s oom=%s error=%s\n", livePostgresLifecycleMarker, execResult, absoluteResult, absoluteError, rootfs, direct, directError, state, restarts, oom, errorValue)
}

func (c *liveRecordCapture) failureBytes(stdout []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stage != "" {
		return []byte("SUB2API_LIVE_STAGE=" + c.stage + "\n")
	}
	if out := c.fallback.Bytes(); len(out) != 0 {
		return out
	}
	return stdout
}

func (c *liveRecordCapture) forward(w io.Writer) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lineLen != 0 && c.marker {
		c.invalid = true
	}
	if c.invalid {
		_, _ = io.WriteString(w, "live observer: observer-error\n")
		return false
	}
	for _, record := range c.records {
		_, _ = io.WriteString(w, record)
	}
	return true
}

type liveProvider struct {
	client pulumirpc.ResourceProviderClient
	cmd    *exec.Cmd
	done   chan error
}

func startLiveProvider(t *testing.T, binary string) *liveProvider {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(os.Getenv("SUB2API_PROVIDER_RUNTIME_LIVE_ROOT"), "home"))
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal("released Provider start failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	port := readProviderPort(t, stdout)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, net.JoinHostPort("127.0.0.1", port), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal("released Provider connection failed")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &liveProvider{client: pulumirpc.NewResourceProviderClient(conn), cmd: cmd, done: done}
}
func (p *liveProvider) close(t *testing.T) {
	t.Helper()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Error("released Provider cleanup failed")
	}
}

func liveDataInputs(release, destination, source string) property.Map {
	target := hostcontract.Target{
		ReleaseArtifact: release,
		DataServices: []hostcontract.LocalDataServiceTarget{
			{
				ID:          "postgres",
				Type:        "postgres",
				Port:        5432,
				Persistence: true,
				Bindings: []hostcontract.LocalDataBinding{
					{Address: destination, AllowedSources: []string{source}},
				},
				Clients: []hostcontract.LocalDataClient{
					{AppID: "api", Username: "api_user", Database: "api_db"},
				},
			},
			{
				ID:          "redis",
				Type:        "redis",
				Port:        6379,
				Persistence: true,
				Bindings: []hostcontract.LocalDataBinding{
					{Address: destination, AllowedSources: []string{source}},
				},
				Clients: []hostcontract.LocalDataClient{
					{AppID: "api", Username: "api_user", Database: "0"},
				},
			},
		},
	}
	secrets := hostcontract.Secrets{
		LocalDataServices: map[string]hostcontract.LocalDataServiceSecrets{
			"postgres": {
				AdminPassword:   "LivePgAdmin_123",
				ClientPasswords: map[string]string{"api": "LivePgClient_123"},
			},
			"redis": {
				AdminPassword:   "LiveRedisAdmin_123",
				ClientPasswords: map[string]string{"api": "LiveRedisClient_123"},
			},
		},
	}
	return property.NewMap(map[string]property.Value{"resource": jsonProperty(hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}), "server": jsonProperty(hostcontract.ServerTarget{SSHAlias: "live-data"}), "target": jsonProperty(target), "secrets": jsonProperty(secrets).WithSecret(true)})
}
func liveAppInputs(release, dataIP string) property.Map {
	target := hostcontract.Target{ReleaseArtifact: release, Apps: []hostcontract.AppTarget{{
		ID: "api", Image: "sub2api-live-app:mx-allowlist", Hostname: "live-app.example", ReadinessPath: "/ready", InitialAdminEmail: "admin@example.test",
		DataLinks: []hostcontract.DataLink{
			{Name: "postgres", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "live-postgres", Endpoint: dataIP, Port: 5432, Database: "api_db", TLSMode: "disable"}},
			{Name: "redis", Identity: hostcontract.DataIdentity{Kind: "redis", ProviderID: "live-redis", Endpoint: dataIP, Port: 6379, Database: "0", TLSMode: "disable"}},
		},
	}}}
	secrets := hostcontract.Secrets{Apps: map[string]hostcontract.AppSecrets{
		"api": {Postgres: &hostcontract.DataCredentials{Username: "api_user", Password: "LivePgClient_123"}, Redis: &hostcontract.DataCredentials{Username: "api_user", Password: "LiveRedisClient_123"}},
	}}
	return property.NewMap(map[string]property.Value{"resource": jsonProperty(hostcontract.ResourceIdentity{Environment: "live", ServerKey: "app"}), "server": jsonProperty(hostcontract.ServerTarget{SSHAlias: "live-app"}), "target": jsonProperty(target), "secrets": jsonProperty(secrets).WithSecret(true)})
}
func command(t *testing.T, timeout time.Duration, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out boundedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if cmd.Run() != nil || ctx.Err() != nil {
		t.Fatal("live prerequisite failed")
	}
}
func commandOutput(t *testing.T, timeout time.Duration, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out boundedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if cmd.Run() != nil || ctx.Err() != nil {
		t.Fatal("live prerequisite failed")
	}
	return out.Bytes()
}
func liveToken(value string) string {
	sum := sha256.Sum256([]byte(value + strconv.FormatInt(time.Now().UnixNano(), 10)))
	return hex.EncodeToString(sum[:])[:8]
}
func liveFailureCategory(ctx context.Context, output []byte) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	knownStages := map[string]bool{
		"network-setup":                     true,
		"sandbox-start":                     true,
		"data-mount-setup":                  true,
		"data-docker-start":                 true,
		"data-docker-network":               true,
		"data-docker-storage":               true,
		"data-docker-cgroup":                true,
		"data-docker-helper":                true,
		"data-docker-config":                true,
		"data-docker-filesystem":            true,
		"data-docker-initialization":        true,
		"data-docker-containerd":            true,
		"data-docker-containerd-timeout":    true,
		"data-docker-containerd-path":       true,
		"data-docker-containerd-socket":     true,
		"data-docker-containerd-exit":       true,
		"data-docker-conflict":              true,
		"data-docker-resource":              true,
		"data-docker-permission":            true,
		"data-docker-timeout":               true,
		"data-docker-unknown":               true,
		"data-image-load":                   true,
		"data-sshd-start":                   true,
		"app-mount-setup":                   true,
		"app-docker-start":                  true,
		"app-docker-network":                true,
		"app-docker-storage":                true,
		"app-docker-cgroup":                 true,
		"app-docker-helper":                 true,
		"app-docker-config":                 true,
		"app-docker-filesystem":             true,
		"app-docker-initialization":         true,
		"app-docker-containerd":             true,
		"app-docker-containerd-timeout":     true,
		"app-docker-containerd-path":        true,
		"app-docker-containerd-socket":      true,
		"app-docker-containerd-exit":        true,
		"app-docker-conflict":               true,
		"app-docker-resource":               true,
		"app-docker-permission":             true,
		"app-docker-timeout":                true,
		"app-docker-unknown":                true,
		"app-image-load":                    true,
		"app-sshd-start":                    true,
		"sandboxes-ready":                   true,
		"namespace-prerequisites":           true,
		"provider-start":                    true,
		"provider-configure":                true,
		"data-ssh-probe":                    true,
		"data-ssh-probe-host-key":           true,
		"data-ssh-probe-protocol":           true,
		"data-ssh-probe-timeout":            true,
		"data-ssh-probe-transport":          true,
		"data-ssh-probe-unknown":            true,
		"data-create":                       true,
		"data-create-artifact":              true,
		"data-create-bootstrap":             true,
		"data-create-bootstrap-remote":      true,
		"data-create-host":                  true,
		"data-create-observation":           true,
		"data-create-response":              true,
		"data-create-timeout":               true,
		"data-create-transport":             true,
		"data-create-unknown":               true,
		"data-ready-check":                  true,
		"app-create":                        true,
		"app-ready-check":                   true,
		"post-create-assertions":            true,
		"complete":                          true,
	}
	lastStage := ""
	for _, line := range strings.Split(string(output), "\n") {
		stage, found := strings.CutPrefix(line, "SUB2API_LIVE_STAGE=")
		if found && knownStages[stage] {
			lastStage = stage
		}
	}
	if lastStage != "" {
		return lastStage
	}
	if len(output) == 0 {
		return "exit"
	}
	return "isolated-fixture-exit"
}

func reportLiveStage(stage string) {
	_, _ = os.Stderr.WriteString("SUB2API_LIVE_STAGE=" + stage + "\n")
}

func reportLiveMilestone(milestone string) {
	_, _ = os.Stderr.WriteString("live milestone: " + milestone + "\n")
}

func reportLiveNamespaceFailure(stage string) {
	_, _ = os.Stderr.WriteString("live namespace fixture failed: " + stage + "\n")
}

func liveDataCreateFailureStage(err error) string {
	const prefix = "data-create-"
	if err == nil {
		return prefix + "response"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "context deadline exceeded"):
		return prefix + "timeout"
	case strings.Contains(message, "transport failed") || strings.Contains(message, "transport unavailable"):
		return prefix + "transport"
	case strings.Contains(message, "host artifact"):
		return prefix + "artifact"
	case strings.Contains(message, "unsupported host"):
		return prefix + "host"
	case strings.Contains(message, "bootstrap remote response"):
		return prefix + "bootstrap-remote"
	case strings.Contains(message, "bootstrap response"):
		return prefix + "bootstrap"
	case strings.Contains(message, "inspect response") || strings.Contains(message, "remote observation"):
		return prefix + "observation"
	default:
		return prefix + "unknown"
	}
}

func liveSSHProbeFailureStage(err error) string {
	const prefix = "data-ssh-probe-"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return prefix + "timeout"
	case errors.Is(err, openssh.ErrHostKey):
		return prefix + "host-key"
	case errors.Is(err, openssh.ErrProtocol):
		return prefix + "protocol"
	case errors.Is(err, openssh.ErrTransport):
		return prefix + "transport"
	default:
		return prefix + "unknown"
	}
}

func TestLiveFailureCategoryReportsOnlyKnownLastStage(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "empty", want: "exit"},
		{name: "unknown marker", output: "SUB2API_LIVE_STAGE=credential-canary\n", want: "isolated-fixture-exit"},
		{name: "known marker", output: "SUB2API_LIVE_STAGE=data-create\n", want: "data-create"},
		{name: "bootstrap remote", output: "SUB2API_LIVE_STAGE=data-create-bootstrap-remote\n", want: "data-create-bootstrap-remote"},
		{name: "known Docker reason", output: "SUB2API_LIVE_STAGE=app-docker-network\n", want: "app-docker-network"},
		{name: "managed data containerd timeout", output: "SUB2API_LIVE_STAGE=data-docker-containerd-timeout\n", want: "data-docker-containerd-timeout"},
		{name: "managed app containerd timeout", output: "SUB2API_LIVE_STAGE=app-docker-containerd-timeout\n", want: "app-docker-containerd-timeout"},
		{name: "unknown Docker reason", output: "SUB2API_LIVE_STAGE=app-docker-sensitive-detail\n", want: "isolated-fixture-exit"},
		{name: "last known marker", output: "SUB2API_LIVE_STAGE=network-setup\nSUB2API_LIVE_STAGE=app-ready-check\n", want: "app-ready-check"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := liveFailureCategory(context.Background(), []byte(test.output)); got != test.want {
				t.Fatalf("liveFailureCategory() = %q, want %q", got, test.want)
			}
		})
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := liveFailureCategory(canceled, []byte("SUB2API_LIVE_STAGE=data-create\n")); got != "timeout" {
		t.Fatalf("liveFailureCategory(canceled) = %q, want timeout", got)
	}
}

func TestLiveDataCreateFailureStageReportsOnlyFixedClasses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "missing response", want: "data-create-response"},
		{name: "timeout", err: errors.New("rpc error: context deadline exceeded"), want: "data-create-timeout"},
		{name: "transport", err: errors.New("rpc error: transport failed"), want: "data-create-transport"},
		{name: "artifact", err: errors.New("rpc error: host artifact unavailable"), want: "data-create-artifact"},
		{name: "host", err: errors.New("rpc error: unsupported host"), want: "data-create-host"},
		{name: "bootstrap", err: errors.New("rpc error: invalid bootstrap response"), want: "data-create-bootstrap"},
		{name: "bootstrap remote", err: errors.New("rpc error: bootstrap remote response"), want: "data-create-bootstrap-remote"},
		{name: "observation", err: errors.New("rpc error: unsafe remote observation"), want: "data-create-observation"},
		{name: "unknown redacts detail", err: errors.New("credential canary"), want: "data-create-unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := liveDataCreateFailureStage(test.err); got != test.want {
				t.Fatalf("liveDataCreateFailureStage() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLiveSSHProbeFailureStageReportsOnlyFixedClasses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: "data-ssh-probe-timeout"},
		{name: "host key", err: fmt.Errorf("wrapped: %w", openssh.ErrHostKey), want: "data-ssh-probe-host-key"},
		{name: "protocol", err: fmt.Errorf("wrapped: %w", openssh.ErrProtocol), want: "data-ssh-probe-protocol"},
		{name: "transport", err: fmt.Errorf("wrapped: %w", openssh.ErrTransport), want: "data-ssh-probe-transport"},
		{name: "unknown", err: errors.New("credential canary"), want: "data-ssh-probe-unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := liveSSHProbeFailureStage(test.err); got != test.want {
				t.Fatalf("liveSSHProbeFailureStage() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLiveSSHDockerPreflightUsesFixedSSHTransportAndRedactsOutput(t *testing.T) {
	if got := liveSSHDockerPreflightArgs("live-data"); !slices.Equal(got, []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "--", "live-data", "/bin/sh -c '" + liveShellQuote(liveSSHDockerPreflightScript) + "' fixed-argv0"}) {
		t.Fatalf("argv = %#v", got)
	}
	source := string(mustRead(t, filepath.Join(repositoryRoot(t), "internal", "openssh", "openssh.go")))
	for _, want := range liveSSHDockerPreflightArgs("live-data")[:len(liveSSHDockerPreflightArgs("live-data"))-2] {
		if want != "live-data" && !strings.Contains(source, strconv.Quote(want)) {
			t.Fatalf("production SSH transport changed; missing %q", want)
		}
	}
	if !strings.Contains(liveSSHDockerPreflightScript, `test -S /var/run/docker.sock`) || liveBootstrapDiscoveryCommands[0] != `docker container ls --all --filter label=sub2api.host --format '{{.Names}}\t{{.Label "sub2api.host"}}'` || liveBootstrapDiscoveryCommands[1] != `docker network ls --filter label=sub2api.host --format '{{.Name}}\t{{.Label "sub2api.host"}}'` {
		t.Fatal("preflight does not use exact fixed Docker bootstrap discovery")
	}
	if strings.Contains(liveSSHDockerPreflightScript, "-H") || strings.Contains(liveSSHDockerPreflightScript, `"$DOCKER_`) {
		t.Fatal("preflight script leaks override values or changes Docker transport")
	}
}

func TestLiveSSHDockerPreflightRecordAndCaptureFailClosed(t *testing.T) {
	good := "live ssh docker preflight: socket=present container=ok network=ok container-discovery=empty network-discovery=empty docker-host=unset docker-context=unset docker-config=unset\n"
	if !liveSSHDockerPreflightRecord(good) {
		t.Fatal("good preflight rejected")
	}
	for _, input := range []string{
		"prefix " + good,
		"live ssh docker preflight: socket=present container=ok network=ok container-discovery=empty network-discovery=empty docker-host=set docker-context=unset docker-config=unset canary\n",
		"live ssh docker preflight: socket=bad container=ok network=ok container-discovery=empty network-discovery=empty docker-host=unset docker-context=unset docker-config=unset\n",
		strings.Repeat("x", liveRecordLineLimit+1) + good,
		good + good,
	} {
		capture := newLiveRecordCapture()
		_, _ = capture.Write([]byte(input))
		var out bytes.Buffer
		if capture.forward(&out) || out.String() != "live observer: observer-error\n" {
			t.Fatalf("unsafe preflight record forwarded: %q", out.String())
		}
	}
}

func TestLiveRecordCaptureForwardsFullValidDiscoveryPreflight(t *testing.T) {
	record := "live ssh docker preflight: socket=present container=ok network=ok container-discovery=empty network-discovery=unowned docker-host=unset docker-context=unset docker-config=unset\n"
	capture := newLiveRecordCapture()
	_, _ = capture.Write([]byte(record))
	var out bytes.Buffer
	if !capture.forward(&out) || out.String() != record {
		t.Fatalf("preflight forwarding = %q", out.String())
	}
}

func TestLivePostgresReadinessRecordClassifiesFixedCategoriesAndCaptureFailsClosed(t *testing.T) {
	good := "live postgres readiness: pgdata=present server=accepting psql=ok\n"
	if !livePostgresReadinessRecord(good) {
		t.Fatal("good postgres readiness rejected")
	}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"pgdata present", nil, "present"},
		{"pgdata absent", liveExitError(t, 1), "absent"},
		{"pgdata failed", liveExitError(t, 2), "failed"},
		{"pgdata start failed", errors.New("start failed"), "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := livePostgresPGDataCategory(test.err); got != test.want {
				t.Fatalf("pgdata category = %q, want %q", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		code int
		want string
	}{{0, "accepting"}, {1, "rejecting"}, {2, "no-response"}, {3, "failed"}} {
		var err error
		if test.code != 0 {
			err = liveExitError(t, test.code)
		}
		if got := livePostgresServerCategory(err); got != test.want {
			t.Fatalf("server category = %q, want %q", got, test.want)
		}
	}
	if livePostgresPSQLCategory(nil) != "ok" || livePostgresPSQLCategory(liveExitError(t, 1)) != "failed" {
		t.Fatal("psql category is not fixed")
	}
	if len(good) > liveRecordLineLimit {
		t.Fatalf("readiness record length = %d", len(good))
	}
	expected := liveDataContainerExpectation{kind: "postgres", name: "exact-postgres", port: 5433}
	if got, want := livePostgresPGDataArgs(expected), []string{"docker", "exec", "exact-postgres", "test", "-s", "/var/lib/postgresql/data/PG_VERSION"}; !slices.Equal(got, want) {
		t.Fatalf("pgdata argv = %#v, want %#v", got, want)
	}
	if got, want := livePostgresServerArgs(expected), []string{"docker", "exec", "exact-postgres", "pg_isready", "-h", "/var/run/postgresql", "-p", "5433", "-d", "postgres"}; !slices.Equal(got, want) {
		t.Fatalf("server argv = %#v, want %#v", got, want)
	}
	capture := newLiveRecordCapture()
	_, _ = capture.Write([]byte(good))
	var out bytes.Buffer
	if !capture.forward(&out) || out.String() != good {
		t.Fatalf("readiness forwarding = %q", out.String())
	}
	for _, input := range []string{
		"prefix " + good,
		"live postgres readiness: pgdata=present server=accepting psql=ok extra=field\n",
		good + good,
		strings.Repeat("x", liveRecordLineLimit+1) + good,
	} {
		capture := newLiveRecordCapture()
		_, _ = capture.Write([]byte(input))
		out.Reset()
		if capture.forward(&out) || out.String() != "live observer: observer-error\n" {
			t.Fatalf("unsafe readiness record forwarded: %q", out.String())
		}
	}
}

func liveExitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatal("expected nonzero exit")
	}
	return err
}

func TestLiveBootstrapDiscoveryClassificationMirrorsReleasedValidation(t *testing.T) {
	tests := []struct {
		name string
		out  []byte
		want string
	}{
		{name: "empty", want: "empty"},
		{name: "accepted unowned row", out: []byte("existing-object\t\n"), want: "unowned"},
		{name: "ownership conflict", out: []byte("existing-object\towned\n"), want: "owned"},
		{name: "duplicate name", out: []byte("existing-object\t\nexisting-object\t\n"), want: "malformed"},
		{name: "carriage return", out: []byte("existing-object\t\r\n"), want: "malformed"},
		{name: "bad fields", out: []byte("existing-object\n"), want: "malformed"},
		{name: "too large", out: bytes.Repeat([]byte("x"), liveDiscoveryOutputLimit+1), want: "malformed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := liveBootstrapDiscoveryClassification(test.out, nil); got != test.want {
				t.Fatalf("classification = %q, want %q", got, test.want)
			}
		})
	}
	if got := liveBootstrapDiscoveryClassification(nil, errors.New("docker failed")); got != "failed" {
		t.Fatalf("command failure classification = %q, want failed", got)
	}
}

func TestLiveDiscoveryOutputLimitClassifiesAsCommandFailure(t *testing.T) {
	var output liveDiscoveryOutput
	_, _ = output.Write(bytes.Repeat([]byte("x"), liveDiscoveryOutputLimit+1))
	err := output.commandError(nil)
	if err == nil {
		t.Fatal("output limit did not fail command")
	}
	if got := liveDockerCommandCategory(err); got != "failed" {
		t.Fatalf("command category = %q, want failed", got)
	}
	if got := liveBootstrapDiscoveryClassification(output.Bytes(), err); got != "failed" {
		t.Fatalf("discovery category = %q, want failed", got)
	}
}

func TestLiveSSHDockerPreflightRecordRejectsMalformedDiscoveryMarkers(t *testing.T) {
	good := "live ssh docker preflight: socket=present container=ok network=ok container-discovery=empty network-discovery=unowned docker-host=unset docker-context=unset docker-config=unset\n"
	if !liveSSHDockerPreflightRecord(good) {
		t.Fatal("good discovery preflight rejected")
	}
	for _, record := range []string{
		strings.Replace(good, "container-discovery=empty", "container-discovery=unknown", 1),
		strings.Replace(good, "network-discovery=unowned", "network-discovery=unowned extra=marker", 1),
		strings.Replace(good, "network-discovery=unowned", "network-discovery=unowned\r", 1),
	} {
		if liveSSHDockerPreflightRecord(record) {
			t.Fatalf("malformed discovery record accepted: %q", record)
		}
	}
}

func TestLiveMilestoneObserverEmitsOnlyOrderedFixedMilestones(t *testing.T) {
	var events []string
	observer := liveMilestoneObserver{expectations: liveTestDataExpectations, inspect: matchingLiveContainer, ready: func(context.Context, liveDataContainerExpectation) (bool, error) { return true, nil }, emit: func(milestone string) { events = append(events, milestone) }, poll: time.Millisecond}
	result := observer.run(context.Background())
	if result.observerError || result.lastMilestone != liveFinalMilestone {
		t.Fatalf("result = %#v", result)
	}
	if strings.Join(events, ",") != strings.Join(liveMilestones[:], ",") {
		t.Fatalf("events = %q, want %q", events, liveMilestones)
	}
}

func liveTestDataExpectations(context.Context) ([]liveDataContainerExpectation, bool, error) {
	return []liveDataContainerExpectation{
		{kind: "postgres", name: "pg", image: "postgres:18-alpine", owner: "owner", target: "target", port: 5432},
		{kind: "redis", name: "redis", image: "redis:8-alpine", owner: "owner", target: "target", port: 6379},
	}, true, nil
}

func matchingLiveContainer(_ context.Context, expected liveDataContainerExpectation) (liveContainerInspection, bool, error) {
	return liveContainerInspection{name: expected.name, image: expected.image, owner: expected.owner, target: expected.target, running: true}, true, nil
}

func TestLiveObserverUsesOnlyDerivedNonSecretFactsAndReleasedReadinessArgv(t *testing.T) {
	observer := mustRead(t, filepath.Join(repositoryRoot(t), "internal", "integration", "providerruntime", "live_mx_allowlist_linux_test.go"))
	start := bytes.Index(observer, []byte("type liveDataContainerExpectation struct"))
	end := bytes.Index(observer, []byte("func (f *liveFixture) createReady"))
	if start < 0 || end < start {
		t.Fatal("observer source boundary unavailable")
	}
	for _, forbidden := range []string{"liveCheckpointInputs", "hostcontract.Secrets", "hostcontract.TargetRevision", "property.Map"} {
		if bytes.Contains(observer[start:end], []byte(forbidden)) {
			t.Fatalf("observer source handles %s", forbidden)
		}
	}
	snapshotStart := bytes.Index(observer, []byte("func reportLivePostCreateSnapshot"))
	snapshotEnd := bytes.Index(observer, []byte("func (f *liveFixture) sandboxOutputForObserver"))
	if snapshotStart < 0 || snapshotEnd < snapshotStart {
		t.Fatal("snapshot source boundary unavailable")
	}
	snapshot := observer[snapshotStart:snapshotEnd]
	if !bytes.Contains(snapshot, []byte(`"docker", "container", "ls", "--all"`)) {
		t.Fatal("snapshot must list all containers")
	}
	postgres := liveDataReadinessArgs(liveDataContainerExpectation{kind: "postgres", name: "postgres-name", port: 5433})
	wantPostgres := []string{"docker", "exec", "postgres-name", "psql", "-X", "-U", "s2h_admin", "-d", "postgres", "-p", "5433", "-v", "ON_ERROR_STOP=1", "-c", "SELECT 1"}
	if strings.Join(postgres, "\x00") != strings.Join(wantPostgres, "\x00") {
		t.Fatalf("postgres readiness argv = %#v, want %#v", postgres, wantPostgres)
	}
}

func TestLiveMilestoneObserverRequiresExactIdentityAndJoinsBeforeCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	observer := liveMilestoneObserver{expectations: liveTestDataExpectations, inspect: func(ctx context.Context, _ liveDataContainerExpectation) (liveContainerInspection, bool, error) {
		close(entered)
		<-ctx.Done()
		return liveContainerInspection{}, false, ctx.Err()
	}, ready: func(context.Context, liveDataContainerExpectation) (bool, error) { return false, nil }, poll: time.Millisecond}
	done := make(chan liveMilestoneObserverResult, 1)
	go func() { done <- observer.run(ctx) }()
	<-entered
	cancel()
	result := <-done
	if result.observerError || result.lastMilestone != "none" {
		t.Fatalf("canceled result = %#v", result)
	}
	expectations, _, _ := liveTestDataExpectations(context.Background())
	want := expectations[0]
	for _, got := range []liveContainerInspection{
		{name: "other", image: want.image, owner: want.owner, target: want.target, running: true},
		{name: want.name, image: "postgres:latest", owner: want.owner, target: want.target, running: true},
		{name: want.name, image: want.image, owner: "wrong", target: want.target, running: true},
		{name: want.name, image: want.image, owner: want.owner, target: "other", running: true},
		{name: want.name, image: want.image, owner: want.owner, target: want.target, running: false},
	} {
		if exactLiveContainer(got, want) {
			t.Fatalf("ambiguous container matched: %#v", got)
		}
	}
}

func TestLiveDataObserverStateRequiresCurrentPendingReconcile(t *testing.T) {
	resource := hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}
	revision := liveTestRevision('c')
	prior := liveTestRevision('b')
	facts := liveDataObserverFacts{resource: resource, revision: revision, priorRevision: prior, machine: liveMachineIdentity("data"), release: "release"}
	valid := string(livePendingStateJSON(t, facts, "owner"))
	for _, test := range []struct {
		name, state string
		valid       bool
	}{
		{name: "valid", state: valid, valid: true},
		{name: "malformed", state: `{`},
		{name: "wrong resource", state: strings.Replace(valid, `"data"`, `"other"`, 1)},
		{name: "wrong action", state: strings.Replace(valid, `"reconcile"`, `"retire-preserve-data"`, 1)},
		{name: "complete", state: strings.Replace(valid, `"pending"`, `"complete"`, 1)},
		{name: "stale revision", state: strings.Replace(valid, revision, liveTestRevision('d'), 1)},
		{name: "wrong version", state: strings.Replace(valid, `"version":1`, `"version":2`, 1)},
		{name: "empty machine", state: strings.Replace(valid, `"machine":{"value":"`+facts.machine.Value+`"}`, `"machine":{"value":""}`, 1)},
		{name: "wrong applied revision", state: strings.Replace(valid, `"appliedRevision":"`+prior+`"`, `"appliedRevision":"`+liveTestRevision('d')+`"`, 1)},
		{name: "observation not ready", state: strings.Replace(valid, `"ready":true`, `"ready":false`, 1)},
		{name: "observation mismatch", state: strings.Replace(valid, `"machine":{"value":"`+facts.machine.Value+`"},"ownership"`, `"machine":{"value":"other"},"ownership"`, 1)},
		{name: "wrong expected machine", state: strings.ReplaceAll(valid, `"machine":{"value":"`+facts.machine.Value+`"}`, `"machine":{"value":"other"}`)},
		{name: "wrong release", state: strings.Replace(valid, `"hostRelease":"release"`, `"hostRelease":"other"`, 1)},
		{name: "drifted", state: strings.Replace(valid, `"ready":true`, `"drifted":true,"ready":true`, 1)},
		{name: "extra app", state: strings.Replace(valid, `,"journal":`, `,"observation":{"apps":[{"id":"extra","activeImage":"extra","ready":true}]},"journal":`, 1)},
		{name: "extra data", state: strings.Replace(valid, `,"journal":`, `,"observation":{"data":[{"identity":{"kind":"extra","providerId":"extra","endpoint":"extra","port":1},"ready":true}]},"journal":`, 1)},
		{name: "stale prior", state: strings.Replace(valid, `"priorAppliedRevision":"`+prior+`"`, `"priorAppliedRevision":"`+liveTestRevision('d')+`"`, 1)},
		{name: "result", state: strings.Replace(valid, `"status":"pending"`, `"status":"pending","result":{"status":"applied","appliedRevision":"`+revision+`"}`, 1)},
		{name: "approval", state: strings.Replace(valid, `"status":"pending"`, `"status":"pending","approval":{}`, 1)},
		{name: "last operation", state: strings.Replace(valid, `"journal":`, `"lastOperation":{},"journal":`, 1)},
		{name: "retirement", state: strings.Replace(valid, `"journal":`, `"retirement":{},"journal":`, 1)},
		{name: "unknown", state: strings.Replace(valid, `"version":1`, `"version":1,"unknown":true`, 1)},
		{name: "duplicate", state: strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1)},
		{name: "trailing", state: valid + ` {}`},
		{name: "oversized", state: valid + strings.Repeat(" ", 1<<20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, present, err := parseLiveDataObserverState([]byte(test.state), facts)
			if test.valid {
				if err != nil || !present {
					t.Fatalf("valid state = present %v, err %v", present, err)
				}
			} else if err == nil || present {
				t.Fatalf("unsafe state = present %v, err %v", present, err)
			}
		})
	}
}

func TestLiveMilestoneObserverFailsClosedAndAllowsOnlyPreStateAbsence(t *testing.T) {
	for _, test := range []struct {
		name    string
		state   func(context.Context) ([]liveDataContainerExpectation, bool, error)
		inspect func(context.Context, liveDataContainerExpectation) (liveContainerInspection, bool, error)
	}{
		{"malformed state", func(context.Context) ([]liveDataContainerExpectation, bool, error) {
			return nil, false, errors.New("malformed")
		}, nil},
		{"command error", liveTestDataExpectations, func(context.Context, liveDataContainerExpectation) (liveContainerInspection, bool, error) {
			return liveContainerInspection{}, false, errors.New("command")
		}},
		{"timeout", liveTestDataExpectations, func(context.Context, liveDataContainerExpectation) (liveContainerInspection, bool, error) {
			return liveContainerInspection{}, false, context.DeadlineExceeded
		}},
		{"identity mismatch", liveTestDataExpectations, func(context.Context, liveDataContainerExpectation) (liveContainerInspection, bool, error) {
			return liveContainerInspection{name: "pg", image: "wrong", owner: "owner", target: "target", running: true}, true, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := liveMilestoneObserver{expectations: test.state, inspect: test.inspect, ready: func(context.Context, liveDataContainerExpectation) (bool, error) { return true, nil }, poll: time.Millisecond}
			if result := observer.run(context.Background()); !result.observerError {
				t.Fatalf("result = %#v", result)
			}
		})
	}
	stateCalls := 0
	inspectionCalls := 0
	observer := liveMilestoneObserver{
		expectations: func(ctx context.Context) ([]liveDataContainerExpectation, bool, error) {
			stateCalls++
			if stateCalls == 1 {
				return nil, false, nil
			}
			if stateCalls == 2 {
				return liveTestDataExpectations(ctx)
			}
			return liveTestDataExpectations(ctx)
		},
		inspect: func(ctx context.Context, expected liveDataContainerExpectation) (liveContainerInspection, bool, error) {
			inspectionCalls++
			if inspectionCalls > 1 {
				return matchingLiveContainer(ctx, expected)
			}
			return liveContainerInspection{}, false, nil
		},
		ready: func(context.Context, liveDataContainerExpectation) (bool, error) { return true, nil },
		poll:  time.Millisecond,
	}
	if result := observer.run(context.Background()); result.observerError || result.lastMilestone != liveFinalMilestone || stateCalls != 3 {
		t.Fatalf("resolved initial absence = %#v, state calls = %d", result, stateCalls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	observer = liveMilestoneObserver{expectations: func(context.Context) ([]liveDataContainerExpectation, bool, error) { return nil, false, nil }, poll: time.Millisecond}
	done := make(chan liveMilestoneObserverResult, 1)
	go func() { done <- observer.run(ctx) }()
	cancel()
	if result := <-done; result.observerError || result.lastMilestone != "none" {
		t.Fatalf("pre-state cancellation = %#v", result)
	}
	deadline, deadlineCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer deadlineCancel()
	if result := observer.run(deadline); !result.observerError {
		t.Fatalf("deadline result = %#v", result)
	}
}

func TestLiveMilestoneObserverRevalidatesStateBeforeEachPoll(t *testing.T) {
	for _, test := range []struct {
		name string
		next func(context.Context) ([]liveDataContainerExpectation, bool, error)
	}{
		{"state absent", func(context.Context) ([]liveDataContainerExpectation, bool, error) { return nil, false, nil }},
		{"stale journal", func(context.Context) ([]liveDataContainerExpectation, bool, error) {
			return nil, false, errors.New("journal revision changed")
		}},
		{"ownership changed", func(ctx context.Context) ([]liveDataContainerExpectation, bool, error) {
			expectations, present, err := liveTestDataExpectations(ctx)
			expectations[0].owner = "new-owner"
			return expectations, present, err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateCalls := 0
			readyCalls := 0
			var events []string
			observer := liveMilestoneObserver{
				expectations: func(ctx context.Context) ([]liveDataContainerExpectation, bool, error) {
					stateCalls++
					if stateCalls == 1 {
						return liveTestDataExpectations(ctx)
					}
					return test.next(ctx)
				},
				inspect: matchingLiveContainer,
				ready: func(context.Context, liveDataContainerExpectation) (bool, error) {
					readyCalls++
					return readyCalls > 1, nil
				},
				emit: func(milestone string) { events = append(events, milestone) },
				poll: time.Millisecond,
			}
			result := observer.run(context.Background())
			if !result.observerError || result.lastMilestone != "postgres-owned-container" {
				t.Fatalf("result = %#v", result)
			}
			if stateCalls != 2 || strings.Join(events, ",") != "postgres-owned-container" {
				t.Fatalf("state calls = %d, events = %q", stateCalls, events)
			}
		})
	}
}

func TestLiveDataCreateRequiresRedisReadyObserverResult(t *testing.T) {
	for _, test := range []struct {
		result liveMilestoneObserverResult
		want   bool
	}{
		{liveMilestoneObserverResult{lastMilestone: liveFinalMilestone}, true},
		{liveMilestoneObserverResult{lastMilestone: "redis-owned-container"}, false},
		{liveMilestoneObserverResult{lastMilestone: liveFinalMilestone, observerError: true}, false},
	} {
		if got := liveDataCreateObserved(test.result); got != test.want {
			t.Fatalf("liveDataCreateObserved(%#v) = %v, want %v", test.result, got, test.want)
		}
	}
}

func TestLiveFailedCreateAlwaysEmitsOneObserverStatus(t *testing.T) {
	for _, test := range []struct {
		name            string
		createSucceeded bool
		result          liveMilestoneObserverResult
		want            string
	}{
		{"failed after redis ready", false, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone}, "observer-inconclusive"},
		{"failed observer error", false, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone, observerError: true}, "observer-error"},
		{"successful full observation", true, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := liveCreateObserverStatus(test.createSucceeded, test.result); got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLivePostCreateSnapshotClassifiesStateAndContainers(t *testing.T) {
	facts := liveTestDataObserverFacts(hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}, "revision")
	valid := livePendingStateJSON(t, facts, "owner")
	exit42 := exec.Command("sh", "-c", "exit 42").Run()
	for _, test := range []struct {
		name string
		err  error
		body []byte
		want string
	}{
		{"arbitrary exit 42 is unavailable", exit42, nil, "unavailable"},
		{"root absent", nil, []byte("root-absent\n"), "root-absent"},
		{"state absent", nil, []byte("state-absent\n"), "state-absent"},
		{"old absence framing is invalid", nil, []byte("absent\n"), "invalid"},
		{"malformed framing", nil, []byte("present\n{"), "invalid"},
		{"invalid", nil, []byte("{"), "invalid"},
		{"unavailable", errors.New("namespace unavailable"), nil, "unavailable"},
		{"not exact", nil, append([]byte("present\n"), []byte(strings.Replace(string(valid), `"pending"`, `"complete"`, 1))...), "not-exact"},
		{"pending exact", nil, append([]byte("present\n"), valid...), "pending-exact"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, _ := livePostCreateState(context.Background(), func(context.Context) ([]byte, error) { return test.body, test.err }, facts); got != test.want {
				t.Fatalf("state = %q, want %q", got, test.want)
			}
		})
	}
	want := liveDataContainerExpectation{name: "pg", image: "postgres:18-alpine", owner: "owner", target: "target", kind: "postgres", port: 5432}
	listArgs := []string{"docker", "container", "ls", "--all", "--filter", "name=^/pg$", "--format", "{{.Names}}"}
	inspectArgs := []string{"docker", "container", "inspect", "--format", "{{.Name}}\t{{.Config.Image}}\t{{index .Config.Labels \"sub2api.host\"}}\t{{index .Config.Labels \"sub2api.host.target\"}}\t{{.State.Status}}", "pg"}
	inspection := func(status string) string {
		return "/pg\tpostgres:18-alpine\towner\ttarget\t" + status + "\n"
	}
	for _, test := range []struct {
		name, list, inspect, want     string
		listErr, inspectErr, probeErr error
		probe                         []byte
		wantProbes                    int
	}{
		{"list error", "", "", "unavailable", errors.New("list failed"), nil, nil, nil, 0},
		{"absent", "", "", "absent", nil, nil, nil, nil, 0},
		{"duplicate list", "pg\npg\n", "", "ambiguous", nil, nil, nil, nil, 0},
		{"malformed list", "pg", "", "ambiguous", nil, nil, nil, nil, 0},
		{"inspect error", "pg\n", "", "unavailable", nil, errors.New("inspect failed"), nil, nil, 0},
		{"malformed inspect", "pg\n", "/pg\tpostgres:18-alpine\towner\ttarget\n", "ambiguous", nil, nil, nil, nil, 0},
		{"unterminated inspect", "pg\n", "/pg\tpostgres:18-alpine\towner\ttarget\trunning", "ambiguous", nil, nil, nil, nil, 0},
		{"identity mismatch", "pg\n", "/pg\twrong\towner\ttarget\trunning\n", "identity-mismatch", nil, nil, nil, nil, 0},
		{"exited", "pg\n", inspection("exited"), "exited", nil, nil, nil, nil, 0},
		{"created", "pg\n", inspection("created"), "not-running", nil, nil, nil, nil, 0},
		{"starting", "pg\n", inspection("starting"), "not-running", nil, nil, nil, nil, 0},
		{"restarting", "pg\n", inspection("restarting"), "not-running", nil, nil, nil, nil, 0},
		{"dead", "pg\n", inspection("dead"), "not-running", nil, nil, nil, nil, 0},
		{"running ready", "pg\n", inspection("running"), "ready", nil, nil, nil, []byte("ok"), 1},
		{"running probe failure", "pg\n", inspection("running"), "running-probe-failed", nil, nil, errors.New("probe failed"), nil, 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			var commands [][]string
			probes := 0
			got := livePostCreateContainer(context.Background(), want, func(_ context.Context, args ...string) ([]byte, error) {
				commands = append(commands, append([]string(nil), args...))
				if len(commands) == 1 {
					return []byte(test.list), test.listErr
				}
				return []byte(test.inspect), test.inspectErr
			}, func(context.Context, liveDataContainerExpectation) ([]byte, error) {
				probes++
				return test.probe, test.probeErr
			}, nil)
			if got != test.want {
				t.Fatalf("container = %q, want %q", got, test.want)
			}
			if len(commands) == 0 || !slices.Equal(commands[0], listArgs) {
				t.Fatalf("list argv = %#v, want %#v", commands, listArgs)
			}
			wantInspect := test.listErr == nil && test.list == want.name+"\n"
			if (len(commands) == 2) != wantInspect || (wantInspect && !slices.Equal(commands[1], inspectArgs)) {
				t.Fatalf("inspect argv = %#v, want inspect %t %#v", commands, wantInspect, inspectArgs)
			}
			if probes != test.wantProbes {
				t.Fatalf("probes = %d, want %d", probes, test.wantProbes)
			}
		})
	}
	redis := liveDataContainerExpectation{name: "redis", image: "redis:8-alpine", owner: "owner", target: "target", kind: "redis", port: 6379}
	if got := livePostCreateContainer(context.Background(), redis, func(_ context.Context, args ...string) ([]byte, error) {
		if args[2] == "ls" {
			return []byte("redis\n"), nil
		}
		return []byte("/redis\tredis:8-alpine\towner\ttarget\trunning\n"), nil
	}, func(context.Context, liveDataContainerExpectation) ([]byte, error) { return []byte("NOPE\n"), nil }, nil); got != "running-unready" {
		t.Fatalf("redis container = %q", got)
	}
}

func TestLivePostCreatePostgresDiagnosticsRunBeforeReadinessRetry(t *testing.T) {
	if livePostCreateReportTimeout != livePostCreateStateTimeout+2*livePostCreateCommandTimeout+livePostgresLifecycleTimeout {
		t.Fatal("post-create phase budgets do not reserve diagnostics")
	}
	parent, cancel := context.WithCancel(context.Background())
	diagnostic, diagnosticCancel := livePostgresLifecycleContext(parent)
	defer diagnosticCancel()
	cancel()
	select {
	case <-diagnostic.Done():
	case <-time.After(time.Second):
		t.Fatal("diagnostic context is not derived from bounded report context")
	}
	expected := liveDataContainerExpectation{name: "pg", image: "postgres:18-alpine", owner: "owner", target: "target", kind: "postgres", port: 5432}
	redis := liveDataContainerExpectation{name: "redis", image: "redis:8-alpine", owner: "owner", target: "target", kind: "redis", port: 6379}
	var order []string
	command := func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case slices.Equal(args, []string{"docker", "container", "ls", "--all", "--filter", "name=^/pg$", "--format", "{{.Names}}"}):
			order = append(order, "pg-list")
			return []byte("pg\n"), nil
		case slices.Equal(args, []string{"docker", "container", "inspect", "--format", "{{.Name}}\t{{.Config.Image}}\t{{index .Config.Labels \"sub2api.host\"}}\t{{index .Config.Labels \"sub2api.host.target\"}}\t{{.State.Status}}", "pg"}):
			order = append(order, "pg-inspect")
			return []byte("/pg\tpostgres:18-alpine\towner\ttarget\trunning\n"), nil
		case slices.Equal(args, livePostgresNeutralArgs(expected)):
			order = append(order, "neutral")
			return nil, nil
		case slices.Equal(args, livePostgresAbsoluteArgs(expected)):
			order = append(order, "absolute")
			return nil, nil
		case slices.Equal(args, livePostgresPGDataArgs(expected)):
			order = append(order, "pgdata")
			return nil, nil
		case slices.Equal(args, livePostgresServerArgs(expected)):
			order = append(order, "server")
			return nil, nil
		case slices.Equal(args, liveDataReadinessArgs(expected)):
			order = append(order, "psql")
			return nil, nil
		case slices.Equal(args, livePostgresLifecycleInspectArgs(expected)):
			order = append(order, "lifecycle-inspect")
			return []byte("/pg\tpostgres:18-alpine\towner\ttarget\trunning\t0\tfalse\tfalse\n"), nil
		case slices.Equal(args, []string{"docker", "container", "ls", "--all", "--filter", "name=^/redis$", "--format", "{{.Names}}"}):
			order = append(order, "redis-list")
			return []byte("redis\n"), nil
		case slices.Equal(args, []string{"docker", "container", "inspect", "--format", "{{.Name}}\t{{.Config.Image}}\t{{index .Config.Labels \"sub2api.host\"}}\t{{index .Config.Labels \"sub2api.host.target\"}}\t{{.State.Status}}", "redis"}):
			order = append(order, "redis-inspect")
			return []byte("/redis\tredis:8-alpine\towner\ttarget\trunning\n"), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}
	probe := func(_ context.Context, service liveDataContainerExpectation) ([]byte, error) {
		if service.kind == "redis" {
			order = append(order, "redis-retry")
			return []byte("PONG\n"), nil
		}
		order = append(order, "pg-retry")
		return nil, errors.New("not ready")
	}
	got := livePostCreateContainer(context.Background(), expected, command, probe, func(ctx context.Context, expected liveDataContainerExpectation, command func(context.Context, ...string) ([]byte, error)) {
		reportLivePostgresLifecycle(ctx, expected, command, nil)
	})
	if got != "running-probe-failed" {
		t.Fatalf("postgres category = %q", got)
	}
	if got := livePostCreateContainer(context.Background(), redis, command, probe, nil); got != "ready" {
		t.Fatalf("redis category = %q", got)
	}
	if strings.Join(order, ",") != "pg-list,pg-inspect,neutral,absolute,pgdata,server,psql,lifecycle-inspect,pg-retry,pg-retry,pg-retry,pg-retry,pg-retry,pg-retry,pg-retry,pg-retry,pg-retry,pg-retry,redis-list,redis-inspect,redis-retry" {
		t.Fatalf("category = %q, command order = %q", got, strings.Join(order, ","))
	}
}

func TestLivePostgresLifecycleRecordAndInspectionFailClosed(t *testing.T) {
	expected := liveDataContainerExpectation{name: "pg", image: "postgres:18-alpine", owner: "owner", target: "target", kind: "postgres", port: 5432}
	inspect := livePostgresLifecycleInspectArgs(expected)
	wantInspect := []string{"docker", "container", "inspect", "--format", "{{.Name}}\t{{.Config.Image}}\t{{index .Config.Labels \"sub2api.host\"}}\t{{index .Config.Labels \"sub2api.host.target\"}}\t{{.State.Status}}\t{{.RestartCount}}\t{{.State.OOMKilled}}\t{{if .State.Error}}true{{else}}false{{end}}\t{{.Id}}\t{{.State.Pid}}", "pg"}
	if !slices.Equal(inspect, wantInspect) || !slices.Equal(livePostgresNeutralArgs(expected), []string{"docker", "exec", "pg", "true"}) || !slices.Equal(livePostgresAbsoluteArgs(expected), []string{"docker", "exec", "pg", "/bin/busybox", "true"}) {
		t.Fatal("postgres lifecycle argv is not fixed and neutral")
	}
	for _, test := range []struct {
		output, want         string
		execErr, absoluteErr error
	}{
		{"/pg\tpostgres:18-alpine\towner\ttarget\trunning\t0\tfalse\tfalse\t0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\t123\n", "live postgres container: exec=ok absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent\n", nil, nil},
		{"/pg\tpostgres:18-alpine\towner\ttarget\trestarting\t1\ttrue\ttrue\t0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\t123\n", "live postgres container: exec=exit-1 absolute=exit-127 absolute-error=unknown rootfs=unavailable direct=failed direct-error=unknown state=restarting restarts=nonzero oom=yes error=present\n", livePostgresExitError(t, 1), livePostgresExitError(t, 127)},
		{"/pg\tpostgres:18-alpine\twrong\ttarget\trunning\t0\tfalse\tfalse\t0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\t123\n", "live postgres container: exec=failed absolute=timeout absolute-error=unknown rootfs=unavailable direct=failed direct-error=unknown state=failed restarts=unknown oom=unknown error=unknown\n", errors.New("exec failed"), context.DeadlineExceeded},
		{"/pg\tpostgres:18-alpine\towner\ttarget\trunning\t01\tfalse\tfalse\t0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\t123\n", "live postgres container: exec=failed absolute=failed absolute-error=unknown rootfs=unavailable direct=failed direct-error=unknown state=failed restarts=unknown oom=unknown error=unknown\n", errors.New("exec failed"), errors.New("absolute failed")},
	} {
		if got := livePostgresLifecycleRecord(test.execErr, test.absoluteErr, expected, []byte(test.output)); got != test.want {
			t.Fatalf("record = %q, want %q", got, test.want)
		}
	}
	for _, category := range []string{"ok", "timeout", "exit-1", "exit-126", "exit-127", "exit-other", "failed"} {
		good := fmt.Sprintf("live postgres container: exec=%s absolute=%s absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent\n", category, category)
		if !livePostgresLifecycleRecordValid(good) {
			t.Fatalf("valid lifecycle record rejected: %q", category)
		}
		capture := newLiveRecordCapture()
		_, _ = capture.Write([]byte(good))
		var captured bytes.Buffer
		if !capture.forward(&captured) || captured.String() != good {
			t.Fatalf("lifecycle capture = %q", captured.String())
		}
	}
	good := "live postgres container: exec=ok absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent\n"
	for _, input := range []string{"prefix " + good, good + good, "live postgres container: exec=exit-37 absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent\n", "live postgres container: exec=ok absolute=exit-37 absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent\n", "live postgres container: exec=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent\n", "live postgres container: absolute=ok exec=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent\n", "live postgres container: exec=ok absolute=ok absolute-error=none direct=failed direct-error=unknown rootfs=unavailable state=running restarts=zero oom=no error=absent\n", "live postgres container: exec=ok absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown direct-error=unknown state=running restarts=zero oom=no error=absent\n", "live postgres container: exec=ok absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=running restarts=zero oom=no error=absent extra=x\n"} {
		capture := newLiveRecordCapture()
		_, _ = capture.Write([]byte(input))
		var out bytes.Buffer
		if capture.forward(&out) || out.String() != "live observer: observer-error\n" {
			t.Fatalf("unsafe lifecycle record = %q", out.String())
		}
	}
}

func TestLivePostgresLifecyclePIDAndDirectObservationArePrivateAndStable(t *testing.T) {
	expected := liveDataContainerExpectation{name: "pg", image: "postgres:18-alpine", owner: "owner", target: "target", kind: "postgres", port: 5432}
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	inspection := []byte("/pg\tpostgres:18-alpine\towner\ttarget\trunning\t0\tfalse\tfalse\t" + id + "\t123\n")
	for _, pid := range []string{"", "0", "01", "+1", "-1", " 1", "1 ", "1\t2", "18446744073709551616"} {
		out := []byte(strings.Replace(string(inspection), "\t123\n", "\t"+pid+"\n", 1))
		if _, ok := parseLivePostgresLifecycleInspection(expected, out); ok {
			t.Fatal("noncanonical PID accepted")
		}
	}
	for _, value := range []string{"", strings.ToUpper(id), id[:63], strings.Repeat("g", 64)} {
		out := []byte(strings.Replace(string(inspection), "\t"+id+"\t", "\t"+value+"\t", 1))
		if _, ok := parseLivePostgresLifecycleInspection(expected, out); ok {
			t.Fatal("invalid container ID accepted")
		}
	}
	invalidObservations := 0
	invalidID := []byte(strings.Replace(string(inspection), "\t"+id+"\t", "\t"+strings.ToUpper(id)+"\t", 1))
	invalid := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			return invalidID, nil
		}
		return nil, nil
	}, func(context.Context, string) (string, string, string) {
		invalidObservations++
		return "present", "ok", "none"
	}, func(string) (string, bool) { return "10", true })
	if invalidObservations != 0 || !livePostgresLifecycleRecordValid(invalid) {
		t.Fatal("invalid immutable identity reached observer or escaped fail-closed record")
	}
	if !slices.Equal(livePostgresDirectArgs("123"), []string{"nsenter", "--target", "123", "--mount", "--root", "--wd", "--", "/bin/busybox", "true"}) {
		t.Fatal("direct rootfs diagnostic argv is not fixed")
	}
	var calls []string
	command := func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			calls = append(calls, "inspect")
			return inspection, nil
		}
		calls = append(calls, "probe")
		return nil, nil
	}
	observe := func(_ context.Context, pid string) (string, string, string) {
		if pid != "123" {
			t.Fatal("unvalidated PID reached observer")
		}
		calls = append(calls, "direct")
		return "present", "ok", "none"
	}
	stable := livePostgresLifecycleDiagnostic(context.Background(), expected, command, observe, func(string) (string, bool) { return "10", true })
	if !livePostgresLifecycleRecordValid(stable) {
		t.Fatal("stable lifecycle record invalid")
	}
	if strings.Join(calls, ",") != "probe,probe,probe,probe,probe,inspect,direct,inspect" {
		t.Fatalf("lifecycle order = %q", strings.Join(calls, ","))
	}

	inspectCalls, observations := 0, 0
	changed := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			inspectCalls++
			if inspectCalls == 1 {
				return inspection, nil
			}
			return []byte("/pg\tpostgres:18-alpine\towner\ttarget\trunning\t0\tfalse\tfalse\t" + strings.Repeat("f", 64) + "\t123\n"), nil
		}
		return nil, nil
	}, func(_ context.Context, pid string) (string, string, string) {
		observations++
		return "present", "ok", "none"
	}, func(string) (string, bool) { return "10", true })
	if observations != 1 || inspectCalls != 2 || changed != "live postgres container: exec=ok absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=failed restarts=unknown oom=unknown error=unknown\n" {
		t.Fatalf("unstable lifecycle was not closed: observations=%d inspections=%d record=%q", observations, inspectCalls, changed)
	}

	observations = 0
	skipped := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			return []byte("/pg\tpostgres:18-alpine\towner\ttarget\trunning\t0\tfalse\tfalse\t" + id + "\t01\n"), nil
		}
		return nil, nil
	}, func(context.Context, string) (string, string, string) {
		observations++
		return "present", "ok", "none"
	}, func(string) (string, bool) { return "10", true })
	if observations != 0 || skipped != "live postgres container: exec=ok absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=failed restarts=unknown oom=unknown error=unknown\n" {
		t.Fatalf("invalid PID was not skipped and closed: observations=%d record=%q", observations, skipped)
	}

	inspectCalls, observations = 0, 0
	absent := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			inspectCalls++
			return inspection, nil
		}
		return nil, nil
	}, func(context.Context, string) (string, string, string) {
		observations++
		return "absent", "failed", "unknown"
	}, func(string) (string, bool) { return "10", true })
	if observations != 1 || inspectCalls != 2 || !livePostgresLifecycleRecordValid(absent) {
		t.Fatal("skipped direct rootfs observation did not require stable inspection")
	}

	nonRunning := []byte(strings.Replace(string(inspection), "\trunning\t", "\texited\t", 1))
	inspectCalls, observations = 0, 0
	exited := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			inspectCalls++
			return nonRunning, nil
		}
		return nil, nil
	}, func(context.Context, string) (string, string, string) {
		observations++
		return "present", "ok", "none"
	}, func(string) (string, bool) { return "10", true })
	if observations != 0 || inspectCalls != 2 || !strings.Contains(exited, "state=exited") {
		t.Fatal("non-running lifecycle did not require stable inspection without observation")
	}

	inspectCalls = 0
	changedNonRunning := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			inspectCalls++
			if inspectCalls == 1 {
				return nonRunning, nil
			}
			return inspection, nil
		}
		return nil, nil
	}, nil, nil)
	if inspectCalls != 2 || !strings.Contains(changedNonRunning, "state=failed") {
		t.Fatal("changed non-running inspection was not closed")
	}

	inspectCalls = 0
	nilObserver := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			inspectCalls++
			return inspection, nil
		}
		return nil, nil
	}, nil, nil)
	if inspectCalls != 2 || !livePostgresLifecycleRecordValid(nilObserver) {
		t.Fatal("nil observer lifecycle did not require stable inspection")
	}

	startCalls := 0
	changedStart := livePostgresLifecycleDiagnostic(context.Background(), expected, func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Equal(args, livePostgresLifecycleInspectArgs(expected)) {
			return inspection, nil
		}
		return nil, nil
	}, func(context.Context, string) (string, string, string) { return "absent", "failed", "unknown" }, func(string) (string, bool) {
		startCalls++
		if startCalls == 1 {
			return "10", true
		}
		return "11", true
	})
	if changedStart != "live postgres container: exec=ok absolute=ok absolute-error=none rootfs=unavailable direct=failed direct-error=unknown state=failed restarts=unknown oom=unknown error=unknown\n" {
		t.Fatal("changed process starttime was not closed")
	}

	if first, ok := parseLivePostgresLifecycleInspection(expected, inspection); !ok || first.pid != "123" {
		t.Fatal("canonical PID inspection unavailable")
	}
	// The record builder itself must not include raw diagnostics.
	failed := livePostgresLifecycleFailedRecord(errors.New("/proc/123/root/bin/busybox secret-path"), errors.New("pid=123 starttime=987654 "+id))
	if strings.Contains(failed, "123") || strings.Contains(failed, "987654") || strings.Contains(failed, id) || strings.Contains(failed, "secret-path") || !livePostgresLifecycleRecordValid(failed) {
		t.Fatal("private diagnostic details escaped lifecycle record")
	}
}

func TestLiveProcessStartTimeParser(t *testing.T) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "1"
	}
	fields[18] = "987654"
	stat := []byte("123 (worker with spaces ) and parens) S " + strings.Join(fields, " ") + "\n")
	if got, ok := liveProcessStartTime(stat); !ok || got != "987654" {
		t.Fatal("robust process starttime parser rejected valid stat")
	}
	for _, stat := range [][]byte{[]byte("123 (bad S 1"), []byte("123 (bad) S 1 2"), []byte("123 (bad) S " + strings.Repeat("1 ", 18) + "x\n")} {
		if _, ok := liveProcessStartTime(stat); ok {
			t.Fatal("malformed process stat accepted")
		}
	}
}

func TestLivePostgresRootfsCategory(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "busybox")
	if err := os.WriteFile(regular, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(regular)
	if err != nil || livePostgresRootfsCategory(info, err) != "present" {
		t.Fatal("executable regular rootfs entry was not present")
	}
	if err := os.Chmod(regular, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(regular)
	if err != nil || livePostgresRootfsCategory(info, err) != "nonexecutable" {
		t.Fatal("nonexecutable rootfs entry was not classified")
	}
	info, err = os.Stat(dir)
	if err != nil || livePostgresRootfsCategory(info, err) != "nonregular" {
		t.Fatal("nonregular rootfs entry was not classified")
	}
	_, err = os.Stat(filepath.Join(dir, "missing"))
	if livePostgresRootfsCategory(nil, err) != "absent" || livePostgresRootfsCategory(nil, errors.New("private stat error")) != "unavailable" {
		t.Fatal("rootfs stat failures were not categorized")
	}
}

func TestLivePostgresLifecycleExecCategoryIsFixedAndSanitized(t *testing.T) {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-timeoutCtx.Done()
	for _, test := range []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{timeoutCtx.Err(), "timeout"},
		{livePostgresExitError(t, 1), "exit-1"},
		{livePostgresExitError(t, 126), "exit-126"},
		{livePostgresExitError(t, 127), "exit-127"},
		{livePostgresExitError(t, 37), "exit-other"},
		{context.Canceled, "failed"},
		{errors.New("sensitive child error"), "failed"},
	} {
		if got := livePostgresLifecycleExecCategory(test.err); got != test.want {
			t.Fatalf("category = %q, want %q", got, test.want)
		}
	}
}

func TestLiveObserverCommandErrorClassifiesPrivateStderrWithoutExposure(t *testing.T) {
	for _, test := range []struct {
		stderr, want string
	}{
		{"", "empty"},
		{"permission denied: OCI runtime secret-token", "permission"},
		{"operation not permitted", "permission"},
		{"cgroup: secret-token", "cgroup"},
		{"setns failed", "namespace"},
		{"root filesystem mount failed", "rootfs"},
		{"executable file not found", "not-found"},
		{"OCI runtime create failed", "runtime"},
		{"Error response from daemon: secret-token", "daemon"},
		{"unrecognized secret-token", "unknown"},
	} {
		var stderr boundedBuffer
		_, _ = stderr.Write([]byte(test.stderr))
		wrapped := &liveObserverCommandError{err: livePostgresExitError(t, 127), category: liveObserverStderrCategory(stderr)}
		if wrapped.Error() != "live observer command failed" || strings.Contains(wrapped.Error(), "secret-token") || livePostgresAbsoluteErrorCategory(wrapped) != test.want {
			t.Fatalf("sanitized observer error category = %q", livePostgresAbsoluteErrorCategory(wrapped))
		}
		var exit *exec.ExitError
		if !errors.As(wrapped, &exit) || !errors.Is(wrapped, exit) || exit.ExitCode() != 127 {
			t.Fatal("wrapped observer error did not preserve exit classification")
		}
		record := livePostgresLifecycleFailedRecord(nil, wrapped)
		if strings.Contains(record, "secret-token") || !strings.Contains(record, "absolute-error="+test.want) || !livePostgresLifecycleRecordValid(record) {
			t.Fatal("sensitive observer stderr escaped lifecycle record")
		}
		capture := newLiveRecordCapture()
		_, _ = capture.Write([]byte(record))
		var forwarded bytes.Buffer
		if !capture.forward(&forwarded) || strings.Contains(forwarded.String(), "secret-token") {
			t.Fatal("sensitive observer stderr escaped forwarded evidence")
		}
	}
	var overflow boundedBuffer
	_, _ = overflow.Write(bytes.Repeat([]byte("x"), liveBoundedBufferLimit+1))
	if !overflow.overflow || liveObserverStderrCategory(overflow) != "unknown" {
		t.Fatal("overflowing observer stderr was not discarded as unknown")
	}
}

func livePostgresExitError(t *testing.T, code int) error {
	t.Helper()
	command := exec.Command("sh", "-c", "exit \"$1\"", "exit", strconv.Itoa(code))
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != code {
		t.Fatal("test exit error unavailable")
	}
	return err
}

func TestLivePostCreateStateReaderUsesBoundedFindFraming(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent's-path")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "sub2api-host")
	script := livePostCreateStateReadScriptForPaths(parent, root)
	if !strings.Contains(script, `find "$parent"/. -mindepth 1 -maxdepth 1 -name sub2api-host -print`) || !strings.Contains(script, `find "$root"/. -mindepth 1 -maxdepth 1 -name state.json -print`) || !strings.Contains(script, "status=$?") || strings.Contains(script, "test ! -e") {
		t.Fatalf("invalid state reader script: %q", script)
	}
	fixed := livePostCreateStateReadScript()
	if fixed != livePostCreateStateReadScriptForPaths("/var/lib", "/var/lib/sub2api-host") {
		t.Fatalf("production state reader is not fixed: %q", fixed)
	}
	run := func(parent, root string) ([]byte, error) {
		return exec.Command("sh", "-c", livePostCreateStateReadScriptForPaths(parent, root)).Output()
	}
	facts := liveTestDataObserverFacts(hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}, "revision")
	assertRead := func(parent, root, want string, wantError bool) {
		out, err := run(parent, root)
		if (err != nil) != wantError || string(out) != want {
			t.Fatalf("state read = %q, %v; want %q, error %t", out, err, want, wantError)
		}
		if wantError {
			if state, _ := livePostCreateState(context.Background(), func(context.Context) ([]byte, error) { return out, err }, facts); state != "unavailable" {
				t.Fatalf("unsafe state read classified %q, want unavailable", state)
			}
		}
	}
	assertRead(parent, root, "root-absent\n", false)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	assertRead(parent, root, "state-absent\n", false)
	statePath := filepath.Join(root, "state.json")
	if err := os.WriteFile(statePath, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRead(parent, root, "present\nstate", false)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	assertRead(parent, root, "", true)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	readableRoot := filepath.Join(parent, "readable-root")
	if err := os.Mkdir(readableRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readableRoot, "state.json"), []byte("linked-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(readableRoot, statePath); err != nil {
		t.Fatal(err)
	}
	assertRead(parent, root, "", true)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(readableRoot, root); err != nil {
		t.Fatal(err)
	}
	assertRead(parent, root, "", true)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRead(parent, root, "", true)
	notDirectory := filepath.Join(parent, "not-directory")
	if err := os.WriteFile(notDirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertRead(notDirectory, root, "", true)
	assertRead(filepath.Join(parent, "missing-parent"), root, "", true)
}

func TestLivePostCreateSnapshotReadinessRetriesAndCaptureIsStrict(t *testing.T) {
	pg := liveDataContainerExpectation{kind: "postgres", name: "pg", port: 5432}
	calls := 0
	if got := livePostCreateReadiness(context.Background(), pg, func(context.Context, liveDataContainerExpectation) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("not ready")
		}
		return []byte("ignored"), nil
	}, 3, 0); got != "ready" || calls != 3 {
		t.Fatalf("postgres = %q after %d calls", got, calls)
	}
	redis := liveDataContainerExpectation{kind: "redis", name: "redis", port: 6379}
	if got := livePostCreateReadiness(context.Background(), redis, func(context.Context, liveDataContainerExpectation) ([]byte, error) { return []byte("NOPE\n"), nil }, 2, 0); got != "running-unready" {
		t.Fatalf("redis = %q", got)
	}
	if got := livePostCreateReadiness(context.Background(), redis, func(context.Context, liveDataContainerExpectation) ([]byte, error) { return nil, errors.New("failed") }, 2, 0); got != "running-probe-failed" {
		t.Fatalf("failed probe = %q", got)
	}
	capture := newLiveRecordCapture()
	for _, part := range []string{"live post-create snapshot: state=pending-exact postgres=ready ", "redis=running-unready\n"} {
		_, _ = capture.Write([]byte(part))
	}
	var out bytes.Buffer
	if !capture.forward(&out) || out.String() != "live post-create snapshot: state=pending-exact postgres=ready redis=running-unready\n" {
		t.Fatalf("snapshot capture = %q", out.String())
	}
	absentSnapshots := [][]byte{
		[]byte("live post-create snapshot: state=root-absent postgres=not-inspected redis=not-inspected\n"),
		[]byte("live post-create snapshot: state=state-absent postgres=not-inspected redis=not-inspected\n"),
	}
	for _, snapshot := range absentSnapshots {
		capture := newLiveRecordCapture()
		_, _ = capture.Write(snapshot)
		var out bytes.Buffer
		if !capture.forward(&out) || out.String() != string(snapshot) {
			t.Fatalf("absence snapshot = %q", out.String())
		}
	}
	absentSnapshot := []byte("live post-create snapshot: state=absent postgres=not-inspected redis=not-inspected\n")
	for _, input := range [][]byte{
		append([]byte("prefix "), absentSnapshot...),
		[]byte("live post-create snapshot: state=unknown postgres=not-inspected redis=not-inspected\n"),
		append(append([]byte(nil), absentSnapshot...), absentSnapshot...),
		append(bytes.Repeat([]byte("x"), 161), absentSnapshot...),
	} {
		capture := newLiveRecordCapture()
		_, _ = capture.Write(input)
		var out bytes.Buffer
		if capture.forward(&out) || out.String() != "live observer: observer-error\n" {
			t.Fatalf("unsafe snapshot = %q", out.String())
		}
	}
}

func TestLiveDataObserverMatchesFrozenRunLocalOwnershipFormula(t *testing.T) {
	host := mustRead(t, filepath.Join(repositoryRoot(t), "internal", "hostruntime", "reconcile.go"))
	if !bytes.Contains(host, []byte(`ownershipLabelFor(s.Resource, s.Ownership, o.Role, o.AppToken, "")`)) || !bytes.Contains(host, []byte(`func nameForLocal(s State, token string) string { return objectName(s, "local-data", token, "live") }`)) {
		t.Fatal("frozen Host runLocal ownership/name slots changed")
	}
	facts := liveTestDataObserverFacts(hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}, "revision")
	expectations, err := liveDataContainerExpectationsForState(facts, liveObserverState{ownership: "owner", revision: "revision"})
	if err != nil || len(expectations) != 2 {
		t.Fatalf("expectations = %#v, %v", expectations, err)
	}
	id := liveRuntimeToken("local-data", "postgres")
	wantName := "s2h-" + liveRuntimeToken("live", "data", "owner", "local-data", id, "live")
	wantOwner := "s2h1:" + liveRuntimeToken("live", "data", "owner", "local-data", id, "")
	if expectations[0].name != wantName || expectations[0].owner != wantOwner || expectations[0].name == "s2h-"+strings.TrimPrefix(wantOwner, "s2h1:") {
		t.Fatalf("local-data expectation = %#v", expectations[0])
	}
}

func TestLiveOuterCaptureRetainsTrailingFixedRecordsAfterNoise(t *testing.T) {
	output := newLiveRecordCapture()
	_, _ = output.Write(append(bytes.Repeat([]byte("canary-noise"), 512), '\n'))
	_, _ = output.Write([]byte("SUB2API_LIVE_STAGE=data-create-bootstrap-remote\n"))
	for _, milestone := range liveMilestones {
		_, _ = output.Write([]byte("live milestone: " + milestone + "\n"))
	}
	_, _ = output.Write([]byte("live observer: observer-error\n"))
	if liveFailureCategory(context.Background(), output.failureBytes(nil)) != "data-create-bootstrap-remote" {
		t.Fatal("trailing fixed stage was displaced by noise")
	}
	var got bytes.Buffer
	want := "live milestone: " + strings.Join(liveMilestones[:], "\nlive milestone: ") + "\nlive observer: observer-error\n"
	if ok := output.forward(&got); !ok || got.String() != want {
		t.Fatalf("fixed records lost or raw output forwarded: %q", got.String())
	}
	if strings.Contains(got.String(), "canary-noise") {
		t.Fatalf("raw canary forwarded: %q", got.String())
	}
}

func TestLiveRecordCaptureSeparatesStreamsAndHandlesFragments(t *testing.T) {
	stdout, stderr := newLiveStdoutCapture(), newLiveRecordCapture()
	for _, part := range []string{"live milestone: postgres-", "owned-container\n"} {
		_, _ = stderr.Write([]byte(part))
	}
	for _, part := range []string{"live milestone: postgres-", "ready\n", "live milestone: redis-owned-container\n", "live milestone: redis-ready\n"} {
		_, _ = stderr.Write([]byte(part))
	}
	_, _ = stdout.Write([]byte("live milestone: postgres-owned-container\n"))
	var got bytes.Buffer
	if !stderr.forward(&got) || got.String() != "live milestone: postgres-owned-container\nlive milestone: postgres-ready\nlive milestone: redis-owned-container\nlive milestone: redis-ready\n" {
		t.Fatalf("stderr records = %q", got.String())
	}
	var stdoutForwarded bytes.Buffer
	if stdout.forward(&stdoutForwarded) || stdoutForwarded.String() != "live observer: observer-error\n" {
		t.Fatalf("stdout marker was forwarded: %q", stdoutForwarded.String())
	}
}

func TestLiveRecordCaptureFailsClosedForInvalidMarkers(t *testing.T) {
	valid := []byte("live milestone: postgres-owned-container\nlive milestone: postgres-ready\nlive milestone: redis-owned-container\nlive milestone: redis-ready\n")
	for _, test := range []struct {
		name  string
		input []byte
	}{
		{"oversized", append(bytes.Repeat([]byte("x"), 4097), []byte("live milestone: postgres-owned-container\n")...)},
		{"unanchored", []byte("prefix live milestone: postgres-owned-container\n")},
		{"duplicate", append(valid, []byte("live milestone: redis-ready\n")...)},
		{"out of order", []byte("live milestone: redis-ready\n")},
		{"malformed", []byte("live milestone: postgres-ready extra\n")},
		{"unknown", []byte("live milestone: unknown\n")},
		{"missing newline", []byte("live milestone: postgres-owned-container")},
		{"observer then malformed milestone", []byte("live observer: observer-inconclusive\nmalformed live milestone: nope\n")},
		{"duplicate observer", []byte("live observer: observer-error\nlive observer: observer-inconclusive\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := newLiveRecordCapture()
			for _, part := range [][]byte{test.input[:len(test.input)/2], test.input[len(test.input)/2:]} {
				_, _ = capture.Write(part)
			}
			var got bytes.Buffer
			if capture.forward(&got) || got.String() != "live observer: observer-error\n" {
				t.Fatalf("unsafe record = %q", got.String())
			}
		})
	}
}

func TestLiveRecordCaptureStateIsBounded(t *testing.T) {
	capture := newLiveRecordCapture()
	_, _ = capture.Write(bytes.Repeat([]byte("noise"), 1<<20))
	if capture.lineLen != len(capture.line) || capture.rollLen > len(capture.rolling) || len(capture.fallback.data) != 4096 {
		t.Fatalf("unbounded capture state: line %d fallback %d", capture.lineLen, len(capture.fallback.data))
	}
}

func TestLiveCompletedStateRequiresCreateOwnership(t *testing.T) {
	resource := hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}
	revision := liveTestRevision('c')
	facts := liveTestDataObserverFacts(resource, revision)
	valid := string(liveCompletedStateJSON(t, resource, revision, "owner"))
	machine := liveMachineIdentity("data").Value
	for _, test := range []struct {
		name, state, owner string
		ok                 bool
	}{
		{"valid", valid, "owner", true},
		{"wrong ownership", strings.Replace(valid, `"owner"`, `"other"`, 1), "owner", false},
		{"stale revision", strings.Replace(valid, revision, liveTestRevision('d'), 1), "owner", false},
		{"missing result", strings.Replace(valid, `,"result":{"status":"applied","appliedRevision":"`+revision+`"}`, "", 1), "owner", false},
		{"not ready", strings.Replace(valid, `"ready":true`, `"ready":false`, 1), "owner", false},
		{"observation mismatch", strings.Replace(valid, `"appliedRevision":"`+revision+`","ready":true`, `"appliedRevision":"`+liveTestRevision('d')+`","ready":true`, 1), "owner", false},
		{"machine mismatch", strings.Replace(valid, `"machine":{"value":"`+machine+`"},"ownership"`, `"machine":{"value":"other"},"ownership"`, 1), "owner", false},
		{"arbitrary matching machine", strings.ReplaceAll(valid, `"machine":{"value":"`+machine+`"}`, `"machine":{"value":"arbitrary"}`), "owner", false},
		{"wrong release", strings.Replace(valid, `"hostRelease":"release"`, `"hostRelease":"other"`, 1), "owner", false},
		{"empty release", strings.Replace(valid, `"hostRelease":"release"`, `"hostRelease":""`, 1), "owner", false},
		{"drifted", strings.Replace(valid, `"ready":true`, `"drifted":true,"ready":true`, 1), "owner", false},
		{"extra app", strings.Replace(valid, `"data":`, `"apps":[{"id":"extra","activeImage":"extra","ready":true}],"data":`, 1), "owner", false},
		{"missing data", strings.Replace(valid, `"data":[`, `"data":[]`, 1), "owner", false},
		{"extra data", strings.Replace(valid, `]},"journal"`, `,{"identity":{"kind":"extra","providerId":"extra","endpoint":"extra","port":1},"ready":true}]},"journal"`, 1), "owner", false},
		{"result observation", strings.Replace(valid, `"appliedRevision":"`+revision+`"}}}`, `"appliedRevision":"`+revision+`","observation":{}}}}`, 1), "owner", false},
		{"result machine", strings.Replace(valid, `"appliedRevision":"`+revision+`"}}}`, `"appliedRevision":"`+revision+`","machine":{}}}}`, 1), "owner", false},
		{"result ownership", strings.Replace(valid, `"appliedRevision":"`+revision+`"}}}`, `"appliedRevision":"`+revision+`","ownership":{}}}}`, 1), "owner", false},
		{"result retirement", strings.Replace(valid, `"appliedRevision":"`+revision+`"}}}`, `"appliedRevision":"`+revision+`","retirement":{}}}}`, 1), "owner", false},
		{"result operation evidence", strings.Replace(valid, `"appliedRevision":"`+revision+`"}}}`, `"appliedRevision":"`+revision+`","operationEvidence":{}}}}`, 1), "owner", false},
		{"unknown", strings.Replace(valid, `"version":1`, `"version":1,"unknown":true`, 1), "owner", false},
		{"duplicate", strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1), "owner", false},
		{"trailing", valid + ` {}`, "owner", false},
		{"oversized", valid + strings.Repeat(" ", 1<<20), "owner", false},
		{"malformed", `{`, "owner", false},
		{"absent", ``, "owner", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, present, err := parseLiveCompletedObserverState([]byte(test.state), facts, test.owner)
			if (err == nil && present) != test.ok {
				t.Fatalf("present %v err %v", present, err)
			}
		})
	}
}

func TestLivePostCreateCompletionUsesFallbackOnlyAfterSuccessfulCreate(t *testing.T) {
	incomplete := liveMilestoneObserverResult{observerError: true}
	for _, test := range []struct {
		name            string
		createSucceeded bool
		ownership       string
		ownershipOK     bool
		initial         liveMilestoneObserverResult
		callbackResult  liveMilestoneObserverResult
		wantCalls       int
		wantObserved    bool
	}{
		{"failed Create retains observer result and skips extraction/fallback", false, "", false, incomplete, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone}, 0, false},
		{"successful Create missing ownership", true, "", false, incomplete, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone}, 0, false},
		{"successful incomplete completion", true, "owner", true, incomplete, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone, stateSeen: true, ownership: "owner"}, 1, true},
		{"full matching observation", true, "owner", true, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone, stateSeen: true, ownership: "owner"}, liveMilestoneObserverResult{}, 0, true},
		{"full mismatched observation", true, "owner", true, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone, stateSeen: true, ownership: "other"}, liveMilestoneObserverResult{}, 0, false},
		{"full observation missing ownership", true, "owner", true, liveMilestoneObserverResult{lastMilestone: liveFinalMilestone, stateSeen: true}, liveMilestoneObserverResult{}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			got := livePostCreateCompletion(test.createSucceeded, test.ownership, test.ownershipOK, test.initial, func(ownership string, result liveMilestoneObserverResult) liveMilestoneObserverResult {
				calls++
				if ownership != "owner" {
					t.Fatalf("fallback ownership = %q", ownership)
				}
				return test.callbackResult
			})
			if calls != test.wantCalls || liveDataCreateObserved(got) != test.wantObserved {
				t.Fatalf("calls %d, result %#v", calls, got)
			}
		})
	}
}

func TestLiveCreateOwnershipRequiresExactNonSecretOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		values property.Map
		want   string
		ok     bool
	}{
		{"valid", property.NewMap(map[string]property.Value{"ownership": property.New(property.NewMap(map[string]property.Value{"value": property.New("owner")}))}), "owner", true},
		{"missing", property.NewMap(nil), "", false},
		{"secret", property.NewMap(map[string]property.Value{"ownership": property.New(property.NewMap(map[string]property.Value{"value": property.New("owner")})).WithSecret(true)}), "", false},
		{"computed", property.NewMap(map[string]property.Value{"ownership": property.New(property.Computed)}), "", false},
		{"extra field", property.NewMap(map[string]property.Value{"ownership": property.New(property.NewMap(map[string]property.Value{"value": property.New("owner"), "extra": property.New("x")}))}), "", false},
		{"malformed", property.NewMap(map[string]property.Value{"ownership": property.New("owner")}), "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := liveCreateOwnership(test.values)
			if got != test.want || ok != test.ok {
				t.Fatalf("ownership = %q, %v", got, ok)
			}
		})
	}
}

func TestLiveCompletionFallbackClearsPriorObserverErrorOnlyAfterFullVerification(t *testing.T) {
	resource, revision := hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}, liveTestRevision('c')
	facts := liveTestDataObserverFacts(resource, revision)
	for _, test := range []struct {
		name       string
		state      []byte
		wantStrict bool
	}{
		{"strict state", liveCompletedStateJSON(t, resource, revision, "owner"), true},
		{"wrong release", []byte(strings.Replace(string(liveCompletedStateJSON(t, resource, revision, "owner")), `"hostRelease":"release"`, `"hostRelease":"other"`, 1)), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			emitted := []string{}
			result := liveDataCompletionResult(facts, "owner", liveMilestoneObserverResult{observerError: true}, test.state, liveDataContainerExpectationsForState, func(_ context.Context, expected liveDataContainerExpectation) (liveContainerInspection, bool, error) {
				return liveContainerInspection{name: expected.name, image: expected.image, owner: expected.owner, target: expected.target, running: true}, true, nil
			}, func(context.Context, liveDataContainerExpectation) (bool, error) { return true, nil }, func(record string) { emitted = append(emitted, record) }, context.Background())
			if liveDataCreateObserved(result) != test.wantStrict || result.observerError == test.wantStrict || (test.wantStrict && len(emitted) != len(liveMilestones)) {
				t.Fatalf("completion fallback = %#v, emitted %v", result, emitted)
			}
		})
	}
}

func TestLiveCompletedStateUsesFixedLocalDataIdentityShape(t *testing.T) {
	resource, revision := hostcontract.ResourceIdentity{Environment: "live", ServerKey: "data"}, liveTestRevision('c')
	facts := liveTestDataObserverFacts(resource, revision)
	var state hostruntime.State
	if err := json.Unmarshal(liveCompletedStateJSON(t, resource, revision, "owner"), &state); err != nil {
		t.Fatal(err)
	}
	if !exactLiveDataObservations(facts, "owner", state.Observation.Data) {
		t.Fatalf("completed data observations do not match live local-data contract: %#v", state.Observation.Data)
	}
	for _, mutate := range []func([]hostcontract.DataObservation){
		func(data []hostcontract.DataObservation) {
			data[0].Identity.ProviderID, data[0].Identity.Endpoint = "other", "other"
		},
		func(data []hostcontract.DataObservation) { data[0], data[1] = data[1], data[0] },
		func(data []hostcontract.DataObservation) { data[1] = data[0] },
		func(data []hostcontract.DataObservation) { data[0].Identity.Database = "wrong" },
		func(data []hostcontract.DataObservation) { data[0].Identity.TLSMode = "required" },
	} {
		data := append([]hostcontract.DataObservation(nil), state.Observation.Data...)
		mutate(data)
		if exactLiveDataObservations(facts, "owner", data) {
			t.Fatalf("accepted invalid local-data identity: %#v", data)
		}
	}
	if exactLiveDataObservations(facts, "owner", state.Observation.Data[:1]) {
		t.Fatal("accepted missing local-data identity")
	}
	if exactLiveDataObservations(facts, "owner", append(append([]hostcontract.DataObservation(nil), state.Observation.Data...), state.Observation.Data[0])) {
		t.Fatal("accepted extra local-data identity")
	}
}

func liveTestRevision(character rune) string {
	return "tr1:0123456789abcdef:" + strings.Repeat(string(character), 64)
}

func liveTestDataObserverFacts(resource hostcontract.ResourceIdentity, revision string) liveDataObserverFacts {
	return liveDataObserverFacts{
		resource:      resource,
		revision:      revision,
		priorRevision: liveTestRevision('b'),
		machine:       liveMachineIdentity("data"),
		release:       "release",
		services: []hostcontract.LocalDataServiceTarget{
			{ID: "postgres", Type: "postgres", Port: 5432, Persistence: true},
			{ID: "redis", Type: "redis", Port: 6379, Persistence: true},
		},
	}
}

func liveCompletedStateJSON(t *testing.T, resource hostcontract.ResourceIdentity, revision, ownership string) []byte {
	t.Helper()
	prior := liveTestRevision('b')
	machine := liveMachineIdentity("data")
	facts := liveTestDataObserverFacts(resource, revision)
	state := hostruntime.State{Version: 1, Resource: resource, Machine: machine, Ownership: hostcontract.OwnershipIdentity{Value: ownership}, AppliedRevision: revision, Observation: hostcontract.StableObservation{Machine: machine, Ownership: hostcontract.OwnershipIdentity{Value: ownership}, HostRelease: "release", AppliedRevision: revision, Ready: true, Data: liveLocalDataObservations(facts, ownership)}, Journal: &hostruntime.Journal{Key: hostcontract.OperationKey{Resource: resource, Action: hostcontract.ActionReconcile, TargetRevision: revision, PriorAppliedRevision: prior}, Status: "complete", Result: &hostprotocol.Result{Status: hostprotocol.ResultApplied, AppliedRevision: revision}}}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func livePendingStateJSON(t *testing.T, facts liveDataObserverFacts, ownership string) []byte {
	t.Helper()
	state := hostruntime.State{Version: 1, Resource: facts.resource, Machine: facts.machine, Ownership: hostcontract.OwnershipIdentity{Value: ownership}, AppliedRevision: facts.priorRevision, Observation: hostcontract.StableObservation{Machine: facts.machine, Ownership: hostcontract.OwnershipIdentity{Value: ownership}, HostRelease: facts.release, AppliedRevision: facts.priorRevision, Ready: true}, Journal: &hostruntime.Journal{Key: hostcontract.OperationKey{Resource: facts.resource, Action: hostcontract.ActionReconcile, TargetRevision: facts.revision, PriorAppliedRevision: facts.priorRevision}, Status: "pending"}}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func liveLocalDataObservations(facts liveDataObserverFacts, ownership string) []hostcontract.DataObservation {
	expected, err := liveDataContainerExpectationsForState(facts, liveObserverState{ownership: ownership, revision: facts.revision})
	if err != nil {
		panic("invalid local data observations")
	}
	observations := make([]hostcontract.DataObservation, 0, len(expected))
	for _, service := range expected {
		database, tlsServerName := "sub2api", service.name
		if service.kind == "redis" {
			database, tlsServerName = "0", ""
		}
		observations = append(observations, hostcontract.DataObservation{Identity: hostcontract.DataIdentity{Kind: service.kind, ProviderID: service.name, Endpoint: service.name, Port: service.port, Database: database, TLSServerName: tlsServerName}, Ready: true})
	}
	return observations
}

func TestLiveDockerFailureReasonUsesSpecificPrecedenceAndExplicitFallback(t *testing.T) {
	script := filepath.Join(repositoryRoot(t), "internal", "integration", "providerruntime", "testdata", "live-host-sandbox.sh")
	tests := []struct {
		name     string
		log      string
		fallback string
		want     string
	}{
		{name: "empty early exit", fallback: "unknown", want: "unknown"},
		{name: "empty readiness timeout", fallback: "timeout", want: "timeout"},
		{name: "permission before storage", log: "overlay operation not permitted", fallback: "unknown", want: "permission"},
		{name: "resource before containerd", log: "containerd: no space left", fallback: "unknown", want: "resource"},
		{name: "conflict before network", log: "network controller address already in use", fallback: "unknown", want: "conflict"},
		{name: "flag and config conflict before storage", log: "the following directives are specified both as a flag and in the configuration file: storage-driver", fallback: "unknown", want: "conflict"},
		{name: "network", log: "failed to create network controller", fallback: "unknown", want: "network"},
		{name: "storage", log: "failed to initialize storage driver overlay", fallback: "unknown", want: "storage"},
		{name: "cgroup", log: "failed to start daemon: unable to find cpu cgroup mount", fallback: "unknown", want: "cgroup"},
		{name: "benign cgroup warning", log: `level=warning msg="Your kernel does not support cgroup blkio weight"`, fallback: "unknown", want: "unknown"},
		{name: "benign unmounted cgroup warning", log: `level=warning msg="Your kernel does not support memory limit capabilities or the cgroup is not mounted. Limitation discarded."`, fallback: "unknown", want: "unknown"},
		{name: "runtime helper", log: "failed to start daemon: docker-proxy executable file not found in PATH", fallback: "unknown", want: "helper"},
		{name: "configuration", log: "failed to start daemon: invalid configuration", fallback: "unknown", want: "config"},
		{name: "filesystem state", log: "failed to start daemon: error creating daemon root", fallback: "unknown", want: "filesystem"},
		{name: "generic initialization", log: "failed to start daemon: unsupported runtime state", fallback: "unknown", want: "initialization"},
		{name: "containerd", log: "failed to connect to containerd", fallback: "unknown", want: "containerd"},
		{name: "containerd startup timeout", log: "failed to start containerd: timeout waiting for containerd to start", fallback: "unknown", want: "containerd-timeout"},
		{name: "containerd path", log: "failed to start containerd: listen unix socket: file name too long", fallback: "unknown", want: "containerd-path"},
		{name: "containerd socket", log: "containerd socket connection refused", fallback: "unknown", want: "containerd-socket"},
		{name: "containerd exit", log: "containerd exited with exit status 1", fallback: "unknown", want: "containerd-exit"},
		{name: "benign containerd signal handler", log: `level=info msg="containerd signal handler registered"`, fallback: "unknown", want: "unknown"},
		{name: "benign containerd client timeout field", log: `level=info msg="Creating a containerd client" address=/run/containerd/containerd.sock timeout=1m0s`, fallback: "unknown", want: "unknown"},
		{name: "benign storage text during early exit", log: `time="2026-09-05T00:00:00Z" level=info msg="using storage driver vfs"`, fallback: "unknown", want: "unknown"},
		{name: "benign containerd text during early exit", log: `time="2026-09-05T00:00:00Z" level=info msg="starting containerd"`, fallback: "unknown", want: "unknown"},
		{name: "fatal graphdriver after benign storage text", log: "level=info msg=\"using storage driver vfs\"\nfailed to start daemon: error initializing graphdriver", fallback: "unknown", want: "storage"},
		{name: "unrecognized timeout", log: "daemon still starting", fallback: "timeout", want: "timeout"},
		{name: "benign storage text during timeout", log: "using storage driver vfs", fallback: "timeout", want: "timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "dockerd.log")
			if err := os.WriteFile(log, []byte(test.log), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", script, "--classify-docker-log", log, test.fallback)
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("classify Docker failure: %v", err)
			}
			if got := string(output); got != test.want {
				t.Fatalf("Docker failure reason = %q, want %q", got, test.want)
			}
		})
	}
}
