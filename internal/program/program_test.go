package program

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostresource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
)

const pinnedRelease = "ghcr.io/example/sub2api-deploy@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const (
	cloudflareToken = "cloudflare-api-token-canary"
	upstashToken    = "upstash-api-key-canary"
	dnsToken        = "dns-challenge-token-canary"
	adminPassword   = "app-admin-password-canary"
	jwtSecret       = "app-jwt-secret-canary"
	totpKey         = "app-totp-key-canary"
	postgresPassword = "external-postgres-password-canary"
	redisPassword    = "external-redis-password-canary"
	upstashPassword  = "upstash-password-canary"
	upstashTwoToken  = "upstash-two-api-key-canary"
	upstashTwoPassword = "upstash-two-password-canary"
	privateFlag = "private-flag-canary"
	postgresAdmin    = "postgres-admin-password-canary"
	redisAdmin       = "redis-admin-password-canary"
)

type recordingMocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
	calls     []pulumi.MockCallArgs
}

func (m *recordingMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, args)
	m.mu.Unlock()

	state := args.Inputs.Copy()
	if args.TypeToken == "upstash:index/redisDatabase:RedisDatabase" {
		databaseName := stringValueNoFail(args.Inputs["databaseName"])
		endpoint, password := "redis.example.test", upstashPassword
		if databaseName == "app-redis-two" {
			endpoint, password = "redis-two.example.test", upstashTwoPassword
		}
		state["endpoint"] = resource.NewStringProperty(endpoint)
		state["port"] = resource.NewNumberProperty(6380)
		state["password"] = resource.MakeSecret(resource.NewStringProperty(password))
	}
	return args.Name + "-id", state, nil
}

func (m *recordingMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	m.mu.Lock()
	m.calls = append(m.calls, args)
	m.mu.Unlock()
	return nil, fmt.Errorf("unexpected Pulumi Call %q", args.Token)
}

func runRegister(t *testing.T, mocks *recordingMocks, release, config, secrets string) error {
	t.Helper()
	return pulumi.RunErr(func(ctx *pulumi.Context) error {
		return Register(ctx, release, []byte(config), []byte(secrets))
	}, pulumi.WithMocks("sub2api-environment", "canary", mocks))
}

func TestRegisterFoundationGraph(t *testing.T) {
	mocks := &recordingMocks{}
	if err := runRegister(t, mocks, pinnedRelease, managedConfig(), managedSecrets()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	assertNoCalls(t, mocks)

	cloudflare := onlyResource(t, mocks.resources, "pulumi:providers:cloudflare")
	upstashProvider := onlyResource(t, mocks.resources, "pulumi:providers:upstash")
	assertProviderToken(t, cloudflare.Inputs, "apiToken", cloudflareToken)
	assertProviderToken(t, upstashProvider.Inputs, "apiKey", upstashToken)
	assertNotContains(t, upstashProvider.Inputs, cloudflareToken)
	assertNotContains(t, cloudflare.Inputs, upstashToken)

	upstash := managedUpstash(t, mocks.resources)
	assertProvider(t, upstash, upstashProvider)
	if upstash.RegisterRPC == nil || !upstash.RegisterRPC.GetProtect() {
		t.Fatal("managed Upstash resource must be protected")
	}
	if !upstash.RegisterRPC.GetRetainOnDelete() || !boolValue(t, property(upstash.Inputs, "tls")) {
		t.Fatal("managed Upstash resource must retain on delete and enable TLS")
	}

	hosts := resourcesOfType(mocks.resources, hostresource.HostToken)
	if len(hosts) != 2 {
		t.Fatalf("Host registrations = %d, want 2", len(hosts))
	}
	alpha, bravo := hostForServer(t, hosts, "alpha"), hostForServer(t, hosts, "bravo")
	assertHostBase(t, alpha, "alpha", "alpha-ssh")
	assertHostBase(t, bravo, "bravo", "bravo-ssh")
	assertAppSchema(t, alpha, true)
	assertAppSchema(t, bravo, false)
	assertManagedApp(t, alpha, "app", "app-postgres", postgresIdentity("app-postgres", "postgres.external.test", 5433, "sub2api", "postgres.external.test"), "app-redis", redisIdentity(upstash.Name+"-id", "redis.example.test", 6380, "0"), postgresPassword, upstashPassword)
	assertManagedApp(t, bravo, "app", "app-postgres", postgresIdentity("app-postgres", "postgres.external.test", 5433, "sub2api", "postgres.external.test"), "app-redis", redisIdentity(upstash.Name+"-id", "redis.example.test", 6380, "0"), postgresPassword, upstashPassword)
	assertHostSecrets(t, alpha, []string{dnsToken, adminPassword, jwtSecret, totpKey, privateFlag, postgresPassword, upstashPassword}, []string{cloudflareToken, upstashToken, postgresAdmin, redisAdmin, redisPassword})
	assertHostSecrets(t, bravo, []string{dnsToken, jwtSecret, totpKey, privateFlag, postgresPassword, upstashPassword}, []string{adminPassword, cloudflareToken, upstashToken, postgresAdmin, redisAdmin, redisPassword})
	assertHostDependsOn(t, bravo, alpha)
	assertHostDoesNotDependOn(t, alpha, bravo)
	assertPropertyDependency(t, alpha, upstash)
	assertPropertyDependency(t, bravo, upstash)

	dns := resourcesOfType(mocks.resources, "cloudflare:index/dnsRecord:DnsRecord")
	if len(dns) != 2 {
		t.Fatalf("Cloudflare DNS registrations = %d, want 2 public-server records", len(dns))
	}
	seenHosts := map[string]bool{}
	for _, record := range dns {
		assertProvider(t, record, cloudflare)
		assertDNSInputs(t, record)
		dependencies := directDependencies(record)
		dependsAlpha, dependsBravo := containsURN(dependencies, alpha), containsURN(dependencies, bravo)
		if dependsAlpha == dependsBravo {
			t.Fatalf("DNS record %q must depend on exactly one corresponding Host: %v", record.Name, dependencies)
		}
		if dependsAlpha { seenHosts[alpha.Name] = true }
		if dependsBravo { seenHosts[bravo.Name] = true }
		if dependsAlpha { assertDNSContent(t, record, "198.51.100.11") }
		if dependsBravo { assertDNSContent(t, record, "198.51.100.12") }
	}
	if !seenHosts[alpha.Name] || !seenHosts[bravo.Name] {
		t.Fatalf("DNS publication dependencies do not cover both public Hosts: %#v", seenHosts)
	}

	for _, forbidden := range []string{"command:local:Command", "sub2api:host:Edge", "sub2api:host:Site", "neon:provider:Project"} {
		assertCount(t, mocks.resources, forbidden, 0)
	}
	for _, item := range mocks.resources {
		if strings.HasPrefix(item.TypeToken, "sub2api:") && item.TypeToken != hostresource.HostToken {
			t.Fatalf("unexpected custom resource %q", item.TypeToken)
		}
		if item.TypeToken != "pulumi:providers:cloudflare" && item.TypeToken != "pulumi:providers:upstash" {
			assertNotContains(t, item.Inputs, cloudflareToken, upstashToken)
		}
	}
}

func TestRegisterRejectsBeforeRegistration(t *testing.T) {
	for _, tc := range []struct{ name, release, config, secrets string }{
		{"strict YAML", pinnedRelease, managedConfig() + "\nunknown: true\n", managedSecrets()},
		{"empty artifact", "", managedConfig(), managedSecrets()},
		{"unpinned artifact", "ghcr.io/example/sub2api-deploy:latest", managedConfig(), managedSecrets()},
		{"duplicate hostname", pinnedRelease, duplicateHostnameConfig(), duplicateHostnameSecrets()},
		{"managed Neon", pinnedRelease, managedNeonConfig(), managedNeonSecrets()},
		{"Cloudflare load balancer", pinnedRelease, strings.Replace(managedConfig(), "mode: dns\n        connectBy: publicAddress", "mode: loadBalancer\n        connectBy: publicAddress", 1), managedSecrets()},
		{"Cloudflare tunnel", pinnedRelease, strings.Replace(managedConfig(), "mode: dns\n        connectBy: publicAddress", "mode: tunnel", 1), managedSecrets()},
		{"cross Host docker PostgreSQL", pinnedRelease, crossHostDockerConfig("postgres"), crossHostDockerSecrets("postgres")},
		{"cross Host docker Redis", pinnedRelease, crossHostDockerConfig("redis"), crossHostDockerSecrets("redis")},
		{"zero App drain timeout", pinnedRelease, appDrainTimeoutConfig("0s"), externalSecrets()},
		{"sub-second App drain timeout", pinnedRelease, appDrainTimeoutConfig("500ms"), externalSecrets()},
		{"over-maximum App drain timeout", pinnedRelease, appDrainTimeoutConfig("10m1s"), externalSecrets()},
		// PostgreSQL and Redis share Host link and local-service namespaces until the Host contract carries qualified names.
		{"cross-kind Docker service ID collision", pinnedRelease, crossKindDockerCollisionConfig(), crossKindDockerCollisionSecrets()},
		{"external PostgreSQL TLS disable", pinnedRelease, strings.Replace(externalConfig(), "tls: {mode: verify-full}", "tls: {mode: disable}", 1), externalSecrets()},
		{"enabled outbound proxy", pinnedRelease, enabledOutboundProxyConfig(), enabledOutboundProxySecrets()},
		{"server singBox", pinnedRelease, singBoxConfig(), externalSecrets()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mocks := &recordingMocks{}
			if err := runRegister(t, mocks, tc.release, tc.config, tc.secrets); err == nil {
				t.Fatal("Register() error = nil, want pre-registration rejection")
			}
			if len(mocks.resources) != 0 || len(mocks.calls) != 0 {
				t.Fatalf("rejected input registered resources/calls: resources=%d calls=%d", len(mocks.resources), len(mocks.calls))
			}
		})
	}
}

