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
	if !runner.hasCall([]string{"exec", cache.Name, "redis-cli", "--raw", "-h", "127.0.0.1", "-p", "6380", "ping"}) {
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
	rt.nft.(*recordingNFTRunner).calls = nil
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
	if err := rt.runLocal(context.Background(), state, local, hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380}); err != nil {
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

func TestAppEnvironmentUsesCanonicalDataLinksAndReservesDeploymentKeys(t *testing.T) {
	postgres := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "pg", Endpoint: "10.0.0.8", Port: 5432, Database: "sub2api", TLSMode: "require"}
	redis := hostcontract.DataIdentity{Kind: "redis", ProviderID: "redis", Endpoint: "cache", Port: 6380, Database: "2", TLSMode: "require"}
	a := app("one", "image")
	a.InitialAdminEmail = "admin@example.test"
	a.RuntimeSettings = map[string]string{"Z_SETTING": "z"}
	a.DataLinks = []hostcontract.DataLink{{Name: "postgres", Identity: postgres}, {Name: "redis", Identity: redis}}
	env := string(envBytes(a, hostcontract.AppSecrets{RuntimeEnvironment: map[string]string{"A_SECRET": "a"}, Postgres: &hostcontract.DataCredentials{Username: "pguser", Password: "PG_SECRET"}, Redis: &hostcontract.DataCredentials{Username: "redisuser", Password: "REDIS_SECRET"}}))
	if !strings.HasPrefix(env, "A_SECRET=a\nZ_SETTING=z\nADMIN_EMAIL=admin@example.test\n") {
		t.Fatalf("generic and deployment-owned environment order = %q", env)
	}
	for _, line := range []string{"ADMIN_EMAIL=admin@example.test", "DATABASE_HOST=10.0.0.8", "DATABASE_PORT=5432", "DATABASE_USER=pguser", "DATABASE_PASSWORD=PG_SECRET", "DATABASE_DBNAME=sub2api", "DATABASE_SSLMODE=require", "REDIS_HOST=cache", "REDIS_PORT=6380", "REDIS_USERNAME=redisuser", "REDIS_PASSWORD=REDIS_SECRET", "REDIS_DB=2", "REDIS_ENABLE_TLS=true"} {
		if !strings.Contains(env, line+"\n") {
			t.Fatalf("missing %q in %q", line, env)
		}
	}
	bad := a
	bad.RuntimeSettings = map[string]string{"DATABASE_HOST": "override"}
	if safeEnvironment(bad, hostcontract.AppSecrets{}) {
		t.Fatal("deployment-owned data key accepted")
	}
}

func TestDeploymentOwnedEnvironmentKeysAreReservedInSettingsAndSecrets(t *testing.T) {
	keys := []string{"ADMIN_EMAIL", "INITIAL_ADMIN_PASSWORD", "JWT_SECRET", "TOTP_ENCRYPTION_KEY", "ADMIN_API_KEY", "DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_DBNAME", "DATABASE_SSLMODE", "REDIS_HOST", "REDIS_PORT", "REDIS_USERNAME", "REDIS_PASSWORD", "REDIS_DB", "REDIS_ENABLE_TLS"}
	for _, source := range []string{"settings", "secrets"} {
		for _, key := range keys {
			t.Run(source+"/"+key, func(t *testing.T) {
				a := app("one", "image")
				s := hostcontract.AppSecrets{}
				if source == "settings" {
					a.RuntimeSettings = map[string]string{key: "override"}
				} else {
					s.RuntimeEnvironment = map[string]string{key: "override"}
				}
				if safeEnvironment(a, s) {
					t.Fatalf("accepted %s in %s", key, source)
				}
			})
		}
	}
}

func TestRuntimeAdmissionRejectsMissingDuplicateAndUnsafeDataLinksBeforeBegin(t *testing.T) {
	postgres := hostcontract.DataIdentity{Kind: "postgres", ProviderID: "pg", Endpoint: "10.0.0.8", Port: 5432, Database: "app", TLSMode: "require"}
	redis := hostcontract.DataIdentity{Kind: "redis", ProviderID: "cache", Endpoint: "10.0.0.9", Port: 6379, Database: "0", TLSMode: "disable"}
	for name, mutate := range map[string]func(*hostprotocol.Request){
		"missing postgres credential": func(q *hostprotocol.Request) {
			q.Target.Apps[0].DataLinks = []hostcontract.DataLink{{Name: "pg", Identity: postgres}}
		},
		"duplicate postgres link": func(q *hostprotocol.Request) {
			q.Target.Apps[0].DataLinks = []hostcontract.DataLink{{Name: "pg", Identity: postgres}, {Name: "pg2", Identity: postgres}}
			q.Secrets.Apps["one"] = hostcontract.AppSecrets{Postgres: &hostcontract.DataCredentials{Username: "app_user", Password: "safe"}}
		},
		"extra redis credential": func(q *hostprotocol.Request) {
			q.Secrets.Apps["one"] = hostcontract.AppSecrets{Redis: &hostcontract.DataCredentials{Username: "app_user", Password: "safe"}}
		},
		"unsafe environment": func(q *hostprotocol.Request) {
			q.Target.Apps[0].RuntimeSettings = map[string]string{"SAFE": "ok\nINJECTED=yes"}
		},
		"reserved email setting": func(q *hostprotocol.Request) {
			q.Target.Apps[0].RuntimeSettings = map[string]string{"ADMIN_EMAIL": "attacker@example.test"}
		},
		"reserved email secret": func(q *hostprotocol.Request) {
			q.Secrets.Apps["one"] = hostcontract.AppSecrets{JWTSecret: "BOOTSTRAP_SECRET_CANARY", RuntimeEnvironment: map[string]string{"ADMIN_EMAIL": "attacker@example.test"}}
		},
		"control data endpoint": func(q *hostprotocol.Request) {
			q.Target.Apps[0].DataLinks = []hostcontract.DataLink{{Name: "pg", Identity: hostcontract.DataIdentity{Kind: "postgres", ProviderID: "pg", Endpoint: "10.0.0.8\x1f", Port: 5432, Database: "app", TLSMode: "require"}}}
			q.Secrets.Apps["one"] = hostcontract.AppSecrets{Postgres: &hostcontract.DataCredentials{Username: "app_user", Password: "safe"}}
		},
		"unsupported link": func(q *hostprotocol.Request) {
			q.Target.Apps[0].DataLinks = []hostcontract.DataLink{{Name: "other", Identity: hostcontract.DataIdentity{Kind: "mysql"}}}
		},
		"redis link missing credential": func(q *hostprotocol.Request) {
			q.Target.Apps[0].DataLinks = []hostcontract.DataLink{{Name: "redis", Identity: redis}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			before := mustFile(t, rt.statePath())
			req := requestFor(state, revisionB(), app("one", "image"))
			mutate(&req)
			if _, err := rt.Reconcile(t.Context(), req); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) || len(runner.calls) != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) {
				t.Fatalf("err=%v calls=%#v", err, runner.calls)
			}
		})
	}
}

func TestAppRunUsesStableNetworkAlias(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	if _, err := rt.Reconcile(context.Background(), requestFor(state, revisionB(), app("one", "image"))); err != nil {
		t.Fatal(err)
	}
	if !runner.anyArg("--network-alias", "one") {
		t.Fatalf("app run trace=%#v", runner.calls)
	}
}

