//go:build linux

package enginegraph_test

import (
	"context"
	"errors"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy/deploytest"
	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
)

const (
	hostProviderPackage      = tokens.Package("sub2api-host")
	cloudflareProviderPackage = tokens.Package("cloudflare")
	upstashProviderPackage    = tokens.Package("upstash")
	hostProviderType         = tokens.Type("sub2api-host:index:Host")
)

// engineProviderLoaders supplies only the providers used by the existing Program graph. These
// providers never leave the test process: deploytest keeps their instances in the plugin host.
func engineProviderLoaders(trace *traceFixture) []*deploytest.ProviderLoader {
	return []*deploytest.ProviderLoader{
		deploytest.NewProviderLoader(hostProviderPackage, semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return hostProvider(trace), nil
		}),
		deploytest.NewProviderLoader(cloudflareProviderPackage, semver.MustParse("6.18.0"), func() (plugin.Provider, error) {
			return cloudflareProvider(trace), nil
		}),
		deploytest.NewProviderLoader(upstashProviderPackage, semver.MustParse("1.0.0"), func() (plugin.Provider, error) {
			return upstashProvider(trace), nil
		}),
	}
}

func hostProvider(trace *traceFixture) plugin.Provider {
	return &deploytest.Provider{
		CheckF: func(_ context.Context, req plugin.CheckRequest) (plugin.CheckResponse, error) {
			if req.Type == hostProviderType {
				trace.recordHostCheck(req)
			}
			return plugin.CheckResponse{Properties: req.News}, nil
		},
		DiffF: func(_ context.Context, req plugin.DiffRequest) (plugin.DiffResult, error) {
			if req.OldInputs.DeepEquals(req.NewInputs) {
				return plugin.DiffResult{Changes: plugin.DiffNone}, nil
			}
			return plugin.DiffResult{Changes: plugin.DiffSome}, nil
		},
		CreateF: func(_ context.Context, req plugin.CreateRequest) (plugin.CreateResponse, error) {
			if req.Preview {
				return plugin.CreateResponse{Properties: req.Properties, Status: resource.StatusOK}, nil
			}
			serverKey := hostServerKey(req.Properties)
			if serverKey == "" {
				return plugin.CreateResponse{}, errors.New("test Host input has no server key")
			}
			if req.URN.Name() != "host-"+serverKey {
				return plugin.CreateResponse{}, errors.New("test Host URN does not match server key")
			}
			// hostReadiness is populated before the engine starts and is immutable during the update.
			if !trace.hostReadiness[serverKey] {
				trace.append("host:" + serverKey + ":create:fail")
				if serverKey == "alpha" {
					return plugin.CreateResponse{}, errors.New(scriptedAlphaFailure)
				}
				return plugin.CreateResponse{}, errors.New("scripted Host " + serverKey + " create failure")
			}
			trace.append("host:" + serverKey + ":create:ok")
			return plugin.CreateResponse{
				ID:         resource.ID("host-" + serverKey),
				Properties: req.Properties,
				Status:     resource.StatusOK,
			}, nil
		},
		DeleteF: func(_ context.Context, req plugin.DeleteRequest) (plugin.DeleteResponse, error) {
			serverKey := hostServerKey(req.Inputs)
			if serverKey == "" {
				return plugin.DeleteResponse{}, errors.New("test Host input has no server key")
			}
			if req.URN.Name() != "host-"+serverKey {
				return plugin.DeleteResponse{}, errors.New("test Host URN does not match server key")
			}
			trace.append("host:" + serverKey + ":delete:ok")
			return plugin.DeleteResponse{Status: resource.StatusOK}, nil
		},
		UpdateF: func(_ context.Context, req plugin.UpdateRequest) (plugin.UpdateResponse, error) {
			serverKey := hostServerKey(req.NewInputs)
			if serverKey == "" {
				return plugin.UpdateResponse{}, errors.New("test Host input has no server key")
			}
			if req.URN.Name() != "host-"+serverKey {
				return plugin.UpdateResponse{}, errors.New("test Host URN does not match server key")
			}
			if req.Preview {
				return plugin.UpdateResponse{Properties: req.NewInputs, Status: resource.StatusOK}, nil
			}
			// hostReadiness is populated before the engine starts and is immutable during the update.
			if !trace.hostReadiness[serverKey] {
				trace.append("host:" + serverKey + ":update:fail")
				if serverKey == "alpha" {
					return plugin.UpdateResponse{}, errors.New("scripted Host alpha update failure")
				}
				return plugin.UpdateResponse{}, errors.New("scripted Host " + serverKey + " update failure")
			}
			trace.append("host:" + serverKey + ":update:ok")
			return plugin.UpdateResponse{Properties: req.NewInputs, Status: resource.StatusOK}, nil
		},
	}
}