func TestRegisterProjectsSameHostDockerDataOnlyToOwner(t *testing.T) {
	mocks := &recordingMocks{}
	if err := runRegister(t, mocks, pinnedRelease, sameHostDockerConfig(), dockerSecrets()); err != nil { t.Fatal(err) }
	assertNoCalls(t, mocks)
	assertCount(t, mocks.resources, hostresource.HostToken, 2)
	assertCount(t, mocks.resources, "upstash:index/redisDatabase:RedisDatabase", 0)
	assertCount(t, mocks.resources, "neon:provider:Project", 0)
	owner, other := hostForServer(t, resourcesOfType(mocks.resources, hostresource.HostToken), "alpha"), hostForServer(t, resourcesOfType(mocks.resources, hostresource.HostToken), "bravo")
	assertLocalServices(t, owner)
	assertAppSchema(t, owner, true)
	assertManagedApp(t, owner, "app", "app-postgres", postgresIdentity("app-postgres", "postgres", 5432, "sub2api", "postgres"), "app-redis", redisIdentity("app-redis", "redis", 6379, "0"), "app-postgres-password-canary", "app-redis-password-canary")
	assertHostSecrets(t, owner, []string{dnsToken, adminPassword, jwtSecret, totpKey, "app-postgres-password-canary", "app-redis-password-canary", postgresAdmin, redisAdmin}, []string{cloudflareToken, upstashToken, postgresPassword, redisPassword})
	assertNoApps(t, other)
	assertHostSecrets(t, other, []string{dnsToken}, []string{adminPassword, jwtSecret, totpKey, "app-postgres-password-canary", "app-redis-password-canary", postgresAdmin, redisAdmin, cloudflareToken, upstashToken, postgresPassword, redisPassword})
	assertNoRuntimeSecrets(t, other)
	assertKnownHostContract(t, owner)
	assertKnownHostContract(t, other)
}

func TestRegisterProjectsExternalDataWithoutCloudResources(t *testing.T) {
	mocks := &recordingMocks{}
	if err := runRegister(t, mocks, pinnedRelease, externalConfig(), externalSecrets()); err != nil { t.Fatal(err) }
	assertNoCalls(t, mocks)
	for _, token := range []string{"upstash:index/redisDatabase:RedisDatabase", "neon:provider:Project"} { assertCount(t, mocks.resources, token, 0) }
	for _, host := range resourcesOfType(mocks.resources, hostresource.HostToken) {
		assertManagedApp(t, host, "app", "app-postgres", postgresIdentity("app-postgres", "postgres.external.test", 5433, "sub2api", "postgres.external.test"), "app-redis", redisIdentity("app-redis", "redis.external.test", 6381, "0"), postgresPassword, redisPassword)
		assertAppSchema(t, host, hostForServer(t, resourcesOfType(mocks.resources, hostresource.HostToken), "alpha").Name == host.Name)
		assertKnownHostContract(t, host)
	}
}

