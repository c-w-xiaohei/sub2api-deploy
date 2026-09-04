package hostcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func TestTargetRevisionNormalizesAndCommitsHostSemantics(t *testing.T) {
	key := RevisionKey([]byte("01234567890123456789012345678901"))
	resource := ResourceIdentity{Environment: "production", ServerKey: "edge-a"}
	target, secrets := validTarget()
	baseline := mustRevision(t, key, resource, target, secrets)

	if _, err := ParseRevision(baseline); err != nil {
		t.Fatalf("ParseRevision(%q): %v", baseline, err)
	}
	if !strings.HasPrefix(baseline, "tr1:") {
		t.Fatalf("revision = %q", baseline)
	}
	bareDigest := sha256.Sum256([]byte("CANARY_SECRET_DO_NOT_EXPOSE"))
	if strings.Contains(baseline, "CANARY_SECRET_DO_NOT_EXPOSE") || strings.Contains(baseline, hex.EncodeToString(bareDigest[:])) {
		t.Fatalf("revision exposes a secret oracle: %q", baseline)
	}

	reordered := target
	reordered.Apps = []AppTarget{target.Apps[1], target.Apps[0]}
	reordered.DataServices = []LocalDataServiceTarget{target.DataServices[1], target.DataServices[0]}
	reordered.Connectors = []TunnelConnectorTarget{target.Connectors[1], target.Connectors[0]}
	reordered.Apps[0].DataLinks = []DataLink{target.Apps[1].DataLinks[1], target.Apps[1].DataLinks[0]}
	if got := mustRevision(t, key, resource, reordered, secrets); got != baseline {
		t.Fatalf("semantic order changed revision: got %q want %q", got, baseline)
	}
	changedRelease := cloneTarget(target)
	changedRelease.ReleaseArtifact = "release@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if got := mustRevision(t, key, resource, changedRelease, secrets); got == baseline {
		t.Fatal("target field changes must change revision")
	}
	changedEmail := cloneTarget(target)
	changedEmail.Apps[0].InitialAdminEmail = "changed@example.test"
	if got := mustRevision(t, key, resource, changedEmail, secrets); got == baseline {
		t.Fatal("initial admin email change did not change revision")
	}
	if got := mustRevision(t, key, ResourceIdentity{Environment: "production", ServerKey: "edge-b"}, target, secrets); got == baseline {
		t.Fatal("resource identity did not change revision")
	}
	rotated := cloneSecrets(secrets)
	rotated.Apps["api"] = AppSecrets{JWTSecret: "rotated"}
	if got := mustRevision(t, key, resource, target, rotated); got == baseline {
		t.Fatal("secret-only change did not change revision")
	}

	minimal := Target{ReleaseArtifact: target.ReleaseArtifact, Apps: []AppTarget{{ID: "api", Image: target.Apps[1].Image, Hostname: target.Apps[1].Hostname, ReadinessPath: target.Apps[1].ReadinessPath, InitialAdminEmail: target.Apps[1].InitialAdminEmail}}}
	empty := minimal
	empty.Apps[0].RuntimeSettings = map[string]string{}
	empty.Apps[0].DataLinks = []DataLink{}
	empty.DataServices = []LocalDataServiceTarget{}
	empty.Connectors = []TunnelConnectorTarget{}
	if got := mustRevision(t, key, resource, empty, Secrets{}); got != mustRevision(t, key, resource, minimal, Secrets{}) {
		t.Fatal("nil and empty optional values did not normalize")
	}
}

func TestTargetRevisionRejectsInvalidScopeAndInputWithoutLeakingSecrets(t *testing.T) {
	key := RevisionKey([]byte("01234567890123456789012345678901"))
	resource := ResourceIdentity{Environment: "production", ServerKey: "edge-a"}
	target, secrets := validTarget()

	for name, mutate := range map[string]func(*Target, *Secrets){
		"duplicate app": func(target *Target, _ *Secrets) { target.Apps = append(target.Apps, target.Apps[0]) },
		"duplicate service": func(target *Target, _ *Secrets) {
			target.DataServices = append(target.DataServices, target.DataServices[0])
		},
		"duplicate connector": func(target *Target, _ *Secrets) { target.Connectors = append(target.Connectors, target.Connectors[0]) },
		"invalid data identity": func(target *Target, _ *Secrets) {
			target.Apps[1].DataLinks[0].Identity = DataIdentity{Kind: "postgres", Endpoint: "db"}
		},
		"unknown app secret": func(_ *Target, secrets *Secrets) { secrets.Apps["other"] = AppSecrets{} },
		"invalid UTF-8":      func(target *Target, _ *Secrets) { target.Apps[0].Hostname = "\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			copyTarget, copySecrets := cloneTarget(target), cloneSecrets(secrets)
			mutate(&copyTarget, &copySecrets)
			_, err := TargetRevision(key, resource, copyTarget, copySecrets)
			if err == nil || strings.Contains(err.Error(), "CANARY_SECRET_DO_NOT_EXPOSE") {
				t.Fatalf("TargetRevision error = %v", err)
			}
		})
	}
	if _, err := TargetRevision(RevisionKey("short"), resource, target, secrets); err == nil {
		t.Fatal("short revision key was accepted")
	}
}

