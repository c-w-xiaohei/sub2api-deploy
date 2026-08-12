package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	stageAliasConfig      = "STAGE_ALIAS_CONFIG_CANARY"
	stageAliasSecrets     = "STAGE_ALIAS_SECRETS_CANARY"
	stageAliasRevision    = "STAGE_ALIAS_REVISION_CANARY"
	stageSecondDocument   = "STAGE_SECOND_DOCUMENT_SECRET_CANARY"
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

func TestRenderStagedStackReplacesSemanticTargetKeyAliases(t *testing.T) {
	project, source, manager, _ := stagedStackFixture(t)
	source = editStageConfig(t, source, func(configNode *yaml.Node) {
		setMappingScalar(configNode, "environmentConfig", stageAliasConfig)
		setMappingSecure(t, configNode, "environmentSecrets", manager, stageAliasSecrets)
		setMappingSecure(t, configNode, "sub2api-host:config:revisionKey", manager, stageAliasRevision)
	})

	rendered, err := renderStagedStack(context.Background(), project, source, stageSourcePath, stagePassphrase, stagedStackValues())
	if err != nil {
		t.Fatalf("renderStagedStack() error = %v", err)
	}
	stack := loadStagedStack(t, project, rendered)
	assertStackValue(t, stack, "sub2api-environment:environmentConfig", false, stageEnvironment, config.NopDecrypter)
	assertStackValue(t, stack, "sub2api-environment:environmentSecrets", true, stageSecrets, manager.Decrypter())
	assertStackValue(t, stack, "sub2api-host:revisionKey", true, stageRevision, manager.Decrypter())
	configNode := mappingValue(t, documentMapping(t, loadYAMLNode(t, rendered)), "config")
	for _, key := range []string{
		"sub2api-environment:environmentConfig",
		"sub2api-environment:environmentSecrets",
		"sub2api-host:revisionKey",
	} {
		if mappingKeyCount(configNode, key) != 1 {
			t.Fatalf("rendered stack has duplicate or missing canonical key %q", key)
		}
	}
	for _, alias := range []string{"environmentConfig", "environmentSecrets", "sub2api-host:config:revisionKey"} {
		if mappingKeyCount(configNode, alias) != 0 {
			t.Fatalf("rendered stack retained semantic target alias %q", alias)
		}
	}
	for _, canary := range []string{stageAliasConfig, stageAliasSecrets, stageAliasRevision} {
		if bytes.Contains(rendered, []byte(canary)) {
			t.Fatalf("rendered stack exposed stale alias %q", canary)
		}
	}
}

func TestRenderStagedStackRejectsConfigMergeAliases(t *testing.T) {
	project, _, manager, salt := stagedStackFixture(t)
	secretsCiphertext, err := manager.Encrypter().EncryptValue(context.Background(), stageSecrets)
	if err != nil {
		t.Fatalf("encrypt merged environment secrets: %v", err)
	}
	revisionCiphertext, err := manager.Encrypter().EncryptValue(context.Background(), stageRevision)
	if err != nil {
		t.Fatalf("encrypt merged revision key: %v", err)
	}
	source := []byte("encryptionsalt: " + salt + "\n" +
		"targetAliases: &targetAliases\n" +
		"  environmentSecrets:\n" +
		"    secure: " + secretsCiphertext + "\n" +
		"  sub2api-host:config:revisionKey:\n" +
		"    secure: " + revisionCiphertext + "\n" +
		"config:\n" +
		"  <<: *targetAliases\n")
	merged := loadStagedStack(t, project, source)
	assertStackValue(t, merged, "sub2api-environment:environmentSecrets", true, stageSecrets, manager.Decrypter())
	assertStackValue(t, merged, "sub2api-host:revisionKey", true, stageRevision, manager.Decrypter())

	rendered, err := renderStagedStack(context.Background(), project, source, stageSourcePath, stagePassphrase, stagedStackValues())
	assertStagedStackRejected(t, rendered, err, stagePassphrase, stageSecrets, stageRevision)
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

func TestRenderStagedStackRejectsNonCanonicalPassphraseStates(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	for _, test := range []struct {
		name     string
		saltSize int
	}{
		{"zero", 0},
		{"one", 1},
		{"seven", 7},
		{"nine", 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := passphraseState(t, make([]byte, test.saltSize))
			rendered, err := renderStagedStack(context.Background(), project, setStageEncryptionSalt(t, source, state), stageSourcePath, stagePassphrase, stagedStackValues())
			assertStagedStackRejected(t, rendered, err, stagePassphrase, stageUnrelatedSecret, stageSecrets, stageRevision)
		})
	}

	state := passphraseState(t, bytes.Repeat([]byte{1}, 8))
	nonCanonical := nonCanonicalBase64State(t, state)
	rendered, err := renderStagedStack(context.Background(), project, setStageEncryptionSalt(t, source, nonCanonical), stageSourcePath, stagePassphrase, stagedStackValues())
	assertStagedStackRejected(t, rendered, err, stagePassphrase, stageUnrelatedSecret, stageSecrets, stageRevision)
}

func TestRenderStagedStackRejectsPassphraseStateBase64Whitespace(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	state := passphraseState(t, bytes.Repeat([]byte{1}, 8))
	for _, test := range []struct {
		name      string
		component int
		whitespace string
	}{
		{"outer salt CR", 0, "\r"},
		{"inner nonce LF", 1, "\n"},
		{"inner ciphertext CR", 2, "\r"},
	} {
		t.Run(test.name, func(t *testing.T) {
			variant := base64WhitespaceState(t, state, test.component, test.whitespace)
			rendered, err := renderStagedStack(context.Background(), project, setStageEncryptionSalt(t, source, variant), stageSourcePath, stagePassphrase, stagedStackValues())
			assertStagedStackRejected(t, rendered, err, stagePassphrase, stageUnrelatedSecret, stageSecrets, stageRevision)
		})
	}
}

