import { describe, expect, it } from "vitest";
import * as pulumi from "@pulumi/pulumi";
import { createNeonConnection, managedNeonProjectName, parsePostgresDsn } from "../src/database.js";
import type { DeploymentConfig } from "../src/config.js";

const resources: Array<{ type: string; name: string }> = [];
pulumi.runtime.setMocks({
  newResource: (args) => {
    resources.push({ type: args.type, name: args.name });
    const state = { ...args.inputs } as Record<string, unknown>;
    if (args.type === "neon:resource:Project") {
      state.connection_uri = "postgresql://tenant-a:p%40ss@ep.generated.neon.tech/tenant-a?sslmode=require";
    }
    return { id: `${args.name}-id`, state };
  },
  call: (args) => args.inputs,
}, "sub2api", "test");

describe("managed PostgreSQL resources", () => {
  it("derives a distinct managed Neon project name from the namespace", () => {
    expect(managedNeonProjectName("tenant-a")).toBe("tenant-a-postgres");
  });

  it("parses a PostgreSQL DSN into the upstream split connection contract", () => {
    expect(parsePostgresDsn("postgresql://sub2api:p%40ss@ep.example.neon.tech:5432/sub2api?sslmode=require")).toEqual({
      host: "ep.example.neon.tech",
      port: 5432,
      user: "sub2api",
      password: "p@ss",
      dbname: "sub2api",
      sslmode: "require",
    });
  });

  it("rejects DSNs without a database name or TLS", () => {
    expect(() => parsePostgresDsn("postgresql://user:pass@host/")).toThrow(/database name/);
    expect(() => parsePostgresDsn("postgresql://user:pass@host/db?sslmode=disable")).toThrow(/sslmode=require/);
  });

  it("registers the native managed Neon Project resource and exposes its connection", async () => {
    resources.length = 0;
    const config = {
      resourceNamespace: "tenant-a",
      domain: "sub2api.example.com",
      originIp: "203.0.113.10",
      postgresMode: "neon",
      redisMode: "docker",
      sub2apiImage: "image@sha256:digest",
      traefikImage: "traefik:v3.3.3",
      cloudflareApiToken: "configured",
      cloudflareZoneId: "zone-id",
      acmeEmail: "ops@example.com",
      postgresUser: "sub2api",
      postgresDb: "sub2api",
      neonUser: "sub2api",
      neonDb: "sub2api",
      neonPort: 5432,
      redisPort: 6379,
      redisUsername: "",
      adminEmail: "admin@example.com",
      appProbePath: "/api/ready",
      drainSeconds: 10,
      neonResourceMode: "create",
      upstashResourceMode: "existing",
    } as DeploymentConfig;

    const connection = createNeonConnection(config, "neon-api-key");
    await (connection.host as pulumi.Output<string> & { promise(): Promise<string> }).promise();

    expect(resources.filter(({ type }) => type.startsWith("neon:")).map(({ type }) => type)).toEqual([
      "neon:resource:Project",
    ]);
    expect(await (connection.host as pulumi.Output<string> & { promise(): Promise<string> }).promise()).toBe(
      "ep.generated.neon.tech",
    );
  });
});