func TestRegisterCreatesSeparateUpstashProvidersForDistinctServiceKeys(t *testing.T) {
	mocks := &recordingMocks{}
	if err := runRegister(t, mocks, pinnedRelease, twoUpstashConfig(), twoUpstashSecrets()); err != nil { t.Fatal(err) }
	providers := resourcesOfType(mocks.resources, "pulumi:providers:upstash")
	databases := resourcesOfType(mocks.resources, "upstash:index/redisDatabase:RedisDatabase")
	if len(providers) != 2 || len(databases) != 2 { t.Fatalf("Upstash providers/databases = %d/%d, want 2/2", len(providers), len(databases)) }
	byDatabase := map[string]pulumi.MockResourceArgs{}
	for _, database := range databases { byDatabase[stringValue(t, property(database.Inputs, "databaseName"))] = database }
	for _, want := range []struct { database, token, password, app string }{{"app-redis", upstashToken, upstashPassword, "app"}, {"app-redis-two", upstashTwoToken, upstashTwoPassword, "app-two"}} {
		database := byDatabase[want.database]
		provider := providerFor(t, database, providers)
		assertProviderToken(t, provider.Inputs, "apiKey", want.token)
		host := hostForServer(t, resourcesOfType(mocks.resources, hostresource.HostToken), map[string]string{"app": "alpha", "app-two": "bravo"}[want.app])
		assertManagedApp(t, host, want.app, "app-postgres", postgresIdentity("app-postgres", "postgres.external.test", 5433, map[string]string{"app": "sub2api", "app-two": "sub2api-two"}[want.app], "postgres.external.test"), want.database, redisIdentity(database.Name+"-id", map[string]string{"app-redis": "redis.example.test", "app-redis-two": "redis-two.example.test"}[want.database], 6380, map[string]string{"app-redis": "0", "app-redis-two": "1"}[want.database]), postgresPassword, want.password)
	}
}

func TestRegisterMaintenanceKeepsManagedAndLocalData(t *testing.T) {
	for _, tc := range []struct { name, config, secrets string; managed bool }{{"managed", managedMaintenanceConfig(), managedNoPublicSecrets(), true}, {"docker", dockerMaintenanceConfig(), dockerSecrets(), false}} {
		t.Run(tc.name, func(t *testing.T) {
			mocks := &recordingMocks{}
			if err := runRegister(t, mocks, pinnedRelease, tc.config, tc.secrets); err != nil { t.Fatal(err) }
			assertCount(t, mocks.resources, "cloudflare:index/dnsRecord:DnsRecord", 0)
			for _, host := range resourcesOfType(mocks.resources, hostresource.HostToken) { assertNoApps(t, host); assertKnownHostContract(t, host) }
			if tc.managed { database := managedUpstash(t, mocks.resources); if !database.RegisterRPC.GetProtect() || !database.RegisterRPC.GetRetainOnDelete() { t.Fatal("maintenance must retain protected managed Redis") } } else { assertLocalServices(t, hostForServer(t, resourcesOfType(mocks.resources, hostresource.HostToken), "alpha")) }
		})
	}
}

func TestRegisterPreservesComputedUpstashOutputs(t *testing.T) {
	config, err := environment.ParseConfig([]byte(managedConfig()))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := environment.ParseSecrets([]byte(managedSecrets()))
	if err != nil {
		t.Fatal(err)
	}
	validated, err := environment.Validate(config, secrets)
	if err != nil {
		t.Fatal(err)
	}
	unknownString := pulumi.UnsafeUnknownOutput(nil).ApplyT(func(any) string { panic("unknown value must not be resolved") }).(pulumi.StringOutput)
	unknownInt := pulumi.UnsafeUnknownOutput(nil).ApplyT(func(any) int { panic("unknown value must not be resolved") }).(pulumi.IntOutput)
	unknownPassword := pulumi.ToSecret(unknownString).(pulumi.StringOutput)
	managed := map[string]managedRedisInputs{
		"app-redis": {
			ProviderID: unknownString,
			Endpoint:   unknownString,
			Port:       unknownInt,
			Password:   unknownPassword,
		},
	}
	for _, output := range []struct {
		name   string
		value  pulumi.Output
		secret bool
	}{
		{"provider ID", unknownString, false},
		{"endpoint", unknownString, false},
		{"port", unknownInt, false},
		{"password", unknownPassword, true},
		{"Host target", pulumi.ToOutput(hostTarget(validated.Config, pinnedRelease, "alpha", managed)), false},
		{"Host secrets", pulumi.ToOutput(hostSecrets(validated.Config, secrets, "alpha", managed)), true},
	} {
		result, err := internals.UnsafeAwaitOutput(t.Context(), output.value)
		if err != nil {
			t.Fatalf("await %s: %v", output.name, err)
		}
		if result.Known || result.Secret != output.secret || result.Value != nil {
			t.Fatalf("%s metadata = %#v, want unknown secret=%t nil value", output.name, result, output.secret)
		}
	}
}

func TestRegisterMaintenancePlacementKeepsHostsAndSuppressesPublication(t *testing.T) {
	mocks := &recordingMocks{}
	if err := runRegister(t, mocks, pinnedRelease, maintenanceConfig(), externalSecrets()); err != nil { t.Fatal(err) }
	assertNoCalls(t, mocks)
	assertCount(t, mocks.resources, hostresource.HostToken, 2)
	assertCount(t, mocks.resources, "cloudflare:index/dnsRecord:DnsRecord", 0)
	for _, host := range resourcesOfType(mocks.resources, hostresource.HostToken) { assertNoApps(t, host) }
}

func TestRegisterIsDeterministicAcrossYAMLMapOrder(t *testing.T) {
	first, second := &recordingMocks{}, &recordingMocks{}
	if err := runRegister(t, first, pinnedRelease, externalConfig(), externalSecrets()); err != nil { t.Fatal(err) }
	if err := runRegister(t, second, pinnedRelease, reorderedExternalConfig(), externalSecrets()); err != nil { t.Fatal(err) }
	if got, want := canonicalSnapshot(first.resources), canonicalSnapshot(second.resources); got != want {
		t.Fatalf("map ordering changed public resource snapshot\nfirst:  %s\nsecond: %s", got, want)
	}
}

