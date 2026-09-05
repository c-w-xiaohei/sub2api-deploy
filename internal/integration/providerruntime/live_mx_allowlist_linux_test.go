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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	providerRuntimeLiveGate   = "SUB2API_PROVIDER_RUNTIME_LIVE"
	providerRuntimeLiveHelper = "SUB2API_PROVIDER_RUNTIME_LIVE_HELPER"
	liveCommandTimeout        = 15 * time.Second
)

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
	var output boundedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("live namespace fixture failed: %s", liveFailureCategory(ctx, output.Bytes()))
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
	reportLiveStage("data-create")
	dataCreated, err := provider.client.Create(ctx, &pulumirpc.CreateRequest{Urn: "urn:pulumi:live::mx-allowlist::sub2api-host:index:Host::data", Properties: rpcProperties(t, dataInput)})
	if err != nil || dataCreated == nil || dataCreated.Id == "" {
		t.Fatal("released Provider Create failed")
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
	var config, known strings.Builder
	for _, item := range []struct{ name, ip string }{
		{"data", f.dataIP},
		{"app", f.appIP},
	} {
		host := strings.TrimSpace(string(commandOutput(t, 10*time.Second, "ssh-keygen", "-y", "-f", filepath.Join(root, item.name+".host-key"))))
		writePrivate(t, filepath.Join(root, item.name, "authorized_keys"), []byte(pub+"\n"))
		writePrivate(t, filepath.Join(root, item.name, "sshd_config"), []byte("Port 2222\nListenAddress "+item.ip+"\nHostKey "+filepath.Join(root, item.name+".host-key")+"\nAuthorizedKeysFile "+filepath.Join(root, item.name, "authorized_keys")+"\nPidFile /var/run/sshd.pid\nUsePAM no\nPasswordAuthentication no\nChallengeResponseAuthentication no\nPermitRootLogin prohibit-password\nStrictModes yes\nLogLevel QUIET\n"))
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

type boundedBuffer struct{ data []byte }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	const max = 4096
	if len(b.data) < max {
		n := max - len(b.data)
		if n > len(p) {
			n = len(p)
		}
		b.data = append(b.data, p[:n]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return append([]byte(nil), b.data...) }

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
		"data-docker-containerd-booted":     true,
		"data-docker-containerd-listener":   true,
		"data-docker-containerd-permission": true,
		"data-docker-containerd-resource":   true,
		"data-docker-containerd-plugin":     true,
		"data-docker-containerd-startup":    true,
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
		"app-docker-containerd-booted":      true,
		"app-docker-containerd-listener":    true,
		"app-docker-containerd-permission":  true,
		"app-docker-containerd-resource":    true,
		"app-docker-containerd-plugin":      true,
		"app-docker-containerd-startup":     true,
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
		"data-create":                       true,
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

func TestLiveFailureCategoryReportsOnlyKnownLastStage(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "empty", want: "exit"},
		{name: "unknown marker", output: "SUB2API_LIVE_STAGE=credential-canary\n", want: "isolated-fixture-exit"},
		{name: "known marker", output: "SUB2API_LIVE_STAGE=data-create\n", want: "data-create"},
		{name: "known Docker reason", output: "SUB2API_LIVE_STAGE=app-docker-network\n", want: "app-docker-network"},
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

func TestLiveContainerdStartupReasonReportsOnlyFixedMilestones(t *testing.T) {
	script := filepath.Join(repositoryRoot(t), "internal", "integration", "providerruntime", "testdata", "live-host-sandbox.sh")
	tests := []struct {
		name, log, want string
	}{
		{name: "booted after plugins", log: "loading plugin id=io.containerd.nri.v1.nri\ncontainerd successfully booted", want: "containerd-booted"},
		{name: "listener", log: "failed to get listener for main ttrpc endpoint", want: "containerd-listener"},
		{name: "permission", log: "failed to get listener: operation not permitted", want: "containerd-permission"},
		{name: "resource", log: "failed to load plugin: no space left on device", want: "containerd-resource"},
		{name: "plugin failure", log: "failed to load plugin io.containerd.metadata.v1.bolt", want: "containerd-plugin"},
		{name: "plugin loading stalled", log: "loading plugin id=io.containerd.nri.v1.nri", want: "containerd-plugin"},
		{name: "startup only", log: "starting containerd", want: "containerd-startup"},
		{name: "empty", want: "containerd-timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "containerd.log")
			if err := os.WriteFile(log, []byte(test.log), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("sh", script, "--classify-containerd-log", log).Output()
			if err != nil {
				t.Fatalf("classify containerd startup: %v", err)
			}
			if got := string(output); got != test.want {
				t.Fatalf("containerd startup reason = %q, want %q", got, test.want)
			}
		})
	}
}
