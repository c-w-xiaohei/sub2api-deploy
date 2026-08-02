import * as command from "@pulumi/command";
import * as pulumi from "@pulumi/pulumi";
import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { buildDatabaseConnection, buildDsnDatabaseConnection, createNeonConnection, type DatabaseConnectionInputs } from "./database.js";
import { buildRedisConnection, createUpstashConnection, type RedisConnectionInputs } from "./redis.js";
import { createDomainResources, createStrictSslSetting } from "./cloudflare.js";
import { loadDeploymentConfig } from "./config.js";
import { buildInfraTriggers, buildReleaseTriggers } from "./command-triggers.js";
import { readDeploymentPreflight } from "./deployment-preflight.js";

const config = loadDeploymentConfig();
const pulumiConfig = new pulumi.Config();
readDeploymentPreflight(process.cwd(), config.postgresMode, config.redisMode);
const secretValue = (key: string, required: boolean): pulumi.Input<string> => {
  const value = required ? pulumiConfig.requireSecret(key) : pulumiConfig.getSecret(key);
  return value ?? "";
};
const postgresPassword = config.postgresMode === "docker"
  ? secretValue("postgresPassword", true)
  : config.neonResourceMode === "create" || config.neonDsn ? "" : secretValue("neonPassword", true);
const redisPassword = config.redisMode === "docker"
  ? secretValue("redisPassword", true)
  : config.upstashResourceMode === "create" ? "" : secretValue("upstashPassword", true);
const databaseInputs: DatabaseConnectionInputs = config.postgresMode === "neon" && config.neonResourceMode === "create"
  ? createNeonConnection(config, pulumiConfig.requireSecret("neonApiToken"))
  : config.postgresMode === "neon" && config.neonDsn
    ? buildDsnDatabaseConnection(pulumiConfig.requireSecret("neonDsn"))
  : buildDatabaseConnection(config);
const redisInputs: RedisConnectionInputs = config.redisMode === "upstash" && config.upstashResourceMode === "create"
  ? createUpstashConnection(config, pulumiConfig.requireSecret("upstashApiKey"))
  : buildRedisConnection(config);

// The infra command owns this projection. The application image is deliberately
// passed as a separate ignored bootstrap input so image updates belong to release.
const runtimePayload = pulumi.secret(pulumi.all({
  DATABASE_HOST: databaseInputs.host,
  DATABASE_PORT: databaseInputs.port,
  DATABASE_USER: databaseInputs.user,
  DATABASE_PASSWORD: config.postgresMode === "neon" && (config.neonResourceMode === "create" || config.neonDsn) ? databaseInputs.password : postgresPassword,
  POSTGRES_PASSWORD: config.postgresMode === "docker" ? postgresPassword : "postgres-profile-disabled",
  POSTGRES_USER: databaseInputs.user,
  POSTGRES_DB: databaseInputs.dbname,
  DATABASE_DBNAME: databaseInputs.dbname,
  DATABASE_SSLMODE: databaseInputs.sslmode,
  REDIS_HOST: redisInputs.host,
  REDIS_PORT: redisInputs.port,
  REDIS_USERNAME: redisInputs.username,
  REDIS_PASSWORD: config.redisMode === "upstash" && config.upstashResourceMode === "create" ? redisInputs.password : redisPassword,
  REDIS_DB: redisInputs.db,
  REDIS_ENABLE_TLS: redisInputs.enableTls,
  POSTGRES_MODE: config.postgresMode,
  REDIS_MODE: config.redisMode,
  TRAEFIK_IMAGE: config.traefikImage,
  SLOT: "blue",
  SLOT_DATA_DIR: "blue",
  BLUE_CONTAINER_NAME: "sub2api-blue",
  GREEN_CONTAINER_NAME: "sub2api-green",
  POSTGRES_CONTAINER_NAME: "sub2api-postgres",
  REDIS_CONTAINER_NAME: "sub2api-redis",
  AUTO_SETUP: "true",
  DOMAIN: config.domain,
  CLOUDFLARE_DNS_API_TOKEN: secretValue("cloudflareApiToken", true),
  ACME_EMAIL: config.acmeEmail,
  ORIGIN_IP: config.originIp,
  APP_PROBE_PATH: config.appProbePath,
  DRAIN_SECONDS: config.drainSeconds,
  ADMIN_EMAIL: config.adminEmail,
  ADMIN_PASSWORD: secretValue("adminPassword", false),
  JWT_SECRET: secretValue("jwtSecret", false),
  TOTP_ENCRYPTION_KEY: secretValue("totpEncryptionKey", false),
}).apply((values) => JSON.stringify(values)));

