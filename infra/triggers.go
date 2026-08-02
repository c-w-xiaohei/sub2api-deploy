package main

import (
	"fmt"
	"strconv"
)

type InfraTriggerInput struct {
	ResourceNamespace string
	Domain            string
	OriginIP          string
	PostgresMode      string
	RedisMode         string
	TraefikImage      string
	ACMEEmail         string
	AppProbePath      string
	DrainSeconds      int
	ComposeChecksum   string
	ResourceModes     string
}

func BuildInfraTriggers(input InfraTriggerInput) []string {
	return []string{
		"infra-reconcile-v1",
		input.ResourceNamespace,
		input.Domain,
		input.OriginIP,
		input.PostgresMode,
		input.RedisMode,
		input.TraefikImage,
		input.ACMEEmail,
		input.AppProbePath,
		strconv.Itoa(input.DrainSeconds),
		input.ComposeChecksum,
		input.ResourceModes,
	}
}

func BuildReleaseTriggers(sub2APIImage string) []string {
	return []string{sub2APIImage}
}

func BuildResourceModes(config DeploymentConfig) string {
	return fmt.Sprintf(`{"postgresResourceMode":%q,"redisResourceMode":%q}`, config.NeonResourceMode, config.UpstashResourceMode)
}
