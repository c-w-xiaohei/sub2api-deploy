package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	if err := pulumi.RunErr(deploymentProgram); err != nil {
		panic(err)
	}
}

type DeploymentExports struct {
	DomainName        pulumi.StringOutput
	DNSRecordID       pulumi.IDOutput
	StrictReadinessID pulumi.IDOutput
	DeploymentID      pulumi.IDOutput
}

func deploymentProgram(ctx *pulumi.Context) error {
	programConfig, err := loadProgramConfig(ctx)
	if err != nil {
		return err
	}
	config := programConfig.DeploymentConfig
	if err := readDeploymentPreflight(config.PostgresMode, config.RedisMode); err != nil {
		return err
	}
	composeChecksum, err := deploymentChecksum()
	if err != nil {
		return err
	}
	exports, err := deployResourceGraph(ctx, programConfig, composeChecksum)
	if err != nil {
		return err
	}
	ctx.Export("domainName", exports.DomainName)
	ctx.Export("dnsRecordId", exports.DNSRecordID)
	ctx.Export("strictReadinessId", exports.StrictReadinessID)
	ctx.Export("deploymentId", exports.DeploymentID)
	return nil
}

func deployResourceGraph(ctx *pulumi.Context, programConfig ProgramConfig, composeChecksum string) (DeploymentExports, error) {
	config := programConfig.DeploymentConfig
	database, err := databaseInputs(ctx, config, programConfig.Secrets)
	if err != nil {
		return DeploymentExports{}, err
	}
	redis, err := redisInputs(ctx, config, programConfig.Secrets)
	if err != nil {
		return DeploymentExports{}, err
	}
	runtimePayload := buildRuntimePayload(config, database, redis, programConfig.Secrets)

	domain, err := CreateDomainResources(ctx, config.Domain, config.OriginIP, config.CloudflareZoneID, programConfig.Secrets["cloudflareApiToken"])
	if err != nil {
		return DeploymentExports{}, err
	}
	infraReconcile, err := CreateInfraReconcileCommand(ctx, config, runtimePayload, composeChecksum, domain.DNSRecord)
	if err != nil {
		return DeploymentExports{}, err
	}
	strictSSL, err := CreateStrictSSLSetting(ctx, domain, infraReconcile)
	if err != nil {
		return DeploymentExports{}, err
	}
	strictReadiness, err := CreateStrictReadinessCommand(ctx, config, BuildInfraTriggers(InfraTriggerInput{
		ResourceNamespace: config.ResourceNamespace,
		Domain:            config.Domain,
		OriginIP:          config.OriginIP,
		PostgresMode:      config.PostgresMode,
		RedisMode:         config.RedisMode,
		TraefikImage:      config.TraefikImage,
		ACMEEmail:         config.ACMEEmail,
		AppProbePath:      config.AppProbePath,
		DrainSeconds:      config.DrainSeconds,
		ComposeChecksum:   composeChecksum,
		ResourceModes:     BuildResourceModes(config),
	}), strictSSL)
	if err != nil {
		return DeploymentExports{}, err
	}
	applicationRelease, err := CreateApplicationReleaseCommand(ctx, config, strictReadiness, infraReconcile)
	if err != nil {
		return DeploymentExports{}, err
	}
	return DeploymentExports{
		DomainName:        pulumi.String(config.Domain).ToStringOutput(),
		DNSRecordID:       domain.DNSRecordID,
		StrictReadinessID: strictReadiness.ID(),
		DeploymentID:      applicationRelease.ID(),
	}, nil
}

func databaseInputs(ctx *pulumi.Context, config DeploymentConfig, secrets map[string]pulumi.StringOutput) (DatabaseConnectionInputs, error) {
	if config.PostgresMode == "neon" && config.NeonResourceMode == "create" {
		return CreateNeonConnection(ctx, config, secrets["neonApiToken"])
	}
	if config.PostgresMode == "neon" && config.NeonDSN != "" {
		return BuildDSNDatabaseConnection(secrets["neonDsn"]), nil
	}
	connection := BuildDatabaseConnection(config)
	password := pulumi.StringInput(secrets["postgresPassword"])
	if config.PostgresMode == "neon" {
		password = secrets["neonPassword"]
	}
	return DatabaseConnectionInputs{
		Host:     pulumi.String(connection.Host),
		Port:     pulumi.Int(connection.Port),
		User:     pulumi.String(connection.User),
		Password: password,
		DBName:   pulumi.String(connection.DBName),
		SSLMode:  connection.SSLMode,
	}, nil
}

func redisInputs(ctx *pulumi.Context, config DeploymentConfig, secrets map[string]pulumi.StringOutput) (RedisConnectionInputs, error) {
	if config.RedisMode == "upstash" && config.UpstashResourceMode == "create" {
		return CreateUpstashConnection(ctx, config, secrets["upstashApiKey"])
	}
	connection := BuildRedisConnection(config)
	password := pulumi.StringInput(secrets["redisPassword"])
	if config.RedisMode == "upstash" {
		password = secrets["upstashPassword"]
	}
	return RedisConnectionInputs{
		Host:      pulumi.String(connection.Host),
		Port:      pulumi.Int(connection.Port),
		Username:  pulumi.String(connection.Username),
		Password:  password,
		DB:        pulumi.Int(connection.DB),
		EnableTLS: pulumi.Bool(connection.EnableTLS),
	}, nil
}

type RedisConnectionInputs struct {
	Host      pulumi.StringInput
	Port      pulumi.IntInput
	Username  pulumi.StringInput
	Password  pulumi.StringInput
	DB        pulumi.IntInput
	EnableTLS pulumi.BoolInput
}

