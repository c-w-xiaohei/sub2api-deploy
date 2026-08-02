import { describe, expect, it } from "vitest";
import { buildDatabaseConnection } from "../src/database.js";
import { buildRedisConnection } from "../src/redis.js";
import type { DeploymentConfig } from "../src/config.js";

const base = {
  domain: "sub2api.example.com",
  originIp: "203.0.113.10",
  postgresMode: "docker" as const,
  redisMode: "docker" as const,
  sub2apiImage: "image@sha256:digest",
  traefikImage: "traefik:v3.3.3",
  cloudflareApiToken: "cloudflare-secret",
  cloudflareZoneId: "zone-id",
  acmeEmail: "ops@example.com",
  postgresUser: "sub2api",
  postgresDb: "sub2api",
  postgresPassword: "pg-secret",
  neonPort: 5432,
  neonUser: "neon-user",
  neonDb: "neon-db",
  neonHost: "ep.example.neon.tech",
  neonPassword: "neon-secret",
  redisPort: 6379,
  redisUsername: "",
  redisPassword: "redis-secret",
  upstashPort: 6380,
  upstashUsername: "default",
  upstashHost: "upstash.example.com",
  upstashPassword: "upstash-secret",
  adminEmail: "admin@example.com",
  appProbePath: "/api/ready",
  drainSeconds: 10,
  neonResourceMode: "existing" as const,
  upstashResourceMode: "existing" as const,
} satisfies DeploymentConfig;

describe("runtime connection factories", () => {
  it.each([
    ["docker", "docker", "postgres", 5432, "redis", 6379, "disable", false],
    ["neon", "docker", "ep.example.neon.tech", 5432, "redis", 6379, "require", false],
    ["docker", "upstash", "postgres", 5432, "upstash.example.com", 6380, "disable", true],
    ["neon", "upstash", "ep.example.neon.tech", 5432, "upstash.example.com", 6380, "require", true],
  ])("builds %s/%s split environment connections", (postgresMode, redisMode, dbHost, dbPort, redisHost, redisPort, sslmode, redisTls) => {
    const config = { ...base, postgresMode, redisMode } as DeploymentConfig;
    const database = buildDatabaseConnection(config);
    const redis = buildRedisConnection(config);
    expect(database).toMatchObject({ host: dbHost, port: dbPort, sslmode });
    expect(redis).toMatchObject({ host: redisHost, port: redisPort, enableTls: redisTls });
  });
});
