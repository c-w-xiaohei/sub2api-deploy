import * as pulumi from "@pulumi/pulumi";

export type PostgresMode = "docker" | "neon";
export type RedisMode = "docker" | "upstash";
export type ResourceMode = "existing" | "create";

export interface DeploymentInput {
  domain: string;
  originIp: string;
  postgresMode: PostgresMode;
  redisMode: RedisMode;
  sub2apiImage: string;
  traefikImage: string;
  neonResourceMode?: ResourceMode;
  upstashResourceMode?: ResourceMode;
  cloudflareApiToken?: string;
  cloudflareZoneId?: string;
  acmeEmail?: string;
  postgresUser?: string;
  postgresPassword?: string;
  postgresDb?: string;
  neonHost?: string;
  neonApiToken?: string;
  neonProjectId?: string;
  neonBranchId?: string;
  neonPort?: number;
  neonUser?: string;
  neonPassword?: string;
  neonDb?: string;
  redisHost?: string;
  redisPort?: number;
  redisUsername?: string;
  redisPassword?: string;
  upstashHost?: string;
  upstashApiKey?: string;
  upstashEmail?: string;
  upstashDatabaseName?: string;
  upstashRegion?: string;
  upstashPort?: number;
  upstashUsername?: string;
  upstashPassword?: string;
  adminEmail?: string;
  adminPassword?: string;
  jwtSecret?: string;
  totpEncryptionKey?: string;
  appProbePath?: string;
  drainSeconds?: number;
}

export interface DeploymentConfig extends DeploymentInput {
  postgresMode: PostgresMode;
  redisMode: RedisMode;
  postgresUser: string;
  postgresDb: string;
  neonPort: number;
  neonUser: string;
  neonDb: string;
  redisPort: number;
  redisUsername: string;
  upstashPort: number;
  upstashUsername: string;
  adminEmail: string;
  appProbePath: string;
  drainSeconds: number;
  neonResourceMode: ResourceMode;
  upstashResourceMode: ResourceMode;
}

function required(value: string | undefined, name: string): string {
  if (!value?.trim()) throw new Error(`${name} is required`);
  return value;
}

function selectedMode(value: unknown, name: "postgresMode" | "redisMode"): PostgresMode | RedisMode {
  const allowed = name === "postgresMode" ? ["docker", "neon"] : ["docker", "upstash"];
  if (!allowed.includes(value as string)) {
    throw new Error(`${name} must be docker or ${name === "postgresMode" ? "neon" : "upstash"}`);
  }
  return value as PostgresMode | RedisMode;
}

function resourceMode(value: unknown, name: string): ResourceMode {
  if (value !== "existing" && value !== "create") {
    throw new Error(`${name} must be existing or create`);
  }
  return value;
}