type dataIdentity struct { providerID, endpoint string; port int; database, tls string }
func postgresIdentity(providerID, endpoint string, port int, database, tls string) dataIdentity { return dataIdentity{providerID, endpoint, port, database, tls} }
func redisIdentity(providerID, endpoint string, port int, database string) dataIdentity { return dataIdentity{providerID, endpoint, port, database, ""} }

func resourcesOfType(all []pulumi.MockResourceArgs, token string) []pulumi.MockResourceArgs { var out []pulumi.MockResourceArgs; for _, item := range all { if item.TypeToken == token { out = append(out, item) } }; return out }
func onlyResource(t *testing.T, all []pulumi.MockResourceArgs, token string) pulumi.MockResourceArgs { t.Helper(); matches := resourcesOfType(all, token); if len(matches) != 1 { t.Fatalf("%s registrations = %d, want 1", token, len(matches)) }; return matches[0] }
func managedUpstash(t *testing.T, all []pulumi.MockResourceArgs) pulumi.MockResourceArgs { t.Helper(); matches := resourcesOfType(all, "upstash:index/redisDatabase:RedisDatabase"); if len(matches) != 1 { t.Fatalf("managed Upstash registrations = %d, want 1", len(matches)) }; return matches[0] }
func hostForServer(t *testing.T, hosts []pulumi.MockResourceArgs, server string) pulumi.MockResourceArgs { t.Helper(); for _, host := range hosts { if stringValue(t, property(object(t, host.Inputs, "resource"), "serverKey")) == server { return host } }; t.Fatalf("Host for serverKey %q not found", server); return pulumi.MockResourceArgs{} }
func object(t *testing.T, values resource.PropertyMap, key string) resource.PropertyMap { t.Helper(); value, ok := values[resource.PropertyKey(key)]; value = unwrap(value); if !ok || !value.IsObject() { t.Fatalf("%q is not an object: %v", key, value) }; return value.ObjectValue() }
func assertCount(t *testing.T, all []pulumi.MockResourceArgs, token string, want int) { t.Helper(); if got := len(resourcesOfType(all, token)); got != want { t.Fatalf("%s registrations = %d, want %d", token, got, want) } }
func assertNoCalls(t *testing.T, mocks *recordingMocks) { t.Helper(); if len(mocks.calls) != 0 { t.Fatalf("Pulumi Call count = %d, want 0", len(mocks.calls)) } }
func providerReference(provider pulumi.MockResourceArgs) string {
	return "urn:pulumi:canary::sub2api-environment::" + provider.TypeToken + "::" + provider.Name + "::" + provider.Name + "-id"
}

