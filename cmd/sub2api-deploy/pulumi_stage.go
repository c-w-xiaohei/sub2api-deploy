package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/common/encoding"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"gopkg.in/yaml.v3"
)

var errInvalidStagedStack = errors.New("invalid staged stack")

type stackConfigValues struct {
	environmentConfig  string
	environmentSecrets string
	revisionKey        string
}

func renderStagedStack(ctx context.Context, project *workspace.Project, source []byte, sourcePath, passphrase string, values stackConfigValues) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if project == nil {
		return nil, errInvalidStagedStack
	}

	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	if err := decoder.Decode(&document); err != nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errInvalidStagedStack
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	root := document.Content[0]
	if len(root.Content)%2 != 0 {
		return nil, errInvalidStagedStack
	}
	var configNode *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "config" {
			configNode = root.Content[i+1]
			break
		}
	}
	if configNode == nil || configNode.Kind != yaml.MappingNode || len(configNode.Content)%2 != 0 {
		return nil, errInvalidStagedStack
	}

	preferredKeys := []string{"sub2api-environment:environmentConfig", "sub2api-environment:environmentSecrets", "sub2api-host:revisionKey"}
	preferredTargets := []config.Key{
		config.MustMakeKey("sub2api-environment", "environmentConfig"),
		config.MustMakeKey("sub2api-environment", "environmentSecrets"),
		config.MustMakeKey("sub2api-host", "revisionKey"),
	}
	content := make([]*yaml.Node, 0, len(configNode.Content)+6)
	for i := 0; i < len(configNode.Content); i += 2 {
		keyNode := configNode.Content[i]
		if keyNode.Kind != yaml.ScalarNode || keyNode.Tag == "!!merge" || keyNode.Value == "<<" {
			return nil, errInvalidStagedStack
		}
		key, err := stageConfigKey(project, keyNode.Value)
		if err != nil {
			return nil, errInvalidStagedStack
		}
		isTarget := false
		for _, target := range preferredTargets {
			if key == target {
				isTarget = true
				break
			}
		}
		if !isTarget {
			content = append(content, keyNode, configNode.Content[i+1])
		}
	}
	for _, key := range preferredKeys {
		content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "staged"})
	}
	configNode.Content = content
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var normalized bytes.Buffer
	normalizedEncoder := yaml.NewEncoder(&normalized)
	if err := normalizedEncoder.Encode(&document); err != nil {
		return nil, errInvalidStagedStack
	}
	if err := normalizedEncoder.Close(); err != nil {
		return nil, errInvalidStagedStack
	}
	stack, err := workspace.LoadProjectStackBytes(nil, project, normalized.Bytes(), sourcePath, encoding.YAML)
	if err != nil {
		return nil, errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stack.EncryptionSalt == "" || stack.SecretsProvider != "" || stack.EncryptedKey != "" {
		return nil, errInvalidStagedStack
	}
	state := strings.SplitN(stack.EncryptionSalt, ":", 3)
	if len(state) != 3 || state[0] != "v1" {
		return nil, errInvalidStagedStack
	}
	salt, err := base64.StdEncoding.Strict().DecodeString(state[1])
	if err != nil || len(salt) != 8 || base64.StdEncoding.EncodeToString(salt) != state[1] {
		return nil, errInvalidStagedStack
	}
	sentinelState := strings.Split(state[2], ":")
	if len(sentinelState) != 3 || sentinelState[0] != "v1" {
		return nil, errInvalidStagedStack
	}
	nonce, err := base64.StdEncoding.Strict().DecodeString(sentinelState[1])
	if err != nil || len(nonce) != 12 || base64.StdEncoding.EncodeToString(nonce) != sentinelState[1] {
		return nil, errInvalidStagedStack
	}
	ciphertext, err := base64.StdEncoding.Strict().DecodeString(sentinelState[2])
	if err != nil || len(ciphertext) != 22 || base64.StdEncoding.EncodeToString(ciphertext) != sentinelState[2] {
		return nil, errInvalidStagedStack
	}
	crypter := config.NewSymmetricCrypterFromPassphrase(passphrase, salt)
	sentinel, err := crypter.DecryptValue(ctx, state[2])
	if err != nil || sentinel != "pulumi" {
		return nil, errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ciphertexts, err := crypter.BatchEncrypt(ctx, []string{values.environmentSecrets, values.revisionKey})
	if err != nil || len(ciphertexts) != 2 {
		return nil, errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	targets := map[string]*yaml.Node{
		"sub2api-environment:environmentConfig":  {Kind: yaml.ScalarNode, Tag: "!!str", Value: values.environmentConfig},
		"sub2api-environment:environmentSecrets": {Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "secure"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: ciphertexts[0]}}},
		"sub2api-host:revisionKey":               {Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "secure"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: ciphertexts[1]}}},
	}
	content = content[:len(content)-6]
	for _, key := range preferredKeys {
		content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, targets[key])
	}
	configNode.Content = content

	var rendered bytes.Buffer
	encoder := yaml.NewEncoder(&rendered)
	if err := encoder.Encode(&document); err != nil {
		return nil, errInvalidStagedStack
	}
	if err := encoder.Close(); err != nil {
		return nil, errInvalidStagedStack
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	verified, err := workspace.LoadProjectStackBytes(nil, project, rendered.Bytes(), sourcePath, encoding.YAML)
	if err != nil || verified.EncryptionSalt != stack.EncryptionSalt || verified.SecretsProvider != "" || verified.EncryptedKey != "" {
		return nil, errInvalidStagedStack
	}
	for key, want := range map[string]struct {
		value  string
		secure bool
	}{
		"sub2api-environment:environmentConfig":  {values.environmentConfig, false},
		"sub2api-environment:environmentSecrets": {values.environmentSecrets, true},
		"sub2api-host:revisionKey":               {values.revisionKey, true},
	} {
		value, ok := verified.Config[config.MustParseKey(key)]
		if !ok || value.Secure() != want.secure {
			return nil, errInvalidStagedStack
		}
		text, err := value.Value(crypter)
		if err != nil || text != want.value {
			return nil, errInvalidStagedStack
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rendered.Bytes(), nil
}

func stageConfigKey(project *workspace.Project, value string) (config.Key, error) {
	if !strings.Contains(value, ":") {
		value = project.Name.String() + ":" + value
	}
	return config.ParseKey(value)
}
