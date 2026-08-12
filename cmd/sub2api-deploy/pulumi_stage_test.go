package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/secrets/passphrase"
	"github.com/pulumi/pulumi/sdk/v3/go/common/encoding"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"gopkg.in/yaml.v3"
)

const (
	stagePassphrase       = "STAGE_PASSPHRASE_CANARY"
	stageWrongPassphrase  = "STAGE_WRONG_PASSPHRASE_CANARY"
	stageEnvironment      = "STAGE_ENVIRONMENT_CONFIG_CANARY"
	stageSecrets          = "STAGE_ENVIRONMENT_SECRETS_CANARY"
	stageRevision         = "STAGE_REVISION_KEY_CANARY"
	stageUnrelatedValue   = "STAGE_UNRELATED_VALUE_CANARY"
	stageUnrelatedSecret  = "STAGE_UNRELATED_SECRET_CANARY"
	stageUnknownTopLevel  = "STAGE_UNKNOWN_TOP_LEVEL_CANARY"
	stageStaleConfig      = "STAGE_STALE_CONFIG_CANARY"
	stageStaleSecrets     = "STAGE_STALE_SECRETS_CANARY"
	stageStaleRevision    = "STAGE_STALE_REVISION_CANARY"
	stageSourcePath       = "Pulumi.staged.yaml"
)

func TestRenderStagedStackEncryptsValuesWithoutLeakingPlaintext(t *testing.T) {
	project, source, manager, _ := stagedStackFixture(t)
	before := append([]byte(nil), source...)
	values := stagedStackValues()

	rendered, err := renderStagedStack(context.Background(), project, source, stageSourcePath, stagePassphrase, values)
	if err != nil {
		t.Fatalf("renderStagedStack() error = %v", err)
	}
	if !bytes.Equal(source, before) {
		t.Fatal("renderStagedStack() modified source bytes")
	}
	stack := loadStagedStack(t, project, rendered)

	assertStackValue(t, stack, "sub2api-environment:environmentConfig", false, stageEnvironment, config.NopDecrypter)
	assertStackValue(t, stack, "sub2api-environment:environmentSecrets", true, stageSecrets, manager.Decrypter())
	assertStackValue(t, stack, "sub2api-host:revisionKey", true, stageRevision, manager.Decrypter())
	for _, canary := range []string{stagePassphrase, stageUnrelatedSecret, stageSecrets, stageRevision, stageStaleConfig, stageStaleSecrets, stageStaleRevision} {
		if bytes.Contains(rendered, []byte(canary)) {
			t.Fatalf("rendered stack exposed %q", canary)
		}
	}
}

func TestRenderStagedStackPreservesUnrelatedStackContent(t *testing.T) {
	project, source, manager, salt := stagedStackFixture(t)
	sourceStack := loadStagedStack(t, project, source)
	sourceCiphertext := valueText(t, sourceStack, "sub2api-environment:unrelatedSecret", config.NopDecrypter)

	rendered, err := renderStagedStack(context.Background(), project, source, stageSourcePath, stagePassphrase, stagedStackValues())
	if err != nil {
		t.Fatalf("renderStagedStack() error = %v", err)
	}
	stack := loadStagedStack(t, project, rendered)
	assertStackValue(t, stack, "sub2api-environment:unrelated", false, stageUnrelatedValue, config.NopDecrypter)
	assertStackValue(t, stack, "sub2api-environment:unrelatedSecret", true, stageUnrelatedSecret, manager.Decrypter())
	if got := valueText(t, stack, "sub2api-environment:unrelatedSecret", config.NopDecrypter); got != sourceCiphertext {
		t.Fatalf("unrelated ciphertext = %q, want original %q", got, sourceCiphertext)
	}
	if stack.EncryptionSalt != salt || stack.SecretsProvider != "" || stack.EncryptedKey != "" {
		t.Fatalf("secrets metadata = salt %q, provider %q, key %q", stack.EncryptionSalt, stack.SecretsProvider, stack.EncryptedKey)
	}
	assertRenderedEnvironment(t, stack)
	root := loadYAMLNode(t, rendered)
	unknown := mappingValue(t, documentMapping(t, root), "unknownTopLevel")
	if unknown.Kind != yaml.ScalarNode || unknown.Value != stageUnknownTopLevel {
		t.Fatalf("unknownTopLevel = %#v, want scalar %q", unknown, stageUnknownTopLevel)
	}
	configNode := mappingValue(t, documentMapping(t, root), "config")
	for _, key := range []string{
		"sub2api-environment:environmentConfig",
		"sub2api-environment:environmentSecrets",
		"sub2api-host:revisionKey",
	} {
		if mappingKeyCount(configNode, key) != 1 {
			t.Fatalf("rendered stack has duplicate or missing %q", key)
		}
	}
}