func assertProvider(t *testing.T, dependent, provider pulumi.MockResourceArgs) {
	t.Helper()
	want := providerReference(provider)
	if dependent.RegisterRPC == nil || dependent.RegisterRPC.GetProvider() != want {
		t.Fatalf("%s %q provider = %q, want %q", dependent.TypeToken, dependent.Name, dependent.RegisterRPC.GetProvider(), want)
	}
}
func directDependencies(item pulumi.MockResourceArgs) []string { if item.RegisterRPC == nil { return nil }; return item.RegisterRPC.GetDependencies() }
func containsURN(dependencies []string, resource pulumi.MockResourceArgs) bool {
	for _, dependency := range dependencies {
		if strings.HasSuffix(dependency, "::"+resource.Name) {
			return true
		}
	}
	return false
}
func assertHostDependsOn(t *testing.T, dependent, prerequisite pulumi.MockResourceArgs) { t.Helper(); if !containsURN(directDependencies(dependent), prerequisite) { t.Fatalf("Host %q must depend on Host %q: %v", dependent.Name, prerequisite.Name, directDependencies(dependent)) } }
func assertHostDoesNotDependOn(t *testing.T, dependent, other pulumi.MockResourceArgs) { t.Helper(); if containsURN(directDependencies(dependent), other) { t.Fatalf("Host %q must not depend on Host %q", dependent.Name, other.Name) } }
func assertPropertyDependency(t *testing.T, dependent, prerequisite pulumi.MockResourceArgs) { t.Helper(); if dependent.RegisterRPC == nil { t.Fatalf("%q lacks RegisterRPC", dependent.Name) }; for _, dependencies := range dependent.RegisterRPC.GetPropertyDependencies() { if containsURN(dependencies.GetUrns(), prerequisite) { return } }; t.Fatalf("%q inputs lack a property dependency on %q", dependent.Name, prerequisite.Name) }
func assertHostBase(t *testing.T, host pulumi.MockResourceArgs, server, alias string) { t.Helper(); if stringValue(t, property(object(t, host.Inputs, "resource"), "serverKey")) != server || stringValue(t, property(object(t, host.Inputs, "server"), "sshAlias")) != alias { t.Fatalf("Host input identity = %v", host.Inputs) }; for _, key := range []string{"resource", "server", "target", "secrets"} { if _, ok := host.Inputs[resource.PropertyKey(key)]; !ok { t.Fatalf("Host %q missing %q", server, key) } }; if !host.Inputs["secrets"].IsSecret() { t.Fatalf("Host %q secrets are not secret", server) } }
func property(values resource.PropertyMap, key string) resource.PropertyValue { return values[resource.PropertyKey(key)] }
func unwrap(value resource.PropertyValue) resource.PropertyValue { for value.IsSecret() { value = value.SecretValue().Element }; return value }
func stringValue(t *testing.T, value resource.PropertyValue) string { t.Helper(); value = unwrap(value); if value.IsComputed() || !value.IsString() { t.Fatalf("value is not a known string: %v", value) }; return value.StringValue() }
func boolValue(t *testing.T, value resource.PropertyValue) bool { t.Helper(); value = unwrap(value); if value.IsComputed() || !value.IsBool() { t.Fatalf("value is not a known bool: %v", value) }; return value.BoolValue() }
func optionalBool(t *testing.T, value resource.PropertyValue) bool { t.Helper(); if value.V == nil { return false }; return boolValue(t, value) }
func appTarget(t *testing.T, inputs resource.PropertyMap, id string) resource.PropertyMap { t.Helper(); apps := unwrap(property(object(t, inputs, "target"), "apps")); if !apps.IsArray() { t.Fatalf("target.apps is not an array: %v", apps) }; for _, app := range apps.ArrayValue() { if stringValue(t, property(app.ObjectValue(), "id")) == id { return app.ObjectValue() } }; t.Fatalf("App %q absent from target", id); return nil }
func dataLink(t *testing.T, app resource.PropertyMap, name string) resource.PropertyMap { t.Helper(); links := unwrap(property(app, "dataLinks")); if !links.IsArray() { t.Fatalf("dataLinks is not an array: %v", links) }; for _, link := range links.ArrayValue() { if stringValue(t, property(link.ObjectValue(), "name")) == name { return link.ObjectValue() } }; t.Fatalf("data link %q absent", name); return nil }
func assertIdentity(t *testing.T, link resource.PropertyMap, kind string, want dataIdentity) { t.Helper(); identity := object(t, link, "identity"); got := dataIdentity{stringValue(t, property(identity, "providerId")), stringValue(t, property(identity, "endpoint")), int(unwrap(property(identity, "port")).NumberValue()), stringValue(t, property(identity, "database")), optionalString(t, property(identity, "tlsServerName"))}; if stringValue(t, property(identity, "kind")) != kind || got != want { t.Fatalf("%s identity = %#v / %s, want %#v / %s", stringValue(t, property(link, "name")), got, stringValue(t, property(identity, "kind")), want, kind) } }
func optionalString(t *testing.T, value resource.PropertyValue) string { t.Helper(); if value.V == nil { return "" }; return stringValue(t, value) }
func appCredentials(t *testing.T, inputs resource.PropertyMap, appID, kind string) resource.PropertyMap { t.Helper(); secrets := unwrap(property(inputs, "secrets")); if !secrets.IsObject() { t.Fatalf("Host secrets are not an object: %v", secrets) }; apps := object(t, secrets.ObjectValue(), "apps"); return object(t, apps, appID)[resource.PropertyKey(kind)].ObjectValue() }
func assertManagedApp(t *testing.T, host pulumi.MockResourceArgs, appID, postgresName string, postgres dataIdentity, redisName string, redis dataIdentity, postgresPassword, redisPassword string) { t.Helper(); app := appTarget(t, host.Inputs, appID); assertIdentity(t, dataLink(t, app, postgresName), "postgres", postgres); assertIdentity(t, dataLink(t, app, redisName), "redis", redis); pg, rd := appCredentials(t, host.Inputs, appID, "postgres"), appCredentials(t, host.Inputs, appID, "redis"); if stringValue(t, property(pg, "username")) != "appuser" || stringValue(t, property(pg, "password")) != postgresPassword || stringValue(t, property(rd, "username")) != "default" || stringValue(t, property(rd, "password")) != redisPassword { t.Fatalf("App credentials are not exact: postgres=%v redis=%v", pg, rd) } }
func assertAppSchema(t *testing.T, host pulumi.MockResourceArgs, bootstrap bool) { t.Helper(); app := appTarget(t, host.Inputs, "app"); if stringValue(t, property(app, "readinessPath")) != "/ready" || stringValue(t, property(app, "drainTimeout")) != "10s" { t.Fatalf("App runtime defaults = %v", app) }; settings := object(t, app, "runtimeSettings"); if stringValue(t, property(settings, "FEATURE_FLAG")) != "enabled" || stringValue(t, property(settings, "ADMIN_EMAIL")) != "admin@example.test" { t.Fatalf("App runtime settings = %v", settings) }; if bootstrap != optionalBool(t, property(app, "initialBootstrap")) { t.Fatalf("initialBootstrap = %v, want %t", property(app, "initialBootstrap"), bootstrap) }; secrets := unwrap(property(host.Inputs, "secrets")).ObjectValue(); appSecrets := object(t, object(t, secrets, "apps"), "app"); runtime := appSecrets["runtimeEnvironment"].ObjectValue(); if stringValue(t, property(runtime, "PRIVATE_FLAG")) != privateFlag { t.Fatalf("App private environment = %v", runtime) }; if bootstrap != (property(appSecrets, "initialAdminPassword").V != nil) { t.Fatalf("initial admin secret scope = %v, want bootstrap=%t", appSecrets, bootstrap) } }
func assertDNSInputs(t *testing.T, record pulumi.MockResourceArgs) { t.Helper(); inputs := record.Inputs; if stringValue(t, property(inputs, "name")) != "app.example.test" || stringValue(t, property(inputs, "type")) != "A" || stringValue(t, property(inputs, "zoneId")) != "zone-canary" || !unwrap(property(inputs, "proxied")).BoolValue() || int(unwrap(property(inputs, "ttl")).NumberValue()) != 1 { t.Fatalf("DNS inputs = %v", inputs) } }
func assertDNSContent(t *testing.T, record pulumi.MockResourceArgs, want string) { t.Helper(); if stringValue(t, property(record.Inputs, "content")) != want { t.Fatalf("DNS content = %q, want %q", stringValue(t, property(record.Inputs, "content")), want) } }
func providerFor(t *testing.T, dependent pulumi.MockResourceArgs, providers []pulumi.MockResourceArgs) pulumi.MockResourceArgs { t.Helper(); for _, provider := range providers { if dependent.RegisterRPC.GetProvider() == providerReference(provider) { return provider } }; t.Fatalf("no recorded provider matches %q", dependent.RegisterRPC.GetProvider()); return pulumi.MockResourceArgs{} }
func assertKnownHostContract(t *testing.T, host pulumi.MockResourceArgs) { t.Helper(); target, secrets := decodeKnownHost(t, host); if err := hostcontract.ValidateTarget(target, secrets); err != nil { t.Fatalf("Host %q violates host contract: %v", host.Name, err) } }
func decodeKnownHost(t *testing.T, host pulumi.MockResourceArgs) (hostcontract.Target, hostcontract.Secrets) { t.Helper(); targetJSON := propertyJSON(t, property(host.Inputs, "target")); secretsJSON := propertyJSON(t, property(host.Inputs, "secrets")); var target hostcontract.Target; var secrets hostcontract.Secrets; if err := json.Unmarshal(targetJSON, &target); err != nil { t.Fatalf("decode Host target: %v", err) }; if err := json.Unmarshal(secretsJSON, &secrets); err != nil { t.Fatalf("decode Host secrets: %v", err) }; return target, secrets }
func propertyJSON(t *testing.T, value resource.PropertyValue) []byte { t.Helper(); value = unwrap(value); if containsComputed(value) { t.Fatalf("cannot decode computed Host input: %v", value) }; normalized := normalizeProperty(value); bytes, err := json.Marshal(normalized); if err != nil { t.Fatalf("encode Host input: %v", err) }; return bytes }
func containsComputed(value resource.PropertyValue) bool { value = unwrap(value); if value.IsComputed() { return true }; if value.IsArray() { for _, child := range value.ArrayValue() { if containsComputed(child) { return true } } }; if value.IsObject() { for _, child := range value.ObjectValue() { if containsComputed(child) { return true } } }; return false }
func normalizeProperty(value resource.PropertyValue) any { value = unwrap(value); if value.IsArray() { result := make([]any, len(value.ArrayValue())); for i, child := range value.ArrayValue() { result[i] = normalizeProperty(child) }; return result }; if value.IsObject() { result := map[string]any{}; for key, child := range value.ObjectValue() { result[string(key)] = normalizeProperty(child) }; return result }; return value.V }
func assertLocalServices(t *testing.T, host pulumi.MockResourceArgs) { t.Helper(); services := unwrap(property(object(t, host.Inputs, "target"), "dataServices")); if !services.IsArray() || len(services.ArrayValue()) != 2 { t.Fatalf("local data services = %v, want two", services) }; byID := map[string]resource.PropertyMap{}; for _, service := range services.ArrayValue() { byID[stringValue(t, property(service.ObjectValue(), "id"))] = service.ObjectValue() }; pg, rd := byID["app-postgres"], byID["app-redis"]; if stringValue(t, property(pg, "type")) != "postgres" || int(unwrap(property(pg, "port")).NumberValue()) != 5432 || stringValue(t, property(rd, "type")) != "redis" || int(unwrap(property(rd, "port")).NumberValue()) != 6379 || !unwrap(property(rd, "persistence")).BoolValue() { t.Fatalf("local data services = %v", byID) }; secrets := unwrap(property(host.Inputs, "secrets")).ObjectValue(); local := object(t, secrets, "localDataServices"); if len(local) != 2 || stringValue(t, property(object(t, local, "app-postgres"), "adminPassword")) != postgresAdmin || stringValue(t, property(object(t, local, "app-redis"), "adminPassword")) != redisAdmin { t.Fatalf("local data service secrets = %v", local) } }
func assertNoApps(t *testing.T, host pulumi.MockResourceArgs) { t.Helper(); target := object(t, host.Inputs, "target"); if apps, ok := target["apps"]; ok { apps = unwrap(apps); if apps.IsArray() && len(apps.ArrayValue()) != 0 { t.Fatalf("Host %q unexpectedly projects Apps: %v", host.Name, apps) } } }
func assertNoRuntimeSecrets(t *testing.T, host pulumi.MockResourceArgs) { t.Helper(); secrets := unwrap(property(host.Inputs, "secrets")); if !secrets.IsObject() { t.Fatalf("Host %q secrets are not an object", host.Name) }; for _, key := range []string{"apps", "localDataServices"} { if value, ok := secrets.ObjectValue()[resource.PropertyKey(key)]; ok && unwrap(value).IsObject() && len(unwrap(value).ObjectValue()) != 0 { t.Fatalf("Host %q must not receive %s secrets: %v", host.Name, key, value) } } }
func assertHostSecrets(t *testing.T, host pulumi.MockResourceArgs, present, absent []string) { t.Helper(); secrets := property(host.Inputs, "secrets"); for _, canary := range present { if !containsProperty(secrets, canary) { t.Fatalf("Host %q lacks required secret %q", host.Name, canary) } }; for _, canary := range absent { if containsProperty(secrets, canary) { t.Fatalf("Host %q leaked forbidden secret %q", host.Name, canary) } } }
func assertProviderToken(t *testing.T, inputs resource.PropertyMap, key, canary string) { t.Helper(); value := property(inputs, key); if !containsSecretProperty(value, canary) || occurrences(value, canary) != 1 { t.Fatalf("provider %s must contain exactly one secret %q: %v", key, canary, value) } }
func assertNotContains(t *testing.T, inputs resource.PropertyMap, canaries ...string) { t.Helper(); for _, canary := range canaries { if containsProperty(resource.NewObjectProperty(inputs), canary) { t.Fatalf("secret %q leaked into ordinary resource input", canary) } } }
func containsProperty(value resource.PropertyValue, want string) bool { if value.IsString() { return value.StringValue() == want }; if value.IsSecret() { return containsProperty(value.SecretValue().Element, want) }; if value.IsComputed() { return false }; if value.IsArray() { for _, child := range value.ArrayValue() { if containsProperty(child, want) { return true } } }; if value.IsObject() { for _, child := range value.ObjectValue() { if containsProperty(child, want) { return true } } }; return false }
func containsSecretProperty(value resource.PropertyValue, want string) bool { if value.IsSecret() { return containsProperty(value.SecretValue().Element, want) || containsSecretProperty(value.SecretValue().Element, want) }; if value.IsArray() { for _, child := range value.ArrayValue() { if containsSecretProperty(child, want) { return true } } }; if value.IsObject() { for _, child := range value.ObjectValue() { if containsSecretProperty(child, want) { return true } } }; return false }
func occurrences(value resource.PropertyValue, want string) int { if value.IsString() { if value.StringValue() == want { return 1 }; return 0 }; if value.IsSecret() { return occurrences(value.SecretValue().Element, want) }; if value.IsComputed() { return 0 }; total := 0; if value.IsArray() { for _, child := range value.ArrayValue() { total += occurrences(child, want) } }; if value.IsObject() { for _, child := range value.ObjectValue() { total += occurrences(child, want) } }; return total }

