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
	hostImportTarget   string
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

	targetKeyNames := []string{"sub2api-environment:environmentConfig", "sub2api-environment:environmentSecrets", "sub2api-host:revisionKey", "sub2api-environment:hostImportTarget"}
	targetKeys := []config.Key{
		config.MustMakeKey("sub2api-environment", "environmentConfig"),
		config.MustMakeKey("sub2api-environment", "environmentSecrets"),
		config.MustMakeKey("sub2api-host", "revisionKey"),
		config.MustMakeKey("sub2api-environment", "hostImportTarget"),
	}
	content := make([]*yaml.Node, 0, len(configNode.Content)+2*len(targetKeyNames))
	for i := 0; i < len(configNode.Content); i += 2 {
		keyNode := configNode.Content[i]
		if keyNode.Kind != yaml.ScalarNode {
			return nil, errInvalidStagedStack
		}
		if stageMergeKey(keyNode) {
			if err := stageMergeTargets(project, configNode.Content[i+1], targetKeys, make(map[*yaml.Node]bool)); err != nil {
				return nil, errInvalidStagedStack
			}
			content = append(content, keyNode, configNode.Content[i+1])
			continue
		}
		key, err := stageConfigKey(project, keyNode.Value)
		if err != nil {
			return nil, errInvalidStagedStack
		}
		if !isStageTargetKey(key, targetKeys) {
			content = append(content, keyNode, configNode.Content[i+1])
		}
	}
	for _, key := range targetKeyNames {
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
	passphraseState := strings.SplitN(stack.EncryptionSalt, ":", 3)
	if len(passphraseState) != 3 || passphraseState[0] != "v1" {
		return nil, errInvalidStagedStack
	}
	salt, err := decodeCanonicalStageBase64(passphraseState[1], 8)
	if err != nil {
		return nil, errInvalidStagedStack
	}
	sentinelState := strings.Split(passphraseState[2], ":")
	if len(sentinelState) != 3 || sentinelState[0] != "v1" {
		return nil, errInvalidStagedStack
	}
	if _, err := decodeCanonicalStageBase64(sentinelState[1], 12); err != nil {
		return nil, errInvalidStagedStack
	}
	if _, err := decodeCanonicalStageBase64(sentinelState[2], 22); err != nil {
		return nil, errInvalidStagedStack
	}
	crypter := config.NewSymmetricCrypterFromPassphrase(passphrase, salt)
	sentinel, err := crypter.DecryptValue(ctx, passphraseState[2])
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

	targetValues := map[string]*yaml.Node{
		"sub2api-environment:environmentConfig":  {Kind: yaml.ScalarNode, Tag: "!!str", Value: values.environmentConfig},
		"sub2api-environment:environmentSecrets": {Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "secure"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: ciphertexts[0]}}},
		"sub2api-host:revisionKey":               {Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "secure"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: ciphertexts[1]}}},
		"sub2api-environment:hostImportTarget":   {Kind: yaml.ScalarNode, Tag: "!!str", Value: values.hostImportTarget},
	}
	content = content[:len(content)-2*len(targetKeyNames)]
	for _, key := range targetKeyNames {
		content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, targetValues[key])
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
		"sub2api-environment:hostImportTarget":   {values.hostImportTarget, false},
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

func decodeCanonicalStageBase64(value string, expectedSize int) ([]byte, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != expectedSize || base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, errInvalidStagedStack
	}
	return decoded, nil
}

func isStageTargetKey(key config.Key, targets []config.Key) bool {
	for _, target := range targets {
		if key == target {
			return true
		}
	}
	return false
}

func stageMergeKey(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Value == "<<" && (node.Tag == "" || node.Tag == "!" || node.Tag == "!!merge")
}

func stageMergeTargets(project *workspace.Project, value *yaml.Node, targets []config.Key, active map[*yaml.Node]bool) error {
	switch value.Kind {
	case yaml.MappingNode:
		return stageMergedMappingTargets(project, value, targets, active)
	case yaml.AliasNode:
		if value.Alias == nil || value.Alias.Kind != yaml.MappingNode {
			return errInvalidStagedStack
		}
		return stageMergedMappingTargets(project, value.Alias, targets, active)
	case yaml.SequenceNode:
		for _, item := range value.Content {
			if item.Kind == yaml.MappingNode {
				if err := stageMergedMappingTargets(project, item, targets, active); err != nil {
					return err
				}
				continue
			}
			if item.Kind != yaml.AliasNode || item.Alias == nil || item.Alias.Kind != yaml.MappingNode {
				return errInvalidStagedStack
			}
			if err := stageMergedMappingTargets(project, item.Alias, targets, active); err != nil {
				return err
			}
		}
		return nil
	}
	return errInvalidStagedStack
}

func stageMergedMappingTargets(project *workspace.Project, mapping *yaml.Node, targets []config.Key, active map[*yaml.Node]bool) error {
	if len(mapping.Content)%2 != 0 || active[mapping] {
		return errInvalidStagedStack
	}
	active[mapping] = true
	defer delete(active, mapping)
	for i := 0; i < len(mapping.Content); i += 2 {
		keyNode := mapping.Content[i]
		if keyNode.Kind != yaml.ScalarNode {
			return errInvalidStagedStack
		}
		if stageMergeKey(keyNode) {
			if err := stageMergeTargets(project, mapping.Content[i+1], targets, active); err != nil {
				return err
			}
			continue
		}
		key, err := stageConfigKey(project, keyNode.Value)
		if err != nil {
			return errInvalidStagedStack
		}
		if isStageTargetKey(key, targets) {
			return errInvalidStagedStack
		}
	}
	return nil
}