func TestRenderStagedStackRejectsWrongPassphraseAfterManagerCacheWarms(t *testing.T) {
	project, source, _, salt := stagedStackFixture(t)
	if _, err := passphrase.GetPassphraseSecretsManager(stagePassphrase, salt); err != nil {
		t.Fatalf("warm passphrase manager: %v", err)
	}

	rendered, err := renderStagedStack(context.Background(), project, source, stageSourcePath, stageWrongPassphrase, stagedStackValues())
	assertStagedStackRejected(t, rendered, err, stagePassphrase, stageWrongPassphrase, stageUnrelatedSecret, stageSecrets, stageRevision, stageStaleSecrets, stageStaleRevision)
}

func TestRenderStagedStackRejectsUnsupportedSecretsMetadata(t *testing.T) {
	project, source, _, salt := stagedStackFixture(t)
	for _, test := range []struct {
		name     string
		source   []byte
		canaries []string
	}{
		{"no metadata", editStageMetadata(t, source, func(root *yaml.Node) { deleteMappingKey(root, "encryptionsalt") }), nil},
		{"unknown state version", editStageMetadata(t, source, func(root *yaml.Node) { setMappingScalar(root, "encryptionsalt", "v2:STAGE_UNKNOWN_VERSION_CANARY") }), []string{"STAGE_UNKNOWN_VERSION_CANARY"}},
		{"bad salt encoding", editStageMetadata(t, source, func(root *yaml.Node) { setMappingScalar(root, "encryptionsalt", "v1:STAGE_BAD_BASE64_CANARY:ciphertext") }), []string{"STAGE_BAD_BASE64_CANARY"}},
		{"bad salt ciphertext", editStageMetadata(t, source, func(root *yaml.Node) { setMappingScalar(root, "encryptionsalt", "v1:YWJj:STAGE_BAD_CIPHERTEXT_CANARY") }), []string{"STAGE_BAD_CIPHERTEXT_CANARY"}},
		{"provider only", editStageMetadata(t, source, func(root *yaml.Node) { deleteMappingKey(root, "encryptionsalt"); setMappingScalar(root, "secretsprovider", "awskms://STAGE_PROVIDER_CANARY") }), []string{"STAGE_PROVIDER_CANARY"}},
		{"encrypted key only", editStageMetadata(t, source, func(root *yaml.Node) { deleteMappingKey(root, "encryptionsalt"); setMappingScalar(root, "encryptedkey", "STAGE_ENCRYPTED_KEY_CANARY") }), []string{"STAGE_ENCRYPTED_KEY_CANARY"}},
		{"provider and encrypted key", editStageMetadata(t, source, func(root *yaml.Node) { deleteMappingKey(root, "encryptionsalt"); setMappingScalar(root, "secretsprovider", "awskms://STAGE_PROVIDER_CANARY"); setMappingScalar(root, "encryptedkey", "STAGE_ENCRYPTED_KEY_CANARY") }), []string{"STAGE_PROVIDER_CANARY", "STAGE_ENCRYPTED_KEY_CANARY"}},
		{"mixed salt provider and key", editStageMetadata(t, source, func(root *yaml.Node) { setMappingScalar(root, "encryptionsalt", salt); setMappingScalar(root, "secretsprovider", "awskms://STAGE_PROVIDER_CANARY"); setMappingScalar(root, "encryptedkey", "STAGE_ENCRYPTED_KEY_CANARY") }), []string{"STAGE_PROVIDER_CANARY", "STAGE_ENCRYPTED_KEY_CANARY"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered, err := renderStagedStack(context.Background(), project, test.source, stageSourcePath, stagePassphrase, stagedStackValues())
			assertStagedStackRejected(t, rendered, err, append([]string{stagePassphrase, stageUnrelatedSecret, stageSecrets, stageRevision, stageStaleSecrets, stageStaleRevision}, test.canaries...)...)
		})
	}
}