type snapshotResource struct { Type, Identity, Provider string; Protect, Retain bool; Inputs string; Dependencies []string }
func canonicalSnapshot(resources []pulumi.MockResourceArgs) string { var snapshot []snapshotResource; for _, item := range resources { snapshot = append(snapshot, snapshotResource{item.TypeToken, semanticIdentity(item), providerToken(item, resources), item.RegisterRPC.GetProtect(), item.RegisterRPC.GetRetainOnDelete(), sanitize(item.Inputs), normalizedDependencies(item, resources)}) }; sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Type+snapshot[i].Identity < snapshot[j].Type+snapshot[j].Identity }); bytes, _ := json.Marshal(snapshot); return string(bytes) }
func semanticIdentity(item pulumi.MockResourceArgs) string { if item.TypeToken == hostresource.HostToken { if value, ok := item.Inputs["resource"]; ok && unwrap(value).IsObject() { return stringValueNoFail(unwrap(value).ObjectValue()["serverKey"]) } }; if id, ok := item.Inputs["name"]; ok && unwrap(id).IsString() { return stringValueNoFail(unwrap(id)) }; if id, ok := item.Inputs["databaseName"]; ok && unwrap(id).IsString() { return stringValueNoFail(unwrap(id)) }; return item.Name }
func stringValueNoFail(value resource.PropertyValue) string { value = unwrap(value); if value.IsComputed() || !value.IsString() { return "<unknown>" }; return value.StringValue() }
func providerToken(item pulumi.MockResourceArgs, all []pulumi.MockResourceArgs) string { if item.RegisterRPC == nil { return "" }; for _, provider := range all { if item.RegisterRPC.GetProvider() == providerReference(provider) { return provider.TypeToken } }; return "" }
func normalizedDependencies(item pulumi.MockResourceArgs, all []pulumi.MockResourceArgs) []string { var result []string; for _, urn := range directDependencies(item) { for _, dependency := range all { if strings.Contains(urn, "::"+dependency.Name+"::") { result = append(result, dependency.TypeToken+":"+semanticIdentity(dependency)) } } }; sort.Strings(result); return result }
func sanitize(value resource.PropertyMap) string { var scrub func(resource.PropertyValue) any; scrub = func(value resource.PropertyValue) any { if value.IsSecret() { return scrub(value.SecretValue().Element) }; if value.IsComputed() { return "<computed>" }; if value.IsArray() { result := make([]any, len(value.ArrayValue())); for i, child := range value.ArrayValue() { result[i] = scrub(child) }; return result }; if value.IsObject() { result := map[string]any{}; for key, child := range value.ObjectValue() { result[string(key)] = scrub(child) }; return result }; return value.V }; result := map[string]any{}; for key, value := range value { result[string(key)] = scrub(value) }; bytes, _ := json.Marshal(result); return string(bytes) }