func TestValidateTargetRequiresSafeInitialAdminEmail(t *testing.T) {
	for name, email := range map[string]string{
		"empty":        "",
		"newline":      "admin@example.test\nINJECTED=yes",
		"control":      "admin@example.test\x00",
		"invalid utf8": "\xff",
	} {
		t.Run(name, func(t *testing.T) {
			target, secrets := validTarget()
			target.Apps[0].InitialAdminEmail = email
			if err := ValidateTarget(target, secrets); err == nil {
				t.Fatal("unsafe initial admin email accepted")
			}
		})
	}
}

func TestOperationKeyAndApprovalAreExact(t *testing.T) {
	resource := ResourceIdentity{Environment: "production", ServerKey: "edge-a"}
	operation := OperationKey{Resource: resource, Action: ActionReconcile, TargetRevision: "tr1:key:commitment", PriorAppliedRevision: "tr1:key:old"}
	if err := operation.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]OperationKey{
		"inspect":           {Resource: resource, Action: ActionInspect, TargetRevision: "tr1:key:commitment"},
		"no precondition":   {Resource: resource, Action: ActionReconcile, TargetRevision: "tr1:key:commitment"},
		"two preconditions": {Resource: resource, Action: ActionReconcile, TargetRevision: "tr1:key:commitment", PriorAppliedRevision: "a", PriorObservation: "b"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := invalid.Validate(); err == nil {
				t.Fatal("invalid operation accepted")
			}
		})
	}

	target, _ := validTarget()
	approval := ApprovalSubject{Kind: ApprovalDataLink, Environment: "production", Resource: resource, AppID: "api", DataKind: "postgres", OldData: DataIdentity{Kind: "postgres", ProviderID: "old", Endpoint: "old.example", Port: 5432, Database: "app", TLSServerName: "old.example"}, NewData: target.Apps[1].DataLinks[1].Identity, TargetRevision: operation.TargetRevision}
	if err := approval.Validate(); err != nil {
		t.Fatal(err)
	}
	if !approval.Matches(operation, "api") {
		t.Fatal("exact data approval did not match")
	}
	if approval.Matches(OperationKey{Resource: resource, Action: ActionReconcile, TargetRevision: "tr1:key:other", PriorAppliedRevision: "tr1:key:old"}, "api") {
		t.Fatal("approval matched another revision")
	}
	if !approval.MatchesReconcileTarget(operation, target) {
		t.Fatal("approval did not match target link")
	}
	wrongTarget := cloneTarget(target)
	wrongTarget.Apps[1].DataLinks[1].Identity.ProviderID = "other"
	if approval.MatchesReconcileTarget(operation, wrongTarget) {
		t.Fatal("approval matched wrong target data identity")
	}
	for _, invalid := range []ApprovalSubject{{Kind: ApprovalDataLink, Environment: "production", Resource: resource, AppID: "api", DataKind: "postgres", OldData: approval.NewData, NewData: approval.NewData, TargetRevision: operation.TargetRevision}, {Kind: ApprovalRetire, Environment: "production", Resource: resource, Machine: MachineIdentity{Value: "machine-1"}, TargetRevision: "tr1:key:retire", PreserveData: true}} {
		if err := invalid.Validate(); err == nil {
			t.Fatal("invalid approval accepted")
		}
	}
	retire := ApprovalSubject{Kind: ApprovalRetire, Environment: "production", Resource: resource, Machine: MachineIdentity{Value: "machine-1"}, Ownership: OwnershipIdentity{Value: "owner-1"}, TargetRevision: "tr1:key:retire", PreserveData: true}
	if err := retire.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestObservationAndConnectorSemantics(t *testing.T) {
	target, secrets := validTarget()
	target.Connectors[0].AppIDs = []string{"web", "api"}
	target.Connectors[1].AppIDs = []string{"api"}
	if err := ValidateTarget(target, secrets); err != nil {
		t.Fatal(err)
	}
	bad := cloneTarget(target)
	bad.Connectors[0].AppIDs = []string{"missing", "missing"}
	if err := ValidateTarget(bad, secrets); err == nil {
		t.Fatal("invalid connector app references accepted")
	}
	revision := mustRevision(t, RevisionKey([]byte("01234567890123456789012345678901")), ResourceIdentity{Environment: "production", ServerKey: "edge-a"}, target, secrets)
	observation := StableObservation{Machine: MachineIdentity{Value: "machine"}, Ownership: OwnershipIdentity{Value: "owner"}, HostRelease: "release", AppliedRevision: revision, Apps: []AppObservation{{ID: "api", ActiveImage: "image"}}, Data: []DataObservation{{Identity: target.Apps[1].DataLinks[0].Identity}}}
	if err := observation.Validate(); err != nil {
		t.Fatal(err)
	}
	observation.HostRelease = ""
	if err := observation.Validate(); err == nil {
		t.Fatal("observation without host release accepted")
	}
	observation.HostRelease = "release"
	observation.Apps[0].ActiveImage = "\xff"
	if err := observation.Validate(); err == nil {
		t.Fatal("observation with invalid UTF-8 accepted")
	}
}

