//go:build linux

// Package automationtest contains the narrow test-only seams used by the
// external integration suites. It deliberately depends only on public SDK APIs.
package automationtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)


// NewCommand adapts an already-installed official Pulumi CLI to the stock
// Automation API command implementation. stagingRoot is owned by the caller.
func NewCommand(ctx context.Context, cliPath, stagingRoot string) (auto.PulumiCommand, error) {
	if ctx == nil || cliPath == "" || stagingRoot == "" {
		return nil, fmt.Errorf("invalid Pulumi CLI path")
	}
	if !filepath.IsAbs(cliPath) || !filepath.IsAbs(stagingRoot) {
		return nil, fmt.Errorf("Pulumi CLI path and staging root must be absolute")
	}
	info, err := os.Stat(cliPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("Pulumi CLI is not an executable file")
	}
	rootInfo, err := os.Stat(stagingRoot)
	if err != nil || !rootInfo.IsDir() {
		return nil, fmt.Errorf("Pulumi command staging root is not a directory")
	}
	binDir := filepath.Join(stagingRoot, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		return nil, fmt.Errorf("create Pulumi command staging directory: %w", err)
	}
	if err := os.Symlink(cliPath, filepath.Join(binDir, "pulumi")); err != nil {
		return nil, fmt.Errorf("stage Pulumi CLI: %w", err)
	}
	return auto.NewPulumiCommand(&auto.PulumiCommandOptions{
		Root:    stagingRoot,
		Version: semver.Version{Major: 3, Minor: 256, Patch: 0},
	})
}

// ProviderServer starts an SDK provider server on a local TCP port. The
// Pulumi Engine attaches to this address through PULUMI_DEBUG_PROVIDERS.
type ProviderServer struct {
	port   int
	cancel chan bool
	done   <-chan error
	stop   sync.Once
}

