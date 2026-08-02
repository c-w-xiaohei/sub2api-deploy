import { describe, expect, it } from "vitest";
import { validateDeploymentConfig } from "../src/config.js";

const valid = {
  domain: "sub2api.example.com",
  originIp: "203.0.113.10",
  postgresMode: "docker" as const,
  redisMode: "docker" as const,
  sub2apiImage: "weishaw/sub2api@sha256:abcdef1234567890",
  traefikImage: "traefik:v3.3.3",
  cloudflareApiToken: "cloudflare-secret",
  cloudflareZoneId: "zone-id",
  acmeEmail: "ops@example.com",
  appProbePath: "/api/ready",
  postgresPassword: "postgres-secret",
  redisPassword: "redis-secret",
};

describe("validateDeploymentConfig", () => {
  it("rejects unsupported data service modes", () => {
    expect(() => validateDeploymentConfig({ ...valid, postgresMode: "mysql" as never })).toThrow(
      /postgresMode must be docker or neon/,
    );
  });

  it("requires credentials for the selected cloud data service", () => {
    expect(() =>
      validateDeploymentConfig({
        ...valid,
        postgresMode: "neon",
        neonHost: undefined,
        neonPassword: undefined,
      }),
    ).toThrow(/neonHost/);
  });

  it("rejects mutable application image references", () => {
    expect(() => validateDeploymentConfig({ ...valid, sub2apiImage: "weishaw/sub2api:latest" })).toThrow(
      /sub2apiImage must contain an immutable @sha256 digest/,
    );
  });

  it("requires the Cloudflare API token for domain deployment", () => {
    expect(() => validateDeploymentConfig({ ...valid, cloudflareApiToken: undefined })).toThrow(
      /cloudflareApiToken is required/,
    );
  });

  it("returns a validated copy without reading dotenv files", () => {
    const result = validateDeploymentConfig(valid);
    expect(result.domain).toBe(valid.domain);
    expect(result.sub2apiImage).toContain("@sha256:");
  });

  it("allows Neon create mode with provider inputs instead of existing connection fields", () => {
    const result = validateDeploymentConfig({
      ...valid,
      postgresMode: "neon",
      postgresPassword: undefined,
      neonHost: undefined,
      neonPassword: undefined,
      neonResourceMode: "create",
      neonApiToken: "neon-api-token",
      neonProjectId: "project-id",
      neonBranchId: "branch-id",
    });
    expect(result.neonResourceMode).toBe("create");
  });

  it("allows Upstash create mode with provider inputs instead of existing connection fields", () => {
    const result = validateDeploymentConfig({
      ...valid,
      redisMode: "upstash",
      redisPassword: undefined,
      upstashHost: undefined,
      upstashPassword: undefined,
      upstashResourceMode: "create",
      upstashApiKey: "upstash-api-key",
      upstashEmail: "ops@example.com",
      upstashDatabaseName: "sub2api",
      upstashRegion: "us-east-1",
    });
    expect(result.upstashResourceMode).toBe("create");
  });

  it("requires an explicit non-health application probe path", () => {
    expect(() => validateDeploymentConfig({ ...valid, appProbePath: undefined })).toThrow(/appProbePath is required/);
    expect(() => validateDeploymentConfig({ ...valid, appProbePath: "/health" })).toThrow(/must not be \/health/);
    expect(() => validateDeploymentConfig({ ...valid, appProbePath: "https://example.com/ready" })).toThrow(/absolute path/);
  });

  it("requires a zone id and ACME email", () => {
    expect(() => validateDeploymentConfig({ ...valid, cloudflareZoneId: undefined })).toThrow(/cloudflareZoneId is required/);
    expect(() => validateDeploymentConfig({ ...valid, acmeEmail: undefined })).toThrow(/acmeEmail is required/);
  });
});
