//go:build linux

package enginegraph_test

import (
	"context"
	"errors"
	"fmt"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/integration/automationtest"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

const (
	hostProviderPackage      = tokens.Package("sub2api-host")
	cloudflareProviderPackage = tokens.Package("cloudflare")
	upstashProviderPackage    = tokens.Package("upstash")
	hostProviderType         = tokens.Type("sub2api-host:index:Host")
)

func startEngineGraphProviders(ctx context.Context, trace *traceFixture) (map[string]*automationtest.ProviderServer, error) {
	providers := map[string]p.Provider{
		string(hostProviderPackage):      hostProvider(trace),
		string(cloudflareProviderPackage): cloudflareProvider(trace),
		string(upstashProviderPackage):    upstashProvider(trace),
	}
	servers := make(map[string]*automationtest.ProviderServer, len(providers))
	for name, provider := range providers {
		server, err := automationtest.StartProvider(ctx, name, providerVersion(name), provider)
		if err != nil {
			for _, started := range servers {
				started.Close()
			}
			return nil, err
		}
		servers[name] = server
	}
	return servers, nil
}

func providerVersion(name string) string {
	if name == string(cloudflareProviderPackage) {
		return "6.18.0"
	}
	return "1.0.0"
}

func hostProvider(trace *traceFixture) p.Provider {
	return p.Provider{
		GetSchema: func(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
			return p.GetSchemaResponse{Schema: providerSchema(string(hostProviderPackage), providerVersion(string(hostProviderPackage)), string(hostProviderType))}, nil
		},
		Check: func(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
			if req.Urn.Type() == hostProviderType {
				trace.recordHostCheck(req)
			}
			return p.CheckResponse{Inputs: req.Inputs}, nil
		},
		Diff: func(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
			if req.OldInputs.Equals(req.Inputs) {
				return p.DiffResponse{HasChanges: false}, nil
			}
			return p.DiffResponse{HasChanges: true}, nil
		},
		Create: func(_ context.Context, req p.CreateRequest) (p.CreateResponse, error) {
			if req.DryRun {
				return p.CreateResponse{Properties: req.Properties}, nil
			}
			properties := resource.ToResourcePropertyMap(req.Properties)
			serverKey := hostServerKey(properties)
			if serverKey == "" {
				return p.CreateResponse{}, errors.New("test Host input has no server key")
			}
			if req.Urn.Name() != "host-"+serverKey {
				return p.CreateResponse{}, errors.New("test Host URN does not match server key")
			}
			// hostReadiness is populated before the engine starts and is immutable during the update.
			if !trace.hostReadiness[serverKey] {
				trace.append("host:" + serverKey + ":create:fail")
				if serverKey == "alpha" {
					return p.CreateResponse{}, errors.New(scriptedAlphaFailure)
				}
				return p.CreateResponse{}, errors.New("scripted Host " + serverKey + " create failure")
			}
			trace.append("host:" + serverKey + ":create:ok")
			return p.CreateResponse{
				ID:         "host-" + serverKey,
				Properties: req.Properties,
			}, nil
		},
		Delete: func(_ context.Context, req p.DeleteRequest) error {
			serverKey := hostServerKey(resource.ToResourcePropertyMap(req.Properties))
			if serverKey == "" {
				return errors.New("test Host input has no server key")
			}
			if req.Urn.Name() != "host-"+serverKey {
				return errors.New("test Host URN does not match server key")
			}
			trace.append("host:" + serverKey + ":delete:ok")
			return nil
		},
		Update: func(_ context.Context, req p.UpdateRequest) (p.UpdateResponse, error) {
			properties := resource.ToResourcePropertyMap(req.Inputs)
			serverKey := hostServerKey(properties)
			if serverKey == "" {
				return p.UpdateResponse{}, errors.New("test Host input has no server key")
			}
			if req.Urn.Name() != "host-"+serverKey {
				return p.UpdateResponse{}, errors.New("test Host URN does not match server key")
			}
			if req.DryRun {
				return p.UpdateResponse{Properties: req.Inputs}, nil
			}
			// hostReadiness is populated before the engine starts and is immutable during the update.
			if !trace.hostReadiness[serverKey] {
				trace.append("host:" + serverKey + ":update:fail")
				if serverKey == "alpha" {
					return p.UpdateResponse{}, errors.New("scripted Host alpha update failure")
				}
				return p.UpdateResponse{}, errors.New("scripted Host " + serverKey + " update failure")
			}
			trace.append("host:" + serverKey + ":update:ok")
			return p.UpdateResponse{Properties: req.Inputs}, nil
		},
	}
}

func cloudflareProvider(trace *traceFixture) p.Provider {
	return p.Provider{
		GetSchema: func(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
			return p.GetSchemaResponse{Schema: providerSchema(string(cloudflareProviderPackage), providerVersion(string(cloudflareProviderPackage)), "cloudflare:index/dnsRecord:DnsRecord")}, nil
		},
		Check: func(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
			return p.CheckResponse{Inputs: req.Inputs}, nil
		},
		Create: func(_ context.Context, req p.CreateRequest) (p.CreateResponse, error) {
			if req.Urn.Type() != "cloudflare:index/dnsRecord:DnsRecord" {
				return p.CreateResponse{}, errors.New("test Cloudflare create request is not a DNS record")
			}
			if req.Urn.Name() == "" {
				return p.CreateResponse{}, errors.New("test Cloudflare create request has no logical resource name")
			}
			if !req.DryRun {
				trace.mu.Lock()
				trace.events = append(trace.events, "cloudflare:dns:"+req.Urn.Name()+":create:ok")
				trace.publicationEvents = append(trace.publicationEvents, "cloudflare:dns:create")
				trace.mu.Unlock()
			}
			return recordingCreate(req, "cloudflare-"+req.Urn.Name()), nil
		},
		Delete: func(_ context.Context, req p.DeleteRequest) error {
			if req.Urn.Type() != "cloudflare:index/dnsRecord:DnsRecord" {
				return errors.New("test Cloudflare delete request is not a DNS record")
			}
			if req.Urn.Name() == "" {
				return errors.New("test Cloudflare delete request has no logical resource name")
			}
			trace.append("cloudflare:dns:" + req.Urn.Name() + ":delete:ok")
			return nil
		},
	}
}

func upstashProvider(trace *traceFixture) p.Provider {
	return p.Provider{
		GetSchema: func(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
			return p.GetSchemaResponse{Schema: providerSchema(string(upstashProviderPackage), providerVersion(string(upstashProviderPackage)), "upstash:index/redisDatabase:RedisDatabase")}, nil
		},
		Check: func(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
			return p.CheckResponse{Inputs: req.Inputs}, nil
		},
		Create: func(_ context.Context, req p.CreateRequest) (p.CreateResponse, error) {
			if !req.DryRun {
				trace.append("upstash:" + req.Urn.Name() + ":create:ok")
			}
			response := recordingCreate(req, "upstash-"+req.Urn.Name())
			if req.Urn.Type() == "upstash:index/redisDatabase:RedisDatabase" && req.DryRun {
				response.Properties = response.Properties.Set("endpoint", property.New(property.Computed)).Set("port", property.New(property.Computed)).Set("password", property.New(property.Computed).WithSecret(true))
			} else if req.Urn.Type() == "upstash:index/redisDatabase:RedisDatabase" && !req.DryRun {
				response.Properties = response.Properties.Set("endpoint", property.New("redis.example.test")).Set("port", property.New(6380.0)).Set("password", property.New("upstash-password-canary").WithSecret(true))
			}
			return response, nil
		},
		Update: func(_ context.Context, req p.UpdateRequest) (p.UpdateResponse, error) {
			if !req.DryRun {
				trace.append("upstash:" + req.Urn.Name() + ":update:ok")
			}
			return p.UpdateResponse{Properties: req.Inputs}, nil
		},
		Delete: func(_ context.Context, req p.DeleteRequest) error {
			trace.append("upstash:" + req.Urn.Name() + ":delete:ok")
			return nil
		},
	}
}

func recordingCreate(req p.CreateRequest, id string) p.CreateResponse {
	response := p.CreateResponse{Properties: req.Properties}
	if !req.DryRun {
		response.ID = id
	}
	return response
}

func providerSchema(name, version, resourceType string) string {
	return fmt.Sprintf(`{"name":%q,"version":%q,"resources":{"%s":{"inputProperties":{"resource":{"$ref":"pulumi.json#/Any"},"server":{"$ref":"pulumi.json#/Any"},"target":{"$ref":"pulumi.json#/Any"},"secrets":{"$ref":"pulumi.json#/Any"},"name":{"$ref":"pulumi.json#/Any"},"content":{"$ref":"pulumi.json#/Any"},"proxied":{"$ref":"pulumi.json#/Any"},"ttl":{"$ref":"pulumi.json#/Any"},"type":{"$ref":"pulumi.json#/Any"},"zoneId":{"$ref":"pulumi.json#/Any"},"databaseName":{"$ref":"pulumi.json#/Any"},"region":{"$ref":"pulumi.json#/Any"},"tls":{"$ref":"pulumi.json#/Any"}}}}}`, name, version, resourceType)
}

func hostServerKey(inputs resource.PropertyMap) string {
	resourceInput, ok := inputs["resource"]
	if !ok || !resourceInput.IsObject() {
		return ""
	}
	serverKey, ok := resourceInput.ObjectValue()["serverKey"]
	if !ok || !serverKey.IsString() {
		return ""
	}
	return serverKey.StringValue()
}