func StartProvider(ctx context.Context, name, version string, provider p.Provider) (*ProviderServer, error) {
	if ctx == nil {
		return nil, fmt.Errorf("provider context is nil")
	}
	if provider.Handshake == nil {
		provider.Handshake = func(context.Context, p.HandshakeRequest) (p.HandshakeResponse, error) {
			return p.HandshakeResponse{}, nil
		}
	}
	if provider.CheckConfig == nil {
		provider.CheckConfig = func(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
			return p.CheckResponse{Inputs: req.Inputs}, nil
		}
	}
	if provider.DiffConfig == nil {
		provider.DiffConfig = func(context.Context, p.DiffRequest) (p.DiffResponse, error) {
			return p.DiffResponse{HasChanges: false}, nil
		}
	}
	cancel := make(chan bool)
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: cancel,
		Init: func(server *grpc.Server) error {
			resourceProvider, err := p.RawServer(name, version, provider)(nil)
			if err != nil {
				return err
			}
			pulumirpc.RegisterResourceProviderServer(server, resourceProvider)
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	result := &ProviderServer{port: handle.Port, cancel: cancel, done: handle.Done}
	go func() {
		<-ctx.Done()
		result.Close()
	}()
	return result, nil
}

func (s *ProviderServer) Port() int { return s.port }

func (s *ProviderServer) Close() {
	s.stop.Do(func() {
		close(s.cancel)
		<-s.done
	})
}

// DebugProviders renders the official SDK attach contract without exposing
// provider arguments or any other process environment in test output.
func DebugProviders(ports map[string]int) string {
	providers := make([]string, 0, len(ports))
	for name, port := range ports {
		providers = append(providers, name+":"+strconv.Itoa(port))
	}
	slices.Sort(providers)
	return strings.Join(providers, ",")
}

// DecodeExport converts the public Automation export payload into the public
// apitype deployment shape. It intentionally does not validate Pulumi's
// internal checkpoint integrity algorithm.
func DecodeExport(export apitype.UntypedDeployment) (*apitype.DeploymentV3, error) {
	if len(bytes.TrimSpace(export.Deployment)) == 0 || bytes.Equal(bytes.TrimSpace(export.Deployment), []byte("null")) {
		return &apitype.DeploymentV3{}, nil
	}
	var deployment apitype.DeploymentV3
	if err := json.Unmarshal(export.Deployment, &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}

func ExportResources(export apitype.UntypedDeployment) ([]apitype.ResourceV3, error) {
	deployment, err := DecodeExport(export)
	if err != nil {
		return nil, err
	}
	return deployment.Resources, nil
}

// Resource is the observable subset of an exported resource used by the
// integration assertions. Properties are decoded through the SDK RPC codec so
// secret and computed markers survive the JSON export boundary.
type Resource struct {
	URN                      resource.URN
	Custom                   bool
	Delete                   bool
	ID                       resource.ID
	Type                     tokens.Type
	Inputs                   resource.PropertyMap
	Outputs                  resource.PropertyMap
	Parent                   resource.URN
	Protect                  bool
	External                 bool
	Dependencies             []resource.URN
	InitErrors               []string
	Provider                 string
	PropertyDependencies     map[resource.PropertyKey][]resource.URN
	PendingReplacement       bool
	AdditionalSecretOutputs  []resource.PropertyKey
	Aliases                  []resource.URN
	CustomTimeouts           resource.CustomTimeouts
	RetainOnDelete           bool
	ImportID                 resource.ID
	DeletedWith              resource.URN
	ReplaceWith              []resource.URN
	IgnoreChanges            []string
	RefreshBeforeUpdate      bool
	ViewOf                   resource.URN
}

type Checkpoint struct {
	Manifest          apitype.ManifestV1
	Resources         []Resource
	PendingOperations []apitype.OperationV2
	Metadata          apitype.SnapshotMetadataV1
	Snippets          []apitype.SnippetV1
	Extensions        map[apitype.ExtensionRef]apitype.Extension
}

func DecodeCheckpoint(export apitype.UntypedDeployment) (*Checkpoint, error) {
	deployment, err := DecodeExport(export)
	if err != nil {
		return nil, err
	}
	checkpoint := &Checkpoint{
		Manifest:          deployment.Manifest,
		PendingOperations: deployment.PendingOperations,
		Metadata:          deployment.Metadata,
		Snippets:          deployment.Snippets,
		Extensions:        deployment.Extensions,
		Resources:         make([]Resource, 0, len(deployment.Resources)),
	}
	for _, state := range deployment.Resources {
		inputs, err := ExportPropertyMap(state.Inputs)
		if err != nil {
			return nil, err
		}
		outputs, err := ExportPropertyMap(state.Outputs)
		if err != nil {
			return nil, err
		}
		customTimeouts := resource.CustomTimeouts{}
		if state.CustomTimeouts != nil {
			customTimeouts = *state.CustomTimeouts
		}
		checkpoint.Resources = append(checkpoint.Resources, Resource{
			URN: state.URN, Custom: state.Custom, Delete: state.Delete, ID: state.ID,
			Type: state.Type, Inputs: inputs, Outputs: outputs, Parent: state.Parent,
			Protect: state.Protect, External: state.External,
			Dependencies: append([]resource.URN(nil), state.Dependencies...),
			InitErrors: append([]string(nil), state.InitErrors...), Provider: state.Provider,
			PropertyDependencies: state.PropertyDependencies,
			PendingReplacement: state.PendingReplacement,
			AdditionalSecretOutputs: append([]resource.PropertyKey(nil), state.AdditionalSecretOutputs...),
			Aliases: append([]resource.URN(nil), state.Aliases...), CustomTimeouts: customTimeouts,
			RetainOnDelete: state.RetainOnDelete, ImportID: state.ImportID,
			DeletedWith: state.DeletedWith, ReplaceWith: append([]resource.URN(nil), state.ReplaceWith...),
			IgnoreChanges: append([]string(nil), state.IgnoreChanges...),
			RefreshBeforeUpdate: state.RefreshBeforeUpdate, ViewOf: state.ViewOf,
		})
	}
	return checkpoint, nil
}

func ExportPropertyMap(values map[string]any) (resource.PropertyMap, error) {
	if values == nil {
		return resource.PropertyMap{}, nil
	}
	normalized, err := normalizeDeploymentValues(values)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	var encoded structpb.Struct
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	return plugin.UnmarshalProperties(&encoded, plugin.MarshalOptions{KeepUnknowns: true, KeepResources: true, KeepSecrets: true, KeepOutputValues: true, PropagateNil: true})
}

func normalizeDeploymentValues(value any) (any, error) {
	switch typed := value.(type) {
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			normalized, err := normalizeDeploymentValues(item)
			if err != nil {
				return nil, err
			}
			result[i] = normalized
		}
		return result, nil
	case map[string]any:
		if typed[resource.SigKey] == resource.SecretSig {
			plaintext, ok := typed["plaintext"]
			if !ok || typed["ciphertext"] != nil {
				return nil, fmt.Errorf("exported secret is not plaintext")
			}
			normalized, err := normalizeDeploymentValues(plaintext)
			if err != nil {
				return nil, err
			}
			return map[string]any{resource.SigKey: resource.SecretSig, "value": normalized}, nil
		}
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized, err := normalizeDeploymentValues(item)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	default:
		return value, nil
	}
}
