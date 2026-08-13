package hostruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

func TestRunProcessBoundsOutputAndReturnsSanitizedErrors(t *testing.T) {
	got, err := runProcess(context.Background(), "/bin/sh", []string{"-c", "head -c 1048576 /dev/zero"}, nil)
	if err == nil || strings.Contains(err.Error(), "PROCESS_SECRET_CANARY") {
		t.Fatalf("oversize error = %v, bytes=%d", err, len(got))
	}
}

func TestRunProcessCancellationReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := runProcess(ctx, "/bin/sh", []string{"-c", "sleep 5"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestReconcileBlueGreenOwnedOnlyAndTerminalReplay(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := requestFor(state, revisionB(), app("one", "image-one"))
	result, err := rt.Reconcile(context.Background(), request)
	if err != nil || result.Status != hostprotocol.ResultApplied {
		t.Fatalf("reconcile = %#v, %v", result, err)
	}
	if runner.mutations("run") != 1 || runner.mutations("rm") != 0 || runner.hasSecret("CANARY") {
		t.Fatalf("trace = %#v", runner.calls)
	}
	inv, err := rt.readInventory()
	if err != nil || len(inv.Objects) != 1 || inv.Objects[0].AppToken != appToken("one") {
		t.Fatalf("inventory = %#v, %v", inv, err)
	}
	calls := len(runner.calls)
	if _, err := rt.Reconcile(context.Background(), request); err != nil || calls != len(runner.calls) {
		t.Fatalf("terminal replay = %v; calls %d -> %d", err, calls, len(runner.calls))
	}
}

func TestReconcileProjectsLocalDataBeforeAppsAndTraefikRoute(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	postgres := hostcontract.LocalDataServiceTarget{ID: "primary", Type: "postgres", Port: 5433}
	redis := hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380, Persistence: true}
	proxy := &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	request := requestFor(state, revisionB(), app("one", "image-one"))
	request.Target.DataServices = []hostcontract.LocalDataServiceTarget{postgres, redis}
	request.Target.ReverseProxy = proxy
	request.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "POSTGRES_CANARY"}, "cache": {AdminPassword: "REDIS_CANARY"}}
	request.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "DNS_CANARY"}

	result, err := rt.Reconcile(context.Background(), request)
	if err != nil || result.Status != hostprotocol.ResultApplied {
		t.Fatalf("reconcile = %#v, %v", result, err)
	}
	stored, _ := rt.readState()
	if runner.hasSecret("CANARY") || len(stored.Observation.Data) != 2 {
		t.Fatalf("secret leaked or data missing: %#v %#v", stored.Observation, runner.calls)
	}
	pg := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	cache := findLocalData(mustInventory(t, rt), localDataToken("cache"))
	if pg.Image != postgresImage || cache.Image != redisImage || pg.DataIdentity.Database != "sub2api" || cache.DataIdentity.Database != "0" {
		t.Fatalf("data inventory = %#v %#v", pg, cache)
	}
	if !runner.beforeRun(pg.Name, objectName(state, "app", appToken("one"), "green")) || !runner.beforeRun(cache.Name, objectName(state, "app", appToken("one"), "green")) {
		t.Fatalf("data was not ready before app: %#v", runner.calls)
	}
	appName := objectName(state, "app", appToken("one"), "green")
	proxyName := objectName(state, "proxy", "proxy", "live")
	if !runner.hasCall([]string{"exec", appName, "wget", "-q", "-O", "/dev/null", "--header", "Host:one.example", "http://" + proxyName + ":8081/health"}) {
		t.Fatalf("post-route proxy readiness = %#v", runner.calls)
	}
	if !runner.hasCall([]string{"exec", pg.Name, "psql", "-h", "127.0.0.1", "-p", "5433", "-U", "sub2api", "-d", "sub2api", "-v", "ON_ERROR_STOP=1", "-c", "SELECT 1"}) || !runner.hasCall([]string{"exec", cache.Name, "redis-cli", "-h", "127.0.0.1", "-p", "6380", "ping"}) {
		t.Fatalf("local readiness = %#v", runner.calls)
	}
	route := mustRouteArtifact(t, rt, routeName(appToken("one")))
	if !bytes.Contains(route, []byte(`"routers"`)) || !bytes.Contains(route, []byte(`Host(`)) || bytes.Contains(route, []byte("CANARY")) {
		t.Fatalf("dynamic route = %s", route)
	}
	var dynamic traefikRoute
	if err := json.Unmarshal(route, &dynamic); err != nil || len(dynamic.HTTP.Routers) != 2 || len(dynamic.HTTP.Services) != 1 {
		t.Fatalf("dynamic route = %q, %v", route, err)
	}
	var public, probe traefikRouter
	var publicKey, probeKey string
	for key, router := range dynamic.HTTP.Routers {
		if router.Rule != "Host(`one.example`)" {
			t.Fatalf("router = %#v", router)
		}
		switch strings.Join(router.EntryPoints, ",") {
		case "websecure":
			public, publicKey = router, key
		case "probe":
			probe, probeKey = router, key
		default:
			t.Fatalf("router entry points = %#v", router)
		}
	}
	var raw struct {
		HTTP struct {
			Routers map[string]map[string]json.RawMessage `json:"routers"`
		} `json:"http"`
	}
	if err := json.Unmarshal(route, &raw); err != nil {
		t.Fatal(err)
	}
	publicTLS, publicHasTLS := raw.HTTP.Routers[publicKey]["tls"]
	_, probeHasTLS := raw.HTTP.Routers[probeKey]["tls"]
	if !publicHasTLS || string(publicTLS) != `{"certResolver":"cloudflare"}` || probeHasTLS || public.Service == "" || public.Service != probe.Service {
		t.Fatalf("public=%#v probe=%#v", public, probe)
	}
	if runner.anyArg("-p", "8081:8081") {
		t.Fatalf("probe port was published: %#v", runner.calls)
	}
	if _, err := os.Stat(rt.dataPath(pg.DataToken)); err != nil {
		t.Fatalf("postgres data directory = %v", err)
	}
	if _, err := os.Stat(rt.proxyACMEPath()); err != nil {
		t.Fatalf("acme = %v", err)
	}
}

func TestRoutesAreOnlyWrittenToMountedDynamicDirectory(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	req := requestFor(state, revisionB(), app("one", "image"))
	req.Target.ReverseProxy = &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	req.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rt.root, "runtime", "dynamic", routeName(appToken("one")))); err != nil {
		t.Fatalf("dynamic route = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rt.root, "runtime", "managed", routeName(appToken("one")))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("route leaked into managed artifacts: %v", err)
	}
	if !runner.anyArg("-v", rt.dynamicPath()+":/etc/traefik/dynamic:ro") {
		t.Fatalf("proxy does not mount dynamic routes: %#v", runner.calls)
	}
}

func TestReconcileCreatesOwnedSharedNetworkAndUsesItForEveryContainer(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	req := requestFor(state, revisionB(), app("one", "image"))
	req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380}}
	req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "safe"}}
	req.Target.ReverseProxy = &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	req.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	network := networkName(state)
	if !runner.hasCall([]string{"network", "create", "--label", "sub2api.host=" + ownershipLabelFor(state.Resource, state.Ownership, "network", "", ""), "--label", "sub2api.host.network=" + networkLabelFor(state.Resource, state.Ownership), network}) {
		t.Fatalf("network trace=%#v", runner.calls)
	}
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "run" && !containsPair(call, "--network", network) {
			t.Fatalf("container missing network: %#v", call)
		}
		if len(call) > 0 && call[0] == "run" && (!hasLabel(call, "sub2api.host") || !hasLabel(call, "sub2api.host.target")) {
			t.Fatalf("container missing keyed ownership labels: %#v", call)
		}
	}
}

func TestAppRunUsesRestartUnlessStoppedAndDefaultDrain(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); err != nil {
		t.Fatal(err)
	}
	object := findApp(mustInventory(t, rt), appToken("one"))
	if object.DrainSeconds != 30 {
		t.Fatalf("default drain = %d", object.DrainSeconds)
	}
	if !runner.hasCallPrefix([]string{"run", "-d", "--restart", "unless-stopped"}) {
		t.Fatalf("app restart argv=%#v", runner.calls)
	}
}

func TestAppBlueGreenSlotsSharePreservedDataDirectory(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "old"))); err != nil {
		t.Fatal(err)
	}
	data := rt.dataPath(token("app-data", appToken("one")))
	if !runner.anyArg("-v", data+":/app/data") {
		t.Fatalf("initial app data mount = %#v", runner.calls)
	}
	if err := os.WriteFile(filepath.Join(data, "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), app("one", "new"))); err != nil {
		t.Fatal(err)
	}
	if !runner.anyArg("-v", data+":/app/data") {
		t.Fatalf("replacement app data mount = %#v", runner.calls)
	}
	if got, err := os.ReadFile(filepath.Join(data, "sentinel")); err != nil || string(got) != "keep" {
		t.Fatalf("preserved app data = %q, %v", got, err)
	}
}

func TestEveryRunRoleUsesExactKeyedOwnershipAndTargetLabels(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	local := localObject(state, hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380}, revisionB())
	local.DataToken = token("data", local.AppToken)
	if err := rt.runLocal(context.Background(), state, local); err != nil {
		t.Fatal(err)
	}
	proxy := proxyObject(state, hostcontract.ReverseProxyTarget{Image: "traefik:v3"}, revisionB())
	if err := rt.runProxy(context.Background(), state, proxy); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); err != nil {
		t.Fatal(err)
	}
	for _, object := range []managedObject{local, proxy, findApp(mustInventory(t, rt), appToken("one"))} {
		if runner.inspect[object.Name] != ownershipLabelFor(state.Resource, state.Ownership, object.Role, object.AppToken, object.Active) || runner.targets[object.Name] != targetLabelFor(object) {
			t.Fatalf("labels for %s = ownership %q target %q", object.Role, runner.inspect[object.Name], runner.targets[object.Name])
		}
	}
}

func TestReconcileRejectsUnownedNetworkAndHostileInputsBeforeMutation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{networks: map[string]string{networkName(state): "other"}}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.mutations() != 0 {
		t.Fatalf("network=%v %#v", err, runner.calls)
	}
	runner.calls, runner.networks = nil, nil
	req := requestFor(state, revisionB(), app("one", "image"))
	req.Target.Apps[0].Hostname = "x`) || true || Host(`x"
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) || runner.mutations() != 0 {
		t.Fatalf("hostname=%v %#v", err, runner.calls)
	}
}

func TestRedisConfigQuotesPasswordAndStaticConfigRendersEmailWithoutToken(t *testing.T) {
	got, err := redisConfigValue(`a b#"\\c`)
	if err != nil || got != `"a b#\"\\\\c"` {
		t.Fatalf("redis encoding=%q %v", got, err)
	}
	if _, err := redisConfigValue("bad\n"); err == nil {
		t.Fatal("newline accepted")
	}
	config := string(traefikStaticConfig("ops+test@example.test"))
	if !strings.Contains(config, "email: \"ops+test@example.test\"") || !strings.Contains(config, "probe:\n    address: \":8081\"") || strings.Contains(config, "ACME_EMAIL") || strings.Contains(config, "DNS_CANARY") {
		t.Fatalf("static config=%q", config)
	}
}