func TestRevisionRejectsResourceUTF8AndNormalizesMicroSocks(t *testing.T) {
	target, secrets := validTarget()
	key := RevisionKey([]byte("01234567890123456789012345678901"))
	resource := ResourceIdentity{Environment: "production", ServerKey: "edge-a"}
	first := mustRevision(t, key, resource, target, secrets)
	reordered := cloneTarget(target)
	reordered.MicroSocks.Clients = []MicroSocksClientTarget{{ID: "api"}}
	if got := mustRevision(t, key, resource, reordered, secrets); got != first {
		t.Fatal("microsocks ordering changed revision")
	}
	for name, mutate := range map[string]func(*Target, *Secrets){
		"duplicate client": func(t *Target, _ *Secrets) {
			t.MicroSocks.Clients = append(t.MicroSocks.Clients, t.MicroSocks.Clients[0])
		},
		"undeclared credential": func(_ *Target, s *Secrets) {
			s.MicroSocks.ClientCredentials = map[string]DataCredentials{"other": {Username: "u", Password: "p"}}
		},
		"server credential without server": func(t *Target, _ *Secrets) { t.MicroSocks.Server = false },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := cloneTarget(target), cloneSecrets(secrets)
			mutate(&a, &b)
			if _, err := TargetRevision(key, resource, a, b); err == nil {
				t.Fatal("invalid microsocks scope accepted")
			}
		})
	}
	if _, err := TargetRevision(key, ResourceIdentity{Environment: "\xff", ServerKey: "edge"}, target, secrets); err == nil {
		t.Fatal("invalid UTF-8 resource accepted")
	}
}

func TestTargetRevisionNormalizationIsPureAndMicroSocksEmptyIsNil(t *testing.T) {
	key := RevisionKey([]byte("01234567890123456789012345678901"))
	resource := ResourceIdentity{Environment: "production", ServerKey: "edge-a"}
	target, secrets := validTarget()
	target.MicroSocks.Clients = []MicroSocksClientTarget{{ID: "web"}, {ID: "api"}}
	target.Connectors[0].AppIDs = []string{"web", "api"}
	secrets.MicroSocks.ClientCredentials = map[string]DataCredentials{"web": {Username: "w", Password: "p"}, "api": {Username: "a", Password: "p"}}
	beforeTarget, beforeSecrets := cloneTarget(target), cloneSecrets(secrets)
	first := mustRevision(t, key, resource, target, secrets)
	if !reflect.DeepEqual(target, beforeTarget) || !reflect.DeepEqual(secrets, beforeSecrets) {
		t.Fatal("successful TargetRevision mutated caller input")
	}
	reordered := cloneTarget(target)
	reordered.MicroSocks.Clients = []MicroSocksClientTarget{{ID: "api"}, {ID: "web"}}
	if got := mustRevision(t, key, resource, reordered, secrets); got != first {
		t.Fatal("two-client microsocks order changed revision")
	}

	emptyTarget := Target{ReleaseArtifact: "release", MicroSocks: &MicroSocksTarget{}}
	emptySecrets := Secrets{MicroSocks: &MicroSocksSecrets{}}
	if got := mustRevision(t, key, resource, emptyTarget, emptySecrets); got != mustRevision(t, key, resource, Target{ReleaseArtifact: "release"}, Secrets{}) {
		t.Fatal("empty microsocks was not normalized to nil")
	}
	invalid := cloneTarget(target)
	invalid.Connectors[0].AppIDs = []string{"missing"}
	invalidBefore := cloneTarget(invalid)
	if _, err := TargetRevision(key, resource, invalid, secrets); err == nil {
		t.Fatal("invalid target accepted")
	}
	if !reflect.DeepEqual(invalid, invalidBefore) {
		t.Fatal("failed TargetRevision mutated caller input")
	}
}