export function validateDeploymentConfig(input: DeploymentInput): DeploymentConfig {
  const postgresMode = selectedMode(input.postgresMode, "postgresMode") as PostgresMode;
  const redisMode = selectedMode(input.redisMode, "redisMode") as RedisMode;
  const config: DeploymentConfig = {
    ...input,
    domain: required(input.domain, "domain"),
    originIp: required(input.originIp, "originIp"),
    sub2apiImage: required(input.sub2apiImage, "sub2apiImage"),
    traefikImage: required(input.traefikImage, "traefikImage"),
    cloudflareZoneId: required(input.cloudflareZoneId, "cloudflareZoneId"),
    acmeEmail: required(input.acmeEmail, "acmeEmail"),
    postgresMode,
    redisMode,
    postgresUser: input.postgresUser?.trim() || "sub2api",
    postgresDb: input.postgresDb?.trim() || "sub2api",
    neonPort: input.neonPort ?? 5432,
    neonUser: input.neonUser?.trim() || "sub2api",
    neonDb: input.neonDb?.trim() || "sub2api",
    redisPort: input.redisPort ?? 6379,
    redisUsername: input.redisUsername ?? "",
    upstashPort: input.upstashPort ?? 6379,
    upstashUsername: input.upstashUsername ?? "default",
    adminEmail: input.adminEmail?.trim() || "admin@sub2api.local",
    appProbePath: input.appProbePath?.trim() || "",
    drainSeconds: input.drainSeconds ?? 10,
    neonResourceMode: resourceMode(input.neonResourceMode ?? "existing", "neonResourceMode"),
    upstashResourceMode: resourceMode(input.upstashResourceMode ?? "existing", "upstashResourceMode"),
  };

  if (!config.sub2apiImage.includes("@sha256:")) {
    throw new Error("sub2apiImage must contain an immutable @sha256 digest");
  }
  required(input.cloudflareApiToken, "cloudflareApiToken");
  const appProbePath = required(input.appProbePath, "appProbePath");
  if (!appProbePath.startsWith("/")) {
    throw new Error("appProbePath must be an absolute path");
  }
  if (appProbePath === "/health") {
    throw new Error("appProbePath must not be /health; /health is only a liveness probe");
  }
  if (postgresMode === "neon") {
    if (config.neonResourceMode === "create") {
      required(input.neonApiToken, "neonApiToken");
      required(input.neonProjectId, "neonProjectId");
      required(input.neonBranchId, "neonBranchId");
    } else {
      required(input.neonHost, "neonHost");
      required(input.neonPassword, "neonPassword");
    }
  } else {
    required(input.postgresPassword, "postgresPassword");
  }
  if (redisMode === "upstash") {
    if (config.upstashResourceMode === "create") {
      required(input.upstashApiKey, "upstashApiKey");
      required(input.upstashEmail, "upstashEmail");
      required(input.upstashDatabaseName, "upstashDatabaseName");
      required(input.upstashRegion, "upstashRegion");
    } else {
      required(input.upstashHost, "upstashHost");
      required(input.upstashPassword, "upstashPassword");
    }
  } else {
    required(input.redisPassword, "redisPassword");
  }
  return config;
}

export function loadDeploymentConfig(): DeploymentConfig {
  const config = new pulumi.Config();
  const get = (key: string): string | undefined => config.get(key);
  const configuredSecret = (key: string): string | undefined =>
    config.getSecret(key) ? "__configured_secret__" : undefined;

  return validateDeploymentConfig({
    domain: config.require("domain"),
    originIp: config.require("originIp"),
    postgresMode: (get("postgresMode") ?? "docker") as PostgresMode,
    redisMode: (get("redisMode") ?? "docker") as RedisMode,
    sub2apiImage: config.require("sub2apiImage"),
    traefikImage: get("traefikImage") ?? "traefik:v3.3.3",
    neonResourceMode: (get("neonResourceMode") ?? "existing") as ResourceMode,
    upstashResourceMode: (get("upstashResourceMode") ?? "existing") as ResourceMode,
    cloudflareApiToken: configuredSecret("cloudflareApiToken"),
    cloudflareZoneId: get("cloudflareZoneId"),
    acmeEmail: get("acmeEmail"),
    postgresUser: get("postgresUser"),
    postgresPassword: configuredSecret("postgresPassword"),
    postgresDb: get("postgresDb"),
    neonHost: get("neonHost"),
    neonApiToken: configuredSecret("neonApiToken"),
    neonProjectId: get("neonProjectId"),
    neonBranchId: get("neonBranchId"),
    neonPort: get("neonPort") ? Number(get("neonPort")) : undefined,
    neonUser: get("neonUser"),
    neonPassword: configuredSecret("neonPassword"),
    neonDb: get("neonDb"),
    redisHost: get("redisHost"),
    redisPort: get("redisPort") ? Number(get("redisPort")) : undefined,
    redisUsername: get("redisUsername"),
    redisPassword: configuredSecret("redisPassword"),
    upstashHost: get("upstashHost"),
    upstashApiKey: configuredSecret("upstashApiKey"),
    upstashEmail: get("upstashEmail"),
    upstashDatabaseName: get("upstashDatabaseName"),
    upstashRegion: get("upstashRegion"),
    upstashPort: get("upstashPort") ? Number(get("upstashPort")) : undefined,
    upstashUsername: get("upstashUsername"),
    upstashPassword: configuredSecret("upstashPassword"),
    adminEmail: get("adminEmail"),
    adminPassword: configuredSecret("adminPassword"),
    jwtSecret: configuredSecret("jwtSecret"),
    totpEncryptionKey: configuredSecret("totpEncryptionKey"),
    appProbePath: get("appProbePath"),
    drainSeconds: get("drainSeconds") ? Number(get("drainSeconds")) : undefined,
  });
}