func TestReconcileRejectsInvalidLocalPasswordsBeforeBegin(t *testing.T) {
	for name, password := range map[string]string{"empty": "", "newline": "bad\nvalue", "nul": "bad\x00value", "control": "bad\x1fvalue"} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			before := mustFile(t, rt.statePath())
			req := requestFor(state, revisionB())
			req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
			req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: password}}
			if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) || len(runner.calls) != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
				t.Fatalf("password=%q err=%v calls=%#v", password, err, runner.calls)
			}
		})
	}
}

func TestRuntimeDirectorySymlinkAndUnsafeACMEAreRejectedWithoutDocker(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if err := os.MkdirAll(filepath.Join(rt.root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.Mkdir(sentinel, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(rt.root, "runtime", "data")); err != nil {
		t.Fatal(err)
	}
	req := requestFor(state, revisionB())
	req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380}}
	req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "safe"}}
	if _, err := rt.Reconcile(context.Background(), req); err == nil || runner.mutations() != 0 {
		t.Fatalf("symlink=%v %#v", err, runner.calls)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel changed: %v", err)
	}
}

func TestInventoryRejectsIrrelevantFieldsAndMalformedLocalTokens(t *testing.T) {
	base := inventory{Version: inventoryVersion, Resource: resource(), Ownership: ownership()}
	for name, object := range map[string]managedObject{
		"app data identity":   {Role: "app", AppToken: appToken("one"), Name: "app", Image: "image", Revision: revision(), Active: "blue", Hostname: "one.example", ReadinessPath: "/health", DrainSeconds: 30, DataIdentity: dataIdentity("x")},
		"metadata live field": {Role: "local-data-meta", AppToken: localDataToken("one"), Type: "redis", DataToken: token("data", localDataToken("one")), PathToken: token("path", localDataToken("one")), DataIdentity: hostcontract.DataIdentity{Kind: "redis", ProviderID: "x", Endpoint: "x", Port: 1, Database: "0"}, Env: "env-0123456789abcdef0123456789abcdef0123456789abcdef01234567"},
		"bad data token":      {Role: "local-data", AppToken: localDataToken("one"), Name: "x", Type: "redis", Image: redisImage, Revision: revision(), Port: 1, DataToken: "bad", PathToken: token("path", localDataToken("one")), DataIdentity: hostcontract.DataIdentity{Kind: "redis", ProviderID: "x", Endpoint: "x", Port: 1, Database: "0"}},
	} {
		base.Objects = []managedObject{object}
		if err := validateInventory(base); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestInventoryVersionOneFailsClosed(t *testing.T) {
	legacy := inventory{Version: 1, Resource: resource(), Ownership: ownership()}
	if err := validateInventory(legacy); err == nil {
		t.Fatal("inventory v1 accepted")
	}
}

func TestLocalDataRemovalRestoresMetadataAndRejectsDifferentTypeBeforeMutation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	pg := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	if err := os.WriteFile(filepath.Join(rt.dataPath(pg.DataToken), "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC())); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
	wrong := requestFor(state, revision())
	wrong.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "redis", Port: 6379}}
	wrong.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new"}}
	runner.calls = nil
	if _, err := rt.Reconcile(context.Background(), wrong); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.dockerMutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("wrong type = %v calls=%#v", err, runner.calls)
	}
	restore := requestFor(state, revisionB())
	restore.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	restore.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), restore); err != nil {
		t.Fatal(err)
	}
	got := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	if got.DataToken != pg.DataToken || got.PathToken != pg.PathToken || got.DataIdentity != pg.DataIdentity {
		t.Fatalf("restored identity = %#v, want %#v", got, pg)
	}
	if _, err := os.Stat(filepath.Join(rt.dataPath(pg.DataToken), "sentinel")); err != nil {
		t.Fatalf("data was not preserved: %v", err)
	}
	for _, o := range mustInventory(t, rt).Objects {
		if o.Role == "local-data-meta" && o.AppToken == localDataToken("primary") {
			t.Fatal("restored metadata was not consumed")
		}
	}
}

func TestProxyReadinessOccursOnlyAfterRouteAndBeforeOldRemoval(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	proxy := &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	first := requestFor(state, revisionB(), app("one", "old"))
	first.Target.ReverseProxy, first.Secrets.ReverseProxy = proxy, &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	events := []string{}
	routeWriteHook = func() error { events = append(events, "route"); return nil }
	t.Cleanup(func() { routeWriteHook = nil })
	runner.event = func(argv []string) {
		if len(argv) > 0 && argv[0] == "exec" {
			events = append(events, "exec:"+argv[1]+":"+argv[len(argv)-1])
		}
		if len(argv) > 0 && argv[0] == "rm" {
			events = append(events, "rm:"+argv[len(argv)-1])
		}
	}
	second := requestFor(state, revisionC(), app("one", "new"))
	second.Target.ReverseProxy, second.Secrets.ReverseProxy = proxy, &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	old := objectName(state, "app", appToken("one"), "green")
	candidate := objectName(state, "app", appToken("one"), "blue")
	direct, route, proxyProbe, removal := indexEvent(events, "exec:"+candidate+":http://localhost:8080/health"), indexEvent(events, "route"), indexEvent(events, "exec:"+candidate+":http://"+objectName(state, "proxy", "proxy", "live")+":8081/health"), indexEvent(events, "rm:"+old)
	if !(direct >= 0 && direct < route && route < proxyProbe && proxyProbe < removal) {
		t.Fatalf("readiness order=%#v", events)
	}
}

func TestProxyProbeFailureRestoresOldRouteAndCleansCandidate(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	proxy := &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	first := requestFor(state, revisionB(), app("one", "old"))
	first.Target.ReverseProxy, first.Secrets.ReverseProxy = proxy, &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	oldRoute := mustRouteArtifact(t, rt, routeName(appToken("one")))
	old := findApp(mustInventory(t, rt), appToken("one"))
	candidate := objectName(state, "app", appToken("one"), "blue")
	runner.fail = func(argv []string) error {
		if len(argv) > 5 && argv[0] == "exec" && argv[1] == candidate && containsPair(argv, "--header", "Host:one.example") {
			return errors.New("proxy probe")
		}
		return nil
	}
	second := requestFor(state, revisionC(), app("one", "new"))
	second.Target.ReverseProxy, second.Secrets.ReverseProxy = proxy, &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
	if _, err := rt.Reconcile(context.Background(), second); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("reconcile=%v", err)
	}
	if got := mustRouteArtifact(t, rt, routeName(appToken("one"))); !bytes.Equal(got, oldRoute) || runner.mutated(old.Name) || !runner.mutated(candidate) || runner.inspect[old.Name] == "" {
		t.Fatalf("rollback route=%q calls=%#v", got, runner.calls)
	}
}

func TestPostgresHasNoConfigArtifactAndInventoryRejectsWrongConfig(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	req := requestFor(state, revisionB())
	req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "safe"}}
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	pg := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	if pg.Config != "" {
		t.Fatalf("postgres config=%q", pg.Config)
	}
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "run" && containsPair(call, "--name", pg.Name) && strings.Contains(strings.Join(call, "\x00"), "config-") {
			t.Fatalf("postgres referenced config: %#v", call)
		}
	}
	pg.Config = artifactConfigPrefix + token(pg.AppToken, pg.Revision)
	if err := validateInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, Objects: []managedObject{pg}}); err == nil {
		t.Fatal("postgres config accepted")
	}
	redis := localObject(state, hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380}, revisionB())
	redis.DataToken, redis.PathToken = token("data", redis.AppToken), token("path", redis.AppToken)
	redis.Config = ""
	if err := validateInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, Objects: []managedObject{redis}}); err == nil {
		t.Fatal("redis missing config accepted")
	}
}

func TestPostgresPasswordRotationChangesCredentialBeforeShellReplacementWithoutLeaks(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "OLD_SECRET"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	runner.calls = nil
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new'pass"}}
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	alter, rm, run := -1, -1, -1
	for i, call := range runner.calls {
		if len(call) > 2 && call[0] == "exec" && call[1] == "-i" {
			alter = i
		}
		if len(call) > 0 && call[0] == "rm" && call[len(call)-1] == old.Name {
			rm = i
		}
		if len(call) > 0 && call[0] == "run" {
			run = i
		}
	}
	if !(alter >= 0 && alter < rm && rm < run) || runner.hasSecret("OLD_SECRET") || runner.hasSecret("new'pass") {
		t.Fatalf("credential ordering/leak calls=%#v", runner.calls)
	}
	if len(runner.stdinDigests) != 1 || runner.stdinDigests[0] != token("ALTER ROLE sub2api PASSWORD 'new''pass';") {
		t.Fatalf("alter stdin digest=%#v", runner.stdinDigests)
	}
	if _, err := rt.readArtifactBytes(old.Env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old password artifact remains: %v", err)
	}
}

func TestPostgresPasswordChangeWithMissingOldContainerRequiresRecoveryBeforeBegin(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	delete(runner.inspect, old.Name)
	runner.calls = nil
	beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new"}}
	if _, err := rt.Reconcile(context.Background(), second); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.dockerMutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("admission = %v calls=%#v", err, runner.calls)
	}
}

func TestPostgresSamePasswordMissingContainerRepairsDrift(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "same"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	delete(runner.inspect, old.Name)
	delete(runner.targets, old.Name)
	runner.calls = nil
	second := requestFor(state, revisionC())
	second.Target.DataServices, second.Secrets.LocalDataServices = first.Target.DataServices, first.Secrets.LocalDataServices
	result, err := rt.Reconcile(context.Background(), second)
	if err != nil || result.Status != hostprotocol.ResultApplied || runner.mutations("run") != 1 || runner.mutations("rm") != 0 {
		t.Fatalf("same-password repair = %#v %v calls=%#v", result, err, runner.calls)
	}
	got := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	if got.Name != old.Name || runner.inspect[got.Name] == "" || runner.targets[got.Name] != targetLabelFor(got) {
		t.Fatalf("repaired postgres = %#v", got)
	}
}

func TestPendingStableNameRejectsWrongExactTargetBeforeRetry(t *testing.T) {
	for name, configure := range map[string]func(*hostprotocol.Request){
		"local": func(req *hostprotocol.Request) {
			req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380, Persistence: true}}
			req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "safe"}}
		},
		"proxy": func(req *hostprotocol.Request) {
			req.Target.ReverseProxy = &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
			req.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			first := requestFor(state, revisionB())
			configure(&first)
			if _, err := rt.Reconcile(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			state, _ = rt.readState()
			runner.calls = nil
			runner.failAfter = true
			runner.fail = func(argv []string) error {
				if len(argv) > 0 && argv[0] == "run" {
					return errors.New("response lost")
				}
				return nil
			}
			second := requestFor(state, revisionC())
			configure(&second)
			if _, err := rt.Reconcile(context.Background(), second); err == nil || runner.mutations("run") != 1 {
				t.Fatalf("first = %v calls=%#v", err, runner.calls)
			}
			for container := range runner.targets {
				runner.targets[container] = "wrong-target"
			}
			runner.fail, runner.failAfter = nil, false
			if _, err := rt.Reconcile(context.Background(), second); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.mutations("run") != 1 {
				t.Fatalf("wrong target = %v calls=%#v", err, runner.calls)
			}
		})
	}
}