func TestRenderStagedStackRejectsCancelledContext(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rendered, err := renderStagedStack(ctx, project, source, stageSourcePath, stagePassphrase, stagedStackValues())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("renderStagedStack() error = %v, want context.Canceled", err)
	}
	assertStagedStackRejected(t, rendered, err, stagePassphrase, stageUnrelatedSecret, stageSecrets, stageRevision, stageStaleSecrets, stageStaleRevision)
}

func stagedStackFixture(t *testing.T) (*workspace.Project, []byte, secrets.Manager, string) {
	t.Helper()
	state, manager, err := passphrase.NewPassphraseSecretsManager(stagePassphrase)
	if err != nil {
		t.Fatalf("NewPassphraseSecretsManager() error = %v", err)
	}
	metadata := &workspace.ProjectStack{}
	if err := passphrase.EditProjectStack(metadata, manager.State()); err != nil {
		t.Fatalf("EditProjectStack() error = %v", err)
	}
	ciphertext, err := manager.Encrypter().EncryptValue(context.Background(), stageUnrelatedSecret)
	if err != nil {
		t.Fatalf("encrypt unrelated value: %v", err)
	}
	staleSecrets, err := manager.Encrypter().EncryptValue(context.Background(), stageStaleSecrets)
	if err != nil {
		t.Fatalf("encrypt stale environment secrets: %v", err)
	}
	staleRevision, err := manager.Encrypter().EncryptValue(context.Background(), stageStaleRevision)
	if err != nil {
		t.Fatalf("encrypt stale revision key: %v", err)
	}
	project := &workspace.Project{Name: tokens.PackageName("sub2api-environment"), Runtime: workspace.NewProjectRuntimeInfo("go", nil)}
	source := []byte("# staged stack fixture\n" +
		"encryptionsalt: " + metadata.EncryptionSalt + "\n" +
		"config:\n" +
		"  sub2api-environment:unrelated: " + stageUnrelatedValue + "\n" +
		"  sub2api-environment:unrelatedSecret:\n" +
		"    secure: " + ciphertext + "\n" +
		"  sub2api-environment:environmentConfig: " + stageStaleConfig + "\n" +
		"  sub2api-environment:environmentSecrets:\n" +
		"    secure: " + staleSecrets + "\n" +
		"  sub2api-host:revisionKey:\n" +
		"    secure: " + staleRevision + "\n" +
		"environment:\n" +
		"  imports:\n" +
		"    - shared-environment\n" +
		"  values:\n" +
		"    retained: true\n" +
		"unknownTopLevel: " + stageUnknownTopLevel + "\n")
	return project, source, manager, state
}

func stagedStackValues() stackConfigValues {
	return stackConfigValues{
		environmentConfig:  stageEnvironment,
		environmentSecrets: stageSecrets,
		revisionKey:        stageRevision,
	}
}

