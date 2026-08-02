package main

import (
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/upstash/pulumi-upstash/sdk/go/upstash"
)

type RedisConnection struct {
	Host      string
	Port      int
	Username  string
	Password  string
	DB        int
	EnableTLS bool
}

func ManagedUpstashDatabaseName(namespace, explicitName string) string {
	if strings.TrimSpace(explicitName) != "" {
		return strings.TrimSpace(explicitName)
	}
	return namespace + "-redis"
}

func BuildRedisConnection(config DeploymentConfig) RedisConnection {
	if config.RedisMode == "docker" {
		return RedisConnection{Host: "redis", Port: 6379, Username: config.RedisUsername, Password: config.RedisPassword, DB: 0, EnableTLS: false}
	}
	return RedisConnection{Host: config.UpstashHost, Port: config.UpstashPort, Username: config.UpstashUsername, Password: config.UpstashPassword, DB: 0, EnableTLS: true}
}

func CreateUpstashConnection(ctx *pulumi.Context, config DeploymentConfig, apiKey pulumi.StringInput) (RedisConnectionInputs, error) {
	provider, err := upstash.NewProvider(ctx, "upstash", &upstash.ProviderArgs{
		ApiKey: apiKey.ToStringPtrOutput(),
		Email:  pulumi.StringPtr(config.UpstashEmail),
	}, pulumi.Version("0.5.0"))
	if err != nil {
		return RedisConnectionInputs{}, err
	}
	database, err := upstash.NewRedisDatabase(ctx, config.ResourceNamespace+"-upstash-redis", &upstash.RedisDatabaseArgs{
		DatabaseName: pulumi.String(ManagedUpstashDatabaseName(config.ResourceNamespace, config.UpstashDatabaseName)),
		Region:       pulumi.String(config.UpstashRegion),
		Tls:          pulumi.BoolPtr(true),
	}, pulumi.Provider(provider), pulumi.Version("0.5.0"))
	if err != nil {
		return RedisConnectionInputs{}, err
	}
	return RedisConnectionInputs{
		Host:      database.Endpoint,
		Port:      database.Port,
		Username:  pulumi.String("default"),
		Password:  pulumi.ToSecret(database.Password).(pulumi.StringOutput),
		DB:        pulumi.Int(0),
		EnableTLS: pulumi.Bool(true),
	}, nil
}