func TestLocalDataRunBindsExactlyAndUsesStableAlias(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	target := hostcontract.LocalDataServiceTarget{ID: "primary", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}, {Address: "2001:db8::8", AllowedSources: []string{"2001:db8::9"}}}}
	o := localObject(state, target, revisionB())
	o.DataToken = token("data", o.AppToken)
	if err := rt.writeLocalSecrets(context.Background(), state, o, target, hostcontract.LocalDataServiceSecrets{AdminPassword: "safe"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.runLocal(context.Background(), state, o, target); err != nil {
		t.Fatal(err)
	}
	call := runner.calls[len(runner.calls)-1]
	if !containsPair(call, "--network-alias", "primary") || !containsPair(call, "-p", "10.0.0.8:5432:5432/tcp") || !containsPair(call, "-p", "[2001:db8::8]:5432:5432/tcp") || containsPair(call, "-p", "5432:5432") || containsPair(call, "-p", "0.0.0.0:5432:5432") {
		t.Fatalf("docker argv=%#v", call)
	}
	runner.calls = nil
	local := hostcontract.LocalDataServiceTarget{ID: "local", Type: "redis", Port: 6380}
	o = localObject(state, local, revisionB())
	o.DataToken = token("data", o.AppToken)
	if err := rt.writeLocalSecrets(context.Background(), state, o, local, hostcontract.LocalDataServiceSecrets{AdminPassword: "safe"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.runLocal(context.Background(), state, o, local); err != nil {
		t.Fatal(err)
	}
	if runner.anyArg("-p", "6380:6380") || !containsPair(runner.calls[0], "--network-alias", "local") {
		t.Fatalf("local docker argv=%#v", runner.calls[0])
	}
}

func TestNftTransactionOwnsDeterministicPreDNATAllowlistWithoutSecrets(t *testing.T) {
	_, state := initialized(t)
	targets := []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.10", "10.0.0.9"}}, {Address: "2001:db8::8", AllowedSources: []string{"2001:db8::10"}}}}}
	got, err := nftTransaction(state, targets)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"table inet " + nftTableName(state), nftOwnershipComment(state), "hook prerouting priority -110", "ip saddr 10.0.0.9 ip daddr 10.0.0.8 tcp dport 5432 accept", "ip daddr 10.0.0.8 tcp dport 5432 drop", "ip6 saddr 2001:db8::10 ip6 daddr 2001:db8::8 tcp dport 5432 accept"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "SECRET") {
		t.Fatalf("secret in nft transaction: %s", text)
	}
	accept := strings.Index(text, "ip saddr 10.0.0.10 ip daddr 10.0.0.8 tcp dport 5432 accept")
	drop := strings.Index(text, "ip daddr 10.0.0.8 tcp dport 5432 drop")
	if accept < 0 || drop < accept || strings.Contains(text[accept:drop], "ip6 ") {
		t.Fatalf("accept/drop grouping lost: %s", text)
	}
}

func TestDockerPortBindingsRequireExactCanonicalTCPPublications(t *testing.T) {
	if !exactDockerPublications([]byte(`{"HostConfig":{"PortBindings":{}}}`), 5432, nil) {
		t.Fatal("unbound local-only service rejected")
	}
	want := []hostcontract.LocalDataBinding{{Address: "10.0.0.8"}, {Address: "2001:db8::8"}}
	good := []byte(`{"HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"10.0.0.8","HostPort":"5432"},{"HostIp":"2001:db8::8","HostPort":"5432"}]}}}`)
	if !exactDockerPublications(good, 5432, want) {
		t.Fatal("exact IPv4/IPv6 publications rejected")
	}
	for _, bad := range [][]byte{
		[]byte(`{"HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"","HostPort":"5432"}]}}}`),
		[]byte(`{"HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"5432"}]}}}`),
		[]byte(`{"HostConfig":{"PortBindings":{"5432/udp":[{"HostIp":"10.0.0.8","HostPort":"5432"}]}}}`),
		[]byte(`{"HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"10.0.0.8","HostPort":"5432"}],"6379/tcp":[{"HostIp":"10.0.0.8","HostPort":"6379"}]}}}`),
		[]byte(`{"HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"10.0.0.8","HostPort":"5433"}]}}}`),
	} {
		if exactDockerPublications(bad, 5432, want) {
			t.Fatalf("unsafe publications accepted: %s", bad)
		}
	}
}

func TestNftUnionDeduplicatesOldAndNewSourcesWithoutLooseningPolicyValidation(t *testing.T) {
	_, state := initialized(t)
	old := localObject(state, hostcontract.LocalDataServiceTarget{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}, revisionB())
	inv := inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: revisionB(), Objects: []managedObject{old}}
	desired := []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9", "10.0.0.10"}}}}}
	union := nftUnion(inv, desired)
	if len(union) != 1 || len(union[0].Bindings) != 1 || !sameStringSlice(union[0].Bindings[0].AllowedSources, []string{"10.0.0.10", "10.0.0.9"}) {
		t.Fatalf("union=%#v", union)
	}
	if _, err := nftPolicyForTargets(union); err != nil {
		t.Fatalf("deduplicated union rejected: %v", err)
	}
	duplicate := desired
	duplicate[0].Bindings[0].AllowedSources = []string{"10.0.0.10", "10.0.0.10"}
	if _, err := nftPolicyForTargets(duplicate); err == nil {
		t.Fatal("malformed duplicate target sources accepted")
	}
}

func TestBindChangeKeepsUnionPolicyUntilDockerPublicationIsReplaced(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"db": {AdminPassword: "safe"}}
	rt.nft.(*recordingNFTRunner).nextTable = nftJSONFixture(state, nftPolicyFor(state, first.Target.DataServices))
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	trace := []string{}
	runner.calls = nil
	runner.event = func(argv []string) {
		if len(argv) > 0 && (argv[0] == "rm" || argv[0] == "run") {
			trace = append(trace, argv[0])
		}
	}
	nft := rt.nft.(*recordingNFTRunner)
	nft.calls = nil
	nft.event = func(argv []string) {
		if len(argv) == 2 && argv[0] == "-f" {
			trace = append(trace, "nft")
		}
	}
	second := requestFor(state, revisionC())
	second.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.10", AllowedSources: []string{"10.0.0.9"}}}}}
	second.Secrets.LocalDataServices = first.Secrets.LocalDataServices
	inv := mustInventory(t, rt)
	nft.nextTables = [][]byte{nftJSONFixture(state, nftPolicyFor(state, nftUnion(inv, second.Target.DataServices))), nftJSONFixture(state, nftPolicyFor(state, second.Target.DataServices))}
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatalf("bind change=%v trace=%#v", err, trace)
	}
	if !sameStrings(trace, []string{"nft", "rm", "run", "nft"}) {
		t.Fatalf("expected U -> docker -> N, trace=%#v", trace)
	}
}