func loadStagedStack(t *testing.T, project *workspace.Project, source []byte) *workspace.ProjectStack {
	t.Helper()
	stack, err := workspace.LoadProjectStackBytes(nil, project, source, stageSourcePath, encoding.YAML)
	if err != nil {
		t.Fatalf("LoadProjectStackBytes() error = %v\nsource:\n%s", err, source)
	}
	return stack
}

func assertStackValue(t *testing.T, stack *workspace.ProjectStack, key string, secure bool, want string, decrypter config.Decrypter) {
	t.Helper()
	value, ok := stack.Config[config.MustParseKey(key)]
	if !ok {
		t.Fatalf("stack config missing %q", key)
	}
	if value.Secure() != secure {
		t.Fatalf("stack config %q secure = %t, want %t", key, value.Secure(), secure)
	}
	if got := valueText(t, stack, key, decrypter); got != want {
		t.Fatalf("stack config %q = %q, want %q", key, got, want)
	}
}

func valueText(t *testing.T, stack *workspace.ProjectStack, key string, decrypter config.Decrypter) string {
	t.Helper()
	value, ok := stack.Config[config.MustParseKey(key)]
	if !ok {
		t.Fatalf("stack config missing %q", key)
	}
	text, err := value.Value(decrypter)
	if err != nil {
		t.Fatalf("stack config %q value: %v", key, err)
	}
	return text
}

func assertStagedStackRejected(t *testing.T, rendered []byte, err error, canaries ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("renderStagedStack() unexpectedly succeeded")
	}
	if len(rendered) != 0 {
		t.Fatalf("renderStagedStack() returned output on failure: %q", rendered)
	}
	for _, canary := range canaries {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("renderStagedStack() exposed %q in error: %v", canary, err)
		}
	}
}

func assertRenderedEnvironment(t *testing.T, stack *workspace.ProjectStack) {
	t.Helper()
	if stack.Environment == nil {
		t.Fatal("environment was not preserved")
	}
	var environment yaml.Node
	if err := yaml.Unmarshal(stack.EnvironmentBytes(), &environment); err != nil {
		t.Fatalf("parse preserved environment: %v", err)
	}
	root := documentMapping(t, &environment)
	imports := mappingValue(t, root, "imports")
	if imports.Kind != yaml.SequenceNode || len(imports.Content) != 1 || imports.Content[0].Value != "shared-environment" {
		t.Fatalf("environment imports = %#v", imports)
	}
	values := mappingValue(t, root, "values")
	retained := mappingValue(t, values, "retained")
	if retained.Kind != yaml.ScalarNode || retained.Tag != "!!bool" || retained.Value != "true" {
		t.Fatalf("environment values.retained = %#v", retained)
	}
}

func editStageMetadata(t *testing.T, source []byte, edit func(*yaml.Node)) []byte {
	t.Helper()
	root := loadYAMLNode(t, source)
	edit(documentMapping(t, root))
	var rendered bytes.Buffer
	encoder := yaml.NewEncoder(&rendered)
	if err := encoder.Encode(root); err != nil {
		t.Fatalf("encode metadata fixture: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("close metadata fixture encoder: %v", err)
	}
	return rendered.Bytes()
}

func loadYAMLNode(t *testing.T, source []byte) *yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal(source, &node); err != nil {
		t.Fatalf("parse YAML node: %v", err)
	}
	return &node
}

func documentMapping(t *testing.T, node *yaml.Node) *yaml.Node {
	t.Helper()
	if node.Kind != yaml.DocumentNode || len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		t.Fatalf("YAML document is not a mapping: %#v", node)
	}
	return node.Content[0]
}

func mappingValue(t *testing.T, mapping *yaml.Node, key string) *yaml.Node {
	t.Helper()
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	t.Fatalf("mapping missing %q", key)
	return nil
}

func mappingKeyCount(mapping *yaml.Node, key string) int {
	count := 0
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			count++
		}
	}
	return count
}

func setMappingScalar(mapping *yaml.Node, key, value string) {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func deleteMappingKey(mapping *yaml.Node, key string) {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			return
		}
	}
}
