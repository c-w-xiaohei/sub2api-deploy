package main

import (
	"strings"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/upstash/pulumi-upstash/sdk/go/upstash"
)

type RedisConnection struct { Host string; Port int; Username string; Password string; DB int; EnableTLS bool }
type RedisConnectionInputs struct { Host pulumi.StringInput; Port pulumi.IntInput; Username pulumi.StringInput; Password pulumi.StringInput; DB pulumi.IntInput; EnableTLS pulumi.BoolInput }
func ManagedUpstashDatabaseName(namespace, explicitName string) string { if strings.TrimSpace(explicitName) != "" { return strings.TrimSpace(explicitName) }; return namespace + "-redis" }

func siteRedisInputs(ctx *pulumi.Context, site pulumi.Resource, siteID string, spec SiteSpec, secrets SiteSecrets) (RedisConnectionInputs, error) {
	if spec.Redis.Mode == "docker" { return RedisConnectionInputs{Host: pulumi.String("redis"), Port: pulumi.Int(6379), Username: pulumi.String(""), Password: pulumi.ToSecret(pulumi.String(secrets.Redis.Password)).(pulumi.StringOutput), DB: pulumi.Int(0), EnableTLS: pulumi.Bool(false)}, nil }
	if spec.Redis.ResourceMode == "create" {
		provider, err := upstash.NewProvider(ctx, "site-"+siteID+"-upstash", &upstash.ProviderArgs{ApiKey: pulumi.ToSecret(pulumi.String(secrets.Redis.APIKey)).(pulumi.StringOutput)}, pulumi.Parent(site), pulumi.Version("0.5.0")); if err != nil { return RedisConnectionInputs{}, err }
		// Retirement must explicitly unprotect this persistent database first.
		database, err := upstash.NewRedisDatabase(ctx, "site-"+siteID+"-upstash-redis", &upstash.RedisDatabaseArgs{DatabaseName: pulumi.String(ManagedUpstashDatabaseName(spec.ResourcePrefix, "")), Region: pulumi.String(spec.Redis.Region), Tls: pulumi.BoolPtr(true)}, pulumi.Parent(site), pulumi.Provider(provider), pulumi.Version("0.5.0"), pulumi.Protect(true), pulumi.RetainOnDelete(true)); if err != nil { return RedisConnectionInputs{}, err }
		return RedisConnectionInputs{Host: database.Endpoint, Port: database.Port, Username: pulumi.String("default"), Password: pulumi.ToSecret(database.Password).(pulumi.StringOutput), DB: pulumi.Int(0), EnableTLS: pulumi.Bool(true)}, nil
	}
	endpoint, err := normalizeUpstashEndpoint(spec.Redis.Endpoint)
	if err != nil { return RedisConnectionInputs{}, err }
	return RedisConnectionInputs{Host: pulumi.String(endpoint), Port: pulumi.Int(6379), Username: pulumi.String("default"), Password: pulumi.ToSecret(pulumi.String(secrets.Redis.Password)).(pulumi.StringOutput), DB: pulumi.Int(0), EnableTLS: pulumi.Bool(true)}, nil
}
