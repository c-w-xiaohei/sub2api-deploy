package environment

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `version: 1
reverseProxy:
  image: traefik:v3.3.3
  acmeEmail: ops@example.com
servers:
  Edge_Box:
    addresses:
      public:
        ipv4: 203.0.113.10
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

const validSecrets = `revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
apps:
  web-app:
    initialAdminPassword: SENTINEL_ONLY_FOR_REDACTION_TEST
    jwtSecret: jwt-placeholder
    totpEncryptionKey: totp-placeholder
    postgres:
      username: sub2api
      password: postgres-placeholder
    redis:
      username: default
      password: redis-placeholder
postgres:
  main-db:
    adminPassword: postgres-admin-placeholder
redis:
  main-cache:
    adminPassword: redis-admin-placeholder
reverseProxy:
  dnsChallengeToken: dns-placeholder
`

const emptyTopologyConfig = `version: 1
reverseProxy:
  image: traefik:v3.3.3
  acmeEmail: ops@example.com
servers: {}
postgres: {}
redis: {}
apps: {}
`

const emptyTopologySecrets = `revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
reverseProxy:
  dnsChallengeToken: dns-placeholder
`

func TestParseAndValidateAcceptsValidEnvironment(t *testing.T) {
	config, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := ParseSecrets([]byte(validSecrets))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := secrets.RevisionKey, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="; got != want {
		t.Fatalf("revisionKey = %q, want %q", got, want)
	}
	validated, err := Validate(config, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if validated.Apps["web-app"].DrainTimeout.String() != "10s" {
		t.Fatalf("default drain timeout = %s", validated.Apps["web-app"].DrainTimeout)
	}
	if validated.Postgres["main-db"].Port == nil || *validated.Postgres["main-db"].Port != 5432 || validated.Redis["main-cache"].Port == nil || *validated.Redis["main-cache"].Port != 6379 {
		t.Fatalf("service defaults were not applied")
	}
}

func TestValidateAcceptsExplicitlyEmptyTopology(t *testing.T) {
	validated, err := Validate(mustParseConfig(t, emptyTopologyConfig), mustParseSecrets(t, emptyTopologySecrets))
	if err != nil {
		t.Fatalf("Validate rejected explicitly empty topology: %v", err)
	}
	if len(validated.ServerIDs) != 0 {
		t.Fatalf("ServerIDs = %v, want no servers", validated.ServerIDs)
	}
}

func TestValidateStillRequiresRevisionAndReverseProxySecretsForEmptyTopology(t *testing.T) {
	for name, secrets := range map[string]string{
		"revision key": strings.Replace(emptyTopologySecrets, "revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\n", "", 1),
		"reverse proxy": strings.Replace(emptyTopologySecrets, "reverseProxy:\n  dnsChallengeToken: dns-placeholder\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate(mustParseConfig(t, emptyTopologyConfig), mustParseSecrets(t, secrets)); err == nil {
				t.Fatalf("Validate accepted empty topology without %s", name)
			}
		})
	}
}

func TestValidateRejectsOmittedOrNullTopologyMaps(t *testing.T) {
	for name, replacement := range map[string]string{
		"omitted servers": "",
		"null postgres":  "postgres: null\n",
	} {
		t.Run(name, func(t *testing.T) {
			input := emptyTopologyConfig
			if name == "omitted servers" {
				input = strings.Replace(input, "servers: {}\n", replacement, 1)
			} else {
				input = strings.Replace(input, "postgres: {}\n", replacement, 1)
			}
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, emptyTopologySecrets)); err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
		})
	}
}

func TestValidateRejectsPartiallyEmptyTopology(t *testing.T) {
	input := strings.Replace(emptyTopologyConfig, "servers: {}", "servers:\n  edge: {}", 1)
	if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, emptyTopologySecrets)); err == nil {
		t.Fatal("Validate accepted topology with only servers configured")
	}
}

func TestValidateRejectsInvalidRevisionKey(t *testing.T) {
	for name, test := range map[string]struct {
		key        string
		leakMarker string
	}{
		"missing": {
			key: "",
		},
		"malformed base64": {
			key:        "NOT_BASE64_CANARY!",
			leakMarker: "CANARY",
		},
		"non-canonical padding bits": {
			key:        "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWZ=",
			leakMarker: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWZ=",
		},
		"31 decoded bytes": {
			key:        base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 31))),
			leakMarker: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 31))),
		},
		"33 decoded bytes": {
			key:        base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 33))),
			leakMarker: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 33))),
		},
	} {
		t.Run(name, func(t *testing.T) {
			secretsInput := validSecrets
			if test.key == "" {
				secretsInput = strings.Replace(secretsInput, "revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\n", "", 1)
			} else {
				secretsInput = strings.Replace(secretsInput, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", test.key, 1)
			}

			_, err := Validate(mustParseConfig(t, validConfig), mustParseSecrets(t, secretsInput))
			if err == nil {
				t.Fatal("Validate accepted invalid revisionKey")
			}
			if !strings.Contains(err.Error(), "revisionKey") {
				t.Fatalf("Validate error = %q, want revisionKey context", err)
			}
			if test.leakMarker != "" && strings.Contains(err.Error(), test.leakMarker) {
				t.Fatalf("Validate error leaked revisionKey: %q", err)
			}
		})
	}
}

func TestParseAndValidateAcceptsStandardAlphabetRevisionKey(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xfb", 32)))
	if !strings.ContainsAny(key, "+/") {
		t.Fatalf("test revisionKey %q lacks standard base64 alphabet characters", key)
	}
	secrets := mustParseSecrets(t, strings.Replace(validSecrets, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", key, 1))
	if got := secrets.RevisionKey; got != key {
		t.Fatalf("revisionKey = %q, want %q", got, key)
	}
	if _, err := Validate(mustParseConfig(t, validConfig), secrets); err != nil {
		t.Fatalf("Validate rejected standard alphabet revisionKey: %v", err)
	}
}

func TestValidateAllowsEmptyAppPlacementOnlyWhenPublicAccessIsNone(t *testing.T) {
	maintenance := strings.Replace(validConfig, "servers: [Edge_Box]", "servers: []", 1)
	validated, err := Validate(mustParseConfig(t, maintenance), mustParseSecrets(t, validSecrets))
	if err != nil {
		t.Fatalf("Validate rejected maintenance placement: %v", err)
	}
	app := validated.Apps["web-app"]
	if app.Postgres.Name != "main-db" || app.Redis.Name != "main-cache" {
		t.Fatalf("maintenance placement lost data links: %#v", app)
	}

	for name, publicAccess := range map[string]string{
		"external":   "type: external\n      servers: [Edge_Box]",
		"cloudflare": "type: cloudflare\n      servers: [Edge_Box]\n      cloudflare:\n        mode: dns\n        connectBy: publicAddress",
	} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(maintenance, "type: none", publicAccess, 1)
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted empty placement with %s public access", name)
			}
		})
	}
}

func TestParseConfigRejectsNullOrMissingValueAppPlacement(t *testing.T) {
	for name, placement := range map[string]string{"null": "servers: null", "missing value": "servers:"} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(validConfig, "servers: [Edge_Box]", placement, 1)
			if _, err := ParseConfig([]byte(input)); err == nil {
				t.Fatalf("ParseConfig accepted %s app placement", name)
			}
		})
	}
}

func TestValidateAllowsEmptyPlacementWithoutDockerServiceInternalAddress(t *testing.T) {
	input := strings.Replace(validConfig, "      internal:\n        ipv4: 10.0.0.10\n", "", 1)
	input = strings.Replace(input, "servers: [Edge_Box]", "servers: []", 1)
	validated, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets))
	if err != nil {
		t.Fatalf("Validate rejected maintenance placement without active App host: %v", err)
	}
	app := validated.Apps["web-app"]
	if app.Postgres.Name != "main-db" || app.Redis.Name != "main-cache" {
		t.Fatalf("maintenance placement lost data links: %#v", app)
	}
}

func TestValidateProjectsStableServerKeysAndSortedUniqueSSHAliases(t *testing.T) {
	input := strings.Replace(validConfig, "  Edge_Box:\n", "  Edge_Box:\n    sshAlias: zeta\n  Worker:\n    sshAlias: Alpha_1\n", 1)
	validated, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(validated.ServerIDs, ","), "Edge_Box,Worker"; got != want {
		t.Fatalf("ServerIDs = %q, want %q", got, want)
	}
	if got, want := strings.Join(validated.SSHAliases, ","), "Alpha_1,zeta"; got != want {
		t.Fatalf("SSHAliases = %q, want %q", got, want)
	}
	if got := validated.Servers["Worker"].SSHAlias; got != "Alpha_1" {
		t.Fatalf("explicit sshAlias = %q", got)
	}

	defaultAlias, err := Validate(mustParseConfig(t, validConfig), mustParseSecrets(t, validSecrets))
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultAlias.Servers["Edge_Box"].SSHAlias; got != "Edge_Box" {
		t.Fatalf("default sshAlias = %q", got)
	}

	duplicate := strings.Replace(input, "sshAlias: Alpha_1", "sshAlias: zeta", 1)
	if _, err := Validate(mustParseConfig(t, duplicate), mustParseSecrets(t, validSecrets)); err == nil || !strings.Contains(err.Error(), "sshAlias") {
		t.Fatalf("Validate duplicate alias error = %v", err)
	}
}

func TestValidateRejectsExplicitEmptyOrNullSSHAlias(t *testing.T) {
	for name, alias := range map[string]string{"empty": `""`, "null": "null"} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(validConfig, "  Edge_Box:\n", "  Edge_Box:\n    sshAlias: "+alias+"\n", 1)
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil || !strings.Contains(err.Error(), "sshAlias") {
				t.Fatalf("Validate explicit %s sshAlias error = %v", name, err)
			}
		})
	}
}

func TestStrictYAMLRejectsUnknownAndDuplicateFields(t *testing.T) {
	for name, input := range map[string]string{
		"unknown":   validConfig + "unknown: true\n",
		"duplicate": strings.Replace(validConfig, "version: 1", "version: 1\nversion: 1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(input)); err == nil {
				t.Fatalf("ParseConfig accepted %s YAML", name)
			}
		})
	}
}

func TestStrictYAMLRejectsMalformedTrailingDocument(t *testing.T) {
	input := validConfig + "\n--- [unclosed\n"
	if _, err := ParseConfig([]byte(input)); err == nil {
		t.Fatal("ParseConfig accepted a malformed trailing YAML document")
	}
}

func TestUnionPresenceRulesRejectExplicitEmptyAndNullFields(t *testing.T) {
	cases := map[string]string{
		"external empty server":         strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: external\n    server: \"\"\n    host: db.example.com", 1),
		"disabled empty outbound proxy": strings.Replace(validConfig, "publicAccess:\n      type: none", "outboundProxy: {}\n    publicAccess:\n      type: none", 1),
		"none empty servers":            strings.Replace(validConfig, "publicAccess:\n      type: none", "publicAccess:\n      type: none\n      servers: []", 1),
		"none null cloudflare":          strings.Replace(validConfig, "publicAccess:\n      type: none", "publicAccess:\n      type: none\n      cloudflare: null", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted explicit empty/null union field for %s", name)
			}
		})
	}
}

func TestExternalPublicAccessRejectsCloudflareNullByPresence(t *testing.T) {
	input := strings.Replace(validConfig, "publicAccess:\n      type: none", "publicAccess:\n      type: external\n      servers: [Edge_Box]\n      cloudflare: null", 1)
	if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil {
		t.Fatal("Validate accepted publicAccess.external with cloudflare: null")
	}
}

func TestDisabledOutboundProxyIsValidOnlyWithAllowedPresence(t *testing.T) {
	validDisabled := strings.Replace(validConfig, "publicAccess:\n      type: none", "outboundProxy:\n      enabled: false\n    publicAccess:\n      type: none", 1)
	if _, err := Validate(mustParseConfig(t, validDisabled), mustParseSecrets(t, validSecrets)); err != nil {
		t.Fatalf("Validate rejected disabled outbound proxy: %v", err)
	}
	validDisabled = strings.Replace(validDisabled, "enabled: false", "enabled: false\n      required: false", 1)
	if _, err := Validate(mustParseConfig(t, validDisabled), mustParseSecrets(t, validSecrets)); err != nil {
		t.Fatalf("Validate rejected disabled outbound proxy with required:false: %v", err)
	}
	for name, extra := range map[string]string{
		"required true": "required: true",
		"required null": "required: null",
		"type":          "type: microsocks",
		"servers":       "servers: [Edge_Box]",
	} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(validConfig, "publicAccess:\n      type: none", "outboundProxy:\n      enabled: false\n      "+extra+"\n    publicAccess:\n      type: none", 1)
			config, parseErr := ParseConfig([]byte(input))
			if parseErr == nil {
				_, parseErr = Validate(config, mustParseSecrets(t, validSecrets))
			}
			if parseErr == nil {
				t.Fatalf("Validate accepted disabled outbound proxy with %s", name)
			}
		})
	}
	t.Run("enabled null", func(t *testing.T) {
		input := strings.Replace(validConfig, "publicAccess:\n      type: none", "outboundProxy:\n      enabled: null\n    publicAccess:\n      type: none", 1)
		config, parseErr := ParseConfig([]byte(input))
		if parseErr == nil {
			_, parseErr = Validate(config, mustParseSecrets(t, validSecrets))
		}
		if parseErr == nil {
			t.Fatal("Validate accepted disabled outbound proxy with enabled null")
		}
	})
}

func TestNeonComputeIsOptionalButCompleteWhenPresent(t *testing.T) {
	withoutCompute := strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: neon\n    region: eu-central-1", 1)
	withoutComputeSecrets := strings.Replace(validSecrets, "postgres:\n  main-db:\n    adminPassword: postgres-admin-placeholder", "postgres:\n  main-db:\n    apiToken: neon-token", 1)
	withoutComputeSecrets = strings.Replace(withoutComputeSecrets, "    postgres:\n      username: sub2api\n      password: postgres-placeholder\n", "", 1)
	validated, err := Validate(mustParseConfig(t, withoutCompute), mustParseSecrets(t, withoutComputeSecrets))
	if err != nil {
		t.Fatal(err)
	}
	if validated.Postgres["main-db"].Compute != nil {
		t.Fatal("Validate generated a compute object when compute was omitted")
	}

	for name, compute := range map[string]string{
		"missing maxCU":        "compute:\n      minCU: 0.5\n      suspendAfter: 5m",
		"missing minCU":        "compute:\n      maxCU: 0.5\n      suspendAfter: 5m",
		"missing suspendAfter": "compute:\n      minCU: 0.5\n      maxCU: 0.5",
	} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(withoutCompute, "region: eu-central-1", "region: eu-central-1\n    "+compute, 1)
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, withoutComputeSecrets)); err == nil {
				t.Fatalf("Validate accepted incomplete Neon compute for %s", name)
			}
		})
	}
}

func TestDockerSecretsRejectProviderOnlyFields(t *testing.T) {
	secrets := strings.Replace(validSecrets, "adminPassword: postgres-admin-placeholder", "adminPassword: postgres-admin-placeholder\n    apiToken: provider-only", 1)
	secrets = strings.Replace(secrets, "adminPassword: redis-admin-placeholder", "adminPassword: redis-admin-placeholder\n    apiKey: provider-only", 1)
	if _, err := Validate(mustParseConfig(t, validConfig), mustParseSecrets(t, secrets)); err == nil {
		t.Fatal("Validate accepted provider-only Docker service secrets")
	}
}

func TestComputeAndDockerSecretNullFieldsDoNotSatisfyUnionPresence(t *testing.T) {
	computeInput := strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: neon\n    compute:\n      minCU: null\n      maxCU: 0.5\n      suspendAfter: 5m", 1)
	computeSecrets := strings.Replace(validSecrets, "postgres:\n  main-db:\n    adminPassword: postgres-admin-placeholder", "postgres:\n  main-db:\n    apiToken: neon-token", 1)
	computeSecrets = strings.Replace(computeSecrets, "    postgres:\n      username: sub2api\n      password: postgres-placeholder\n", "", 1)
	parsedCompute, err := ParseConfig([]byte(computeInput))
	if err == nil {
		_, err = Validate(parsedCompute, mustParseSecrets(t, computeSecrets))
	}
	if err == nil {
		t.Fatal("Validate accepted null Neon compute minCU")
	}

	secretInput := strings.Replace(validSecrets, "adminPassword: postgres-admin-placeholder", "adminPassword: postgres-admin-placeholder\n    apiToken: null", 1)
	if _, err := Validate(mustParseConfig(t, validConfig), mustParseSecrets(t, secretInput)); err == nil {
		t.Fatal("Validate accepted null Docker PostgreSQL apiToken")
	}
}

func TestSecretUnionRejectsForbiddenNullFieldsAcrossTypes(t *testing.T) {
	cases := map[string]struct {
		config  string
		secrets string
	}{
		"neon postgres adminPassword": {
			config:  strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: neon", 1),
			secrets: strings.Replace(strings.Replace(validSecrets, "postgres:\n  main-db:\n    adminPassword: postgres-admin-placeholder", "postgres:\n  main-db:\n    apiToken: neon-token\n    adminPassword: null", 1), "    postgres:\n      username: sub2api\n      password: postgres-placeholder\n", "", 1),
		},
		"external postgres adminPassword": {
			config:  strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: external\n    host: db.example.com", 1),
			secrets: validSecrets,
		},
		"upstash redis adminPassword": {
			config:  strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: upstash", 1),
			secrets: strings.Replace(strings.Replace(validSecrets, "redis:\n  main-cache:\n    adminPassword: redis-admin-placeholder", "redis:\n  main-cache:\n    apiKey: upstash-key\n    adminPassword: null", 1), "    redis:\n      username: default\n      password: redis-placeholder\n", "", 1),
		},
		"external redis adminPassword": {
			config:  strings.Replace(validConfig, "main-cache:\n    type: docker\n    server: Edge_Box", "main-cache:\n    type: external\n    host: redis.example.com", 1),
			secrets: validSecrets,
		},
		"neon app postgres": {
			config:  strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: neon", 1),
			secrets: strings.Replace(strings.Replace(validSecrets, "postgres:\n  main-db:\n    adminPassword: postgres-admin-placeholder", "postgres:\n  main-db:\n    apiToken: neon-token", 1), "    postgres:\n      username: sub2api\n      password: postgres-placeholder", "    postgres: null", 1),
		},
		"upstash app redis": {
			config:  strings.Replace(validConfig, "main-cache:\n    type: docker\n    server: Edge_Box", "main-cache:\n    type: upstash", 1),
			secrets: strings.Replace(strings.Replace(validSecrets, "redis:\n  main-cache:\n    adminPassword: redis-admin-placeholder", "redis:\n  main-cache:\n    apiKey: upstash-key", 1), "    redis:\n      username: default\n      password: redis-placeholder", "    redis: null", 1),
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			config, configErr := ParseConfig([]byte(testCase.config))
			secrets, secretsErr := ParseSecrets([]byte(testCase.secrets))
			if configErr != nil || secretsErr != nil {
				t.Fatalf("fixture parse failed: config=%v secrets=%v", configErr, secretsErr)
			}
			if _, err := Validate(config, secrets); err == nil {
				t.Fatal("Validate accepted forbidden secret field presence")
			}
		})
	}
}

func TestRequiredProviderSecretsRejectNullValues(t *testing.T) {
	cases := map[string]struct {
		config  string
		secrets string
	}{
		"docker postgres adminPassword": {
			config:  validConfig,
			secrets: strings.Replace(validSecrets, "adminPassword: postgres-admin-placeholder", "adminPassword: null", 1),
		},
		"neon postgres apiToken": {
			config:  strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: neon", 1),
			secrets: strings.Replace(strings.Replace(validSecrets, "adminPassword: postgres-admin-placeholder", "apiToken: null", 1), "    postgres:\n      username: sub2api\n      password: postgres-placeholder\n", "", 1),
		},
		"docker redis adminPassword": {
			config:  validConfig,
			secrets: strings.Replace(validSecrets, "adminPassword: redis-admin-placeholder", "adminPassword: null", 1),
		},
		"upstash redis apiKey": {
			config:  strings.Replace(validConfig, "main-cache:\n    type: docker\n    server: Edge_Box", "main-cache:\n    type: upstash", 1),
			secrets: strings.Replace(strings.Replace(validSecrets, "adminPassword: redis-admin-placeholder", "apiKey: null", 1), "    redis:\n      username: default\n      password: redis-placeholder", "    redis: null", 1),
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			config, configErr := ParseConfig([]byte(testCase.config))
			secrets, secretsErr := ParseSecrets([]byte(testCase.secrets))
			if configErr != nil || secretsErr != nil {
				t.Fatalf("fixture parse failed: config=%v secrets=%v", configErr, secretsErr)
			}
			if _, err := Validate(config, secrets); err == nil {
				t.Fatal("Validate accepted required provider secret with null value")
			}
		})
	}
}

func TestCustomParentDecodersRejectNestedUnknownFields(t *testing.T) {
	cases := map[string]string{
		"TLSConfig":        strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: external\n    host: db.example.com\n    tls:\n      mode: require\n      unknown: true", 1),
		"AppPostgres":      strings.Replace(validConfig, "database: sub2api", "database: sub2api\n      unknown: true", 1),
		"AppRedis":         strings.Replace(validConfig, "database: 0", "database: 0\n      unknown: true", 1),
		"CloudflareAccess": strings.Replace(validConfig, "publicAccess:\n      type: none", "publicAccess:\n      type: cloudflare\n      servers: [Edge_Box]\n      cloudflare:\n        mode: dns\n        connectBy: publicAddress\n        unknown: true", 1),
		"HealthCheck":      strings.Replace(validConfig, "publicAccess:\n      type: none", "publicAccess:\n      type: cloudflare\n      servers: [Edge_Box]\n      cloudflare:\n        mode: loadBalancer\n        connectBy: publicAddress\n        healthCheck:\n          path: /health\n          unknown: true", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(input)); err == nil {
				t.Fatalf("ParseConfig accepted nested unknown field in %s", name)
			}
		})
	}
}

func TestValidateRejectsExternalRedisBadHost(t *testing.T) {
	for _, host := range []string{"Redis.Example.com", "not a hostname"} {
		t.Run(host, func(t *testing.T) {
			input := strings.Replace(validConfig, "main-cache:\n    type: docker\n    server: Edge_Box", "main-cache:\n    type: external\n    host: "+host, 1)
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted external Redis host %q", host)
			}
		})
	}
}

func TestHostnameLengthLimitIsAppliedToAllHostnameForms(t *testing.T) {
	tooLong := strings.Repeat("a", 250) + ".com"
	cases := map[string]string{
		"app":                strings.Replace(validConfig, "app.example.com", tooLong, 1),
		"external postgres":  strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: external\n    host: "+tooLong, 1),
		"singBox serverName": strings.Replace(validConfig, "  Edge_Box:\n    addresses:", "  Edge_Box:\n    singBox:\n      serverName: "+tooLong+"\n      target: 10.0.0.2:443\n    addresses:", 1),
		"singBox target":     strings.Replace(validConfig, "  Edge_Box:\n    addresses:", "  Edge_Box:\n    singBox:\n      serverName: relay.example.com\n      target: "+tooLong+":443\n    addresses:", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted hostname longer than 253 characters for %s", name)
			}
		})
	}
}

func TestNeonComputeRejectsNonFiniteValues(t *testing.T) {
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		t.Run(value, func(t *testing.T) {
			input := strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: neon\n    compute:\n      minCU: 0.5\n      maxCU: "+value+"\n      suspendAfter: 5m", 1)
			secrets := strings.Replace(validSecrets, "postgres:\n  main-db:\n    adminPassword: postgres-admin-placeholder", "postgres:\n  main-db:\n    apiToken: neon-token", 1)
			secrets = strings.Replace(secrets, "    postgres:\n      username: sub2api\n      password: postgres-placeholder\n", "", 1)
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, secrets)); err == nil {
				t.Fatalf("Validate accepted non-finite compute value %s", value)
			}
		})
	}
}

func TestValidateRejectsInvalidIDsHostnamesImagesAndReferences(t *testing.T) {
	cases := map[string]string{
		"app id":    strings.Replace(validConfig, "web-app:", "Web_App:", 1),
		"hostname":  strings.Replace(validConfig, "app.example.com", "App.Example.com", 1),
		"image":     strings.Replace(validConfig, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256:bad", 1),
		"reference": strings.Replace(validConfig, "server: Edge_Box", "server: missing", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			config, err := ParseConfig([]byte(input))
			if err != nil {
				return
			}
			secrets, err := ParseSecrets([]byte(validSecrets))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Validate(config, secrets); err == nil {
				t.Fatalf("Validate accepted invalid %s", name)
			}
		})
	}
}

func TestValidateRejectsUnsafeOpenSSHServerNames(t *testing.T) {
	for _, serverID := range []string{"-node", ".", "..", "user@host", "node/path", "node *"} {
		t.Run(serverID, func(t *testing.T) {
			input := strings.Replace(validConfig, "Edge_Box:", serverID+":", 1)
			input = strings.Replace(input, "Edge_Box", serverID, -1)
			config := mustParseConfig(t, input)
			if _, err := Validate(config, mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted unsafe server ID %q", serverID)
			}
		})
	}
}

func TestValidatePreservesExplicitZeroDurationAndDefaultsPartialCompute(t *testing.T) {
	input := strings.Replace(validConfig, "readinessPath: /ready", "readinessPath: /ready\n    drainTimeout: 0s", 1)
	input = strings.Replace(input, "type: docker\n    server: Edge_Box", "type: neon\n    region: eu-central-1\n    compute:\n      minCU: 0.5", 1)
	input = strings.Replace(input, "minCU: 0.5", "minCU: 0.5\n      maxCU: 0.5\n      suspendAfter: 5m", 1)
	secrets := strings.Replace(validSecrets, "postgres:\n  main-db:\n    adminPassword: postgres-admin-placeholder", "postgres:\n  main-db:\n    apiToken: neon-token", 1)
	secrets = strings.Replace(secrets, "    postgres:\n      username: sub2api\n      password: postgres-placeholder\n", "", 1)
	validated, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, secrets))
	if err != nil {
		t.Fatal(err)
	}
	if validated.Apps["web-app"].DrainTimeout.String() != "0s" {
		t.Fatalf("explicit zero drain timeout became %s", validated.Apps["web-app"].DrainTimeout)
	}
	if validated.Postgres["main-db"].Compute == nil || validated.Postgres["main-db"].Compute.MaxCU == nil || *validated.Postgres["main-db"].Compute.MaxCU != 0.5 {
		t.Fatalf("partial compute defaults were not applied: %#v", validated.Postgres["main-db"].Compute)
	}
}

func TestValidateAllowsExternalServiceWithoutServiceSecretAndRequiresProxyCredentials(t *testing.T) {
	input := strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: external\n    host: db.example.com", 1)
	input = strings.Replace(input, "type: docker\n    server: Edge_Box", "type: external\n    host: redis.example.com", 1)
	input = strings.Replace(input, "publicAccess:\n      type: none", "outboundProxy:\n      enabled: true\n      type: microsocks\n      servers: [Edge_Box]\n    publicAccess:\n      type: none", 1)
	secrets := strings.Replace(validSecrets, "    postgres:\n      username: sub2api\n      password: postgres-placeholder", "    adminApiKey: proxy-api-key\n    postgres:\n      username: sub2api\n      password: postgres-placeholder", 1)
	secrets = strings.Replace(secrets, "postgres:\n  main-db:\n    adminPassword: postgres-admin-placeholder\nredis:\n  main-cache:\n    adminPassword: redis-admin-placeholder\n", "", 1)
	if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, secrets)); err == nil {
		t.Fatal("Validate accepted an enabled proxy without per-server credentials")
	}
	secrets = strings.Replace(secrets, "reverseProxy:\n", "outboundProxy:\n  Edge_Box:\n    username: proxy\n    password: proxy-password\nreverseProxy:\n", 1)
	validated, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, secrets))
	if err != nil {
		t.Fatal(err)
	}
	if validated.Redis["main-cache"].Port == nil || *validated.Redis["main-cache"].Port != 6379 {
		t.Fatalf("external service default was not applied")
	}
}

func TestValidateRequiresPublicAddressForEveryCloudflarePublicAddressMode(t *testing.T) {
	input := strings.Replace(validConfig, "publicAccess:\n      type: none", "publicAccess:\n      type: cloudflare\n      servers: [Edge_Box]\n      cloudflare:\n        mode: loadBalancer\n        connectBy: publicAddress", 1)
	input = strings.Replace(input, "      public:\n        ipv4: 203.0.113.10\n", "", 1)
	config := mustParseConfig(t, input)
	config.Cloudflare = &CloudflareConfig{ZoneID: "zone"}
	secrets := strings.Replace(validSecrets, "reverseProxy:\n", "cloudflare:\n  apiToken: cf-token\nreverseProxy:\n", 1)
	if _, err := Validate(config, mustParseSecrets(t, secrets)); err == nil {
		t.Fatal("Validate accepted Cloudflare publicAddress without a public server address")
	}
}

func TestValidateRejectsExplicitZeroPortAndDockerTLSField(t *testing.T) {
	for name, input := range map[string]string{
		"postgres port": strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: docker\n    server: Edge_Box\n    port: 0", 1),
		"redis tls":     strings.Replace(validConfig, "main-cache:\n    type: docker\n    server: Edge_Box", "main-cache:\n    type: docker\n    server: Edge_Box\n    tls: false", 1),
	} {
		t.Run(name, func(t *testing.T) {
			config := mustParseConfig(t, input)
			if _, err := Validate(config, mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted explicit invalid %s", name)
			}
		})
	}
}

func TestValidateRequiresRedisUsernameForDockerAndExternal(t *testing.T) {
	input := strings.Replace(validSecrets, "      username: default\n", "", 1)
	if _, err := Validate(mustParseConfig(t, validConfig), mustParseSecrets(t, input)); err == nil {
		t.Fatal("Validate accepted Redis secrets without username")
	}
}

func TestValidateRejectsMalformedServiceHostsAndSingBoxPorts(t *testing.T) {
	for name, input := range map[string]string{
		"external postgres host": strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: external\n    host: DB.Example.com", 1),
		"singBox port suffix":    strings.Replace(validConfig, "  Edge_Box:\n    addresses:", "  Edge_Box:\n    singBox:\n      serverName: relay.example.com\n      target: 10.0.0.2:443x\n    addresses:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			config := mustParseConfig(t, input)
			if _, err := Validate(config, mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted malformed %s", name)
			}
		})
	}
}

func TestValidateRequiresInternalAddressesForMultipleMicroSocksServers(t *testing.T) {
	input := strings.Replace(validConfig, "  Edge_Box:\n    addresses:\n      public:\n        ipv4: 203.0.113.10\n      internal:\n        ipv4: 10.0.0.10", "  Edge_Box:\n    addresses:\n      public:\n        ipv4: 203.0.113.10\n  Other_Box: {}", 1)
	input = strings.Replace(input, "servers: [Edge_Box]\n    postgres:", "servers: [Edge_Box, Other_Box]\n    outboundProxy:\n      enabled: true\n      type: microsocks\n      servers: [Edge_Box, Other_Box]\n    postgres:", 1)
	if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil {
		t.Fatal("Validate accepted multi-server MicroSocks without internal addresses")
	}
}

func TestValidateRejectsKnownComposeControlEnvironmentKeys(t *testing.T) {
	for _, key := range []string{"APP_PROBE_PATH", "SUB2API_IMAGE", "CLOUDFLARE_DNS_API_TOKEN", "BIND_HOST", "PGDATA"} {
		t.Run(key, func(t *testing.T) {
			input := strings.Replace(validConfig, "publicAccess:\n      type: none", "environment:\n      "+key+": unsafe\n    publicAccess:\n      type: none", 1)
			if _, err := Validate(mustParseConfig(t, input), mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted deployment-owned key %s", key)
			}
		})
	}
}

func TestValidateEnforcesTypeSpecificFieldsAndSecretRedaction(t *testing.T) {
	config, err := ParseConfig([]byte(strings.Replace(validConfig, "type: docker\n    server: Edge_Box", "type: external\n    server: Edge_Box", 1)))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := ParseSecrets([]byte(validSecrets))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(config, secrets); err == nil {
		t.Fatal("Validate accepted docker-only server field for external PostgreSQL")
	}

	secrets, err = ParseSecrets([]byte(strings.Replace(validSecrets, "initialAdminPassword: SENTINEL_ONLY_FOR_REDACTION_TEST", "initialAdminPassword: ", 1)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(mustParseConfig(t, validConfig), secrets)
	if err == nil || strings.Contains(err.Error(), "SENTINEL_ONLY_FOR_REDACTION_TEST") {
		t.Fatalf("secret was exposed or missing secret was accepted: %v", err)
	}
}

func TestValidateRejectsEnvironmentKeyOverlapAndReservedKeys(t *testing.T) {
	input := strings.Replace(validConfig, "publicAccess:\n      type: none", "environment:\n      DATABASE_HOST: unsafe\n      FEATURE_FLAG: ordinary\n    publicAccess:\n      type: none", 1)
	config := mustParseConfig(t, input)
	secrets := mustParseSecrets(t, strings.Replace(validSecrets, "postgres:\n      username", "environment:\n      FEATURE_FLAG: secret\n    postgres:\n      username", 1))
	if _, err := Validate(config, secrets); err == nil {
		t.Fatal("Validate accepted reserved or overlapping environment keys")
	}
}

func TestValidateDurationAndPublicAccessRules(t *testing.T) {
	for name, replacement := range map[string]string{
		"bare duration":        "drainTimeout: 10",
		"day duration":         "drainTimeout: 1d",
		"health path":          "readinessPath: /health",
		"access subset":        "publicAccess:\n      type: external\n      servers: [missing]",
		"cross server address": "servers:\n  Edge_Box:\n    addresses:\n      public:\n        ipv4: 203.0.113.10\n  Other_Box: {}",
	} {
		t.Run(name, func(t *testing.T) {
			input := validConfig
			if name == "cross server address" {
				input = strings.Replace(input, "servers: [Edge_Box]", "servers: [Edge_Box, Other_Box]", 1)
				input = strings.Replace(input, "type: docker\n    server: Edge_Box", "type: docker\n    server: Other_Box", 1)
			} else if name == "access subset" {
				input = strings.Replace(input, "publicAccess:\n      type: none", replacement, 1)
			} else if name == "health path" {
				input = strings.Replace(input, "readinessPath: /ready", replacement, 1)
			} else {
				input = strings.Replace(input, "readinessPath: /ready", "readinessPath: /ready\n    "+replacement, 1)
			}
			config, err := ParseConfig([]byte(input))
			if err != nil {
				return
			}
			if _, err := Validate(config, mustParseSecrets(t, validSecrets)); err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
		})
	}
}

func TestResolveEnvironmentRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	environments := filepath.Join(root, "environments")
	if err := os.Mkdir(environments, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "config.yaml"), []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(environments, "production")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveEnvironment(root, "production"); err == nil {
		t.Fatal("ResolveEnvironment accepted an environment symlink escaping environments root")
	}
	if _, err := ResolveEnvironment(root, "../outside"); err == nil {
		t.Fatal("ResolveEnvironment accepted an invalid environment ID")
	}
}

func mustParseConfig(t *testing.T, input string) Config {
	t.Helper()
	config, err := ParseConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func mustParseSecrets(t *testing.T, input string) Secrets {
	t.Helper()
	secrets, err := ParseSecrets([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return secrets
}
