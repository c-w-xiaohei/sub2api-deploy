package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestRenderDotenvEscapesAndRejectsUnsafeValues(t *testing.T) {
	rendered, err := RenderDotenv(map[string]any{
		"DATABASE_PASSWORD": `p@ss\word'quoted`,
		"JWT_SECRET":        "jwt-secret",
		"SLOT":              "blue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `DATABASE_PASSWORD="p@ss\\word'quoted"`) || !strings.Contains(rendered, `JWT_SECRET="jwt-secret"`) {
		t.Fatalf("rendered dotenv = %q", rendered)
	}
	for _, value := range []string{"line1\nline2", "bad\x00value"} {
		if _, err := RenderDotenv(map[string]any{"SECRET": value}); err == nil {
			t.Fatalf("RenderDotenv accepted unsafe value %q", value)
		}
	}
}

func TestWriteRuntimeEnvIsAtomicPrivateAndSupportsSlotOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "runtime.env")
	if err := WriteRuntimeEnv(path, map[string]any{"SLOT": "blue", "SLOT_DATA_DIR": "blue"}, "green", "green", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "SLOT=\"green\"") || !strings.Contains(string(data), "SLOT_DATA_DIR=\"green\"") {
		t.Fatalf("runtime env = %q", data)
	}
	if mode := mustStat(t, path).Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 600", mode)
	}
}

func TestWriteDeployStateRejectsUnsupportedFields(t *testing.T) {
	err := WriteDeployState(filepath.Join(t.TempDir(), "deploy-state.json"), map[string]any{
		"activeSlot": "blue", "activeImage": "image@sha256:digest", "password": "secret",
	})
	if err == nil || !strings.Contains(err.Error(), "credential or unsupported") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseHostStateRejectsSecretsAndPreservesLegacyContract(t *testing.T) {
	if _, err := ParseHostState([]byte(`{"version":1,"sites":["code2"],"databaseDsn":"postgres://secret"}`)); err == nil || !strings.Contains(err.Error(), "credential or unsupported") {
		t.Fatalf("secret host state error = %v", err)
	}
	state, err := ParseHostState([]byte(`{"version":1,"sites":["code2"],"legacyCode2":{"runtimeRoot":"runtime","composeProject":"sub2api","routeLayout":"flat","handoverComplete":false}}`))
	if err != nil || state.LegacyCode2 == nil || state.LegacyCode2.HandoverComplete {
		t.Fatalf("legacy state = %#v, error = %v", state, err)
	}
}

func TestRenderSiteRouteValidatesOwnershipAndNormalizesDomain(t *testing.T) {
	template := "Host(`${DOMAIN}`) -> sub2api-${SITE_ID}-${SLOT} (${ACTIVE_EDGE_ALIAS})"
	rendered, err := RenderSiteRoute(template, "code2", "Code2.Example.Test", "blue", "sub2api-code2-blue")
	if err != nil || !strings.Contains(rendered, "code2.example.test") {
		t.Fatalf("rendered = %q, error = %v", rendered, err)
	}
	if _, err := RenderSiteRoute(template, "code3", "code3.example.test", "blue", "sub2api-blue"); err == nil {
		t.Fatal("RenderSiteRoute accepted an alias owned by another Site")
	}
}

func TestSelectNeonEndpointRequiresExactlyOneMatchingEndpoint(t *testing.T) {
	endpoint, err := SelectNeonEndpoint([]NeonEndpoint{{ID: "ep-a", Host: "ep-a.neon.tech"}}, "ep-a.neon.tech")
	if err != nil || endpoint.ID != "ep-a" {
		t.Fatalf("endpoint = %#v, error = %v", endpoint, err)
	}
	if _, err := SelectNeonEndpoint([]NeonEndpoint{{ID: "a", Host: "same"}, {ID: "b", Host: "same"}}, "same"); err == nil {
		t.Fatal("SelectNeonEndpoint accepted an ambiguous host")
	}
}

func TestLegacyAppEnvRequiresExactSafeDotenvMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oidc.env")
	if err := os.WriteFile(path, []byte("OIDC_CLIENT_ID=client\nOIDC_CLIENT_SECRET=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLegacyAppEnv(path, "true", []byte(`{"OIDC_CLIENT_ID":"client","OIDC_CLIENT_SECRET":"secret"}`)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLegacyAppEnv(path, "true", []byte(`{"OIDC_CLIENT_ID":"client","OIDC_CLIENT_SECRET":"secret","EXTRA":"not-on-disk"}`)); err == nil {
		t.Fatal("VerifyLegacyAppEnv accepted an appEnv key absent from legacy oidc.env")
	}
	for _, contents := range []string{"KEY=$(touch /tmp/nope)\n", "KEY=value\nKEY=other\n", "KEY=bad value\n", "KEY=bad\r\n"} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyLegacyAppEnv(path, "true", []byte(`{"KEY":"value"}`)); err == nil {
			t.Fatalf("unsafe legacy dotenv accepted: %q", contents)
		}
	}
}

func TestStateTransitionsPreserveDataModesWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy-state.json")
	initial := map[string]any{"activeSlot": "blue", "activeImage": "old@sha256:old", "postgresMode": "docker", "redisMode": "upstash"}
	if err := WriteDeployState(path, initial); err != nil {
		t.Fatal(err)
	}
	transition, err := TransitionState(path, "green", "new@sha256:new")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(transition), &value); err != nil {
		t.Fatal(err)
	}
	if value["activeSlot"] != "green" || value["previousSlot"] != "blue" || value["previousImage"] != "old@sha256:old" || value["postgresMode"] != "docker" || value["redisMode"] != "upstash" {
		t.Fatalf("transition = %#v", value)
	}
	if _, err := SwapState(path); err == nil {
		t.Fatal("SwapState accepted a state before transition was persisted")
	}
}

func TestRunDispatchesRuntimeCommandsWithoutWritingStdout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.env")
	var output bytes.Buffer
	if err := Run([]string{"dotenv", "write", path, "--slot=green", "--slot-data-dir=green"}, strings.NewReader(`{"TOKEN":"value"}`), &output, &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("dotenv command wrote unexpected stdout: %q", output.String())
	}
	value, err := ReadRuntimeEnv(path, "TOKEN")
	if err != nil || value != "value" {
		t.Fatalf("runtime env TOKEN = %q, error = %v", value, err)
	}
}

func TestRunRejectsMalformedHostStateCommandArity(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{"host-state", "write"}, strings.NewReader(""), &output, &output); err == nil {
		t.Fatal("host-state write accepted missing path and Site IDs")
	}
	if err := Run([]string{"host-state", "write", filepath.Join(t.TempDir(), "state.json")}, strings.NewReader(""), &output, &output); err == nil {
		t.Fatal("host-state write accepted missing Site IDs")
	}
}

func TestJSONFieldAcceptsInlineJSONOnlyThroughStdin(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{"json-field", "--stdin", "serverName"}, strings.NewReader(`{"serverName":"www.cloudflare.com","target":"host.docker.internal:8443"}`), &output, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "www.cloudflare.com" {
		t.Fatalf("stdin JSON field = %q", output.String())
	}
	if err := Run([]string{"json-field", "--stdin", "serverName"}, strings.NewReader(`{"serverName":null}`), &output, &output); err == nil {
		t.Fatal("json-field accepted a null string field")
	}
}