function behaviorFiles(directory: string): string[] {
  if (!existsSync(directory)) return [];
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    return entry.isDirectory() ? behaviorFiles(path) : [path];
  });
}

const deploymentFiles = ["Pulumi.yaml", ...behaviorFiles("compose"), ...behaviorFiles("scripts"), ...behaviorFiles("traefik")].sort();
const composeChecksum = createHash("sha256")
  .update(deploymentFiles.map((path) => `${path}\0${readFileSync(path)}\0`).join(""))
  .digest("hex");

// Cloud providers own DNS and optional data resources only.
const domain = createDomainResources({
  domain: config.domain,
  originIp: config.originIp,
  zoneId: config.cloudflareZoneId!,
  apiToken: pulumiConfig.requireSecret("cloudflareApiToken"),
});

// Infra reconciliation owns runtime.env, local data profiles, Traefik, and first blue.
const infraTriggers = buildInfraTriggers({
  resourceNamespace: config.resourceNamespace,
  domain: config.domain,
  originIp: config.originIp,
  postgresMode: config.postgresMode,
  redisMode: config.redisMode,
  traefikImage: config.traefikImage,
  acmeEmail: config.acmeEmail!,
  appProbePath: config.appProbePath,
  drainSeconds: config.drainSeconds,
  composeChecksum,
  resourceModes: JSON.stringify({ postgresResourceMode: config.neonResourceMode, redisResourceMode: config.upstashResourceMode }),
});
const infraReconcile = new command.local.Command("infra-reconcile", {
  create: "bash scripts/infra-reconcile.sh",
  update: "bash scripts/infra-reconcile.sh",
  environment: {
    RUNTIME_JSON: runtimePayload,
    SUB2API_IMAGE: config.sub2apiImage,
    POSTGRES_MODE: config.postgresMode,
    REDIS_MODE: config.redisMode,
    DOMAIN: config.domain,
    APP_PROBE_PATH: config.appProbePath,
    ORIGIN_IP: config.originIp,
    ACME_EMAIL: config.acmeEmail!,
    DRAIN_SECONDS: String(config.drainSeconds),
    TRAEFIK_IMAGE: config.traefikImage,
  },
  logging: "none",
  triggers: infraTriggers,
}, {
  dependsOn: [domain.dnsRecord],
  ignoreChanges: ["environment.SUB2API_IMAGE"],
  additionalSecretOutputs: ["stdout", "stderr"],
});

domain.originReady = infraReconcile;
const strictSsl = createStrictSslSetting(domain);
const postStrictPublicReadiness = new command.local.Command("post-strict-public-readiness", {
  create: 'bash scripts/probe-origin.sh "$DOMAIN" "/health"',
  update: 'bash scripts/probe-origin.sh "$DOMAIN" "/health"',
  environment: { DOMAIN: config.domain },
  logging: "none",
  triggers: infraTriggers,
}, { dependsOn: [strictSsl], additionalSecretOutputs: ["stdout", "stderr"] });

// Release only consumes the desired image; all setup/mode checks remain in the script.
const releaseTriggers = buildReleaseTriggers(config.sub2apiImage);
const applicationRelease = new command.local.Command("application-release", {
  create: "bash scripts/application-release.sh",
  update: "bash scripts/application-release.sh",
  environment: {
    SUB2API_IMAGE: config.sub2apiImage,
  },
  logging: "none",
  triggers: releaseTriggers,
}, {
  dependsOn: [infraReconcile, postStrictPublicReadiness],
  additionalSecretOutputs: ["stdout", "stderr"],
});

export const domainName = pulumi.output(config.domain);
export const dnsRecordId = domain.dnsRecordId;
export const strictReadinessId = postStrictPublicReadiness.id;
// deploymentId identifies the application release; strictReadinessId identifies edge readiness.
export const deploymentId = applicationRelease.id;
