//go:build linux

package cloudflarecontract_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/cloudflareresource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	providerBinaryEnv = "CF_PROVIDER_BINARY"
	requiredModeEnv   = "CF_PROVIDER_CONTRACT_REQUIRED"
	providerVersion   = "6.18.0"

	providerToken    = "pulumi:providers:cloudflare"
	dnsRecordToken   = "cloudflare:index/dnsRecord:DnsRecord"
	zoneSettingToken = "cloudflare:index/zoneSetting:ZoneSetting"
	maxSchemaBytes   = 64 << 20
)

// TestOfficialCloudflareProviderSchemaContract gates the typed registration
// inputs against the fixed official v6.18.0 provider schema. The source oracle
// is provider/cmd/pulumi-resource-cloudflare/schema.json at tag v6.18.0.
func TestOfficialCloudflareProviderSchemaContract(t *testing.T) {
	requireLoopbackOnlyNetwork(t)
	wire := captureTypedRegistrationInputs(t)
	provider := startOfficialProvider(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	response, err := provider.client.GetSchema(ctx, &pulumirpc.GetSchemaRequest{})
	if err != nil {
		t.Fatalf("official Cloudflare provider GetSchema failed: %s", publicRPCError(err))
	}
	var schema packageSchema
	if err := json.Unmarshal([]byte(response.GetSchema()), &schema); err != nil {
		t.Fatalf("official Cloudflare provider returned invalid schema: %s", publicError(err))
	}
	// The checked-in schema omits version; a release binary may inject it.
	if schema.Name != "cloudflare" || (schema.Version != nil && schema.Version != providerVersion) {
		t.Fatalf("official provider schema identity differs from v%s contract", providerVersion)
	}
	if providerToken != "pulumi:providers:"+schema.Name {
		t.Fatal("official provider token differs from typed registration token")
	}
	assertSchemaAcceptsWireInputs(t, schema, providerToken, wire[providerToken], map[string]fieldContract{
		"apiKey": {typ: "string"}, "apiToken": {typ: "string"}, "apiUserServiceKey": {typ: "string"},
	})
	if len(schema.Config.Required) != 0 {
		t.Fatal("official v6.18.0 provider schema unexpectedly requires configuration")
	}
	assertSchemaAcceptsWireInputs(t, schema, dnsRecordToken, wire[dnsRecordToken], map[string]fieldContract{
		"zoneId": {typ: "string", required: true}, "name": {typ: "string", required: true}, "content": {typ: "string"},
		"proxied": {typ: "boolean"}, "ttl": {typ: "number", required: true}, "type": {typ: "string", required: true},
	})
	assertSchemaAcceptsWireInputs(t, schema, zoneSettingToken, wire[zoneSettingToken], map[string]fieldContract{
		"zoneId": {typ: "string", required: true}, "settingId": {typ: "string", required: true}, "value": {ref: "pulumi.json#/Any", required: true},
	})
	info, err := provider.client.GetPluginInfo(ctx, &emptypb.Empty{})
	if err != nil && status.Code(err) != codes.Unimplemented {
		t.Fatalf("official provider GetPluginInfo failed: %s", publicRPCError(err))
	}
	if err == nil && info.GetVersion() != providerVersion {
		t.Fatalf("official provider plugin version differs from %s", providerVersion)
	}
}

// TestOfficialCloudflareProviderCheckContract passes the exact wire inputs
// captured from the typed constructors to the official provider. It never
// Configure-s the provider and never invokes Create, Update, Delete, or Read.
func TestOfficialCloudflareProviderCheckContract(t *testing.T) {
	requireLoopbackOnlyNetwork(t)
	wire := captureTypedRegistrationInputs(t)
	provider := startOfficialProvider(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for token, name := range map[string]string{dnsRecordToken: "origin", zoneSettingToken: "ssl"} {
		response, err := provider.client.Check(ctx, &pulumirpc.CheckRequest{
			Urn:  "urn:pulumi:contract::cloudflare-contract::" + token + "::" + name,
			News: rpcProperties(t, wire[token]),
		})
		if err != nil {
			if os.Getenv(requiredModeEnv) == "1" {
				t.Fatalf("official provider Check is unavailable without Configure for %s: %s", token, publicRPCError(err))
			}
			t.Skipf("official provider Check needs Configure for %s; schema contract remains covered", token)
		}
		if len(response.GetFailures()) != 0 {
			t.Fatalf("official provider Check rejected typed %s wire inputs", token)
		}
	}
}

type fieldContract struct {
	typ, ref string
	required bool
}

func assertSchemaAcceptsWireInputs(t *testing.T, schema packageSchema, token string, inputs resource.PropertyMap, fields map[string]fieldContract) {
	t.Helper()
	if token == providerToken {
		for name, contract := range fields {
			field, ok := schema.Config.Variables[name]
			if !ok || field.Type != contract.typ || !inputs[resource.PropertyKey(name)].IsSecret() {
				t.Fatalf("provider input %q differs from v%s schema or lost secret marking", name, providerVersion)
			}
		}
		return
	}
	resourceSchema, ok := schema.Resources[token]
	if !ok {
		t.Fatalf("official schema lacks typed resource token %q", token)
	}
	for name, contract := range fields {
		field, exists := resourceSchema.InputProperties[name]
		_, captured := inputs[resource.PropertyKey(name)]
		if !exists || !captured || field.Type != contract.typ || field.Ref != contract.ref || contract.required && !contains(resourceSchema.RequiredInputs, name) {
			t.Fatalf("%s input %q differs from v%s schema", token, name, providerVersion)
		}
	}
}

type packageSchema struct {
	Name      string                    `json:"name"`
	Version   any                       `json:"version"`
	Config    configSchema              `json:"config"`
	Resources map[string]resourceSchema `json:"resources"`
}
type configSchema struct {
	Variables map[string]propertySchema `json:"variables"`
	Required  []string                  `json:"required"`
}
type resourceSchema struct {
	InputProperties map[string]propertySchema `json:"inputProperties"`
	RequiredInputs  []string                  `json:"requiredInputs"`
}
type propertySchema struct {
	Type string `json:"type"`
	Ref  string `json:"$ref"`
}

func captureTypedRegistrationInputs(t *testing.T) map[string]resource.PropertyMap {
	t.Helper()
	mocks := &registrationMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := cloudflareresource.NewProvider(ctx, "cloudflare", &cloudflareresource.ProviderArgs{
			ApiKey: pulumi.StringPtr("key"), ApiToken: pulumi.StringPtr("token"), ApiUserServiceKey: pulumi.StringPtr("service-key"),
		})
		if err != nil {
			return err
		}
		if _, err = cloudflareresource.NewDnsRecord(ctx, "origin", &cloudflareresource.DnsRecordArgs{
			ZoneId: pulumi.String("0123456789abcdef0123456789abcdef"), Name: pulumi.String("origin.example.test"), Content: pulumi.StringPtr("198.51.100.10"),
			Proxied: pulumi.BoolPtr(true), Ttl: pulumi.Float64(1), Type: pulumi.String("A"),
		}, pulumi.Provider(provider)); err != nil {
			return err
		}
		_, err = cloudflareresource.NewZoneSetting(ctx, "ssl", &cloudflareresource.ZoneSettingArgs{
			ZoneId: pulumi.String("0123456789abcdef0123456789abcdef"), SettingId: pulumi.String("ssl"), Value: pulumi.String("strict"),
		}, pulumi.Provider(provider))
		return err
	}, pulumi.WithMocks("cloudflare-contract", "test", mocks))
	if err != nil {
		t.Fatalf("capture typed Cloudflare registrations: %s", publicError(err))
	}
	result := map[string]resource.PropertyMap{}
	for _, item := range mocks.resources {
		if item.TypeToken == providerToken || item.TypeToken == dnsRecordToken || item.TypeToken == zoneSettingToken {
			result[item.TypeToken] = item.Inputs.Copy()
		}
	}
	if len(result) != 3 {
		t.Fatal("typed constructors did not register all Cloudflare contract tokens")
	}
	return result
}

type registrationMocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
}

func (m *registrationMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resources = append(m.resources, args)
	return args.Name + "-id", args.Inputs.Copy(), nil
}
func (*registrationMocks) Call(pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func rpcProperties(t *testing.T, values resource.PropertyMap) *structpb.Struct {
	t.Helper()
	encoded, err := plugin.MarshalProperties(values, plugin.MarshalOptions{KeepUnknowns: true, KeepSecrets: true, KeepResources: true})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type officialProviderProcess struct {
	client pulumirpc.ResourceProviderClient
	conn   *grpc.ClientConn
	cmd    *exec.Cmd
	done   <-chan struct{}
	stdout *os.File
	stderr *limitedBuffer
}

func startOfficialProvider(t *testing.T) *officialProviderProcess {
	t.Helper()
	binary := os.Getenv(providerBinaryEnv)
	if binary == "" {
		if os.Getenv(requiredModeEnv) == "1" {
			t.Fatalf("%s=1 requires %s", requiredModeEnv, providerBinaryEnv)
		}
		t.Skipf("%s is not set; provide the official v%s binary", providerBinaryEnv, providerVersion)
	}
	if !filepath.IsAbs(binary) {
		t.Fatalf("%s must be absolute", providerBinaryEnv)
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("%s must be an executable regular file", providerBinaryEnv)
	}
	home := t.TempDir()
	stdout, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Dir, cmd.Stdout, cmd.Stderr = filepath.Dir(binary), writer, &limitedBuffer{limit: 4096}
	stderr := cmd.Stderr.(*limitedBuffer)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"HOME=" + home, "TMPDIR=" + home, "PULUMI_HOME=" + filepath.Join(home, "pulumi"), "PATH=/usr/bin:/bin", "LANG=C"}
	if err = cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); _ = writer.Close(); close(done) }()
	process := &officialProviderProcess{cmd: cmd, done: done, stdout: stdout, stderr: stderr}
	t.Cleanup(func() {
		if process.conn != nil {
			_ = process.conn.Close()
		}
		stopOfficialProvider(process.cmd, process.done)
		_ = process.stdout.Close()
	})
	reader := bufio.NewReaderSize(stdout, 32)
	port := readProviderPort(t, reader, done, stderr)
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	dialCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, net.JoinHostPort("127.0.0.1", port), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxSchemaBytes)))
	if err != nil {
		t.Fatalf("dial official Cloudflare provider: %s", publicRPCError(err))
	}
	process.client, process.conn = pulumirpc.NewResourceProviderClient(conn), conn
	return process
}

