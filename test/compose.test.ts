import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const read = (name: string) => readFileSync(new URL(`../compose/${name}`, import.meta.url), "utf8");
const readPath = (path: string) => readFileSync(new URL(path, import.meta.url), "utf8");

describe("compose deployment contract", () => {
  it("keeps the pinned upstream baseline", () => {
    const upstream = read("upstream.yml");
    expect(upstream).toContain("# Source: Wei-Shaw/sub2api deploy/docker-compose.yml");
    expect(upstream).toContain("security_opt:");
    expect(upstream).toContain("healthcheck:");
  });

  it("keeps application traffic behind Traefik and uses slot data", () => {
    const override = read("override.yml");
    expect(override).toContain("SLOT_DATA_DIR:");
    expect(override).toContain("profiles: [postgres]");
    expect(override).toContain("profiles: [redis]");
    expect(override).not.toMatch(/ports:\s*\n\s*-.*8080:8080/);
    expect(override).not.toContain("container_name: sub2api");
    expect(read("edge.yml")).toContain("80:80");
    expect(read("edge.yml")).toContain("443:443");
    expect(readPath("../traefik/traefik.yml")).toContain("/etc/traefik/dynamic");
    expect(readPath("../traefik/traefik.yml")).toContain("acme.json");
    expect(readPath("../traefik/dynamic/active.yml")).toContain("sub2api-${SLOT}:8080");
    expect(override).toContain("ports: !reset []");
    expect(override).toContain("depends_on: !reset []");
  });

  it("renders both slots with the upstream health and hardening contract", () => {
    const envPath = join(mkdtempSync(join(tmpdir(), "sub2api-compose-")), ".env");
    writeFileSync(envPath, [
      "SUB2API_IMAGE=image@sha256:digest", "TRAEFIK_IMAGE=traefik:v3.3.3", "SLOT_DATA_DIR=blue",
      "AUTO_SETUP=false", "DOMAIN=sub2api.example.com", "ACME_EMAIL=ops@example.com",
      "DATABASE_HOST=postgres", "DATABASE_PORT=5432", "DATABASE_USER=sub2api", "DATABASE_PASSWORD=placeholder",
      "DATABASE_DBNAME=sub2api", "DATABASE_SSLMODE=disable", "POSTGRES_USER=sub2api", "POSTGRES_PASSWORD=placeholder",
      "POSTGRES_DB=sub2api", "REDIS_HOST=redis", "REDIS_PORT=6379", "REDIS_PASSWORD=placeholder",
      "REDIS_ENABLE_TLS=false", "CLOUDFLARE_DNS_API_TOKEN=placeholder",
    ].join("\n"));
    const output = execFileSync("docker", ["compose", "--env-file", envPath, "-f", "compose/upstream.yml", "-f", "compose/override.yml", "-f", "compose/edge.yml", "--profile", "postgres", "--profile", "redis", "--profile", "app", "config", "--format", "json"], { cwd: process.cwd(), encoding: "utf8" });
    const config = JSON.parse(output) as { services: Record<string, any> };
    for (const slot of ["sub2api-blue", "sub2api-green"]) {
      expect(config.services[slot].profiles ?? []).not.toContain("base-disabled");
      expect(config.services[slot].restart).toBe("unless-stopped");
      expect(config.services[slot].security_opt).toContain("no-new-privileges:true");
      expect(config.services[slot].ulimits.nofile.soft).toBe(100000);
      expect(config.services[slot].healthcheck.test.join(" ")).toContain("/health");
      expect(config.services[slot].ports ?? []).toEqual([]);
    }
    expect(config.services.traefik.volumes.map((volume: { source: string }) => volume.source)).toEqual(expect.arrayContaining([
      expect.stringContaining("/runtime/"),
      expect.stringContaining("/traefik/"),
    ]));
  });
});