func TestOrdinaryMissingOwnedShellsAreRepairedForEveryRole(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	proxy := &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	first := requestFor(state, revisionB(), app("one", "image"))
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}, {ID: "cache", Type: "redis", Port: 6380, Persistence: true}}
	first.Target.ReverseProxy = proxy
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "postgres"}, "cache": {AdminPassword: "redis"}}
	first.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	for name := range runner.inspect {
		delete(runner.inspect, name)
		delete(runner.targets, name)
	}
	runner.calls = nil
	second := requestFor(state, revisionC(), app("one", "image"))
	second.Target.DataServices, second.Target.ReverseProxy = first.Target.DataServices, proxy
	second.Secrets.LocalDataServices, second.Secrets.ReverseProxy = first.Secrets.LocalDataServices, first.Secrets.ReverseProxy
	if _, err := rt.Reconcile(context.Background(), second); err != nil || runner.mutations("run") != 4 {
		t.Fatalf("repair = %v calls=%#v", err, runner.calls)
	}
	for _, object := range mustInventory(t, rt).Objects {
		if object.Role != "app-data" && object.Role != "local-data-meta" && object.Revision != revisionC() {
			t.Fatalf("unrepaired object=%#v", object)
		}
	}
}

func TestPostgresReadinessRequiresAlteredPersistentCredential(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{disablePostgresAlter: true}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new'password"}}
	if _, err := rt.Reconcile(context.Background(), second); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("rotation without ALTER = %v", err)
	}
	if len(runner.postgresCredentials) != 1 || !containsValue(runner.postgresCredentials, "old") || len(runner.stdinDigests) != 1 || runner.stdinDigests[0] != token("ALTER ROLE sub2api PASSWORD 'new''password';") || runner.hasSecret("new'password") {
		t.Fatalf("credential update was accepted or leaked: stdin=%#v calls=%#v", runner.stdinDigests, runner.calls)
	}
}

func TestPostgresReadinessRejectsWrongReplacementPGPASSWORD(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.wrongPostgresPassword = true
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new"}}
	if _, err := rt.Reconcile(context.Background(), second); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) || runner.hasSecret("new") {
		t.Fatalf("wrong replacement password = %v calls=%#v", err, runner.calls)
	}
}

func TestRedisAndProxyRotationRemovesOldArtifactsAndPreservesData(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380, Persistence: true}}
	first.Target.ReverseProxy = &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "REDIS_OLD"}}
	first.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "DNS_OLD"}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	oldCache, oldProxy := findLocalData(mustInventory(t, rt), localDataToken("cache")), findProxy(mustInventory(t, rt))
	if err := os.WriteFile(filepath.Join(rt.dataPath(oldCache.DataToken), "sentinel"), []byte("redis"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rt.proxyACMEPath(), []byte("acme"), 0600); err != nil {
		t.Fatal(err)
	}
	second := requestFor(state, revisionC())
	second.Target.DataServices, second.Target.ReverseProxy = first.Target.DataServices, first.Target.ReverseProxy
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "REDIS_NEW"}}
	second.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "DNS_NEW"}
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{oldCache.Env, oldCache.Config, oldProxy.Env, oldProxy.Config} {
		if _, err := rt.readArtifactBytes(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old artifact %q remains: %v", artifact, err)
		}
	}
	for _, sentinel := range []string{filepath.Join(rt.dataPath(oldCache.DataToken), "sentinel"), rt.proxyACMEPath()} {
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("preserved %q: %v", sentinel, err)
		}
	}
	if runner.hasSecret("REDIS_OLD") || runner.hasSecret("REDIS_NEW") || runner.hasSecret("DNS_OLD") || runner.hasSecret("DNS_NEW") {
		t.Fatalf("secret leaked into command trace: %#v", runner.calls)
	}
}

func TestUnsafeACMEAndPersistedDataTokensFailBeforeDockerMutation(t *testing.T) {
	for name, prepare := range map[string]func(*testing.T, *Runtime){
		"symlink": func(t *testing.T, rt *Runtime) {
			t.Helper()
			if err := os.Symlink("elsewhere", rt.proxyACMEPath()); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, rt *Runtime) {
			t.Helper()
			source := filepath.Join(filepath.Dir(rt.proxyACMEPath()), "source")
			if err := os.WriteFile(source, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(source, rt.proxyACMEPath()); err != nil {
				t.Fatal(err)
			}
		},
		"permissive": func(t *testing.T, rt *Runtime) {
			t.Helper()
			if err := os.WriteFile(rt.proxyACMEPath(), nil, 0644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			if err := rt.ensureRuntimeDir("proxy", "acme"); err != nil {
				t.Fatal(err)
			}
			prepare(t, rt)
			req := requestFor(state, revisionB())
			req.Target.ReverseProxy = &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
			req.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
			if _, err := rt.Reconcile(context.Background(), req); err == nil || runner.dockerMutations() != 0 {
				t.Fatalf("reconcile=%v calls=%#v", err, runner.calls)
			}
		})
	}
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	bad := []byte(`{"version":2,"resource":{"environment":"production","serverKey":"edge"},"ownership":{"value":"owner1"},"objects":[{"role":"local-data","appToken":"` + localDataToken("cache") + `","name":"x","image":"redis:8-alpine","revision":"` + revision() + `","type":"redis","port":6380,"dataToken":"../bad","pathToken":"` + token("path", localDataToken("cache")) + `","dataIdentity":{"kind":"redis","providerID":"x","endpoint":"x","port":6380,"database":"0"},"env":"` + envName(localDataToken("cache"), revision()) + `","config":"` + artifactConfigPrefix + token(localDataToken("cache"), revision()) + `"}]}`)
	if err := rt.writeArtifact(artifactInventory, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB())); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.dockerMutations() != 0 {
		t.Fatalf("token=%v calls=%#v", err, runner.calls)
	}
}