func cloudflareProvider(trace *traceFixture) plugin.Provider {
	return &deploytest.Provider{
		CheckF: func(_ context.Context, req plugin.CheckRequest) (plugin.CheckResponse, error) {
			return plugin.CheckResponse{Properties: req.News}, nil
		},
		CreateF: func(_ context.Context, req plugin.CreateRequest) (plugin.CreateResponse, error) {
			if req.URN.Type() != "cloudflare:index/dnsRecord:DnsRecord" {
				return plugin.CreateResponse{}, errors.New("test Cloudflare create request is not a DNS record")
			}
			if req.URN.Name() == "" {
				return plugin.CreateResponse{}, errors.New("test Cloudflare create request has no logical resource name")
			}
			if req.Type == "cloudflare:index/dnsRecord:DnsRecord" && !req.Preview {
				trace.mu.Lock()
				trace.events = append(trace.events, "cloudflare:dns:"+req.URN.Name()+":create:ok")
				trace.publicationEvents = append(trace.publicationEvents, "cloudflare:dns:create")
				trace.mu.Unlock()
			}
			return recordingCreate(req, resource.ID("cloudflare-"+req.Name)), nil
		},
		DeleteF: func(_ context.Context, req plugin.DeleteRequest) (plugin.DeleteResponse, error) {
			if req.URN.Type() != "cloudflare:index/dnsRecord:DnsRecord" {
				return plugin.DeleteResponse{}, errors.New("test Cloudflare delete request is not a DNS record")
			}
			if req.URN.Name() == "" {
				return plugin.DeleteResponse{}, errors.New("test Cloudflare delete request has no logical resource name")
			}
			trace.append("cloudflare:dns:" + req.URN.Name() + ":delete:ok")
			return plugin.DeleteResponse{Status: resource.StatusOK}, nil
		},
	}
}

func upstashProvider(trace *traceFixture) plugin.Provider {
	return &deploytest.Provider{
		CheckF: func(_ context.Context, req plugin.CheckRequest) (plugin.CheckResponse, error) {
			return plugin.CheckResponse{Properties: req.News}, nil
		},
		CreateF: func(_ context.Context, req plugin.CreateRequest) (plugin.CreateResponse, error) {
			if !req.Preview {
				trace.append("upstash:" + req.Name + ":create:ok")
			}
			response := recordingCreate(req, resource.ID("upstash-"+req.Name))
			if req.Type == "upstash:index/redisDatabase:RedisDatabase" && !req.Preview {
				response.Properties["endpoint"] = resource.NewStringProperty("redis.example.test")
				response.Properties["port"] = resource.NewNumberProperty(6380)
				response.Properties["password"] = resource.MakeSecret(resource.NewStringProperty("upstash-password-canary"))
			}
			return response, nil
		},
		UpdateF: func(_ context.Context, req plugin.UpdateRequest) (plugin.UpdateResponse, error) {
			if !req.Preview {
				trace.append("upstash:" + req.Name + ":update:ok")
			}
			return plugin.UpdateResponse{Properties: req.NewInputs, Status: resource.StatusOK}, nil
		},
		DeleteF: func(_ context.Context, req plugin.DeleteRequest) (plugin.DeleteResponse, error) {
			trace.append("upstash:" + req.Name + ":delete:ok")
			return plugin.DeleteResponse{Status: resource.StatusOK}, nil
		},
	}
}

func recordingCreate(req plugin.CreateRequest, id resource.ID) plugin.CreateResponse {
	response := plugin.CreateResponse{Properties: req.Properties, Status: resource.StatusOK}
	if !req.Preview {
		response.ID = id
	}
	return response
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