func TestLocalDataServiceNormalizesBindingsAndClients(t *testing.T) {
	target := Target{ReleaseArtifact: "release", DataServices: []LocalDataServiceTarget{{
		ID: "db", Type: "redis", Port: 5432,
		Bindings: []LocalDataBinding{{Address: "2001:db8::1", AllowedSources: []string{"2001:db8::3", "2001:db8::2"}}, {Address: "10.0.0.1", AllowedSources: []string{"10.0.0.3", "10.0.0.2"}}},
		Clients:  []LocalDataClient{{AppID: "z", Username: "z-user", Database: "zdb"}, {AppID: "a", Username: "a-user", Database: "adb"}},
	}}}
	secrets := Secrets{LocalDataServices: map[string]LocalDataServiceSecrets{"db": {AdminPassword: "admin", ClientPasswords: map[string]string{"z": "zp", "a": "ap"}}}}
	normalized, normalizedSecrets := NormalizeTargetSecrets(target, secrets)
	service := normalized.DataServices[0]
	if service.Bindings[0].Address != "10.0.0.1" || service.Bindings[0].AllowedSources[0] != "10.0.0.2" || service.Clients[0].AppID != "a" {
		t.Fatalf("local data service was not deterministically normalized: %#v", service)
	}
	if normalizedSecrets.LocalDataServices["db"].ClientPasswords["a"] != "ap" {
		t.Fatalf("client passwords were not retained: %#v", normalizedSecrets)
	}
	if err := ValidateTarget(normalized, normalizedSecrets); err != nil {
		t.Fatalf("ValidateTarget rejected scoped local data service: %v", err)
	}

	invalid := normalized
	invalid.DataServices[0].Clients = append(invalid.DataServices[0].Clients, LocalDataClient{AppID: "other", Username: "a-user", Database: "other"})
	if err := ValidateTarget(invalid, normalizedSecrets); err == nil {
		t.Fatal("ValidateTarget accepted incompatible duplicate local data username")
	}
}

func TestLocalDataServiceRejectsUnsafeBindingsAndMissingAdminSecrets(t *testing.T) {
	target := Target{ReleaseArtifact: "release", DataServices: []LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Bindings: []LocalDataBinding{{Address: "10.0.0.1", AllowedSources: []string{"2001:db8::2"}}}}}}
	if err := ValidateTarget(target, Secrets{LocalDataServices: map[string]LocalDataServiceSecrets{"db": {}}}); err == nil {
		t.Fatal("accepted missing admin secret and mismatched source family")
	}
	target.DataServices = append(target.DataServices, LocalDataServiceTarget{ID: "cache", Type: "redis", Port: 5432, Bindings: []LocalDataBinding{{Address: "10.0.0.1", AllowedSources: []string{"10.0.0.2"}}}})
	if err := ValidateTarget(target, Secrets{LocalDataServices: map[string]LocalDataServiceSecrets{"db": {AdminPassword: "a"}, "cache": {AdminPassword: "b"}}}); err == nil {
		t.Fatal("accepted local socket collision")
	}
}

func TestLocalDataServiceDuplicateUsernameRequiresIdenticalPassword(t *testing.T) {
	target := Target{ReleaseArtifact: "release", DataServices: []LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Clients: []LocalDataClient{{AppID: "one", Username: "shared", Database: "app"}, {AppID: "two", Username: "shared", Database: "app"}}}}}
	secrets := Secrets{LocalDataServices: map[string]LocalDataServiceSecrets{"db": {AdminPassword: "admin", ClientPasswords: map[string]string{"one": "first", "two": "second"}}}}
	if err := ValidateTarget(target, secrets); err == nil {
		t.Fatal("accepted duplicate username with distinct client passwords")
	}
	secrets.LocalDataServices["db"] = LocalDataServiceSecrets{AdminPassword: "admin", ClientPasswords: map[string]string{"one": "same", "two": "same"}}
	if err := ValidateTarget(target, secrets); err != nil {
		t.Fatalf("rejected identical principal tuple: %v", err)
	}
}