func TestInventoryRejectsMalformedAndDuplicateManagedObjects(t *testing.T) {
	rt, state := initialized(t)
	for name, value := range map[string][]byte{
		"unknown role":   []byte(`{"version":2,"resource":{"environment":"production","serverKey":"edge"},"ownership":{"value":"owner1"},"objects":[{"role":"future","name":"x"}]}`),
		"duplicate name": []byte(`{"version":2,"resource":{"environment":"production","serverKey":"edge"},"ownership":{"value":"owner1"},"objects":[{"role":"app","appToken":"0123456789abcdef01234567","name":"same","image":"old","revision":"tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","active":"blue"},{"role":"app","appToken":"fedcba9876543210fedcba98","name":"same","image":"old","revision":"tr1:0123456789abcdef:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","active":"green"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := rt.writeArtifact(artifactInventory, value, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := rt.readInventory(); err == nil {
				t.Fatal("invalid inventory accepted")
			}
		})
	}
	_ = state
}

func TestReconcileManagedInspectFailureAndCandidateUnownedFailBeforeMutation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{inspect: map[string]string{}}
	rt.runner = runner
	runner.fail = func(argv []string) error {
		if len(argv) > 1 && argv[0] == "container" && argv[1] == "ls" {
			return errors.New("inspect unavailable")
		}
		return nil
	}
	oldName := objectName(state, "app", appToken("one"), "blue")
	if err := rt.writeInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, Objects: []managedObject{{Role: "app", AppToken: appToken("one"), Name: oldName, Image: "old", Revision: state.AppliedRevision, Active: "blue", Env: envName(appToken("one"), state.AppliedRevision), Hostname: "one.example", ReadinessPath: "/health", DrainSeconds: 30}}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.writeArtifact(routeName(appToken("one")), mustRoute(t, rt, state, "one"), 0600); err != nil {
		t.Fatal(err)
	}
	before := mustArtifact(t, rt, artifactInventory)
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.mutations() != 0 || !bytes.Equal(before, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("managed inspect failure = %v %#v", err, runner.calls)
	}
	runner.fail = nil
	runner.calls = nil
	runner.inspect[oldName] = ownershipLabelFor(state.Resource, state.Ownership, "app", appToken("one"), "blue")
	candidate := objectName(state, "app", appToken("one"), "green")
	runner.inspect[candidate] = "unowned"
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.mutations() != 0 {
		t.Fatalf("candidate ownership = %v %#v", err, runner.calls)
	}
}

func TestReconcilePersistedAppWithoutExactRouteRequiresRecoveryBeforeBegin(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "old"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	beforeState := mustFile(t, rt.statePath())
	beforeInventory := mustArtifact(t, rt, artifactInventory)
	for _, route := range [][]byte{nil, []byte(`{"version":1,"resource":"broken"}`)} {
		runner.calls = nil
		if route == nil {
			if err := os.Remove(rt.artifactPath(routeName(appToken("one")))); err != nil {
				t.Fatal(err)
			}
		} else if err := rt.writeArtifact(routeName(appToken("one")), route, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.mutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
			t.Fatalf("route=%q err=%v calls=%#v", route, err, runner.calls)
		}
		if err := rt.writeArtifact(routeName(appToken("one")), mustRoute(t, rt, state, "one"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcileWritesRouteBeforeRemovingOldAndRotatesSameImageNewRevision(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); err != nil {
		t.Fatal(err)
	}
	first, _ := rt.readState()
	runner.calls = nil
	if _, err := rt.Reconcile(context.Background(), requestFor(first, revisionC(), app("one", "image"))); err != nil {
		t.Fatal(err)
	}
	if runner.mutations("run") != 1 || runner.mutations("rm") != 1 || !runner.mutated(objectName(first, "app", appToken("one"), "green")) {
		t.Fatalf("rotation trace = %#v", runner.calls)
	}
	if _, err := rt.readArtifactBytes("route-" + appToken("one") + ".json"); err != nil {
		t.Fatalf("route artifact = %v", err)
	}
}

func TestReconcileRouteReadinessRollbackAndUnknownRouteRetry(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "old"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	oldRoute := mustArtifact(t, rt, routeName(appToken("one")))
	candidate := objectName(state, "app", appToken("one"), "blue")
	ready := 0
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "exec" {
			ready++
			if ready == 2 {
				return errors.New("post-route")
			}
		}
		return nil
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("post-route = %v", err)
	}
	if got := mustArtifact(t, rt, routeName(appToken("one"))); !bytes.Equal(got, oldRoute) || runner.mutations("rm") != 1 {
		t.Fatalf("rollback route=%q trace=%#v", got, runner.calls)
	}
	runner.fail = nil
	runner.calls = nil
	routeWriteHook = func() error { return errors.New("unknown after route") }
	t.Cleanup(func() { routeWriteHook = nil })
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("unknown route = %v", err)
	}
	if runner.mutations("run") != 1 {
		t.Fatalf("first trace=%#v", runner.calls)
	}
	routeWriteHook = nil
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), app("one", "new"))); err != nil {
		t.Fatal(err)
	}
	if runner.mutations("run") != 1 || runner.mutations("rm") != 1 {
		t.Fatalf("retry trace=%#v", runner.calls)
	}
	_ = candidate
}

func TestReconcileApprovalUsesDataLinkNameAndMaintenanceRemovesRouteFirst(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	old := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old", Endpoint: "old.db", Port: 5432, Database: "app", TLSServerName: "old.db"}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithData("one", "one", old), app("two", "two"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	req := requestFor(state, revisionC(), appWithData("one", "one", hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new", Endpoint: "new.db", Port: 5432, Database: "app", TLSServerName: "new.db"}), app("two", "two"))
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorApproval, hostprotocol.CodeApprovalRequired) {
		t.Fatalf("link approval = %v", err)
	}
	runner.calls = nil
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), app("two", "two"))); err != nil {
		t.Fatal(err)
	}
	route := routeName(appToken("one"))
	firstRM := -1
	for i, call := range runner.calls {
		if len(call) > 0 && call[0] == "rm" {
			firstRM = i
			break
		}
	}
	if firstRM < 0 {
		t.Fatalf("no maintenance removal: %#v", runner.calls)
	}
	if _, err := rt.readArtifactBytes(route); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("route not removed: %v", err)
	}
}

func TestReconcileRestoredAppDataRequiresExactApproval(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	a := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "a", Endpoint: "a.db", Port: 5432, Database: "app", TLSServerName: "a.db"}
	b := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "b", Endpoint: "b.db", Port: 5432, Database: "app", TLSServerName: "b.db"}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithData("one", "one", a))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC())); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
	req := requestFor(state, revision(), appWithData("one", "one", b))
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorApproval, hostprotocol.CodeApprovalRequired) || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("missing approval=%v", err)
	}
	req.Approval = &hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: a, NewData: b, TargetRevision: req.TargetRevision}
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	inv := mustInventory(t, rt)
	if inv.hasData(appToken("one")) || !inv.hasApp(appToken("one")) {
		t.Fatalf("restored inventory = %#v", inv)
	}
	state, _ = rt.readState()
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithData("one", "renamed", b))); err != nil {
		t.Fatalf("same identity rename unexpectedly requires approval: %v", err)
	}
}

func TestReconcilePendingDataLinkApprovalResumesWithoutResubmission(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	old := dataIdentity("old")
	new := dataIdentity("new")
	if _, err := rt.Reconcile(t.Context(), requestFor(state, revisionB(), appWithData("one", "one", old))); err != nil { t.Fatal(err) }
	state, _ = rt.readState()
	request := requestFor(state, revisionC(), appWithData("one", "one", new))
	key := requestKey(request)
	approval := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: old, NewData: new, TargetRevision: request.TargetRevision}
	// Persisted intent represents a completed approval followed by process/response loss.
	op, err := rt.Begin(key, &approval)
	if err != nil { t.Fatal(err) }
	if err := op.Close(); err != nil { t.Fatal(err) }
	before, mutations := mustFile(t, rt.statePath()), runner.mutations()
	wrong := approval
	wrong.NewData.Endpoint = "wrong"
	request.Approval = &wrong
	if _, err := rt.Reconcile(t.Context(), request); err == nil || !bytes.Equal(before, mustFile(t, rt.statePath())) || runner.mutations() != mutations {
		t.Fatalf("wrong persisted approval retry mutated: %v", err)
	}
	request.Approval = nil
	result, err := rt.Reconcile(t.Context(), request)
	if err != nil || result.Status != hostprotocol.ResultApplied || result.AppliedRevision != key.TargetRevision || runner.mutations() == mutations {
		t.Fatalf("nil-approval pending resume = %#v, %v", result, err)
	}
}

func TestReconcileTerminalDataLinkApprovalRejectsWrongReplayWithoutMutation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	old, next := dataIdentity("old"), dataIdentity("new")
	if _, err := rt.Reconcile(t.Context(), requestFor(state, revisionB(), appWithData("one", "one", old))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	request := requestFor(state, revisionC(), appWithData("one", "one", next))
	approval := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: old, NewData: next, TargetRevision: request.TargetRevision}
	request.Approval = &approval
	result, err := rt.Reconcile(t.Context(), request)
	if err != nil || result.Status != hostprotocol.ResultApplied {
		t.Fatalf("approved reconcile = %#v, %v", result, err)
	}
	before, mutations := mustFile(t, rt.statePath()), runner.mutations()
	request.Approval = nil
	if replay, err := rt.Reconcile(t.Context(), request); err != nil || !reflect.DeepEqual(replay, result) || runner.mutations() != mutations {
		t.Fatalf("nil terminal replay = %#v, %v", replay, err)
	}
	wrong := approval
	wrong.NewData.Endpoint = "different.example"
	request.Approval = &wrong
	if _, err := rt.Reconcile(t.Context(), request); err == nil || !bytes.Equal(before, mustFile(t, rt.statePath())) || runner.mutations() != mutations {
		t.Fatalf("wrong terminal approval replay mutated: %v", err)
	}
}

func TestReconcileRestoredRenamedDataRejectsWrongApprovalBeforeMutation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	a := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "a", Endpoint: "a.db", Port: 5432, Database: "app", TLSServerName: "a.db"}
	b := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "b", Endpoint: "b.db", Port: 5432, Database: "app", TLSServerName: "b.db"}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithData("one", "one", a))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC())); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	req := requestFor(state, revision(), appWithData("one", "renamed", b))
	before := len(runner.calls)
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorApproval, hostprotocol.CodeApprovalRequired) || len(runner.calls) != before {
		t.Fatalf("missing approval = %v %#v", err, runner.calls)
	}
	req.Approval = &hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: b, NewData: a, TargetRevision: req.TargetRevision}
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorApproval, hostprotocol.CodeApprovalRequired) || len(runner.calls) != before {
		t.Fatalf("wrong approval = %v %#v", err, runner.calls)
	}
	req.Approval = &hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: a, NewData: b, TargetRevision: req.TargetRevision}
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	inv := mustInventory(t, rt)
	if inv.hasData(appToken("one")) || !inv.hasApp(appToken("one")) {
		t.Fatalf("stale restored data = %#v", inv)
	}
	state, _ = rt.readState()
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithData("one", "renamed", b))); err != nil {
		t.Fatalf("next revision = %v", err)
	}
	state, _ = rt.readState()
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC())); err != nil {
		t.Fatal(err)
	}
	if !mustInventory(t, rt).hasData(appToken("one")) {
		t.Fatal("maintenance did not preserve data")
	}
}

func TestReconcileRenamedMultipleDataLinksWithSameIdentitiesNeedsNoApproval(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	a := dataIdentity("a")
	b := dataIdentity("b")
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithLinks("one", "old", "old-a", a, "old-b", b))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), appWithLinks("one", "new", "new-b", b, "new-a", a))); err != nil {
		t.Fatalf("renamed links with unchanged identities = %v", err)
	}
}

func TestReconcileRenamedMultipleDataLinksRequiresExactSingleChangeApproval(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	a := dataIdentity("a")
	b := dataIdentity("b")
	c := dataIdentity("c")
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithLinks("one", "old", "old-a", a, "old-b", b))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	req := requestFor(state, revisionC(), appWithLinks("one", "new", "new-b", b, "new-a", c))
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorApproval, hostprotocol.CodeApprovalRequired) {
		t.Fatalf("missing approval = %v", err)
	}
	req.Approval = &hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: a, NewData: c, TargetRevision: req.TargetRevision}
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("exact approval = %v", err)
	}
}

func TestReconcileRenamedMultipleDataLinksRejectsOneApprovalForTwoChanges(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	a := dataIdentity("a")
	b := dataIdentity("b")
	c := dataIdentity("c")
	d := dataIdentity("d")
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithLinks("one", "old", "old-a", a, "old-b", b))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	req := requestFor(state, revisionC(), appWithLinks("one", "new", "new-a", c, "new-b", d))
	req.Approval = &hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: a, NewData: c, TargetRevision: req.TargetRevision}
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorApproval, hostprotocol.CodeApprovalRequired) {
		t.Fatalf("one approval for two changes = %v", err)
	}
}

func TestReconcileContainerListErrorsNeverMeanAbsent(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	runner.fail = func(argv []string) error {
		if len(argv) > 1 && argv[0] == "container" && argv[1] == "ls" {
			return errors.New("daemon unavailable")
		}
		return nil
	}
	before := mustFile(t, rt.statePath())
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.mutations() != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
		t.Fatalf("list failure = %v %#v", err, runner.calls)
	}
	runner.fail = nil
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"))); err != nil {
		t.Fatalf("empty list absent = %v", err)
	}
}

func TestReconcileSecondCandidateObservationFailureDoesNotRunOrWriteEnv(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	observations := 0
	runner.fail = func(argv []string) error {
		if len(argv) > 1 && argv[0] == "container" && argv[1] == "ls" {
			observations++
			if observations == 3 {
				return errors.New("observation unavailable")
			}
		}
		return nil
	}
	env := envName(appToken("one"), revisionB())
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.mutations("run") != 0 {
		t.Fatalf("second observation = %v %#v", err, runner.calls)
	}
	if _, err := rt.readArtifactBytes(env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env written before candidate observation: %v", err)
	}
}

func TestReconcilePreRouteReadinessOwnershipChangeDoesNotRemoveCandidate(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	candidate := objectName(state, "app", appToken("one"), "green")
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "exec" {
			runner.inspect[candidate] = "changed"
			return errors.New("not ready")
		}
		return nil
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("reconcile = %v", err)
	}
	if runner.mutated(candidate) {
		t.Fatalf("unsafe pre-route cleanup: %#v", runner.calls)
	}
}

func TestCandidateFailureCleanupRemovesEnvAndRestoreFailureRetainsIt(t *testing.T) {
	for name, restoreFails := range map[string]bool{"pre-route": false, "restore-fails": true} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			ready := 0
			runner.fail = func(argv []string) error {
				if len(argv) > 0 && argv[0] == "exec" {
					ready++
					if !restoreFails || ready == 2 {
						return errors.New("not ready")
					}
				}
				return nil
			}
			if restoreFails {
				routeRestoreHook = func() error { return errors.New("restore failed") }
				t.Cleanup(func() { routeRestoreHook = nil })
			}
			if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "new"))); err == nil {
				t.Fatal("reconcile unexpectedly succeeded")
			}
			env := envName(appToken("one"), revisionB())
			_, err := rt.readArtifactBytes(env)
			if restoreFails && err != nil {
				t.Fatalf("candidate env removed after failed restore: %v", err)
			}
			if !restoreFails && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("candidate env remains: %v", err)
			}
		})
	}
}

func TestPendingMaintenanceEnvUnlinkUnknownRetriesWithoutRuntimeMutation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findApp(mustInventory(t, rt), appToken("one"))
	artifactRemoveHook = func(name string) error {
		if name == old.Env {
			return errors.New("env durability unknown")
		}
		return nil
	}
	t.Cleanup(func() { artifactRemoveHook = nil })
	req := requestFor(state, revisionC())
	if _, err := rt.Reconcile(context.Background(), req); err == nil {
		t.Fatal("env unlink unexpectedly succeeded")
	}
	runner.calls = nil
	artifactRemoveHook = nil
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if runner.mutations() != 0 {
		t.Fatalf("retry repeated runtime mutation: %#v", runner.calls)
	}
}

func TestEnvArtifactContainsAllSecretsAndNoCommandCanary(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	a := app("one", "one")
	a.RuntimeSettings = map[string]string{"SETTING_B": "two", "SETTING_A": "one"}
	secrets := hostcontract.AppSecrets{RuntimeEnvironment: map[string]string{"RUNTIME_B": "four", "RUNTIME_A": "three"}, InitialAdminPassword: "admin", JWTSecret: "jwt", TOTPEncryptionKey: "totp", AdminAPIKey: "api", Postgres: &hostcontract.DataCredentials{Username: "pguser", Password: "pgpass"}, Redis: &hostcontract.DataCredentials{Username: "redisuser", Password: "redispass"}}
	req := requestFor(state, revisionB(), a)
	req.Secrets.Apps[a.ID] = secrets
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	inv := mustInventory(t, rt)
	got := string(mustArtifact(t, rt, findApp(inv, appToken("one")).Env))
	want := "RUNTIME_A=three\nRUNTIME_B=four\nSETTING_A=one\nSETTING_B=two\nINITIAL_ADMIN_PASSWORD=admin\nJWT_SECRET=jwt\nTOTP_ENCRYPTION_KEY=totp\nADMIN_API_KEY=api\nPOSTGRES_USERNAME=pguser\nPOSTGRES_PASSWORD=pgpass\nREDIS_USERNAME=redisuser\nREDIS_PASSWORD=redispass\n"
	if got != want || runner.hasSecret("pgpass") || runner.hasSecret("CANARY") {
		t.Fatalf("env=%q calls=%#v", got, runner.calls)
	}
}

func TestReconcileStaleInventoryOwnershipAndPreexistingNewRouteFailBeforeBegin(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	inv := inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: hostcontract.OwnershipIdentity{Value: "other"}}
	if err := rt.writeInventory(inv); err != nil {
		t.Fatal(err)
	}
	before := mustFile(t, rt.statePath())
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || len(runner.calls) != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
		t.Fatalf("stale ownership=%v %#v", err, runner.calls)
	}
	if err := rt.removeArtifact(artifactInventory); err != nil {
		t.Fatal(err)
	}
	if err := rt.writeArtifact(routeName(appToken("one")), []byte(`{"version":1,"resource":"other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"))); !(isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict)) || len(runner.calls) != 0 {
		t.Fatalf("route=%v %#v", err, runner.calls)
	}
}