func TestNftJSONClassifiesExactForeignOldAndMalformedStates(t *testing.T) {
	_, state := initialized(t)
	policy := nftPolicyFor(state, []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}})
	for name, fixture := range map[string][]byte{
		"exact":     nftJSONFixture(state, policy),
		"old":       nftJSONFixture(state, nftPolicyFor(state, []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.10"}}}}})),
		"foreign":   []byte(`{"nftables":[{"table":{"family":"inet","name":"` + nftTableName(state) + `","comment":"foreign"}}]}`),
		"malformed": []byte(`{"nftables":[{"table":{"family":"inet","name":"` + nftTableName(state) + `","comment":"` + nftOwnershipComment(state) + `"}},{"chain":{"family":"inet","table":"` + nftTableName(state) + `","name":"prerouting","type":"filter","hook":"input","prio":-110,"policy":"accept","comment":"x"}}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			got := classifyNftJSON(fixture, state, policy)
			switch name {
			case "exact":
				if got != nftExact {
					t.Fatalf("state=%v", got)
				}
			case "old":
				if got != nftOld {
					t.Fatalf("state=%v", got)
				}
			case "foreign":
				if got != nftForeign {
					t.Fatalf("state=%v", got)
				}
			default:
				if got != nftMalformed {
					t.Fatalf("state=%v", got)
				}
			}
		})
	}
}

func TestNftJSONAcceptsOnlyTerminalVerdictStatements(t *testing.T) {
	_, state := initialized(t)
	policy := nftPolicyFor(state, []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}})
	fixture := nftJSONFixture(state, policy)
	if classifyNftJSON(fixture, state, policy) != nftExact {
		t.Fatal("real terminal verdict fixture was rejected")
	}
	private := bytes.Replace(fixture, []byte(`"accept":null`), []byte(`"verdict":{"jump":"accept"}`), 1)
	if classifyNftJSON(private, state, policy) != nftMalformed {
		t.Fatal("private jump verdict shape was accepted")
	}
}

func TestNftJSONRejectsExtraAndOutOfOrderRules(t *testing.T) {
	_, state := initialized(t)
	policy := nftPolicyFor(state, []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}})
	fixture := nftJSONFixture(state, policy)
	fixture = bytes.Replace(fixture, []byte(`]}`), []byte(`]},{"counter":{"packets":1,"bytes":1}}]}`), 1)
	if got := classifyNftJSON(fixture, state, policy); got != nftMalformed {
		t.Fatalf("extra expression=%v", got)
	}
	badOrder := nftJSONFixture(state, policy)
	badOrder = bytes.Replace(badOrder, []byte(`"saddr"`), []byte(`"daddr"`), 1)
	if got := classifyNftJSON(badOrder, state, policy); got != nftMalformed {
		t.Fatalf("rule order=%v", got)
	}
}

func TestNftExactDesiredIsNoopAndResponseLossRelists(t *testing.T) {
	rt, state := initialized(t)
	nft := rt.nft.(*recordingNFTRunner)
	targets := []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}}
	policy := nftPolicyFor(state, targets)
	nft.table = nftJSONFixture(state, policy)
	if err := rt.reconcileNft(context.Background(), state, targets); err != nil || nft.mutations != 0 {
		t.Fatalf("exact noop=%v mutations=%d", err, nft.mutations)
	}
	nft.table = nil
	nft.nextTable = nftJSONFixture(state, policy)
	nft.failAfterApply = true
	if err := rt.reconcileNft(context.Background(), state, targets); err != nil || nft.mutations != 1 {
		t.Fatalf("response loss=%v mutations=%d", err, nft.mutations)
	}
	if err := rt.reconcileNft(context.Background(), state, targets); err != nil || nft.mutations != 1 {
		t.Fatalf("later retry duplicated apply=%v mutations=%d", err, nft.mutations)
	}
}

func TestNftResponseLossAcrossRuntimeRestartDoesNotRepeatApplyOrDelete(t *testing.T) {
	rt, state := initialized(t)
	targets := []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}}
	nft := rt.nft.(*recordingNFTRunner)
	nft.nextTable = nftJSONFixture(state, nftPolicyFor(state, targets))
	nft.failAfterApply = true
	if err := rt.reconcileNft(t.Context(), state, targets); err != nil || nft.mutations != 1 {
		t.Fatalf("apply=%v mutations=%d", err, nft.mutations)
	}
	restarted := New(rt.root, rt.machinePath)
	restarted.nft = nft
	if err := restarted.reconcileNft(t.Context(), state, targets); err != nil || nft.mutations != 1 {
		t.Fatalf("apply retry=%v mutations=%d", err, nft.mutations)
	}
	nft.failAfterDelete = true
	if err := restarted.removeNft(t.Context(), state, false); err != nil || nft.mutations != 2 {
		t.Fatalf("delete=%v mutations=%d", err, nft.mutations)
	}
	restarted = New(rt.root, rt.machinePath)
	restarted.nft = nft
	if err := restarted.removeNft(t.Context(), state, true); err != nil || nft.mutations != 2 {
		t.Fatalf("delete retry=%v mutations=%d", err, nft.mutations)
	}
}

func TestInventoryAcceptsHyphenatedClientAppID(t *testing.T) {
	_, state := initialized(t)
	o := localObject(state, hostcontract.LocalDataServiceTarget{ID: "db", Type: "postgres", Port: 5432, Clients: []hostcontract.LocalDataClient{{AppID: "api-blue", Username: "api_blue", Database: "api_db"}}}, revisionB())
	o.DataToken, o.PathToken = token("data", o.AppToken), token("path", o.AppToken)
	if err := validateInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: revisionB(), Objects: []managedObject{o}}); err != nil {
		t.Fatalf("hyphenated app ID rejected: %v", err)
	}
}

func TestInventoryAcceptsHyphenatedServiceIDAndSub2apiClient(t *testing.T) {
	_, state := initialized(t)
	o := localObject(state, hostcontract.LocalDataServiceTarget{ID: "primary-db", Type: "postgres", Port: 5432, Clients: []hostcontract.LocalDataClient{{AppID: "api-blue", Username: "sub2api", Database: "app_db"}}}, revisionB())
	o.DataToken, o.PathToken = token("data", o.AppToken), token("path", o.AppToken)
	if err := validateInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: revisionB(), Objects: []managedObject{o}}); err != nil {
		t.Fatalf("valid service/client rejected: %v", err)
	}
	if !validPostgresClient(o.Clients[0]) {
		t.Fatal("sub2api ordinary app client rejected")
	}
}

func TestNftApplyResponseLossAcceptsOnlyExactDesiredState(t *testing.T) {
	rt, state := initialized(t)
	nft := rt.nft.(*recordingNFTRunner)
	targets := []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}}
	nft.nextTable = nftJSONFixture(state, nftPolicyFor(state, targets))
	nft.failAfterApply = true
	if err := rt.reconcileNft(context.Background(), state, targets); err != nil {
		t.Fatalf("desired response loss=%v calls=%#v", err, nft.calls)
	}
	nft.table = []byte(`{"nftables":[{"table":{"family":"inet","name":"` + nftTableName(state) + `","comment":"foreign"}}]}`)
	if err := rt.reconcileNft(context.Background(), state, targets); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) {
		t.Fatalf("foreign=%v", err)
	}
}

func TestNftDeleteResponseLossAndUnknownStateFailClosed(t *testing.T) {
	rt, state := initialized(t)
	nft := rt.nft.(*recordingNFTRunner)
	targets := []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}}
	nft.nextTable = nftJSONFixture(state, nftPolicyFor(state, targets))
	if err := rt.reconcileNft(context.Background(), state, targets); err != nil {
		t.Fatal(err)
	}
	nft.failAfterDelete = true
	if err := rt.removeNft(context.Background(), state, false); err != nil {
		t.Fatalf("delete response loss=%v", err)
	}
	nft.failure = errors.New("permission denied")
	if err := rt.reconcileNft(context.Background(), state, targets); !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) || nft.mutations != 2 {
		t.Fatalf("unknown state=%v mutations=%d", err, nft.mutations)
	}
}

func TestNftForeignTableIsNeverDeleted(t *testing.T) {
	rt, state := initialized(t)
	nft := rt.nft.(*recordingNFTRunner)
	nft.table = []byte(`{"nftables":[{"table":{"family":"inet","name":"` + nftTableName(state) + `","comment":"foreign","handle":1}}]}`)
	if err := rt.removeNft(context.Background(), state, true); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || nft.mutations != 0 {
		t.Fatalf("foreign delete=%v mutations=%d", err, nft.mutations)
	}
}

func TestAllowlistOnlyChangeDoesNotReplaceDataContainerAndObserveMarksNftDrift(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"db": {AdminPassword: "safe"}}
	rt.nft.(*recordingNFTRunner).nextTables = [][]byte{nftJSONFixture(state, nftPolicyFor(state, first.Target.DataServices))}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatalf("first=%v calls=%#v", err, runner.calls)
	}
	state, _ = rt.readState()
	runner.calls = nil
	rt.nft.(*recordingNFTRunner).calls = nil
	second := requestFor(state, revisionC())
	second.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.10"}}}}}
	second.Secrets.LocalDataServices = first.Secrets.LocalDataServices
	union := []hostcontract.LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.10", "10.0.0.9"}}}}}
	rt.nft.(*recordingNFTRunner).nextTables = [][]byte{nftJSONFixture(state, nftPolicyFor(state, union)), nftJSONFixture(state, nftPolicyFor(state, second.Target.DataServices))}
	if _, err := rt.Reconcile(context.Background(), second); err != nil || runner.mutations("rm") != 0 || runner.mutations("run") != 0 {
		t.Fatalf("allowlist update=%v calls=%#v nft=%#v state=%v table=%s", err, runner.calls, rt.nft.(*recordingNFTRunner).calls, classifyNftJSON(rt.nft.(*recordingNFTRunner).table, state, nftPolicyFor(state, union)), rt.nft.(*recordingNFTRunner).table)
	}
	state, _ = rt.readState()
	stored := mustInventory(t, rt)
	if stored.AppliedRevision != revisionC() || findLocalData(stored, localDataToken("db")).Revision != revisionB() {
		t.Fatalf("host/shell revisions=%q %#v", stored.AppliedRevision, findLocalData(stored, localDataToken("db")))
	}
	if observation, err := rt.Inspect(state.Resource); err != nil || observation.Drifted || !observation.Ready {
		t.Fatalf("source-only inspect=%#v %v", observation, err)
	}
	nft := rt.nft.(*recordingNFTRunner)
	nft.table = []byte(`{"nftables":[]}`)
	observation, err := rt.Inspect(state.Resource)
	if err != nil || !observation.Drifted || observation.Ready {
		t.Fatalf("nft drift=%#v %v", observation, err)
	}
}

func TestDataClientLifecycleUsesStdinAndNeverDropsPostgresData(t *testing.T) {
	rt, state := initialized(t)
	capture := newStatefulCatalogRunner()
	rt.runner = capture
	o := localObject(state, hostcontract.LocalDataServiceTarget{ID: "primary", Type: "postgres", Port: 5432, Clients: []hostcontract.LocalDataClient{{AppID: "old", Username: "old_user", Database: "old_db"}}}, revisionB())
	target := hostcontract.LocalDataServiceTarget{ID: "primary", Type: "postgres", Port: 5432, Clients: []hostcontract.LocalDataClient{{AppID: "new", Username: "new_user", Database: "new_db"}}}
	secret := hostcontract.LocalDataServiceSecrets{AdminPassword: "ADMIN_SECRET", ClientPasswords: map[string]string{"new": "CLIENT_SECRET"}}
	inv := inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: state.AppliedRevision, Objects: []managedObject{o}}
	state.Journal = &Journal{Key: reconcileKey(state, revisionB()), Status: journalPending}
	if err := rt.reconcileDataClients(context.Background(), state, inv, o, revisionB(), target, secret); err != nil {
		t.Fatal(err)
	}
	if len(capture.calls) < 5 || capture.hasSecret("CLIENT_SECRET") || capture.hasDrop() {
		t.Fatalf("calls=%#v", capture.calls)
	}
}

func TestPostgresCatalogRoleResponseLossResumesAfterExactObservation(t *testing.T) {
	rt, state := postgresCatalogRuntime(t)
	first := postgresCatalogRequest(state, revisionB(), nil)
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	oldAdmin := mustArtifact(t, rt, findLocalData(mustInventory(t, rt), localDataToken("primary")).Env)
	runner := rt.runner.(*statefulCatalogRunner)
	runner.catalog.roleFailure = catalogAfterCommit
	second := postgresCatalogRequest(state, revisionC(), []hostcontract.LocalDataClient{{AppID: "one", Username: "shared_user", Database: "app_db"}})
	beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
	if _, err := rt.Reconcile(t.Context(), second); err == nil || runner.catalog.roleMutations != 2 || runner.catalog.createMutations != 0 || runner.catalog.finalizeMutations != 0 || !bytes.Equal(oldAdmin, mustArtifact(t, rt, findLocalData(mustInventory(t, rt), localDataToken("primary")).Env)) {
		t.Fatalf("first role response loss=%v counters=%#v", err, runner.catalog)
	}
	if !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) || bytes.Equal(beforeState, mustFile(t, rt.statePath())) || mustState(t, rt).Journal.Status != journalPending {
		t.Fatal("response loss advanced durable artifacts")
	}
	retry := newStatefulCatalogRunnerWith(runner)
	rt = runtimeWithCatalog(t, rt, retry)
	if _, err := rt.Reconcile(t.Context(), second); err != nil || retry.catalog.roleMutations != 2 || retry.catalog.finalizeMutations != 1 {
		t.Fatalf("retry=%v counters=%#v", err, retry.catalog)
	}
	assertPostgresCatalogPersistence(t, rt, retry, "ADMIN_SECRET", "CLIENT_SECRET")
}

func TestPostgresCatalogCreateAndFinalizeResponseLossResumeWithoutDuplicateMutation(t *testing.T) {
	for name, failure := range map[string]catalogFailure{"create": catalogAfterCommit, "finalize": catalogAfterCommit} {
		t.Run(name, func(t *testing.T) {
			rt, state := postgresCatalogRuntime(t)
			runner := rt.runner.(*statefulCatalogRunner)
			if name == "create" {
				runner.catalog.createFailure = failure
			} else {
				runner.catalog.finalizeFailure = failure
			}
			request := postgresCatalogRequest(state, revisionB(), []hostcontract.LocalDataClient{{AppID: "one", Username: "one_user", Database: "app_db"}})
			if _, err := rt.Reconcile(t.Context(), request); err == nil || mustState(t, rt).Journal.Status != journalPending {
				t.Fatalf("first %s response loss=%v counters=%#v calls=%#v", name, err, runner.catalog, runner.calls)
			}
			creates, finalizes := runner.catalog.createMutations, runner.catalog.finalizeMutations
			retry := newStatefulCatalogRunnerWith(runner)
			rt = runtimeWithCatalog(t, rt, retry)
			wantFinalizes := finalizes
			if name == "create" {
				wantFinalizes++
			}
			if _, err := rt.Reconcile(t.Context(), request); err != nil || retry.catalog.createMutations != creates || retry.catalog.finalizeMutations != wantFinalizes {
				t.Fatalf("retry=%v creates=%d/%d finalizes=%d/%d", err, retry.catalog.createMutations, creates, retry.catalog.finalizeMutations, wantFinalizes)
			}
			assertPostgresCatalogPersistence(t, rt, retry, "ADMIN_SECRET", "CLIENT_SECRET")
		})
	}
}

func TestPostgresCatalogObservationFailuresDoNotMutateOrAdvanceArtifacts(t *testing.T) {
	for name, observation := range map[string][]byte{
		"malformed": []byte("not-a-catalog-record\n"),
		"oversized": bytes.Repeat([]byte("x"), maxCommandOutput+1),
		"foreign":   postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "roles", "foreign", "-", "0", "0", "0", "0", "0", "0", "0", "0"}),
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := postgresCatalogRuntime(t)
			runner := rt.runner.(*statefulCatalogRunner)
			runner.catalog.observation = observation
			if _, err := rt.Reconcile(t.Context(), postgresCatalogRequest(state, revisionB(), nil)); err == nil || runner.semanticMutations() != 0 {
				t.Fatalf("%s err=%v mutations=%d", name, err, runner.semanticMutations())
			}
		})
	}
	t.Run("unavailable", func(t *testing.T) {
		rt, state := postgresCatalogRuntime(t)
		runner := rt.runner.(*statefulCatalogRunner)
		runner.catalog.view = catalogUnavailable
		if _, err := rt.Reconcile(t.Context(), postgresCatalogRequest(state, revisionB(), nil)); err == nil || runner.semanticMutations() != 0 {
			t.Fatalf("unavailable err=%v mutations=%d", err, runner.semanticMutations())
		}
	})
}

func TestInspectPostgresCatalogIsReadOnlyAndClassifiesDrift(t *testing.T) {
	rt, state := postgresCatalogRuntime(t)
	request := postgresCatalogRequest(state, revisionB(), []hostcontract.LocalDataClient{{AppID: "one", Username: "shared_user", Database: "app_db"}})
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	runner := rt.runner.(*statefulCatalogRunner)
	before := runner.semanticMutations()
	if observed, err := rt.Inspect(state.Resource); err != nil || !observed.Ready || observed.Drifted || runner.semanticMutations() != before || rt.nft.(*recordingNFTRunner).mutations != 0 {
		t.Fatalf("exact inspect=%#v %v", observed, err)
	}
	for name, state := range map[string]catalogView{"partial": catalogCreateOnly, "foreign": catalogForeign, "unavailable": catalogUnavailable} {
		t.Run(name, func(t *testing.T) {
			runner.catalog.view = state
			before := runner.semanticMutations()
			observed, err := rt.Inspect(request.Resource)
			if name == "partial" {
				if err != nil || observed.Ready || !observed.Drifted {
					t.Fatalf("partial=%#v %v", observed, err)
				}
			} else if !isRemote(err, hostprotocol.ErrorRecoveryRequired, hostprotocol.CodeRecoveryRequired) {
				t.Fatalf("%s=%v", name, err)
			}
			if runner.semanticMutations() != before || rt.nft.(*recordingNFTRunner).mutations != 0 {
				t.Fatal("inspect mutated")
			}
		})
	}
}

func TestInspectPostgresSecurityArtifactDriftIsReadOnly(t *testing.T) {
	for _, artifact := range []string{"Config", "HBA", "Ident"} {
		t.Run(artifact, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			req := requestFor(state, revisionB())
			req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432}}
			req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "safe"}}
			if _, err := rt.Reconcile(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			state = mustState(t, rt)
			pg := findLocalData(mustInventory(t, rt), localDataToken("primary"))
			name := pg.Config
			if artifact == "HBA" {
				name = pg.HBA
			} else if artifact == "Ident" {
				name = pg.Ident
			}
			beforeState, beforeInventory := mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)
			if err := rt.removeArtifact(name); err != nil {
				t.Fatal(err)
			}
			runner.calls = nil
			observed, err := rt.Inspect(state.Resource)
			if err != nil || !observed.Drifted || observed.Ready || runner.dockerMutations() != 0 || !bytes.Equal(beforeState, mustFile(t, rt.statePath())) || !bytes.Equal(beforeInventory, mustArtifact(t, rt, artifactInventory)) {
				t.Fatalf("inspect=%#v err=%v calls=%#v", observed, err, runner.calls)
			}
		})
	}
}

func TestRedisGeneratedACLContainsDesiredClientsOnly(t *testing.T) {
	rt, state := initialized(t)
	target := hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "app_one", Database: "0"}}}
	o := localObject(state, target, revisionB())
	if err := rt.writeLocalSecrets(context.Background(), state, o, target, hostcontract.LocalDataServiceSecrets{AdminPassword: "ADMIN_SECRET", ClientPasswords: map[string]string{"one": "CLIENT_SECRET"}}); err != nil {
		t.Fatal(err)
	}
	config := string(mustArtifact(t, rt, o.Config))
	if !strings.Contains(config, "user app_one on >\"CLIENT_SECRET\"") || strings.Contains(config, "old_user") {
		t.Fatalf("redis ACL=%q", config)
	}
}

func TestRedisReadinessUsesProtectedAuthAndRequiresExactPONG(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	o := localObject(state, hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380}, revisionB())
	o.DataToken = token("data", o.AppToken)
	if err := rt.writeLocalSecrets(t.Context(), state, o, hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380}, hostcontract.LocalDataServiceSecrets{AdminPassword: "safe"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.runLocal(t.Context(), state, o, hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380}); err != nil {
		t.Fatal(err)
	}
	if !runner.anyArg("--env-file", rt.artifactPath(o.Env)) || runner.anyArg("-v", rt.artifactPath(o.Env)+":/run/secrets/redis-cli.env:ro") || runner.hasSecret("safe") {
		t.Fatalf("redis launch=%#v", runner.calls)
	}
	if err := rt.localReady(t.Context(), o); err != nil || !runner.hasCall([]string{"exec", o.Name, "redis-cli", "--raw", "-h", "127.0.0.1", "-p", "6380", "ping"}) {
		t.Fatalf("readiness=%v calls=%#v", err, runner.calls)
	}
	runner.fail = func(argv []string) error { return nil }
	// The explicit fake below models a successful command with non-PONG output.
	bad := stdoutRunner{output: []byte("NOAUTH Authentication required.\n")}
	rt.runner = bad
	if err := rt.localReady(t.Context(), o); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) {
		t.Fatalf("non-PONG output=%v", err)
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

func TestReconcileRejectsPureRedisAndServiceInputsBeforeBegin(t *testing.T) {
	for name, mutate := range map[string]func(*hostprotocol.Request){
		"malformed redis user": func(q *hostprotocol.Request) {
			q.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6379, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "bad-user", Database: "0"}}}}
			q.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "ADMIN_SECRET", ClientPasswords: map[string]string{"one": "CLIENT_SECRET"}}}
		},
		"default redis user": func(q *hostprotocol.Request) {
			q.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6379, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "default", Database: "0"}}}}
			q.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "ADMIN_SECRET", ClientPasswords: map[string]string{"one": "CLIENT_SECRET"}}}
		},
		"noncanonical redis database": func(q *hostprotocol.Request) {
			q.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6379, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "app_one", Database: "01"}}}}
			q.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "ADMIN_SECRET", ClientPasswords: map[string]string{"one": "CLIENT_SECRET"}}}
		},
		"unsafe client password": func(q *hostprotocol.Request) {
			q.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6379, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "app_one", Database: "0"}}}}
			q.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "ADMIN_SECRET", ClientPasswords: map[string]string{"one": "bad\npassword"}}}
		},
		"malformed service ID": func(q *hostprotocol.Request) {
			q.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "bad/service", Type: "redis", Port: 6379}}
			q.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"bad/service": {AdminPassword: "ADMIN_SECRET"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt, state := initialized(t)
			runner := &recordingRunner{}
			rt.runner = runner
			before := mustFile(t, rt.statePath())
			req := requestFor(state, revisionB(), app("one", "image"))
			mutate(&req)
			if _, err := rt.Reconcile(t.Context(), req); !isRemote(err, hostprotocol.ErrorRemoteOperation, hostprotocol.CodeOperationFailed) || len(runner.calls) != 0 || !bytes.Equal(before, mustFile(t, rt.statePath())) || runner.hasSecret("ADMIN_SECRET") || runner.hasSecret("CLIENT_SECRET") {
				t.Fatalf("reconcile=%v calls=%#v", err, runner.calls)
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
		t.Fatalf("first=%v calls=%#v", err, runner.calls)
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
	for _, artifact := range []string{pg.Env, pg.Config, pg.HBA, pg.Ident} {
		if _, err := rt.readArtifactBytes(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed artifact %q remains: %v", artifact, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rt.dataPath(pg.DataToken), "sentinel")); err != nil {
		t.Fatalf("removed service data was not preserved: %v", err)
	}
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

func TestPostgresHasExplicitScramConfigAndHBAArtifacts(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	req := requestFor(state, revisionB())
	req.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "app_one", Database: "app_db"}}}}
	req.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "safe", ClientPasswords: map[string]string{"one": "CLIENT_SECRET"}}}
	if _, err := rt.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	pg := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	config, hba := string(mustArtifact(t, rt, pg.Config)), string(mustArtifact(t, rt, pg.HBA))
	if !strings.Contains(config, "listen_addresses = '*'\npassword_encryption = 'scram-sha-256'\n") || strings.Contains(hba, "trust") || !strings.Contains(hba, "host app_db app_one all scram-sha-256") || strings.Contains(hba, "172.30.") || !strings.Contains(hba, "host all all all reject") || !runner.anyArg("-v", rt.artifactPath(pg.Config)+":/etc/sub2api/postgresql.conf:ro") || !runner.anyArg("-v", rt.artifactPath(pg.HBA)+":/etc/sub2api/pg_hba.conf:ro") || runner.hasSecret("safe") || runner.hasSecret("CLIENT_SECRET") {
		t.Fatalf("config=%q hba=%q calls=%#v", config, hba, runner.calls)
	}
	for name, mutate := range map[string]func(*managedObject){
		"postgres malformed Config": func(o *managedObject) { o.Config = "wrong" },
		"postgres missing Config":   func(o *managedObject) { o.Config = "" },
		"postgres malformed HBA":    func(o *managedObject) { o.HBA = "wrong" },
		"postgres missing HBA":      func(o *managedObject) { o.HBA = "" },
		"postgres malformed Ident":  func(o *managedObject) { o.Ident = "wrong" },
		"postgres missing Ident":    func(o *managedObject) { o.Ident = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := pg
			mutate(&bad)
			if err := validateInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: revisionB(), Objects: []managedObject{bad}}); err == nil {
				t.Fatal("invalid PostgreSQL security artifact accepted")
			}
		})
	}
	redis := localObject(state, hostcontract.LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 6380}, revisionB())
	redis.DataToken, redis.PathToken = token("data", redis.AppToken), token("path", redis.AppToken)
	redis.Config = ""
	if err := validateInventory(inventory{Version: inventoryVersion, Resource: state.Resource, Ownership: state.Ownership, AppliedRevision: revisionB(), Objects: []managedObject{redis}}); err == nil {
		t.Fatal("redis missing config accepted")
	}
}

func TestReconcilePostgresClientChangeReplacesShellAndPreservesData(t *testing.T) {
	rt, state := postgresCatalogRuntime(t)
	bindings := []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}
	firstClients := []hostcontract.LocalDataClient{{AppID: "old", Username: "old_user", Database: "old_db"}}
	first := postgresCatalogRequest(state, revisionB(), firstClients)
	first.Target.DataServices[0].Bindings = bindings
	rt.nft.(*recordingNFTRunner).nextTable = nftJSONFixture(state, nftPolicyFor(state, first.Target.DataServices))
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatalf("first=%v calls=%#v", err, rt.runner.(*statefulCatalogRunner).calls)
	}
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	if err := os.WriteFile(filepath.Join(rt.dataPath(old.DataToken), "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, rt)
	runner := rt.runner.(*statefulCatalogRunner)
	runner.calls = nil
	secondClients := []hostcontract.LocalDataClient{{AppID: "new", Username: "new_user", Database: "new_db"}}
	second := postgresCatalogRequest(state, revisionC(), secondClients)
	second.Target.DataServices[0].Bindings = bindings
	rt.nft.(*recordingNFTRunner).nextTable = nftJSONFixture(state, nftPolicyFor(state, second.Target.DataServices))
	if _, err := rt.Reconcile(t.Context(), second); err != nil {
		t.Fatalf("client change=%v calls=%#v", err, runner.calls)
	}
	replacements := 0
	for _, call := range runner.calls {
		if len(call) > 0 && (call[0] == "rm" || call[0] == "run") {
			replacements++
		}
	}
	if replacements != 2 {
		t.Fatalf("replacement count=%d calls=%#v", replacements, runner.calls)
	}
	next := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	targetOperation := strings.TrimPrefix(next.Env, "env-"+next.AppToken)
	if next.DataToken != old.DataToken || next.PathToken != old.PathToken || runner.catalog.roleOperation != targetOperation {
		t.Fatalf("data/catalog old=%#v next=%#v catalog=%#v", old, next, runner.catalog)
	}
	if database := runner.catalog.databases["new_db"]; !database.finalized || database.operation != targetOperation {
		t.Fatalf("new database catalog=%#v", runner.catalog.databases)
	}
	hba := string(mustArtifact(t, rt, next.HBA))
	if !strings.Contains(hba, "host new_db new_user all scram-sha-256\n") || strings.Contains(hba, "old_db") || strings.Contains(hba, "old_user") || strings.Contains(hba, "10.0.0.9") || strings.Contains(hba, "trust") {
		t.Fatalf("HBA=%q", hba)
	}
	if got, err := os.ReadFile(filepath.Join(rt.dataPath(next.DataToken), "sentinel")); err != nil || string(got) != "keep" {
		t.Fatalf("sentinel=%q err=%v", got, err)
	}
	for _, artifact := range []string{old.Env, old.Config, old.HBA, old.Ident} {
		if _, err := rt.readArtifactBytes(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old artifact %q remains: %v", artifact, err)
		}
	}
	for _, artifact := range []string{next.Env, next.Config, next.HBA, next.Ident} {
		if _, err := rt.readArtifactBytes(artifact); err != nil {
			t.Fatalf("new artifact %q: %v", artifact, err)
		}
	}
	before := runner.semanticMutations()
	if observed, err := rt.Inspect(state.Resource); err != nil || !observed.Ready || observed.Drifted || runner.semanticMutations() != before {
		t.Fatalf("inspect=%#v err=%v", observed, err)
	}
}

func TestPostgresUsesFixedPeerAdminAndPasswordFile(t *testing.T) {
	rt, state := initialized(t)
	rt.runner = &recordingRunner{}
	target := hostcontract.LocalDataServiceTarget{ID: "primary", Type: "postgres", Port: 5432, Bindings: []hostcontract.LocalDataBinding{{Address: "10.0.0.8", AllowedSources: []string{"10.0.0.9"}}}, Clients: []hostcontract.LocalDataClient{{AppID: "api-blue", Username: "api_blue", Database: "app_db"}}}
	o := localObject(state, target, revisionB())
	if err := rt.writeLocalSecrets(context.Background(), state, o, target, hostcontract.LocalDataServiceSecrets{AdminPassword: "a$$;=mid'quote", ClientPasswords: map[string]string{"api-blue": "c$$;=mid'quote"}}); err != nil {
		t.Fatal(err)
	}
	env, config, hba, ident := string(mustArtifact(t, rt, o.Env)), string(mustArtifact(t, rt, o.Config)), string(mustArtifact(t, rt, o.HBA)), string(mustArtifact(t, rt, o.Ident))
	if env != "a$$;=mid'quote\n" || !strings.Contains(config, "ident_file = '/etc/sub2api/pg_ident.conf'\n") || !strings.Contains(hba, "local all all peer map=s2h_admin\n") || !strings.Contains(hba, "host app_db api_blue all scram-sha-256\n") || strings.Contains(hba, "10.0.0.9") || !strings.Contains(hba, "local all all reject\n") || !strings.Contains(ident, "s2h_admin root s2h_admin\n") || strings.Contains(hba, "trust") {
		t.Fatalf("env=%q config=%q hba=%q ident=%q", env, config, hba, ident)
	}
}

func TestPostgresClientSQLUsesStableOwnersAndStdinContainment(t *testing.T) {
	owner := postgresOwner("primary", "app_db")
	if owner != postgresOwner("primary", "app_db") || owner == postgresOwner("primary", "other_db") || !strings.HasPrefix(owner, "s2h_owner_") {
		t.Fatalf("owner = %q", owner)
	}
	_, state := initialized(t)
	clients := []hostcontract.LocalDataClient{{AppID: "api-blue", Username: "api_blue", Database: "app_db"}, {AppID: "api-green", Username: "api_green", Database: "app_db"}}
	sql, err := postgresClientSQL(state, "primary", revisionB(), clients, map[string]string{"api-blue": "a$$;=mid'quote", "api-green": "b$$;=mid'quote"}, nil, "admin")
	databaseSQL, databaseErr := postgresDatabaseSQL(state, "primary", revisionB(), "app_db", clients)
	if err != nil || databaseErr != nil || strings.Contains(sql, "DO $$") || strings.Contains(sql, "DROP ") || !strings.Contains(sql, "ALTER ROLE") || !strings.Contains(sql, "\\gexec") || !strings.Contains(databaseSQL, "SET ROLE") || !strings.Contains(sql, owner) {
		t.Fatalf("sql=%q database=%q err=%v/%v", sql, databaseSQL, err, databaseErr)
	}
}

func TestPostgresRecoveryMarkersBindHostServiceAndOperation(t *testing.T) {
	_, state := initialized(t)
	roles := postgresRolesMarker(state, "primary", revisionB())
	owner := postgresOwnerMarker(state, "primary", revisionB(), "app_db")
	client := postgresClientMarker(state, "primary", revisionB(), "api-blue")
	database := postgresDatabaseMarker(state, "primary", revisionB(), "app_db")
	for _, marker := range []string{roles, owner, client, database} {
		if !strings.HasPrefix(marker, "s2hpg2:") || strings.Contains(marker, "SECRET") {
			t.Fatalf("marker=%q", marker)
		}
	}
	if roles == postgresRolesMarker(state, "other", revisionB()) || roles == postgresRolesMarker(state, "primary", revisionC()) || !strings.Contains(roles, ":roles:") || !strings.Contains(owner, ":owner:") || !strings.Contains(client, ":client:") || !strings.Contains(database, ":database:") {
		t.Fatalf("markers roles=%q owner=%q client=%q database=%q", roles, owner, client, database)
	}
}

func TestRedisACLRemovalReplacesShellButPreservesDataPath(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380, Persistence: true, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "app_one", Database: "0"}}}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "admin", ClientPasswords: map[string]string{"one": "client"}}}
	if _, err := rt.Reconcile(context.Background(), first); err != nil {
		t.Fatalf("first=%v calls=%#v", err, runner.calls)
	}
	old := findLocalData(mustInventory(t, rt), localDataToken("cache"))
	if err := os.WriteFile(filepath.Join(rt.dataPath(old.DataToken), "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	second := requestFor(state, revisionC())
	second.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380, Persistence: true}}
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "admin"}}
	if _, err := rt.Reconcile(context.Background(), second); err != nil || runner.mutations("rm") != 1 || runner.mutations("run") != 1 {
		t.Fatalf("ACL removal=%v calls=%#v", err, runner.calls)
	}
	next := findLocalData(mustInventory(t, rt), localDataToken("cache"))
	config := string(mustArtifact(t, rt, next.Config))
	if strings.Contains(config, "app_one") || next.DataToken != old.DataToken || !runner.anyArg("-v", rt.dataPath(old.DataToken)+":/data") {
		t.Fatalf("ACL/data=%q old=%#v next=%#v calls=%#v", config, old, next, runner.calls)
	}
	if got, err := os.ReadFile(filepath.Join(rt.dataPath(old.DataToken), "sentinel")); err != nil || string(got) != "keep" {
		t.Fatalf("data preservation=%q %v", got, err)
	}
}

func TestRedisClientPasswordRotationReplacesShellAndKeepsDataPath(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	first := requestFor(state, revisionB())
	first.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "cache", Type: "redis", Port: 6380, Persistence: true, Clients: []hostcontract.LocalDataClient{{AppID: "one", Username: "app_one", Database: "0"}}}}
	first.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "admin", ClientPasswords: map[string]string{"one": "old;$(danger)"}}}
	if _, err := rt.Reconcile(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	old := findLocalData(mustInventory(t, rt), localDataToken("cache"))
	state = mustState(t, rt)
	runner.calls = nil
	second := requestFor(state, revisionC())
	second.Target.DataServices = first.Target.DataServices
	second.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"cache": {AdminPassword: "admin", ClientPasswords: map[string]string{"one": "new;$(danger)"}}}
	if _, err := rt.Reconcile(t.Context(), second); err != nil || runner.mutations("rm") != 1 || runner.mutations("run") != 1 {
		t.Fatalf("rotation=%v calls=%#v", err, runner.calls)
	}
	next := findLocalData(mustInventory(t, rt), localDataToken("cache"))
	config := string(mustArtifact(t, rt, next.Config))
	for _, secret := range []string{"old;$(danger)", "new;$(danger)"} {
		if runner.hasSecret(secret) || bytes.Contains(mustFile(t, rt.statePath()), []byte(secret)) || bytes.Contains(mustArtifact(t, rt, artifactInventory), []byte(secret)) {
			t.Fatalf("secret leaked: %q", secret)
		}
	}
	if old.DataToken != next.DataToken || old.PathToken != next.PathToken || !strings.Contains(config, `user app_one on >"new;$(danger)"`) || strings.Contains(config, `old;$(danger)`) {
		t.Fatalf("rotation lost identity or ACL: old=%#v next=%#v config=%q", old, next, config)
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
		t.Fatalf("first=%v calls=%#v", err, runner.calls)
	}
	state, _ = rt.readState()
	old := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	runner.calls = nil
	runner.stdinDigests = nil
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
	if alter < 0 || rm >= 0 || run >= 0 || runner.hasSecret("OLD_SECRET") || runner.hasSecret("new'pass") {
		t.Fatalf("credential ordering/leak calls=%#v", runner.calls)
	}
	if len(runner.stdinDigests) == 0 {
		t.Fatalf("missing catalog stdin=%#v", runner.stdinDigests)
	}
	if got := string(mustArtifact(t, rt, old.Env)); got != "new'pass\n" {
		t.Fatalf("password-file artifact=%q", got)
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

func TestPostgresRotationDoesNotDependOnPasswordAuthentication(t *testing.T) {
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
	if _, err := rt.Reconcile(context.Background(), second); err != nil {
		t.Fatalf("peer-admin rotation = %v", err)
	}
	if len(runner.stdinDigests) == 0 || runner.hasSecret("new'password") {
		t.Fatalf("credential update leaked: stdin=%#v calls=%#v", runner.stdinDigests, runner.calls)
	}
}

func TestPostgresPeerAdminIsIndependentOfReplacementPassword(t *testing.T) {
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
	if _, err := rt.Reconcile(context.Background(), second); err != nil || runner.hasSecret("new") {
		t.Fatalf("peer-admin rotation = %v calls=%#v", err, runner.calls)
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
	for _, artifact := range []string{oldCache.Config, oldProxy.Env, oldProxy.Config} {
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
	if _, err := rt.Reconcile(t.Context(), requestFor(state, revisionB(), appWithData("one", "one", old))); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	request := requestFor(state, revisionC(), appWithData("one", "one", new))
	key := requestKey(request)
	approval := hostcontract.ApprovalSubject{Kind: hostcontract.ApprovalDataLink, Environment: state.Resource.Environment, Resource: state.Resource, AppID: "one", DataKind: "postgres", OldData: old, NewData: new, TargetRevision: request.TargetRevision}
	// Persisted intent represents a completed approval followed by process/response loss.
	op, err := rt.Begin(key, &approval)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	}
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
	want := "RUNTIME_A=three\nRUNTIME_B=four\nSETTING_A=one\nSETTING_B=two\nINITIAL_ADMIN_PASSWORD=admin\nJWT_SECRET=jwt\nTOTP_ENCRYPTION_KEY=totp\nADMIN_API_KEY=api\n"
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
	request := requestFor(state, revisionB(), app("one", "image-one"))
	request.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432, Persistence: true}}
	request.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "admin"}}
	if _, err := rt.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	pg := findLocalData(mustInventory(t, rt), localDataToken("primary"))
	if err := os.WriteFile(filepath.Join(rt.dataPath(pg.DataToken), "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
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
	for _, artifact := range []string{pg.Env, pg.Config, pg.HBA, pg.Ident} {
		if _, err := rt.readArtifactBytes(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retired artifact %q remains: %v", artifact, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rt.dataPath(pg.DataToken), "sentinel")); err != nil {
		t.Fatalf("retired service data was not preserved: %v", err)
	}
	calls := len(runner.calls)
	if result, err := rt.Handle(context.Background(), hostprotocol.Request{Action: hostcontract.ActionInspect, Resource: state.Resource}); err != nil || result.Status != hostprotocol.ResultRetired {
		t.Fatalf("inspect=%#v %v", result, err)
	}
	if _, err := rt.Retire(context.Background(), retireRequest(key, approval)); err != nil || len(runner.calls) != calls {
		t.Fatalf("replay=%v", err)
	}
}

func TestRetireForeignNftAtStartDoesNotMutateRoutesOrContainers(t *testing.T) {
	rt, state := initialized(t)
	runner := &recordingRunner{}
	rt.runner = runner
	request := requestFor(state, revisionB(), app("one", "image"))
	if _, err := rt.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	state, _ = rt.readState()
	runner.calls = nil
	nft := rt.nft.(*recordingNFTRunner)
	nft.table = []byte(`{"nftables":[{"table":{"family":"inet","name":"` + nftTableName(state) + `","comment":"foreign","handle":1}}]}`)
	key := retireKey(state)
	if _, err := rt.Retire(t.Context(), retireRequest(key, retireApproval(key, state))); !isRemote(err, hostprotocol.ErrorConflict, hostprotocol.CodeOperationConflict) || runner.mutations() != 0 || nft.mutations != 0 {
		t.Fatalf("retire=%v docker=%#v nft=%#v", err, runner.calls, nft.calls)
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
		"app image":   func(_ *testing.T, _ *Runtime, _ *State, inv *inventory) { inv.Objects[0].Image = "other-image" },
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
	publications          map[string]map[string][]map[string]string
	disablePostgresAlter  bool
	wrongPostgresPassword bool
	postgresMarkers       map[string]string
	postgresDatabaseMark  string
	postgresCreated       map[string]bool
	postgresFinalized     map[string]bool
	postgresCreateSeen    bool
	postgresFinalizeSeen  bool
	fail                  func([]string) error
	failAfter             bool
	event                 func([]string)
}

type stdoutRunner struct{ output []byte }

func (r stdoutRunner) Run(context.Context, []string, []byte) ([]byte, error) { return r.output, nil }

type recordingNFTRunner struct {
	calls           [][]string
	table           []byte
	nextTable       []byte
	nextTables      [][]byte
	fail            func([]string) error
	failure         error
	failAfterApply  bool
	failAfterDelete bool
	mutations       int
	event           func([]string)
}

type stdinRunner struct {
	calls [][]string
	stdin string
}
type catalogFailure uint8

const (
	catalogNoFailure catalogFailure = iota
	catalogBeforeCommit
	catalogAfterCommit
)

type catalogView uint8

const (
	catalogNormal catalogView = iota
	catalogCreateOnly
	catalogForeign
	catalogUnavailable
	catalogMalformed
)

// statefulCatalog is shared by runner wrappers to model a new Host process
// observing the same PostgreSQL catalog after a lost command response.
type statefulCatalog struct {
	roleOperation                                     string
	databases                                         map[string]catalogDatabase
	roleFailure, createFailure, finalizeFailure       catalogFailure
	view                                              catalogView
	observation                                       []byte
	roleMutations, createMutations, finalizeMutations int
}
type catalogDatabase struct {
	creator, finalized bool
	operation          string
}
type statefulCatalogRunner struct {
	recordingRunner
	catalog *statefulCatalog
	calls   [][]string
}

func newStatefulCatalogRunner() *statefulCatalogRunner {
	return &statefulCatalogRunner{catalog: &statefulCatalog{databases: map[string]catalogDatabase{}}}
}
func newStatefulCatalogRunnerWith(previous *statefulCatalogRunner) *statefulCatalogRunner {
	return &statefulCatalogRunner{catalog: previous.catalog}
}
func (r *statefulCatalogRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	text := string(stdin)
	if postgresCatalogCommand(argv) {
		if strings.Contains(text, "'roles'") {
			return r.catalog.roles(text)
		}
		if strings.Contains(text, "'dbref'") {
			return r.catalog.reference(text)
		}
		if strings.Contains(text, "'dbdetail'") {
			return r.catalog.detail(text)
		}
		if strings.Contains(text, "BEGIN;\nCREATE TEMP TABLE s2h_clients") {
			r.catalog.roleMutations++
			return r.catalog.mutate(&r.catalog.roleFailure, func() { r.catalog.roleOperation = catalogOperation(text, ":roles:") })
		}
		if strings.HasPrefix(text, "SELECT format('CREATE DATABASE") {
			db := postgresSQLQuotedAfter(text, "datname='")
			if db == "" {
				db = catalogDatabaseFromCreate(text)
			}
			r.catalog.createMutations++
			return r.catalog.mutate(&r.catalog.createFailure, func() { value := r.catalog.databases[db]; value.creator = true; r.catalog.databases[db] = value })
		}
		if strings.HasPrefix(text, "BEGIN;\nSELECT format('ALTER DATABASE") {
			db := postgresSQLQuotedAfter(text, "ALTER DATABASE %I OWNER TO %I', '")
			r.catalog.finalizeMutations++
			return r.catalog.mutate(&r.catalog.finalizeFailure, func() {
				value := r.catalog.databases[db]
				value.finalized = true
				value.creator = false
				value.operation = catalogOperation(text, ":database:")
				r.catalog.databases[db] = value
			})
		}
	}
	return r.recordingRunner.Run(ctx, argv, stdin)
}
func postgresCatalogCommand(argv []string) bool {
	return len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt"
}
func (c *statefulCatalog) mutate(failure *catalogFailure, commit func()) ([]byte, error) {
	switch *failure {
	case catalogBeforeCommit:
		*failure = catalogNoFailure
		return nil, errors.New("catalog response lost")
	case catalogAfterCommit:
		*failure = catalogNoFailure
		commit()
		return nil, errors.New("catalog response lost")
	}
	commit()
	return nil, nil
}

func (c *statefulCatalog) roles(sql string) ([]byte, error) {
	if c.observation != nil {
		return c.observation, nil
	}
	if c.view == catalogUnavailable {
		return nil, errors.New("catalog unavailable")
	}
	if c.view == catalogMalformed {
		return []byte("malformed catalog record\n"), nil
	}
	if c.view == catalogMalformed {
		return []byte("malformed catalog record\n"), nil
	}
	if c.view == catalogForeign {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "roles", "foreign", "-", "0", "0", "0", "0", "0", "0", "0", "0"}), nil
	}
	target, prior := catalogOperation(sql, ":roles:"), catalogLastOperation(sql, ":roles:")
	if c.roleOperation == target {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "roles", "target", target, "1", "1", "1", "1", "1", "1", "1", "0"}), nil
	}
	if prior != target {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "roles", "prior", prior, "1", "1", "1", "1", "1", "1", "1", "0"}), nil
	}
	return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "roles", "absent", "-", "0", "0", "0", "0", "0", "0", "0", "1"}), nil
}
func (c *statefulCatalog) reference(sql string) ([]byte, error) {
	db, key, operation := postgresSQLQuotedAfter(sql, "d.datname='"), "", catalogOperation(sql, ":database:")
	prior := catalogLastOperation(sql, ":database:")
	key = postgresCatalogProtocolDatabaseToken(db)
	if c.view == catalogUnavailable {
		return nil, errors.New("catalog unavailable")
	}
	if c.view == catalogForeign {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "foreign", "-", "0", "0"}), nil
	}
	value := c.databases[db]
	if c.view == catalogCreateOnly {
		value.creator, value.finalized = true, false
	}
	if value.finalized && value.operation == operation {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "target", operation, "1", "0"}), nil
	}
	if prior != operation {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "prior", prior, "1", "0"}), nil
	}
	if value.creator {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "create-only", "-", "0", "1"}), nil
	}
	return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "absent", "-", "0", "0"}), nil
}
func (c *statefulCatalog) detail(sql string) ([]byte, error) {
	db, key, operation := postgresSQLQuotedAfter(sql, "d.datname='"), "", catalogOperation(sql, ":database:")
	prior := catalogLastOperation(sql, ":database:")
	key = postgresCatalogProtocolDatabaseToken(db)
	if c.view == catalogUnavailable {
		return nil, errors.New("catalog unavailable")
	}
	if c.view == catalogForeign {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "foreign", "-", "0", "0", "0", "0", "0", "0"}), nil
	}
	value := c.databases[db]
	if c.view == catalogCreateOnly {
		value.creator, value.finalized = true, false
	}
	if value.finalized && value.operation == operation {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "target", operation, "1", "1", "1", "1", "1", "1"}), nil
	}
	if prior != operation {
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "prior", prior, "1", "1", "1", "1", "1", "1"}), nil
	}
	return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "create-only", "-", "1", "1"}), nil
}
func (r *statefulCatalogRunner) semanticMutations() int {
	return r.catalog.roleMutations + r.catalog.createMutations + r.catalog.finalizeMutations
}
func (r *statefulCatalogRunner) hasSecret(secret string) bool {
	for _, call := range r.calls {
		for _, arg := range call {
			if strings.Contains(arg, secret) {
				return true
			}
		}
	}
	return false
}
func (r *statefulCatalogRunner) hasDrop() bool {
	for _, call := range r.calls {
		if strings.Contains(strings.Join(call, " "), "DROP") {
			return true
		}
	}
	return false
}
func catalogLastOperation(sql, phase string) string {
	first, last := catalogOperation(sql, phase), "-"
	for rest := sql; ; {
		start := strings.Index(rest, "s2hpg2:")
		if start < 0 {
			break
		}
		marker := rest[start:]
		marker, _, _ = strings.Cut(marker, "'")
		parts := strings.Split(marker, ":")
		for i := range parts {
			if parts[i] == strings.Trim(phase, ":") && i+1 < len(parts) {
				last = parts[i+1]
			}
		}
		rest = rest[start+len("s2hpg2:"):]
	}
	if last == "-" {
		return first
	}
	return last
}
func catalogDatabaseFromCreate(sql string) string {
	const prefix = "CREATE DATABASE %I OWNER %I', "
	start := strings.Index(sql, prefix)
	if start < 0 {
		return ""
	}
	rest := sql[start+len(prefix):]
	if !strings.HasPrefix(rest, "'") {
		return ""
	}
	return postgresSQLQuotedAfter(rest, "'")
}

func postgresCatalogRuntime(t *testing.T) (*Runtime, State) {
	t.Helper()
	rt, state := initialized(t)
	rt.runner = newStatefulCatalogRunner()
	return rt, state
}
func runtimeWithCatalog(t *testing.T, previous *Runtime, runner *statefulCatalogRunner) *Runtime {
	t.Helper()
	rt := New(previous.root, previous.machinePath)
	rt.nft = previous.nft
	rt.runner = runner
	return rt
}
func postgresCatalogRequest(state State, revision string, clients []hostcontract.LocalDataClient) hostprotocol.Request {
	request := requestFor(state, revision)
	request.Target.DataServices = []hostcontract.LocalDataServiceTarget{{ID: "primary", Type: "postgres", Port: 5432, Clients: clients}}
	passwords := map[string]string{}
	for _, client := range clients {
		passwords[client.AppID] = "CLIENT_SECRET"
	}
	request.Secrets.LocalDataServices = map[string]hostcontract.LocalDataServiceSecrets{"primary": {AdminPassword: "ADMIN_SECRET", ClientPasswords: passwords}}
	return request
}
func assertPostgresCatalogPersistence(t *testing.T, rt *Runtime, runner *statefulCatalogRunner, secrets ...string) {
	t.Helper()
	state := mustState(t, rt)
	if state.Journal == nil || state.Journal.Status != journalComplete || state.Observation.Ready != true {
		t.Fatalf("state=%#v", state)
	}
	for _, output := range [][]byte{mustFile(t, rt.statePath()), mustArtifact(t, rt, artifactInventory)} {
		for _, secret := range secrets {
			if bytes.Contains(output, []byte(secret)) {
				t.Fatalf("secret persisted: %q", secret)
			}
		}
		if bytes.Contains(output, []byte("s2hpg2:")) || bytes.Contains(output, []byte("pg_shdescription")) {
			t.Fatalf("catalog metadata persisted: %q", output)
		}
	}
	if runner.hasSecret("ADMIN_SECRET") || runner.hasSecret("CLIENT_SECRET") || runner.hasDrop() {
		t.Fatalf("unsafe command trace=%#v", runner.calls)
	}
}

// nftJSONFixture models the bounded shape emitted by `nft -j list table`.
// Handles are deliberately numeric because the production parser rejects every
// other handle representation.
func nftJSONFixture(s State, p nftPolicy) []byte {
	table := map[string]any{"family": "inet", "name": nftTableName(s), "comment": nftOwnershipComment(s), "handle": 1}
	chain := map[string]any{"family": "inet", "table": nftTableName(s), "name": "prerouting", "type": "filter", "hook": "prerouting", "prio": -110, "policy": "accept", "comment": nftPolicyCommentFor(p), "handle": 2}
	objects := []any{
		map[string]any{"metainfo": map[string]any{"version": "1.0.9", "json_schema_version": 1}},
		map[string]any{"table": table},
		map[string]any{"chain": chain},
	}
	handle := 3
	for _, group := range p.Groups {
		for _, source := range group.Sources {
			objects = append(objects, map[string]any{"rule": map[string]any{"family": "inet", "table": nftTableName(s), "chain": "prerouting", "expr": []any{
				map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": group.Family, "field": "saddr"}}, "right": source}},
				map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": group.Family, "field": "daddr"}}, "right": group.Destination}},
				map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": "tcp", "field": "dport"}}, "right": group.Port}},
				map[string]any{"accept": nil},
			}, "handle": handle}})
			handle++
		}
		objects = append(objects, map[string]any{"rule": map[string]any{"family": "inet", "table": nftTableName(s), "chain": "prerouting", "expr": []any{
			map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": group.Family, "field": "daddr"}}, "right": group.Destination}},
			map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"payload": map[string]any{"protocol": "tcp", "field": "dport"}}, "right": group.Port}},
			map[string]any{"drop": nil},
		}, "handle": handle}})
		handle++
	}
	result, err := json.Marshal(map[string]any{"nftables": objects})
	if err != nil {
		panic(err)
	}
	return result
}

func (r *stdinRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	r.stdin = string(stdin)
	return nil, nil
}

func (r *recordingNFTRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if r.event != nil {
		r.event(argv)
	}
	if r.fail != nil {
		if err := r.fail(argv); err != nil {
			return nil, err
		}
	}
	if r.failure != nil {
		return nil, r.failure
	}
	if len(argv) == 5 && argv[0] == "-j" {
		if len(r.table) == 0 {
			return nil, errNftNotFound
		}
		return append([]byte(nil), r.table...), nil
	}
	if len(argv) == 2 && argv[0] == "-f" {
		r.mutations++
		if strings.HasPrefix(strings.TrimSpace(string(stdin)), "delete table inet ") && !strings.Contains(string(stdin), "\ntable inet ") {
			r.table = nil
			if r.failAfterDelete {
				return nil, errors.New("response lost")
			}
		} else {
			if len(r.nextTables) != 0 {
				r.table, r.nextTables = append([]byte(nil), r.nextTables[0]...), r.nextTables[1:]
			} else if len(r.nextTable) != 0 {
				r.table = append([]byte(nil), r.nextTable...)
				r.nextTable = nil
			} else {
				r.table = append([]byte(nil), stdin...)
			}
			if r.failAfterApply {
				return nil, errors.New("response lost")
			}
		}
	}
	return nil, nil
}

func (r *recordingRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if len(stdin) > 0 {
		r.stdinDigests = append(r.stdinDigests, token(string(stdin)))
	}
	if r.event != nil {
		r.event(argv)
	}
	if len(argv) == 9 && argv[0] == "exec" && argv[2] == "redis-cli" && argv[3] == "--raw" && argv[4] == "-h" && argv[5] == "127.0.0.1" && argv[6] == "-p" && argv[8] == "ping" {
		return []byte("PONG\n"), nil
	}
	text := string(stdin)
	if len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && strings.Contains(text, "'roles'") {
		op := catalogOperation(text, ":roles:")
		if r.postgresMarkers == nil {
			return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "roles", "absent", "-", "0", "0", "0", "0", "0", "0", "0", "1"}), nil
		}
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "roles", "target", op, "1", "1", "1", "1", "1", "1", "1", "0"}), nil
	}
	if len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && strings.Contains(text, "'dbref'") {
		db, key, op := postgresSQLQuotedAfter(text, "d.datname='"), "", catalogOperation(text, ":database:")
		key = postgresCatalogProtocolDatabaseToken(db)
		if r.postgresFinalizeSeen || r.postgresFinalized != nil && r.postgresFinalized[db] {
			return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "target", op, "1", "0"}), nil
		}
		if r.postgresCreateSeen || r.postgresCreated != nil && r.postgresCreated[db] {
			return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "create-only", "-", "0", "1"}), nil
		}
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbref", key, "absent", "-", "0", "0"}), nil
	}
	if len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && strings.Contains(text, "'dbdetail'") {
		db := postgresSQLQuotedAfter(text, "d.datname='")
		key, op := postgresCatalogProtocolDatabaseToken(db), catalogOperation(text, ":database:")
		if r.postgresFinalizeSeen || r.postgresFinalized != nil && r.postgresFinalized[db] {
			return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "target", op, "1", "1", "1", "1", "1", "1"}), nil
		}
		return postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "create-only", "-", "1", "1"}), nil
	}
	if len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && strings.HasPrefix(text, "SELECT CASE WHEN") {
		database := postgresSQLQuotedAfter(string(stdin), "d.datname='")
		if database == "" {
			database = "-"
		}
		if r.postgresMarkers == nil || len(r.postgresMarkers) == 0 {
			if database == "-" {
				return []byte("prior\t-\n"), nil
			}
			return []byte("prior\t" + database + ":prior\n"), nil
		}
		if database == "-" {
			return []byte("exact\t-\n"), nil
		}
		if r.postgresDatabaseMark != "" {
			return []byte("exact\t" + database + ":exact\n"), nil
		}
		return []byte("exact\t" + database + ":prior\n"), nil
	}
	if len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && strings.Contains(text, "COMMENT ON ROLE") {
		marker := postgresSQLQuotedAfter(string(stdin), "s2hpg2:")
		if r.postgresMarkers == nil {
			r.postgresMarkers = map[string]string{}
		}
		r.postgresMarkers[marker] = "exact"
	}
	if len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && strings.Contains(text, "CREATE DATABASE") {
		db := postgresSQLQuotedAfter(text, "datname='")
		if r.postgresCreated == nil {
			r.postgresCreated = map[string]bool{}
		}
		r.postgresCreated[db] = true
		r.postgresCreateSeen = true
	}
	if len(argv) >= 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && strings.Contains(text, "COMMENT ON DATABASE") {
		r.postgresDatabaseMark = postgresSQLQuotedAfter(string(stdin), "', '")
		db := postgresSQLQuotedAfter(text, "d.datname='")
		if db == "" {
			db = "app_db"
		}
		if r.postgresFinalized == nil {
			r.postgresFinalized = map[string]bool{}
		}
		r.postgresFinalized[db] = true
		r.postgresFinalizeSeen = true
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
	if len(argv) == 5 && argv[0] == "container" && argv[1] == "inspect" {
		bindings := map[string][]map[string]string{}
		if r.publications != nil {
			bindings = r.publications[argv[4]]
		}
		return json.Marshal(map[string]any{"HostConfig": map[string]any{"PortBindings": bindings}})
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
	if len(argv) > 1 && argv[0] == "network" && argv[1] == "inspect" {
		return []byte("172.30.0.0/16 2001:db8:30::/64\n"), nil
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
	if len(argv) == 12 && argv[0] == "exec" && argv[1] == "-i" && argv[3] == "psql" && argv[4] == "-X" && argv[5] == "-qAt" && argv[6] == "-v" && argv[7] == "ON_ERROR_STOP=1" && argv[8] == "-U" && argv[9] == "s2h_admin" && argv[10] == "-d" && argv[11] == "postgres" && strings.Contains(string(stdin), "ALTER ROLE %I PASSWORD %L") {
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
	if len(argv) > 2 && argv[0] == "exec" && argv[1] != "-i" && argv[2] == "psql" && !containsPair(argv, "-U", "s2h_admin") {
		if r.postgresEnvPaths != nil && r.postgresEnvPaths[argv[1]] != "" {
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
		if r.publications == nil {
			r.publications = map[string]map[string][]map[string]string{}
		}
		bindings := map[string][]map[string]string{}
		for i := range argv {
			if argv[i] != "-p" || i+1 == len(argv) {
				continue
			}
			address, hostPort, containerPort := "", "", ""
			publication := argv[i+1]
			if strings.HasPrefix(publication, "[") {
				end := strings.Index(publication, "]")
				if end < 0 || len(publication) <= end+2 {
					continue
				}
				address = publication[1:end]
				parts := strings.Split(publication[end+2:], ":")
				if len(parts) != 2 {
					continue
				}
				hostPort, containerPort = parts[0], strings.TrimSuffix(parts[1], "/tcp")
			} else {
				parts := strings.Split(publication, ":")
				if len(parts) != 3 {
					continue
				}
				address, hostPort, containerPort = parts[0], parts[1], strings.TrimSuffix(parts[2], "/tcp")
			}
			bindings[containerPort+"/tcp"] = append(bindings[containerPort+"/tcp"], map[string]string{"HostIp": address, "HostPort": hostPort})
		}
		r.publications[name] = bindings
		if strings.Contains(strings.Join(argv, "\x00"), "postgres:18-alpine") {
			if r.postgresCredentials == nil {
				r.postgresCredentials = map[string]string{}
				r.postgresDataTokens = map[string]string{}
				r.postgresEnvPaths = map[string]string{}
			}
			for i := range argv {
				if argv[i] == "-v" && i+1 < len(argv) && strings.HasSuffix(argv[i+1], ":/run/secrets/postgres-admin:ro") {
					r.postgresEnvPaths[name] = strings.TrimSuffix(argv[i+1], ":/run/secrets/postgres-admin:ro")
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
				r.postgresCredentials[token] = strings.TrimSuffix(string(env), "\n")
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
		delete(r.publications, argv[len(argv)-1])
	}
	return nil, failure
}
func postgresSQLQuotedAfter(sql, prefix string) string {
	start := strings.Index(sql, prefix)
	if start < 0 {
		return ""
	}
	value := sql[start+len(prefix):]
	value, _, _ = strings.Cut(value, "'")
	return value
}
func catalogOperation(sql, phase string) string {
	for rest := sql; ; {
		start := strings.Index(rest, "s2hpg2:")
		if start < 0 {
			return "-"
		}
		marker := rest[start:]
		marker, _, _ = strings.Cut(marker, "'")
		parts := strings.Split(marker, ":")
		for i := range parts {
			if parts[i] == strings.Trim(phase, ":") && i+1 < len(parts) {
				return parts[i+1]
			}
		}
		rest = rest[start+len("s2hpg2:"):]
	}
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
	const prefix = "COPY s2h_password FROM STDIN;\n"
	start := strings.Index(sql, prefix)
	if start < 0 {
		return "", false
	}
	rest := sql[start+len(prefix):]
	password, _, found := strings.Cut(rest, "\n\\.\n")
	if !found {
		return "", false
	}
	return strings.NewReplacer("\\\\", "\\", "\\t", "\t", "\\n", "\n", "\\r", "\r").Replace(password), true
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
	return hostcontract.AppTarget{ID: id, Image: image, Hostname: id + ".example", ReadinessPath: "/health", InitialAdminEmail: "admin@example.test"}
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