func managedConfig() string { return baseConfig("external", "upstash", "cloudflare") }
func externalConfig() string { return baseConfig("external", "external", "none") }
func reorderedExternalConfig() string { return strings.Replace(externalConfig(), "  alpha:\n    sshAlias: alpha-ssh\n    addresses:\n      public:\n        ipv4: 198.51.100.11\n  bravo:\n    sshAlias: bravo-ssh\n    addresses:\n      public:\n        ipv4: 198.51.100.12", "  bravo:\n    sshAlias: bravo-ssh\n    addresses:\n      public:\n        ipv4: 198.51.100.12\n  alpha:\n    sshAlias: alpha-ssh\n    addresses:\n      public:\n        ipv4: 198.51.100.11", 1) }
func maintenanceConfig() string { return strings.Replace(externalConfig(), "servers: [alpha, bravo]", "servers: []", 1) }
func managedMaintenanceConfig() string { config := strings.Replace(managedConfig(), "cloudflare:\n  zoneId: zone-canary\n", "", 1); config = strings.Replace(config, "type: cloudflare\n      servers: [alpha, bravo]\n      cloudflare:\n        mode: dns\n        connectBy: publicAddress", "type: none", 1); return strings.Replace(config, "servers: [alpha, bravo]", "servers: []", 1) }
func dockerMaintenanceConfig() string { return strings.Replace(sameHostDockerConfig(), "servers: [alpha]", "servers: []", 1) }
func appDrainTimeoutConfig(value string) string { return strings.Replace(externalConfig(), "    readinessPath: /ready\n", "    readinessPath: /ready\n    drainTimeout: "+value+"\n", 1) }
func crossKindDockerCollisionConfig() string { config := strings.Replace(sameHostDockerConfig(), "  app-postgres:\n", "  shared-data:\n", 1); config = strings.Replace(config, "  app-redis:\n", "  shared-data:\n", 1); config = strings.Replace(config, "name: app-postgres", "name: shared-data", 1); return strings.Replace(config, "name: app-redis", "name: shared-data", 1) }
func crossKindDockerCollisionSecrets() string { secrets := strings.Replace(dockerSecrets(), "  app-postgres:\n", "  shared-data:\n", 1); return strings.Replace(secrets, "  app-redis:\n", "  shared-data:\n", 1) }
func sameHostDockerConfig() string { return strings.Replace(baseConfig("docker", "docker", "none"), "servers: [alpha, bravo]", "servers: [alpha]", 1) }
func crossHostDockerConfig(kind string) string { config := baseConfig("external", "external", "none"); if kind == "postgres" { config = strings.Replace(config, "type: external\n    host: postgres.external.test\n    port: 5433\n    tls: {mode: verify-full}", "type: docker\n    server: alpha\n    port: 5432", 1) } else { config = strings.Replace(config, "type: external\n    host: redis.external.test\n    port: 6381\n    tls: true", "type: docker\n    server: alpha\n    port: 6379\n    persistence: true", 1) }; config = strings.Replace(config, "      public:\n        ipv4: 198.51.100.11", "      public:\n        ipv4: 198.51.100.11\n      internal:\n        ipv4: 10.0.0.11", 1); return strings.Replace(config, "      public:\n        ipv4: 198.51.100.12", "      public:\n        ipv4: 198.51.100.12\n      internal:\n        ipv4: 10.0.0.12", 1) }
func duplicateHostnameConfig() string { return externalConfig() + "  app-two:\n    hostname: app.example.test\n    image: ghcr.io/example/sub2api@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\n    initialAdminEmail: admin-two@example.test\n    readinessPath: /ready\n    servers: [alpha]\n    postgres: {name: app-postgres, database: sub2api-two}\n    redis: {name: app-redis, database: 1}\n    publicAccess: {type: none}\n" }
func duplicateHostnameSecrets() string { return externalSecrets() + "  app-two:\n    initialAdminPassword: app-two-admin-password-canary\n    jwtSecret: app-two-jwt-secret-canary\n    totpEncryptionKey: app-two-totp-key-canary\n    postgres: {username: appuser, password: app-two-postgres-password-canary}\n    redis: {username: default, password: app-two-redis-password-canary}\n" }
func managedNeonConfig() string { return strings.Replace(managedConfig(), "type: external\n    host: postgres.external.test\n    port: 5433\n    tls: {mode: verify-full}", "type: neon\n    region: aws-us-east-1", 1) }
func managedNeonSecrets() string { return strings.Replace(managedSecrets(), "    postgres: {username: appuser, password: external-postgres-password-canary}\n", "", 1) + "postgres:\n  app-postgres:\n    apiToken: neon-api-token-canary\n" }
func enabledOutboundProxyConfig() string { return externalConfig() + "    outboundProxy:\n      enabled: true\n      type: microsocks\n      required: true\n      servers: [alpha]\n" }
func singBoxConfig() string { return strings.Replace(externalConfig(), "    addresses:\n      public:\n        ipv4: 198.51.100.11", "    addresses:\n      public:\n        ipv4: 198.51.100.11\n    singBox:\n      serverName: proxy.example.test\n      target: proxy.example.test:443", 1) }
func twoUpstashConfig() string { config := strings.Replace(managedConfig(), "\napps:\n", "\n  app-redis-two:\n    type: upstash\n    region: us-east-1\napps:\n", 1); return config + "  app-two:\n    hostname: app-two.example.test\n    image: ghcr.io/example/sub2api@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\n    initialAdminEmail: admin-two@example.test\n    readinessPath: /ready\n    servers: [bravo]\n    postgres: {name: app-postgres, database: sub2api-two}\n    redis: {name: app-redis-two, database: 1}\n    publicAccess: {type: none}\n" }
func baseConfig(postgres, redis, access string) string { pg := "type: external\n    host: postgres.external.test\n    port: 5433\n    tls: {mode: verify-full}"; if postgres == "docker" { pg = "type: docker\n    server: alpha\n    port: 5432" }; rd := "type: external\n    host: redis.external.test\n    port: 6381\n    tls: true"; if redis == "upstash" { rd = "type: upstash\n    region: us-east-1" } else if redis == "docker" { rd = "type: docker\n    server: alpha\n    port: 6379\n    persistence: true" }; public, cloudflare := "type: none", ""; if access == "cloudflare" { public, cloudflare = "type: cloudflare\n      servers: [alpha, bravo]\n      cloudflare:\n        mode: dns\n        connectBy: publicAddress", "cloudflare:\n  zoneId: zone-canary\n" }; return "version: 1\n" + cloudflare + "reverseProxy:\n  image: traefik@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n  acmeEmail: ops@example.test\nservers:\n  alpha:\n    sshAlias: alpha-ssh\n    addresses:\n      public:\n        ipv4: 198.51.100.11\n  bravo:\n    sshAlias: bravo-ssh\n    addresses:\n      public:\n        ipv4: 198.51.100.12\npostgres:\n  app-postgres:\n    " + pg + "\nredis:\n  app-redis:\n    " + rd + "\napps:\n  app:\n    hostname: app.example.test\n    image: ghcr.io/example/sub2api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n    initialAdminEmail: admin@example.test\n    readinessPath: /ready\n    environment: {FEATURE_FLAG: enabled}\n    servers: [alpha, bravo]\n    postgres: {name: app-postgres, database: sub2api}\n    redis: {name: app-redis, database: 0}\n    publicAccess:\n      " + public + "\n" }
func managedSecrets() string { return secrets(true, false) }
func externalSecrets() string { return secrets(false, false) }
func dockerSecrets() string { return secrets(false, true) }
func managedNoPublicSecrets() string { return strings.Replace(managedSecrets(), "cloudflare:\n  apiToken: "+cloudflareToken+"\n", "", 1) }
func enabledOutboundProxySecrets() string { return externalSecrets() + "    adminApiKey: app-admin-api-key-canary\noutboundProxy:\n  alpha: {username: proxy-user-canary, password: proxy-password-canary}\n" }
func twoUpstashSecrets() string { secrets := strings.Replace(managedSecrets(), "\nredis:\n", "\n  app-two:\n    initialAdminPassword: app-two-admin-password-canary\n    jwtSecret: app-two-jwt-secret-canary\n    totpEncryptionKey: app-two-totp-key-canary\n    postgres: {username: appuser, password: "+postgresPassword+"}\nredis:\n", 1); return strings.Replace(secrets, "  app-redis:\n    apiKey: "+upstashToken+"\n", "  app-redis:\n    apiKey: "+upstashToken+"\n  app-redis-two:\n    apiKey: "+upstashTwoToken+"\n", 1) }
func crossHostDockerSecrets(local string) string { pg, rd, services := "    postgres: {username: appuser, password: " + postgresPassword + "}\n", "    redis: {username: default, password: " + redisPassword + "}\n", ""; if local == "postgres" { pg = "    postgres: {username: appuser, password: app-postgres-password-canary}\n"; services = "postgres:\n  app-postgres:\n    adminPassword: " + postgresAdmin + "\n" } else { rd = "    redis: {username: default, password: app-redis-password-canary}\n"; services = "redis:\n  app-redis:\n    adminPassword: " + redisAdmin + "\n" }; return appSecrets(pg, rd) + services }
func secrets(upstash, docker bool) string { pg, rd, services := "    postgres: {username: appuser, password: " + postgresPassword + "}\n", "    redis: {username: default, password: " + redisPassword + "}\n", ""; cloudflare := ""; if upstash { cloudflare, rd, services = "cloudflare:\n  apiToken: "+cloudflareToken+"\n", "", "redis:\n  app-redis:\n    apiKey: "+upstashToken+"\n" }; if docker { pg, rd, services = "    postgres: {username: appuser, password: app-postgres-password-canary}\n", "    redis: {username: default, password: app-redis-password-canary}\n", "postgres:\n  app-postgres:\n    adminPassword: "+postgresAdmin+"\nredis:\n  app-redis:\n    adminPassword: "+redisAdmin+"\n" }; return cloudflare + appSecrets(pg, rd) + services }
func appSecrets(pg, rd string) string { return "revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\nreverseProxy:\n  dnsChallengeToken: "+dnsToken+"\napps:\n  app:\n    initialAdminPassword: "+adminPassword+"\n    jwtSecret: "+jwtSecret+"\n    totpEncryptionKey: "+totpKey+"\n    environment: {PRIVATE_FLAG: "+privateFlag+"}\n" + pg + rd }