func TestReconcileRejectsReservedAndInvalidEnvironmentBeforeBegin(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	req := requestFor(state, revisionB(), app("one", "one"))
	req.Target.Apps[0].RuntimeSettings = map[string]string{"JWT_SECRET": "override"}
	before := mustFile(t, rt.statePath())
	if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) || len(runner.calls) != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
		t.Fatalf("reserved=%v %#v", err, runner.calls)
	}
}

func TestRetireWrongApprovalLeavesStateAndInventoryUntouched(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	beforeState, _ := os.ReadFile(rt.statePath())
	beforeInventory := mustArtifact(t, rt, artifactInventory)
	wrong := retireApproval(retireKey(state), state)
	wrong.Ownership.Value = "wrong"
	if _, err := rt.Retire(context.Background(), retireRequest(retireKey(state), wrong)); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || runner.mutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("wrong retire = %v %#v", err, runner.calls)
	}
}

func TestRetireTerminalReplayRejectsMissingOrWrongApprovalWithoutCommands(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	key := retireKey(state)
	approval := retireApproval(key, state)
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]hostprotocol.Request{
		"missing": {Action: hostcontract.ActionRetirePreserveData, Resource: key.Resource, TargetRevision: key.TargetRevision, PriorAppliedRevision: key.PriorAppliedRevision},
		"wrong": retireRequest(key, func() hostcontract.ApprovalSubject {
			wrong := approval
			wrong.Ownership.Value = "wrong"
			return wrong
		}()),
	} {
		t.Run(name, func(t *testing.T) {
			runner.calls = nil
			if _, err := rt.Retire(context.Background(), request); err == nil || len(runner.calls) != 0 {
				t.Fatalf("replay err=%v calls=%#v", err, runner.calls)
			}
		})
	}
}

func TestReconcileDataApprovalAndUnownedAdmissionHaveNoJournalOrMutation(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	old := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "old", Endpoint: "old.db", Port: 5432, Database: "app", TLSServerName: "old.db"}
	if err := rt.writeInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, Objects: []managedObject{{Role: "app", AppToken: appToken("one"), Name: objectName(state, "app", appToken("one"), "blue"), Image: "old", Data: []managedLink{{Name: "main", Identity: old}}, Revision: state.AppliedRevision, Active: "blue", Env: envName(appToken("one"), state.AppliedRevision), Hostname: "one.example", ReadinessPath: "/health", DrainSeconds: 30}}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.writeArtifact(routeName(appToken("one")), mustRoute(t, rt, state, "one"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := rt.readArtifactBytes(artifactInventory)
	if err != nil {
		t.Fatal(err)
	}
	request := requestFor(state, revisionB(), appWithData("one", "image-one", hostcontract.DataIdentity{Kind: "postgres", ProviderID: "new", Endpoint: "new.db", Port: 5432, Database: "app", TLSServerName: "new.db"}))
	_, err = rt.Reconcile(context.Background(), request)
	if !isRemote(err, hostprotocol.ErrorApproval, hostprotocol.CodeApprovalRequired) || runner.mutations() != 0 || !bytes.Equal(before, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("missing approval = %v %#v", err, runner.calls)
	}
	request.Approval = &hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: old, NewData: request.Target.Apps[0].DataLinks[0].Identity, TargetRevision: request.TargetRevision}
	runner.inspect = map[string]string{objectName(state, "app", appToken("one"), "blue"): "unowned"}
	_, err = rt.Reconcile(context.Background(), request)
	if !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.mutations() != 0 || !bytes.Equal(before, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("unowned = %v %#v", err, runner.calls)
	}
}

func TestReconcileMaintenanceOnlyRemovesAbsentAppAndPreservesData(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	data := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "db", Endpoint: "db.local", Port: 5432, Database: "app", TLSServerName: "db.local"}
	first := requestFor(state, revisionB(), appWithData("one", "image-one", data), app("two", "image-two"))
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	second := requestFor(state, revisionC(), app("two", "image-two"))
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if runner.mutations("rm") != 2 || !runner.mutated(objectName(state, "app", appToken("one"), "green")) {
		t.Fatalf("maintenance trace = %#v", runner.calls)
	}
	inv, err := rt.readInventory()
	if err != nil || !inv.hasData(appToken("one")) || !inv.hasApp(appToken("two")) {
		t.Fatalf("inventory = %#v, %v", inv, err)
	}
}

func TestRetireEnumeratesInventoryAndPreservesMetadata(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image-one"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	before, err := rt.readArtifactBytes(artifactInventory)
	if err != nil {
		t.Fatal(err)
	}
	key := retireKey(state)
	approval := retireApproval(key, state)
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil {
		t.Fatal(err)
	}
	if runner.mutations("rm") == 0 || !bytes.Equal(before, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("retire trace=%#v", runner.calls)
	}
	calls := len(runner.calls)
	if result, err := rt.Handle(context.Background(), hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource}); err != nil || result.Status != hostprotocol.ResultRetired {
		t.Fatalf("inspect=%#v %v", result, err)
	}
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil || len(runner.calls) != calls {
		t.Fatalf("replay=%v", err)
	}
}

func TestPendingBlueGreenUnknownOldRemovalResumesFromTargetRoute(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "old"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := objectName(state, "app", appToken("one"), "green")
	candidate := managedObject{Role: "app", AppToken: appToken("one"), Name: objectName(state, "app", appToken("one"), "blue"), Image: "new", Revision: revisionC(), Active: "blue", Env: envName(appToken("one"), revisionC()), Hostname: "one.example", ReadinessPath: "/health", DrainSeconds: 30}
	runner.calls = nil
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "rm" && argv[len(argv)-1] == old {
			delete(runner.inspect, old)
			return errors.New("unknown removal")
		}
		return nil
	}
	req := requestFor(state, revisionC(), app("one", "new"))
	if _, err := rt.Reconcile(context.Background(), req); err == nil {
		t.Fatal("first removal unexpectedly succeeded")
	}
	if runner.mutations("run") != 1 || runner.mutations("rm") != 1 || !rt.routeMatches(mustInventory(t, rt), candidate) || runner.inspect[candidate.Name] == "" {
		t.Fatalf("first trace = %#v", runner.calls)
	}
	runner.fail = nil
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if runner.mutations("run") != 1 || runner.mutations("rm") != 1 {
		t.Fatalf("retry trace = %#v", runner.calls)
	}
	calls := len(runner.calls)
	if _, err := rt.Reconcile(context.Background(), req); err != nil || len(runner.calls) != calls {
		t.Fatalf("terminal replay = %v %#v", err, runner.calls)
	}
}

func TestPendingStableNameRunUnknownResumesLocalAndProxyWithoutSecondRun(t *testing.T) {
	for name, configure := range map[string]func(*hostprotocol.Request){
		"local": func(req *hostprotocol.Request) {
			req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380}}
			req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "safe"}}
		},
		"proxy": func(req *hostprotocol.Request) {
			req.Target.ReverseProxy = &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
			req.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			first := requestFor(state, revisionB())
			configure(&first)
			if _, err := rt.Reconcile(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			state, _ = rt.readState()
			runner.calls = nil
			failed := false
			runner.failAfter = true
			runner.fail = func(argv []string) error {
				if !failed && len(argv) > 0 && argv[0] == "run" {
					failed = true
					return errors.New("run response lost")
				}
				return nil
			}
			second := requestFor(state, revisionC())
			configure(&second)
			if _, err := rt.Reconcile(context.Background(), second); err == nil || runner.mutations("run") != 1 {
				t.Fatalf("first=%v calls=%#v", err, runner.calls)
			}
			runner.fail = nil
			runner.failAfter = false
			if _, err := rt.Reconcile(context.Background(), second); err != nil || runner.mutations("run") != 1 {
				t.Fatalf("retry=%v calls=%#v", err, runner.calls)
			}
			if _, err := rt.Reconcile(context.Background(), requestFor(state, revision(), second.Target.Apps...)); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
				t.Fatalf("different revision=%v", err)
			}
		})
	}
}