func readProviderPort(t *testing.T, reader *bufio.Reader, done <-chan struct{}, stderr *limitedBuffer) string {
	t.Helper()
	line := make(chan struct {
		value string
		err   error
	}, 1)
	go func() {
		value, err := reader.ReadSlice('\n')
		line <- struct {
			value string
			err   error
		}{strings.TrimSpace(string(value)), err}
	}()
	select {
	case result := <-line:
		port, err := strconv.Atoi(result.value)
		if result.err != nil || port < 1 || port > 65535 {
			t.Fatalf("official provider did not report a valid loopback port (%s)", stderr.summary())
		}
		return result.value
	case <-done:
		t.Fatalf("official provider exited before reporting its port (%s)", stderr.summary())
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for official provider port (%s)", stderr.summary())
	}
	return ""
}

func stopOfficialProvider(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func requireLoopbackOnlyNetwork(t *testing.T) {
	t.Helper()
	if os.Getenv(requiredModeEnv) != "1" {
		return
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("inspect CI network namespace: %s", publicError(err))
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
			t.Fatal("required CI mode needs a loopback-only network namespace")
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func publicRPCError(err error) string { return fmt.Sprintf("RPC status %s", status.Code(err)) }
func publicError(error) string        { return "operation failed" }

type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.buf.Write(value)
	}
	return written, nil
}
func (b *limitedBuffer) summary() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() == 0 {
		return "provider stderr was empty"
	}
	return fmt.Sprintf("provider wrote %d stderr bytes", b.buf.Len())
}