func TestRenderStagedStackRejectsMultipleYAMLDocuments(t *testing.T) {
	project, source, _, _ := stagedStackFixture(t)
	source = append(source, []byte("---\nconfig:\n  secondDocumentSecret: "+stageSecondDocument+"\n")...)

	rendered, err := renderStagedStack(context.Background(), project, source, stageSourcePath, stagePassphrase, stagedStackValues())
	assertStagedStackRejected(t, rendered, err, stagePassphrase, stageUnrelatedSecret, stageSecrets, stageRevision, stageSecondDocument)
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

func editStageConfig(t *testing.T, source []byte, edit func(*yaml.Node)) []byte {
	return editStageMetadata(t, source, func(root *yaml.Node) {
		edit(mappingValue(t, root, "config"))
	})
}

func setMappingSecure(t *testing.T, mapping *yaml.Node, key string, manager secrets.Manager, plaintext string) {
	t.Helper()
	ciphertext, err := manager.Encrypter().EncryptValue(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("encrypt %q: %v", key, err)
	}
	setMappingNode(mapping, key, &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "secure"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: ciphertext},
		},
	})
}

func setStageEncryptionSalt(t *testing.T, source []byte, state string) []byte {
	return editStageMetadata(t, source, func(root *yaml.Node) { setMappingScalar(root, "encryptionsalt", state) })
}

func passphraseState(t *testing.T, salt []byte) string {
	t.Helper()
	crypter := config.NewSymmetricCrypterFromPassphrase(stagePassphrase, salt)
	ciphertext, err := crypter.EncryptValue(context.Background(), "pulumi")
	if err != nil {
		t.Fatalf("encrypt passphrase sentinel: %v", err)
	}
	return "v1:" + base64.StdEncoding.EncodeToString(salt) + ":" + ciphertext
}

func nonCanonicalBase64State(t *testing.T, state string) string {
	t.Helper()
	parts := strings.SplitN(state, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("passphrase state parts = %q", parts)
	}
	sentinel := strings.Split(parts[2], ":")
	if len(sentinel) != 3 || sentinel[0] != "v1" {
		t.Fatalf("passphrase sentinel = %q", parts[2])
	}
	canonical := sentinel[2]
	decoded, err := base64.StdEncoding.DecodeString(canonical)
	if err != nil {
		t.Fatalf("decode canonical sentinel ciphertext: %v", err)
	}
	paddingStart := len(canonical)
	for paddingStart > 0 && canonical[paddingStart-1] == '=' {
		paddingStart--
	}
	if paddingStart == len(canonical) || paddingStart == 0 {
		t.Fatalf("sentinel ciphertext is not padded: %q", canonical)
	}
	last := canonical[paddingStart-1]
	mutated := ""
	for _, candidate := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/" {
		if candidate != rune(last) {
			trial := canonical[:paddingStart-1] + string(candidate) + canonical[paddingStart:]
			trialDecoded, trialErr := base64.StdEncoding.DecodeString(trial)
			_, strictErr := base64.StdEncoding.Strict().DecodeString(trial)
			if trialErr == nil && bytes.Equal(trialDecoded, decoded) && strictErr != nil {
				mutated = trial
				break
			}
		}
	}
	if mutated == "" {
		t.Fatal("could not construct noncanonical base64 ciphertext")
	}
	sentinel[2] = mutated
	return strings.Join([]string{parts[0], parts[1], strings.Join(sentinel, ":")}, ":")
}

func base64WhitespaceState(t *testing.T, state string, component int, whitespace string) string {
	t.Helper()
	parts := strings.SplitN(state, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("passphrase state parts = %q", parts)
	}
	values := []string{parts[1]}
	sentinel := strings.Split(parts[2], ":")
	if len(sentinel) != 3 || sentinel[0] != "v1" {
		t.Fatalf("passphrase sentinel = %q", parts[2])
	}
	values = append(values, sentinel[1], sentinel[2])
	if component < 0 || component >= len(values) || len(values[component]) < 2 {
		t.Fatalf("base64 component %d = %q", component, values)
	}
	canonical := values[component]
	variant := canonical[:1] + whitespace + canonical[1:]
	decoded, err := base64.StdEncoding.DecodeString(canonical)
	if err != nil {
		t.Fatalf("decode canonical base64 component: %v", err)
	}
	permissive, err := base64.StdEncoding.DecodeString(variant)
	if err != nil || !bytes.Equal(permissive, decoded) {
		t.Fatalf("permissive base64 decode = %q, %v; want %q", permissive, err, decoded)
	}
	strict, err := base64.StdEncoding.Strict().DecodeString(variant)
	if err != nil || !bytes.Equal(strict, decoded) {
		t.Fatalf("strict base64 decode = %q, %v; want %q", strict, err, decoded)
	}
	values[component] = variant
	return strings.Join([]string{parts[0], values[0], strings.Join([]string{sentinel[0], values[1], values[2]}, ":")}, ":")
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
	setMappingNode(mapping, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func setMappingNode(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func deleteMappingKey(mapping *yaml.Node, key string) {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			return
		}
	}
}