func TestAppRunUnknownResumesExactCandidateAndRejectsWrongTarget(t *testing.T) {
	for name, wrongTarget := range map[string]bool{"exact": false, "wrong target": true} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{failAfter: true}
			rt.runner = runner
			runner.fail = func(argv []string) error {
				if len(argv) > 0 && argv[0] == "run" {
					return errors.New("run response lost")
				}
				return nil
			}
			req := requestFor(state, revisionB(), app("one", "image"))
			if _, err := rt.Reconcile(context.Background(), req); err == nil || runner.mutations("run") != 1 {
				t.Fatalf("first = %v calls=%#v", err, runner.calls)
			}
			if wrongTarget {
				runner.targets[objectName(state, "app", appToken("one"), "green")] = "wrong-target"
			}
			runner.fail, runner.failAfter = nil, false
			result, err := rt.Reconcile(context.Background(), req)
			if wrongTarget {
				if !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.mutations("run") != 1 {
					t.Fatalf("wrong target = %v calls=%#v", err, runner.calls)
				}
			} else if err != nil || result.Status != hostprotocol.ResultApplied || runner.mutations("run") != 1 {
				t.Fatalf("retry = %#v %v calls=%#v", result, err, runner.calls)
			}
		})
	}
}

func TestOrdinaryStableNameWrongTargetFailsClosed(t *testing.T) {
	for name, configure := range map[string]func(*hostprotocol.Request){
		"local": func(req *hostprotocol.Request) {
			req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380, Persistence: true}}
			req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "safe"}}
		},
		"proxy": func(req *hostprotocol.Request) {
			req.Target.ReverseProxy = &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
			req.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "dns"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			first := requestFor(state, revisionB())
			configure(&first)
			if _, err := rt.Reconcile(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			state, _ = rt.readState()
			for container := range runner.targets {
				runner.targets[container] = "wrong-target"
			}
			runner.calls = nil
			second := requestFor(state, revisionC())
			configure(&second)
			if _, err := rt.Reconcile(context.Background(), second); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.dockerMutations() != 0 {
				t.Fatalf("wrong target = %v calls=%#v", err, runner.calls)
			}
		})
	}
}

func TestPostgresAlterThenUnknownRemovalResumesWithoutSecondAlter(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	runner.calls = nil
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "rm" && argv[len(argv)-1] == old.Name {
			return errors.New("removal response lost")
		}
		return nil
	}
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new"}}
	if _, err := rt.Reconcile(context.Background(), second); err == nil {
		t.Fatal("first removal unexpectedly succeeded")
	}
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatalf("retry=%v calls=%#v", err, runner.calls)
	}
	alter, remove, run := 0, 0, 0
	for _, call := range runner.calls {
		if len(call) > 2 && call[0] == "exec" && call[1] == "-i" {
			alter++
		}
		if len(call) > 0 && call[0] == "rm" && call[len(call)-1] == old.Name {
			remove++
		}
		if len(call) > 0 && call[0] == "run" {
			run++
		}
	}
	if alter != 1 || remove != 1 || run != 1 || runner.hasSecret("new") {
		t.Fatalf("alter=%d rm=%d run=%d calls=%#v", alter, remove, run, runner.calls)
	}
	calls := len(runner.calls)
	if _, err := rt.Reconcile(context.Background(), second); err != nil || len(runner.calls) != calls {
		t.Fatalf("terminal replay=%v calls=%#v", err, runner.calls)
	}
}

func TestPostgresReplacementRunUnknownResumesWithoutSecondAlterOrRun(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	runner.calls = nil
	runner.failAfter = true
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "run" {
			return errors.New("replacement response lost")
		}
		return nil
	}
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new"}}
	if _, err := rt.Reconcile(context.Background(), second); err == nil {
		t.Fatal("replacement run unexpectedly succeeded")
	}
	runner.fail, runner.failAfter = nil, false
	result, err := rt.Reconcile(context.Background(), second)
	if err != nil || result.Status != hostprotocol.ResultApplied || runner.mutations("run") != 1 || runner.mutations("rm") != 1 || countAlter(runner.calls) != 1 || runner.hasSecret("new") {
		t.Fatalf("retry=%#v %v calls=%#v", result, err, runner.calls)
	}
	if runner.targets[old.Name] != targetLabelFor(findLocalData(mustInventory(t, rt), localDataToken("primary"))) {
		t.Fatalf("replacement target=%q", runner.targets[old.Name])
	}
}

func TestPostgresPendingReplacementRejectsWrongProposedTarget(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{failAfter: true}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "old"}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "run" {
			return errors.New("replacement response lost")
		}
		return nil
	}
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "new"}}
	if _, err := rt.Reconcile(context.Background(), second); err == nil {
		t.Fatal("replacement run unexpectedly succeeded")
	}
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	runner.targets[old.Name] = "wrong-target"
	runner.fail, runner.failAfter = nil, false
	if _, err := rt.Reconcile(context.Background(), second); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.mutations("run") != 1 {
		t.Fatalf("wrong target = %v calls=%#v", err, runner.calls)
	}
}

func TestInspectReportsLiveDriftWithoutPersistingChanges(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
	healthy, err := rt.Inspect(state.Resource)
	if err != nil || healthy.Drifted || !healthy.Ready {
		t.Fatalf("healthy inspect=%#v %v", healthy, err)
	}
	object := findApp(mustInventory(t, rt), appToken("one"))
	delete(runner.inspect, object.Name)
	runner.calls = nil
	drifted, err := rt.Inspect(state.Resource)
	if err != nil || !drifted.Drifted || drifted.Ready || len(drifted.Apps) != 1 || drifted.Apps[0].Ready || runner.dockerMutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("missing app inspect=%#v %v calls=%#v", drifted, err, runner.calls)
	}
	runner.inspect[object.Name] = "wrong-owner"
	if _, err = rt.Inspect(state.Resource); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("wrong ownership=%v", err)
	}
}

func TestInspectPendingAndInventoryStateDisagreementAreDriftOnly(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *Runtime, *State, *inventory){
		"pending": func(t *testing.T, rt *Runtime, state *State, inv *inventory) {
			t.Helper()
			key := reconcileKey(*state, revisionC())
			op, err := rt.Begin(key, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = op.Close(); err != nil {
				t.Fatal(err)
			}
			candidate := appObject(*state, app("one", "new"), revisionC(), "blue")
			inv.Objects = []managedObject{candidate}
			if err = rt.writeInventory(*inv); err != nil {
				t.Fatal(err)
			}
			if err = rt.writeRoute(*inv, candidate); err != nil {
				t.Fatal(err)
			}
			runner := rt.runner.(*recordingRunner)
			delete(runner.inspect, objectName(*state, "app", appToken("one"), "green"))
			runner.inspect[candidate.Name] = ownershipLabelFor(state.Resource, state.Ownership, candidate.Role, candidate.AppToken, candidate.Active)
			runner.targets[candidate.Name] = targetLabelFor(candidate)
		},
		"app revision": func(_ *testing.T, _ *Runtime, _ *State, inv *inventory) {
			inv.Objects[0].Revision = revisionC()
			inv.Objects[0].Env = envName(inv.Objects[0].AppToken, revisionC())
		},
		"app image": func(_ *testing.T, _ *Runtime, _ *State, inv *inventory) { inv.Objects[0].Image = "other-image" },
		"missing app": func(_ *testing.T, _ *Runtime, _ *State, inv *inventory) { inv.Objects = []managedObject{} },
		"extra app": func(_ *testing.T, _ *Runtime, state *State, inv *inventory) {
			inv.Objects = append(inv.Objects, appObject(*state, app("two", "image-two"), state.AppliedRevision, "blue"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); err != nil {
				t.Fatal(err)
			}
			state, _ = rt.readState()
			inv := mustInventory(t, rt)
			mutate(t, rt, &state, &inv)
			if name != "pending" {
				if err := rt.writeInventory(inv); err != nil {
					t.Fatal(err)
				}
				for _, object := range inv.Objects {
					if object.Name != "" {
						runner.inspect[object.Name] = ownershipLabelFor(state.Resource, state.Ownership, object.Role, object.AppToken, object.Active)
						runner.targets[object.Name] = targetLabelFor(object)
					}
				}
			}
			beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
			runner.calls = nil
			observed, err := rt.Inspect(state.Resource)
			if err != nil || !observed.Drifted || observed.Ready || runner.dockerMutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
				t.Fatalf("inspect=%#v err=%v calls=%#v", observed, err, runner.calls)
			}
		})
	}
}

func TestInspectLocalDataInventorySetDisagreementIsDriftOnly(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *Runtime, *inventory){
		"missing data": func(_ *testing.T, _ *Runtime, inv *inventory) {
			for index, object := range inv.Objects {
				if object.Role == "local-data" {
					inv.Objects = append(inv.Objects[:index], inv.Objects[index+1:]...)
					return
				}
			}
		},
		"extra data": func(t *testing.T, rt *Runtime, inv *inventory) {
			extra := localObject(mustState(t, rt), hostcontract.LocalDataServiceTarget{ID: "other", Type: "redis", Port: 6381}, mustState(t, rt).AppliedRevision)
			extra.DataToken, extra.PathToken = token("data", extra.AppToken), token("path", extra.AppToken)
			inv.Objects = append(inv.Objects, extra)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			req := requestFor(state, revisionB(), app("one", "image"))
			req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380}}
			req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "safe"}}
			if _, err := rt.Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			state, _ = rt.readState()
			inv := mustInventory(t, rt)
			mutate(t, rt, &inv)
			if err := rt.writeInventory(inv); err != nil {
				t.Fatal(err)
			}
			for _, object := range inv.Objects {
				if object.Name != "" {
					runner.inspect[object.Name] = ownershipLabelFor(state.Resource, state.Ownership, object.Role, object.AppToken, object.Active)
					runner.targets[object.Name] = targetLabelFor(object)
				}
			}
			beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
			runner.calls = nil
			observed, err := rt.Inspect(state.Resource)
			if err != nil || !observed.Drifted || observed.Ready || runner.dockerMutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
				t.Fatalf("inspect=%#v err=%v calls=%#v", observed, err, runner.calls)
			}
		})
	}
}

func TestAppDrainStopsAfterRouteProbeBeforeOldRemoval(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB(), app("one", "old"))
	first.Target.Apps[0].DrainTimeout = "7s"
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	second := requestFor(state, revisionC(), app("one", "new"))
	second.Target.Apps[0].DrainTimeout = "7s"
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	old := objectName(state, "app", appToken("one"), "green")
	candidate := objectName(state, "app", appToken("one"), "blue")
	probe, stop, remove := callIndex(runner.calls, []string{"exec", candidate}), callIndex(runner.calls, []string{"stop", "--time", "7", old}), callIndex(runner.calls, []string{"rm", "-f", old})
	if !(probe >= 0 && probe < stop && stop < remove) {
		t.Fatalf("drain order calls=%#v", runner.calls)
	}
}

func TestAppDrainStopFailureNeverRemovesOldApp(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "old"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findApp(mustInventory(t, rt), appToken("one"))
	runner.calls = nil
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "stop" {
			return errors.New("stop response lost")
		}
		return nil
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionC(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("stop failure = %v", err)
	}
	if runner.hasCall([]string{"rm", "-f", old.Name}) {
		t.Fatalf("removed after stop failure: %#v", runner.calls)
	}
}