func TestHostStateLegacyFieldsAndMappingStateAreStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-state.json")
	valid := `{"version":1,"sites":["code2"],"legacyCode2":{"runtimeRoot":"runtime","composeProject":"sub2api","routeLayout":"flat","handoverComplete":false}}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckMappingState(path, "pending"); err != nil {
		t.Fatal(err)
	}
	if err := CheckMappingState(path, "either"); err != nil {
		t.Fatal(err)
	}
	if err := CheckMappingState(path, "complete"); err == nil {
		t.Fatal("pending mapping accepted complete")
	}
	if err := CheckMappingState(path, "invalid"); err == nil {
		t.Fatal("invalid requested mapping was accepted")
	}
	for _, value := range []string{"null", `"false"`, "0", "[]"} {
		state := strings.Replace(valid, "false", value, 1)
		if _, err := ParseHostState([]byte(state)); err == nil {
			t.Fatalf("handoverComplete accepted %s", value)
		}
	}
	for _, field := range []string{"runtimeRoot", "composeProject", "routeLayout"} {
		state := strings.Replace(valid, `"`+field+`":"`+map[string]string{"runtimeRoot": "runtime", "composeProject": "sub2api", "routeLayout": "flat"}[field]+`"`, `"`+field+`":null`, 1)
		if _, err := ParseHostState([]byte(state)); err == nil {
			t.Fatalf("%s accepted null", field)
		}
	}
}

func TestNeonProjectListAndConflictRecoveryAreStrict(t *testing.T) {
	requests := make([]string, 0)
	listAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.RequestURI() {
		case "/projects":
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"exists"}`))
				return
			}
			listAttempts++
			if listAttempts == 1 {
				_, _ = w.Write([]byte(`{"projects":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"projects":[],"pagination":{"cursor":"next"}}`))
		case "/projects?cursor=next":
			_, _ = w.Write([]byte(`{"projects":[{"id":"project-id","name":"tenant","region_id":"aws-us-east-1"}]}`))
		case "/projects/project-id":
			_, _ = w.Write([]byte(`{"project":{"id":"project-id","name":"tenant","region_id":"aws-us-east-1","settings":{"quota":3},"active":true}}`))
		case "/projects/project-id/endpoints":
			_, _ = w.Write([]byte(`{"endpoints":[{"id":"endpoint-id","host":"endpoint.neon.tech","type":"read_write"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restoreNeonHTTP(t, server.URL)
	t.Setenv("NEON_API_KEY", "test")
	state := filepath.Join(t.TempDir(), "project.json")
	var output bytes.Buffer
	if err := CreateOrFindNeonProject("tenant", "aws-us-east-1", state, &output); err != nil {
		t.Fatal(err)
	}
	if !contains(requests, "POST /projects") || !contains(requests, "GET /projects?cursor=next") {
		t.Fatalf("requests = %v", requests)
	}
	if !strings.Contains(output.String(), `"default_endpoint_host":"endpoint.neon.tech"`) {
		t.Fatalf("state output = %s", output.String())
	}
	var managed map[string]string
	if err := json.Unmarshal(output.Bytes(), &managed); err != nil {
		t.Fatalf("managed output is incompatible with the Pulumi consumer: %v", err)
	}
	if len(managed) != 4 || managed["id"] != "project-id" || managed["name"] != "tenant" || managed["region_id"] != "aws-us-east-1" {
		t.Fatalf("managed output = %#v", managed)
	}

	server.Close()
	strictServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"pagination":{"cursor":null}}`)) }))
	defer strictServer.Close()
	restoreNeonHTTP(t, strictServer.URL)
	if _, err := listNeonProjects(); err == nil {
		t.Fatal("list accepted an envelope without projects")
	}
}

func TestNeonProjectSelectionRejectsMultipleSameName(t *testing.T) {
	if _, _, err := selectManagedProject([]map[string]any{{"id": "one", "name": "tenant"}, {"id": "two", "name": "tenant"}}, "tenant", "aws-us-east-1"); err == nil {
		t.Fatal("multiple matching Neon projects were accepted")
	}
}

func TestNeonRequestPropagatesTransportFailure(t *testing.T) {
	oldClient := neonHTTPClient
	neonHTTPClient = &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	t.Cleanup(func() { neonHTTPClient = oldClient })
	t.Setenv("NEON_API_KEY", "test")
	if _, _, err := NeonRequest("/projects", http.MethodGet, ""); err == nil {
		t.Fatal("transport failure was accepted")
	}
}