func TestLocalPostgresClientContractRejectsReservedIdentifiersAndUnsafeDSNPasswords(t *testing.T) {
	valid := func() (Target, Secrets) {
		return Target{ReleaseArtifact: "release", Apps: []AppTarget{{ID: "app-id", Image: "image", Hostname: "app", ReadinessPath: "/", InitialAdminEmail: "admin@example.test"}}, DataServices: []LocalDataServiceTarget{{ID: "db", Type: "postgres", Port: 5432, Clients: []LocalDataClient{{AppID: "app-id", Username: "app", Database: "app"}}}}}, Secrets{Apps: map[string]AppSecrets{"app-id": {Postgres: &DataCredentials{Username: "app", Password: "safe$;=\"quote"}}}, LocalDataServices: map[string]LocalDataServiceSecrets{"db": {AdminPassword: "admin", ClientPasswords: map[string]string{"app-id": "safe$;=\"quote"}}}}
	}
	target, secrets := valid()
	if err := ValidateTarget(target, secrets); err != nil {
		t.Fatalf("rejected permitted DSN password: %v", err)
	}
	for name, mutate := range map[string]func(*Target, *Secrets){"reserved role": func(t *Target, _ *Secrets) { t.DataServices[0].Clients[0].Username = "s2h_client" }, "reserved database": func(t *Target, _ *Secrets) { t.DataServices[0].Clients[0].Database = "postgres" }, "unsafe password": func(_ *Target, s *Secrets) {
		s.Apps["app-id"] = AppSecrets{Postgres: &DataCredentials{Username: "app", Password: "bad\npassword"}}
	}} {
		t.Run(name, func(t *testing.T) {
			target, secrets := valid()
			mutate(&target, &secrets)
			if err := ValidateTarget(target, secrets); err == nil {
				t.Fatal("accepted invalid PostgreSQL contract")
			}
		})
	}
}

func validTarget() (Target, Secrets) {
	return Target{ReleaseArtifact: "release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Apps: []AppTarget{{ID: "web", Image: "web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Hostname: "web.example", ReadinessPath: "/ready", DrainTimeout: "10s", InitialBootstrap: true, InitialAdminEmail: "web@example.test"}, {ID: "api", Image: "api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Hostname: "api.example", ReadinessPath: "/ready", InitialAdminEmail: "api@example.test", RuntimeSettings: map[string]string{"A": "1"}, DataLinks: []DataLink{{Name: "cache", Identity: DataIdentity{Kind: "redis", ProviderID: "cache-1", Endpoint: "cache.example", Port: 6379, Database: "0"}}, {Name: "main", Identity: DataIdentity{Kind: "postgres", ProviderID: "db-1", Endpoint: "db.example", Port: 5432, Database: "app", TLSServerName: "db.example"}}}}}, DataServices: []LocalDataServiceTarget{{ID: "redis", Type: "redis", Port: 6379, Persistence: true}, {ID: "postgres", Type: "postgres", Port: 5432}}, ReverseProxy: &ReverseProxyTarget{Image: "traefik:v3", ACMEEmail: "ops@example"}, MicroSocks: &MicroSocksTarget{Server: true, Clients: []MicroSocksClientTarget{{ID: "api"}}}, Connectors: []TunnelConnectorTarget{{ID: "tunnel-b", TunnelID: "b"}, {ID: "tunnel-a", TunnelID: "a"}}}, Secrets{Apps: map[string]AppSecrets{"api": {JWTSecret: "CANARY_SECRET_DO_NOT_EXPOSE", RuntimeEnvironment: map[string]string{"SECRET": "CANARY_SECRET_DO_NOT_EXPOSE"}}, "web": {InitialAdminPassword: "bootstrap"}}, LocalDataServices: map[string]LocalDataServiceSecrets{"postgres": {AdminPassword: "admin"}, "redis": {AdminPassword: "admin"}}, ReverseProxy: &ReverseProxySecrets{DNSChallengeToken: "dns"}, MicroSocks: &MicroSocksSecrets{ServerPassword: "server"}, Connectors: map[string]TunnelConnectorSecrets{"tunnel-a": {Token: "a"}, "tunnel-b": {Token: "b"}}}
}

func mustRevision(t *testing.T, key RevisionKey, resource ResourceIdentity, target Target, secrets Secrets) string {
	t.Helper()
	got, err := TargetRevision(key, resource, target, secrets)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func cloneTarget(target Target) Target {
	encoded, _ := canonicalJSON(target)
	var copy Target
	_ = decodeCanonicalJSON(encoded, &copy)
	return copy
}
func cloneSecrets(secrets Secrets) Secrets {
	encoded, _ := canonicalJSON(secrets)
	var copy Secrets
	_ = decodeCanonicalJSON(encoded, &copy)
	return copy
}