func TestInvalidDrainTimeoutIsRejectedBeforeMutation(t *testing.T) {
	for _, drain := range []string{"-1s", "500ms", "601s", "invalid"} {
		t.Run(drain, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			before := mustFile(t, rt.statePath())
			req := requestFor(state, revisionB(), app("one", "image"))
			req.Target.Apps[0].DrainTimeout = drain
			if _, err := rt.Reconcile(context.Background(), req); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) || len(runner.calls) != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
				t.Fatalf("drain=%q err=%v calls=%#v", drain, err, runner.calls)
			}
		})
	}
}

func TestPendingMaintenanceUnknownRemovalAcceptsExactAbsenceAndCleansEnv(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	data := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "db", Endpoint: "db.local", Port: 5432, Database: "app", TLSServerName: "db.local"}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), appWithData("one", "old", data))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	old := findApp(mustInventory(t, rt), appToken("one"))
	runner.calls = nil
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "rm" {
			delete(runner.inspect, argv[len(argv)-1])
			return errors.New("unknown removal")
		}
		return nil
	}
	req := requestFor(state, revisionC())
	if _, err := rt.Reconcile(context.Background(), req); err == nil {
		t.Fatal("first removal unexpectedly succeeded")
	}
	runner.fail = nil
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.readArtifactBytes(old.Env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env remains: %v", err)
	}
	if !mustInventory(t, rt).hasData(appToken("one")) || runner.mutations("rm") != 1 {
		t.Fatalf("retry trace = %#v", runner.calls)
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revision(), app("one", "different"))); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("different revision = %v", err)
	}
}

func TestMachineMismatchRejectsReconcileAndRetireBeforeCommandsIncludingTerminalReplay(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	before := mustFile(t, rt.statePath())
	if err := os.WriteFile(rt.machinePath, []byte("fedcba9876543210fedcba9876543210\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("reconcile = %v", err)
	}
	key := retireKey(state)
	if _, err := rt.Retire(context.Background(), retireRequest(key, retireApproval(key, state))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("retire = %v", err)
	}
	if len(runner.calls) != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestRollbackReinspectsCandidateOwnershipBeforeRemoval(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	ready := 0
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "exec" {
			ready++
			if ready == 2 {
				return errors.New("post-route")
			}
		}
		return nil
	}
	candidate := objectName(state, "app", appToken("one"), "green")
	routeWriteHook = func() error { runner.inspect[candidate] = "changed"; return nil }
	t.Cleanup(func() { routeWriteHook = nil })
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("rollback = %v", err)
	}
	if runner.mutated(candidate) {
		t.Fatalf("unsafe cleanup: %#v", runner.calls)
	}
}

func TestRollbackRouteRestoreFailureLeavesCandidateForRecovery(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	ready := 0
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "exec" {
			ready++
			if ready == 2 {
				return errors.New("post-route")
			}
		}
		return nil
	}
	routeRestoreHook = func() error { return errors.New("restore unavailable") }
	t.Cleanup(func() { routeRestoreHook = nil })
	candidate := objectName(state, "app", appToken("one"), "green")
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "new"))); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
		t.Fatalf("rollback = %v", err)
	}
	if runner.mutated(candidate) {
		t.Fatalf("candidate removed after uncertain restore: %#v", runner.calls)
	}
	runner.calls = nil
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "new"))); err != nil || runner.mutated(candidate) {
		t.Fatalf("retry after uncertain restore = %v %#v", err, runner.calls)
	}
	inv := mustInventory(t, rt)
	if !rt.routeMatches(inv, findApp(inv, appToken("one"))) {
		t.Fatal("retry did not safely adopt candidate route")
	}
}

func TestPendingRetireUnknownRemovalDeletesRoutesBeforeAllOwnedObjects(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "one"), app("two", "two"))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	trace := []string{}
	routeRemoveHook = func(token string) error { trace = append(trace, "route:"+token); return nil }
	t.Cleanup(func() { routeRemoveHook = nil })
	failed := false
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "rm" {
			trace = append(trace, "rm:"+argv[len(argv)-1])
			if !failed {
				failed = true
				delete(runner.inspect, argv[len(argv)-1])
				return errors.New("unknown removal")
			}
		}
		return nil
	}
	key := retireKey(state)
	approval := retireApproval(key, state)
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err == nil {
		t.Fatal("first removal unexpectedly succeeded")
	}
	runner.fail = func(argv []string) error {
		if len(argv) > 2 && argv[0] == "rm" {
			trace = append(trace, "rm:"+argv[len(argv)-1])
		}
		return nil
	}
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil {
		t.Fatal(err)
	}
	firstRM := -1
	for i, event := range trace {
		if strings.HasPrefix(event, "rm:") {
			firstRM = i
			break
		}
	}
	if firstRM != 2 || len(trace) < 3 {
		t.Fatalf("route-first trace = %#v", trace)
	}
	inv := mustInventory(t, rt)
	for _, o := range inv.Objects {
		if o.Env != "" {
			if _, err := rt.readArtifactBytes(o.Env); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("env remains: %v", err)
			}
		}
	}
	calls := len(runner.calls)
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil || len(runner.calls) != calls {
		t.Fatalf("terminal replay = %v", err)
	}
}

func TestRetirePreservesDataACMEAndRemovesAllSecretArtifactsInOrder(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	proxy := &hostcontract.ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example.test"}
	request := requestFor(state, revisionB(), app("one", "one"), app("two", "two"))
	request.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}, {ID: "cache", Type: "redis", Port: 6380, Persistence: true}}
	request.Target.ReverseProxy = proxy
	request.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "POSTGRES_CANARY"}, "cache": {AdminPassword: "REDIS_CANARY"}}
	request.Secrets.ReverseProxy = &hostcontract.ReverseProxySecrets{DNSChallengeToken: "DNS_CANARY"}
	if _, err := rt.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	inv := mustInventory(t, rt)
	beforeInventory := mustArtifact(t, rt, artifactInventory)
	pg, cache, proxyObject := findLocalData(inv, localDataToken("primary")), findLocalData(inv, localDataToken("cache")), findProxy(inv)
	if err := os.WriteFile(filepath.Join(rt.dataPath(pg.DataToken), "postgres-sentinel"), []byte("postgres"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rt.dataPath(cache.DataToken), "redis-sentinel"), []byte("redis"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rt.proxyACMEPath(), []byte("acme-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := objectName(state, "app", "unowned", "marker")
	runner.inspect[marker] = "unowned"
	trace, failed := []string{}, false
	routeRemoveHook = func(token string) error { trace = append(trace, "route:"+token); return nil }
	t.Cleanup(func() { routeRemoveHook = nil })
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "rm" {
			name := argv[len(argv)-1]
			trace = append(trace, "rm:"+name)
			if !failed {
				failed = true
				delete(runner.inspect, name)
				delete(runner.targets, name)
				return errors.New("removal response lost")
			}
		}
		if len(argv) > 1 && argv[0] == "network" && argv[1] == "rm" {
			trace = append(trace, "network")
		}
		return nil
	}
	key, approval := retireKey(state), retireApproval(retireKey(state), state)
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err == nil {
		t.Fatal("first retirement unexpectedly succeeded")
	}
	runner.fail = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "rm" {
			trace = append(trace, "rm:"+argv[len(argv)-1])
		}
		if len(argv) > 1 && argv[0] == "network" && argv[1] == "rm" {
			trace = append(trace, "network")
		}
		return nil
	}
	if result, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil || result.Status != hostprotocol.ResultRetired {
		t.Fatalf("retire = %#v %v", result, err)
	}
	firstRM, network := indexPrefix(trace, "rm:"), indexEvent(trace, "network")
	wantTrace := []string{"route:" + appToken("one"), "route:" + appToken("two"), "rm:" + objectName(state, "app", appToken("one"), "green"), "rm:" + objectName(state, "app", appToken("two"), "green"), "rm:" + proxyObject.Name, "rm:" + pg.Name, "rm:" + cache.Name, "network"}
	if firstRM != 2 || network != len(trace)-1 || !sameStrings(trace, wantTrace) || runner.mutations("rm") != 5 || runner.inspect[marker] != "unowned" || !eachRemovalAtMostOnce(runner.calls) {
		t.Fatalf("retire order=%#v calls=%#v", trace, runner.calls)
	}
	for _, sentinel := range []string{filepath.Join(rt.dataPath(pg.DataToken), "postgres-sentinel"), filepath.Join(rt.dataPath(cache.DataToken), "redis-sentinel"), rt.proxyACMEPath()} {
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("preserved sentinel %q: %v", sentinel, err)
		}
	}
	for _, object := range inv.Objects {
		for _, artifact := range []string{object.Env, object.Config} {
			if artifact != "" {
				if _, err := rt.readArtifactBytes(artifact); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("artifact remains %q: %v", artifact, err)
				}
			}
		}
	}
	if got, err := rt.readState(); err != nil || got.Retirement == nil || !got.Retirement.PreserveData || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
		t.Fatalf("retirement state = %#v %v", got, err)
	}
	calls := len(runner.calls)
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil || len(runner.calls) != calls {
		t.Fatalf("terminal replay = %v calls=%#v", err, runner.calls)
	}
}

func TestTerminalRetireReplayRejectsChangedMachineBeforeStoredResult(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	key := retireKey(state)
	approval := retireApproval(key, state)
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rt.machinePath, []byte("fedcba9876543210fedcba9876543210\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || len(runner.calls) != 0 {
		t.Fatalf("terminal mismatch = %v %#v", err, runner.calls)
	}
}