func TestPersistedNeonStateFailsClosedOnUnconfirmedDetailErrors(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		requests := make([]string, 0)
		oldURL, oldClient := neonAPIBaseURL, neonHTTPClient
		neonAPIBaseURL = "https://neon.invalid"
		neonHTTPClient = &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
			requests = append(requests, request.Method+" "+request.URL.Path)
			return nil, errors.New("offline")
		})}
		t.Cleanup(func() { neonAPIBaseURL, neonHTTPClient = oldURL, oldClient })
		t.Setenv("NEON_API_KEY", "test")
		state := filepath.Join(t.TempDir(), "project.json")
		if err := os.WriteFile(state, []byte(`{"id":"project-id","name":"tenant","region_id":"aws-us-east-1","default_endpoint_host":"endpoint.neon.tech"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CreateOrFindNeonProject("tenant", "aws-us-east-1", state, io.Discard); err == nil {
			t.Fatal("persisted transport failure was accepted")
		}
		if len(requests) != 1 || requests[0] != "GET /projects/project-id" {
			t.Fatalf("requests = %v", requests)
		}
	})
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := make([]string, 0)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				if r.URL.Path == "/projects/project-id" {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			restoreNeonHTTP(t, server.URL)
			t.Setenv("NEON_API_KEY", "test")
			state := filepath.Join(t.TempDir(), "project.json")
			if err := os.WriteFile(state, []byte(`{"id":"project-id","name":"tenant","region_id":"aws-us-east-1","default_endpoint_host":"endpoint.neon.tech"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := CreateOrFindNeonProject("tenant", "aws-us-east-1", state, io.Discard); err == nil {
				t.Fatal("persisted detail failure was accepted")
			}
			if len(requests) != 1 || requests[0] != "GET /projects/project-id" {
				t.Fatalf("requests = %v", requests)
			}
		})
	}
}