func buildRuntimePayload(config DeploymentConfig, database DatabaseConnectionInputs, redis RedisConnectionInputs, secrets map[string]pulumi.StringOutput) pulumi.StringOutput {
	postgresPassword := pulumi.StringInput(secrets["postgresPassword"])
	if config.PostgresMode == "neon" {
		if config.NeonResourceMode == "create" || config.NeonDSN != "" {
			postgresPassword = database.Password
		} else {
			postgresPassword = secrets["neonPassword"]
		}
	}
	redisPassword := pulumi.StringInput(secrets["redisPassword"])
	if config.RedisMode == "upstash" {
		if config.UpstashResourceMode == "create" {
			redisPassword = redis.Password
		} else {
			redisPassword = secrets["upstashPassword"]
		}
	}

	values := pulumi.All(
		database.Host, database.Port, database.User, database.Password, database.DBName, database.DBName,
		redis.Host, redis.Port, redis.Username, redis.Password, redis.DB, redis.EnableTLS,
		postgresPassword, redisPassword, secrets["cloudflareApiToken"], secrets["adminPassword"], secrets["jwtSecret"], secrets["totpEncryptionKey"],
	).ApplyT(func(values []interface{}) string {
		payload := map[string]interface{}{
			"DATABASE_HOST":            values[0],
			"DATABASE_PORT":            values[1],
			"DATABASE_USER":            values[2],
			"DATABASE_PASSWORD":        values[3],
			"POSTGRES_PASSWORD":        postgresProfilePassword(config, values[12]),
			"POSTGRES_USER":            values[2],
			"POSTGRES_DB":              values[4],
			"DATABASE_DBNAME":          values[5],
			"DATABASE_SSLMODE":         database.SSLMode,
			"REDIS_HOST":               values[6],
			"REDIS_PORT":               values[7],
			"REDIS_USERNAME":           values[8],
			"REDIS_PASSWORD":           values[13],
			"REDIS_DB":                 values[10],
			"REDIS_ENABLE_TLS":         values[11],
			"POSTGRES_MODE":            config.PostgresMode,
			"REDIS_MODE":               config.RedisMode,
			"TRAEFIK_IMAGE":            config.TraefikImage,
			"SLOT":                     "blue",
			"SLOT_DATA_DIR":            "blue",
			"BLUE_CONTAINER_NAME":      "sub2api-blue",
			"GREEN_CONTAINER_NAME":     "sub2api-green",
			"POSTGRES_CONTAINER_NAME":  "sub2api-postgres",
			"REDIS_CONTAINER_NAME":     "sub2api-redis",
			"AUTO_SETUP":               "true",
			"DOMAIN":                   config.Domain,
			"CLOUDFLARE_DNS_API_TOKEN": values[14],
			"ACME_EMAIL":               config.ACMEEmail,
			"ORIGIN_IP":                config.OriginIP,
			"APP_PROBE_PATH":           config.AppProbePath,
			"DRAIN_SECONDS":            config.DrainSeconds,
			"ADMIN_EMAIL":              config.AdminEmail,
			"ADMIN_PASSWORD":           values[15],
			"JWT_SECRET":               values[16],
			"TOTP_ENCRYPTION_KEY":      values[17],
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		return string(encoded)
	}).(pulumi.StringOutput)
	return pulumi.ToSecret(values).(pulumi.StringOutput)
}

func postgresProfilePassword(config DeploymentConfig, password interface{}) interface{} {
	if config.PostgresMode != "docker" {
		return "postgres-profile-disabled"
	}
	return password
}

func deploymentChecksum() (string, error) {
	files := []string{"Pulumi.yaml"}
	for _, directory := range []string{"compose", "scripts", "traefik"} {
		if _, err := os.Stat(directory); os.IsNotExist(err) {
			continue
		}
		if err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				files = append(files, path)
			}
			return nil
		}); err != nil {
			return "", err
		}
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, path := range files {
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00", path, contents)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readDeploymentPreflight(postgresMode, redisMode string) error {
	statePath := filepath.Join("runtime", "deploy-state.json")
	markerPath := filepath.Join("runtime", "bootstrap.marker")
	_, stateErr := os.Stat(statePath)
	_, markerErr := os.Stat(markerPath)
	hasState := stateErr == nil
	hasMarker := markerErr == nil
	if !hasState && !hasMarker {
		return nil
	}
	if hasMarker && !hasState {
		return fmt.Errorf("bootstrap marker exists but deploy-state is missing; restore/adopt state before running pulumi up")
	}
	contents, err := os.ReadFile(statePath)
	if err != nil {
		return err
	}
	var state struct {
		PostgresMode string `json:"postgresMode"`
		RedisMode    string `json:"redisMode"`
	}
	if err := json.Unmarshal(contents, &state); err != nil {
		return err
	}
	if state.PostgresMode == "" || state.RedisMode == "" {
		return fmt.Errorf("deployment state has no persisted postgresMode/redisMode; migration required: verify the existing data placement, then run npx --no-install tsx scripts/deployment-mode.ts adopt runtime/deploy-state.json %s %s", postgresMode, redisMode)
	}
	if state.PostgresMode != "" && state.PostgresMode != postgresMode {
		return fmt.Errorf("postgresMode change from %s to %s requires migration; ordinary pulumi up does not migrate PostgreSQL data", state.PostgresMode, postgresMode)
	}
	if state.RedisMode != "" && state.RedisMode != redisMode {
		return fmt.Errorf("redisMode change from %s to %s requires migration; ordinary pulumi up does not migrate Redis data", state.RedisMode, redisMode)
	}
	return nil
}