func mustInventory(t *testing.T, rt *Runtime) inventory {
	t.Helper()
	inv, err := rt.readInventory()
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

type recordingRunner struct {
	calls                 [][]string
	stdinDigests          []string
	inspect               map[string]string
	targets               map[string]string
	networks              map[string]string
	postgresCredentials   map[string]string
	postgresDataTokens    map[string]string
	postgresEnvPaths      map[string]string
	disablePostgresAlter  bool
	wrongPostgresPassword bool
	fail                  func([]string) error
	failAfter             bool
	event                 func([]string)
}

func (r *recordingRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if len(stdin) > 0 {
		r.stdinDigests = append(r.stdinDigests, token(string(stdin)))
	}
	if r.event != nil {
		r.event(argv)
	}
	var failure error
	if r.fail != nil {
		if err := r.fail(argv); err != nil {
			if !r.failAfter {
				return nil, err
			}
			failure = err
		}
	}
	if len(argv) > 1 && argv[0] == "container" && argv[1] == "ls" {
		name := ""
		const prefix = "name=^/"
		for i := range argv {
			if argv[i] == "--filter" && i+1 < len(argv) && strings.HasPrefix(argv[i+1], prefix) && strings.HasSuffix(argv[i+1], "$") {
				name = strings.TrimSuffix(strings.TrimPrefix(argv[i+1], prefix), "$")
			}
		}
		value, ok := r.inspect[name]
		if !ok {
			return nil, nil
		}
		return []byte(name + "\t" + value + "\t" + r.targets[name] + "\n"), nil
	}
	if len(argv) > 1 && argv[0] == "network" && argv[1] == "ls" {
		name := ""
		for i := range argv {
			if argv[i] == "--filter" && i+1 < len(argv) {
				name = strings.TrimSuffix(strings.TrimPrefix(argv[i+1], "name=^"), "$")
			}
		}
		if label, ok := r.networks[name]; ok {
			return []byte(name + "\t" + ownershipLabelFor(resource(), ownership(), "network", "", "") + "\t" + label + "\n"), nil
		}
		return nil, nil
	}
	if len(argv) > 1 && argv[0] == "network" && argv[1] == "create" {
		if r.networks == nil {
			r.networks = map[string]string{}
		}
		for i := range argv {
			if argv[i] == "--label" && i+1 < len(argv) && strings.HasPrefix(argv[i+1], "sub2api.host.network=") {
				r.networks[argv[len(argv)-1]] = strings.TrimPrefix(argv[i+1], "sub2api.host.network=")
			}
		}
	}
	if len(argv) > 1 && argv[0] == "network" && argv[1] == "rm" && r.networks != nil {
		delete(r.networks, argv[len(argv)-1])
	}
	if len(argv) == 10 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-v" && argv[5] == "ON_ERROR_STOP=1" && argv[6] == "-U" && argv[7] == "sub2api" && argv[8] == "-d" && argv[9] == "postgres" && len(stdin) > 0 {
		password, ok := postgresPasswordFromSQL(string(stdin))
		if !ok {
			return nil, errors.New("invalid postgres sql")
		}
		if !r.disablePostgresAlter {
			if r.postgresCredentials == nil {
				r.postgresCredentials = map[string]string{}
			}
			r.postgresCredentials[r.postgresDataTokens[argv[2]]] = password
		}
	}
	if len(argv) > 2 && argv[0] == "exec" && argv[1] != "-i" && argv[2] == "psql" {
		if r.postgresEnvPaths != nil {
			env, err := os.ReadFile(r.postgresEnvPaths[argv[1]])
			password := envValue(string(env), "PGPASSWORD")
			if r.wrongPostgresPassword {
				password = "wrong-password"
			}
			if err != nil || r.postgresCredentials[r.postgresDataTokens[argv[1]]] != password {
				return nil, errors.New("postgres credential mismatch")
			}
		}
	}
	if len(argv) > 0 && argv[0] == "run" {
		if r.inspect == nil {
			r.inspect = map[string]string{}
		}
		if r.targets == nil {
			r.targets = map[string]string{}
		}
		var label, target, name string
		for i := range argv {
			if argv[i] == "--label" && i+1 < len(argv) {
				if strings.HasPrefix(argv[i+1], "sub2api.host.target=") {
					target = strings.TrimPrefix(argv[i+1], "sub2api.host.target=")
				} else if strings.HasPrefix(argv[i+1], "sub2api.host=") {
					label = strings.TrimPrefix(argv[i+1], "sub2api.host=")
				}
			}
			if argv[i] == "--name" && i+1 < len(argv) {
				name = argv[i+1]
			}
		}
		r.inspect[name] = label
		r.targets[name] = target
		if strings.Contains(strings.Join(argv, "\x00"), "postgres:18-alpine") {
			if r.postgresCredentials == nil {
				r.postgresCredentials = map[string]string{}
				r.postgresDataTokens = map[string]string{}
				r.postgresEnvPaths = map[string]string{}
			}
			for i := range argv {
				if argv[i] == "--env-file" && i+1 < len(argv) {
					r.postgresEnvPaths[name] = argv[i+1]
				}
				if argv[i] == "-v" && i+1 < len(argv) {
					parts := strings.Split(argv[i+1], ":")
					if len(parts) == 2 && parts[1] == "/var/lib/postgresql/data" {
						r.postgresDataTokens[name] = filepath.Base(parts[0])
					}
				}
			}
			env, err := os.ReadFile(r.postgresEnvPaths[name])
			if err != nil {
				return nil, err
			}
			token := r.postgresDataTokens[name]
			if _, ok := r.postgresCredentials[token]; !ok {
				r.postgresCredentials[token] = envValue(string(env), "POSTGRES_PASSWORD")
			}
		}
	}
	if len(argv) > 1 && argv[0] == "rm" && r.inspect != nil {
		if r.postgresDataTokens != nil {
			delete(r.postgresDataTokens, argv[len(argv)-1])
			delete(r.postgresEnvPaths, argv[len(argv)-1])
		}
		delete(r.inspect, argv[len(argv)-1])
		delete(r.targets, argv[len(argv)-1])
	}
	return nil, failure
}
func envValue(env, key string) string {
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}
func postgresPasswordFromSQL(sql string) (string, bool) {
	const prefix, suffix = "ALTER ROLE sub2api PASSWORD '", "';"
	if !strings.HasPrefix(sql, prefix) || !strings.HasSuffix(sql, suffix) {
		return "", false
	}
	return strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(sql, prefix), suffix), "''", "'"), true
}
func containsValue(values map[string]string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func (r *recordingRunner) mutations(kinds ...string) int {
	n := 0
	for _, c := range r.calls {
		if len(c) == 0 {
			continue
		}
		if len(kinds) == 0 && (c[0] == "run" || c[0] == "rm") {
			n++
			continue
		}
		for _, k := range kinds {
			if c[0] == k {
				n++
			}
		}
	}
	return n
}
func (r *recordingRunner) dockerMutations() int {
	n := 0
	for _, call := range r.calls {
		if len(call) > 0 && (call[0] == "run" || call[0] == "rm" || len(call) > 1 && call[0] == "network" && call[1] == "create") {
			n++
		}
	}
	return n
}
func (r *recordingRunner) hasSecret(secret string) bool {
	for _, c := range r.calls {
		for _, x := range c {
			if bytes.Contains([]byte(x), []byte(secret)) {
				return true
			}
		}
	}
	return false
}
func (r *recordingRunner) onlyMutatesFor(token string) bool {
	for _, c := range r.calls {
		if len(c) > 1 && (c[0] == "rm" || c[0] == "stop") && !bytes.Contains([]byte(c[len(c)-1]), []byte(token)) {
			return false
		}
	}
	return true
}
func (r *recordingRunner) mutated(name string) bool {
	for _, c := range r.calls {
		if len(c) > 2 && c[0] == "rm" && c[len(c)-1] == name {
			return true
		}
	}
	return false
}
func (r *recordingRunner) hasCall(want []string) bool {
	for _, call := range r.calls {
		if strings.Join(call, "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}
func (r *recordingRunner) hasCallPrefix(want []string) bool {
	return callIndex(r.calls, want) >= 0
}
func hasLabel(call []string, key string) bool {
	for index := 0; index+1 < len(call); index++ {
		if call[index] == "--label" && strings.HasPrefix(call[index+1], key+"=") {
			return true
		}
	}
	return false
}
func (r *recordingRunner) anyArg(prefix, value string) bool {
	for _, call := range r.calls {
		for i := 0; i+1 < len(call); i++ {
			if call[i] == prefix && call[i+1] == value {
				return true
			}
		}
	}
	return false
}
func containsPair(values []string, first, second string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == first && values[i+1] == second {
			return true
		}
	}
	return false
}
func indexEvent(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}
func indexPrefix(events []string, prefix string) int {
	for i, event := range events {
		if strings.HasPrefix(event, prefix) {
			return i
		}
	}
	return -1
}
func eachRemovalAtMostOnce(calls [][]string) bool {
	seen := map[string]bool{}
	for _, call := range calls {
		if len(call) > 2 && call[0] == "rm" {
			name := call[len(call)-1]
			if seen[name] {
				return false
			}
			seen[name] = true
		}
	}
	return true
}
func countAlter(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if len(call) > 1 && call[0] == "exec" && call[1] == "-i" {
			count++
		}
	}
	return count
}
func callIndex(calls [][]string, prefix []string) int {
	for index, call := range calls {
		if len(call) < len(prefix) {
			continue
		}
		matches := true
		for item := range prefix {
			if call[item] != prefix[item] {
				matches = false
				break
			}
		}
		if matches {
			return index
		}
	}
	return -1
}
func sameStrings(got, want []string) bool {
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
func (r *recordingRunner) beforeRun(first, second string) bool {
	a, b := -1, -1
	for i, call := range r.calls {
		if len(call) > 0 && call[0] == "run" {
			for _, item := range call {
				if item == first {
					a = i
				}
				if item == second {
					b = i
				}
			}
		}
	}
	return a >= 0 && b >= 0 && a < b
}

func app(id, image string) hostcontract.AppTarget {
	return hostcontract.AppTarget{ID: id, Image: image, Hostname: id + ".example", ReadinessPath: "/health"}
}
func appWithData(id, image string, data hostcontract.DataIdentity) hostcontract.AppTarget {
	a := app(id, image)
	a.DataLinks = []hostcontract.DataLink{{Name: "main", Identity: data}}
	return a
}
func appWithLinks(id, image string, firstName string, first hostcontract.DataIdentity, secondName string, second hostcontract.DataIdentity) hostcontract.AppTarget {
	a := app(id, image)
	a.DataLinks = []hostcontract.DataLink{{Name: firstName, Identity: first}, {Name: secondName, Identity: second}}
	return a
}
func dataIdentity(id string) hostcontract.DataIdentity {
	return hostcontract.DataIdentity{Kind: "postgres", ProviderID: id, Endpoint: id + ".db", Port: 5432, Database: "app", TLSServerName: id + ".db"}
}
func requestFor(state State, revision string, apps ...hostcontract.AppTarget) hostprotocol.Request {
	secrets := make(map[string]hostcontract.AppSecrets, len(apps))
	for _, app := range apps {
		secrets[app.ID] = hostcontract.AppSecrets{JWTSecret: "CANARY"}
	}
	return hostprotocol.Request{Action: hostcontract.ActionReconcile, Resource: state.Resource, TargetRevision: revision, PriorAppliedRevision: state.AppliedRevision, Target: &hostcontract.Target{ReleaseArtifact: "release", Apps: apps}, Secrets: &hostcontract.Secrets{Apps: secrets}}
}
func retireRequest(key hostcontract.OperationKey, approval hostcontract.ApprovalSubject) hostprotocol.Request {
	return hostprotocol.Request{Action: hostcontract.ActionRetirePreserveData, Resource: key.Resource, TargetRevision: key.TargetRevision, PriorAppliedRevision: key.PriorAppliedRevision, Approval: &approval}
}
func mustArtifact(t *testing.T, rt *Runtime, name string) []byte {
	t.Helper()
	b, e := rt.readArtifactBytes(name)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func mustRouteArtifact(t *testing.T, rt *Runtime, name string) []byte {
	t.Helper()
	return mustArtifact(t, rt, name)
}
func mustFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func mustRoute(t *testing.T, rt *Runtime, state State, id string) []byte {
	t.Helper()
	inv, err := rt.readInventory()
	if err != nil {
		t.Fatal(err)
	}
	app := findApp(inv, appToken(id))
	b, err := routeBytesFor(inv, app)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var _ = errors.New