func TestPersistedNeonStateAllowsOnlyConfirmedNotFoundRecovery(t *testing.T) {
	requests := make([]string, 0)
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/projects/project-id":
			w.WriteHeader(http.StatusNotFound)
		case "/projects":
			if r.Method == http.MethodPost {
				created = true
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"projects":[{"id":"replacement","name":"tenant","region_id":"aws-us-east-1"}]}`))
		case "/projects/replacement":
			_, _ = w.Write([]byte(`{"project":{"id":"replacement","name":"tenant","region_id":"aws-us-east-1"}}`))
		case "/projects/replacement/endpoints":
			_, _ = w.Write([]byte(`{"endpoints":[{"id":"endpoint","host":"replacement.neon.tech","type":"read_write"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restoreNeonHTTP(t, server.URL)
	t.Setenv("NEON_API_KEY", "test")
	state := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(state, []byte(`{"id":"project-id","name":"tenant","region_id":"aws-us-east-1","default_endpoint_host":"endpoint.neon.tech"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateOrFindNeonProject("tenant", "aws-us-east-1", state, io.Discard); err != nil {
		t.Fatal(err)
	}
	if created || !contains(requests, "GET /projects") || contains(requests, "POST /projects") {
		t.Fatalf("requests = %v", requests)
	}
}

func TestPersistedNeonStateFailsClosedOnEndpointFailure(t *testing.T) {
	requests := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/projects/project-id":
			_, _ = w.Write([]byte(`{"project":{"id":"project-id","name":"tenant","region_id":"aws-us-east-1"}}`))
		case "/projects/project-id/endpoints":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	restoreNeonHTTP(t, server.URL)
	t.Setenv("NEON_API_KEY", "test")
	state := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(state, []byte(`{"id":"project-id","name":"tenant","region_id":"aws-us-east-1","default_endpoint_host":"endpoint.neon.tech"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateOrFindNeonProject("tenant", "aws-us-east-1", state, io.Discard); err == nil {
		t.Fatal("persisted endpoint failure was accepted")
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v", requests)
	}
}

func TestPersistedNeonEndpointNotFoundUsesDeterministicLookup(t *testing.T) {
	requests := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/projects/project-id":
			_, _ = w.Write([]byte(`{"project":{"id":"project-id","name":"tenant","region_id":"aws-us-east-1"}}`))
		case "/projects/project-id/endpoints":
			w.WriteHeader(http.StatusNotFound)
		case "/projects":
			_, _ = w.Write([]byte(`{"projects":[{"id":"replacement","name":"tenant","region_id":"aws-us-east-1"}]}`))
		case "/projects/replacement":
			_, _ = w.Write([]byte(`{"project":{"id":"replacement","name":"tenant","region_id":"aws-us-east-1"}}`))
		case "/projects/replacement/endpoints":
			_, _ = w.Write([]byte(`{"endpoints":[{"id":"endpoint","host":"replacement.neon.tech","type":"read_write"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restoreNeonHTTP(t, server.URL)
	t.Setenv("NEON_API_KEY", "test")
	state := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(state, []byte(`{"id":"project-id","name":"tenant","region_id":"aws-us-east-1","default_endpoint_host":"old.neon.tech"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateOrFindNeonProject("tenant", "aws-us-east-1", state, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /projects/project-id", "GET /projects/project-id/endpoints", "GET /projects", "GET /projects/replacement", "GET /projects/replacement/endpoints"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("recovery requests = %v, want %v", requests, want)
	}
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	project, err := parseManagedProject(data)
	if err != nil || project["id"] != "replacement" || project["default_endpoint_host"] != "replacement.neon.tech" {
		t.Fatal("endpoint 404 recovery did not persist the discovered project")
	}
}

func TestNeonJSONRejectsTrailingValuesAndGarbage(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`{"projects":[]} {}`), []byte(`{"projects":[]} trailing`),
	} {
		if err := decodeNeonJSON(payload, &map[string]json.RawMessage{}); err == nil {
			t.Fatalf("accepted trailing JSON %q", payload)
		}
	}
	for _, payload := range [][]byte{
		[]byte(`{"endpoint":{}} {}`), []byte(`{"endpoint":{}} trailing`),
	} {
		if _, err := decodeNeonEndpoint(payload); err == nil {
			t.Fatalf("endpoint accepted trailing JSON %q", payload)
		}
	}
}

func TestNeonEndpointSelectionAndSettingsUseTypedNumbers(t *testing.T) {
	patched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/project/endpoints":
			_, _ = w.Write([]byte(`{"endpoints":[{"id":"readonly","host":"readonly.neon.tech","type":"read_only"},{"id":"untyped","host":"target.neon.tech"}]}`))
		case "/projects/project/endpoints/untyped":
			if r.Method == http.MethodPatch {
				patched = true
			}
			_, _ = w.Write([]byte(`{"endpoint":{"autoscaling_limit_min_cu":0.25,"autoscaling_limit_max_cu":0.25,"suspend_timeout_seconds":300}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restoreNeonHTTP(t, server.URL)
	t.Setenv("NEON_API_KEY", "test")
	if err := ReconcileNeonEndpoint("project", "target.neon.tech", .25, .25, 300); err != nil {
		t.Fatal(err)
	}
	if patched {
		t.Fatal("typed endpoint settings triggered a PATCH")
	}
	if _, err := neonDefaultEndpoint("project"); err != nil {
		t.Fatal(err)
	}
}

func TestNeonLockOnlyReclaimsConfirmedDeadOwner(t *testing.T) {
	state := filepath.Join(t.TempDir(), "project.json")
	restoreProbe := neonProcessProbe
	t.Cleanup(func() { neonProcessProbe = restoreProbe })
	for _, test := range []struct {
		name     string
		probe    error
		wantLock bool
	}{
		{"live", nil, true}, {"dead", syscall.ESRCH, false}, {"permission", syscall.EPERM, true}, {"unknown", errors.New("unknown"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(state+".lock", []byte(`{"pid":123}`), 0o600); err != nil {
				t.Fatal(err)
			}
			neonProcessProbe = func(int) error { return test.probe }
			release, err := acquireNeonLock(state)
			if test.wantLock {
				if err == nil {
					release()
					t.Fatal("lock was reclaimed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func restoreNeonHTTP(t *testing.T, baseURL string) {
	t.Helper()
	oldURL, oldClient := neonAPIBaseURL, neonHTTPClient
	neonAPIBaseURL, neonHTTPClient = baseURL, http.DefaultClient
	t.Cleanup(func() { neonAPIBaseURL, neonHTTPClient = oldURL, oldClient })
}

type roundTripper func(*http.Request) (*http.Response, error)

func (run roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return run(request) }

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
