import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const read = (name: string) => readFileSync(new URL(`../compose/${name}`, import.meta.url), "utf8");
const readPath = (path: string) => readFileSync(new URL(path, import.meta.url), "utf8");

function renderSiteRoute(template: string, values: Record<string, string>): string {
  let rendered = template;
  for (const [key, value] of Object.entries(values)) rendered = rendered.replaceAll(`\${${key}}`, value);
  if (rendered.includes("${")) throw new Error("site route template was not fully rendered");
  return rendered;
}

function siteEnv(siteId: string): string[] {
  return [
    "SUB2API_IMAGE=image@sha256:digest",
    `SITE_RUNTIME_ROOT=${process.cwd()}/runtime/sites/${siteId}`,
    `BLUE_EDGE_ALIAS=sub2api-${siteId}-blue`,
    `GREEN_EDGE_ALIAS=sub2api-${siteId}-green`,
    "SLOT_DATA_DIR=blue",
    "AUTO_SETUP=false",
    `DOMAIN=${siteId}.contextid.cn`,
    "DATABASE_HOST=postgres", "DATABASE_PORT=5432", "DATABASE_USER=sub2api", "DATABASE_PASSWORD=placeholder",
    "DATABASE_DBNAME=sub2api", "DATABASE_SSLMODE=disable", "POSTGRES_USER=sub2api", "POSTGRES_PASSWORD=placeholder",
    "POSTGRES_DB=sub2api", "REDIS_HOST=redis", "REDIS_PORT=6379", "REDIS_PASSWORD=placeholder",
    "REDIS_ENABLE_TLS=false", "ADMIN_EMAIL=admin@example.com",
  ];
}

function renderCompose(projectName: string, files: string[], env: string[], profiles: string[]): {
  services: Record<string, any>;
  networks: Record<string, any>;
} {
  const envPath = join(mkdtempSync(join(tmpdir(), "sub2api-compose-")), ".env");
  writeFileSync(envPath, env.join("\n"));
  const args = [
    "compose", "--project-name", projectName, "--env-file", envPath,
    ...files.flatMap((file) => ["-f", file]),
    ...profiles.flatMap((profile) => ["--profile", profile]),
    "config", "--format", "json",
  ];
  const output = execFileSync("docker", args, { cwd: process.cwd(), encoding: "utf8" });
  return JSON.parse(output) as { services: Record<string, any>; networks: Record<string, any> };
}

describe("compose deployment contract", () => {
  it("keeps the pinned upstream baseline", () => {
    const upstream = read("upstream.yml");
    expect(upstream).toContain("# Source: Wei-Shaw/sub2api deploy/docker-compose.yml");
    expect(upstream).toContain("security_opt:");
    expect(upstream).toContain("healthcheck:");
  });

  it("splits edge and site Compose ownership", () => {
    const edge = read("edge.yml");
    const site = read("site.yml");
    expect(edge).toContain("80:80");
    expect(edge).toContain("443:443");
    expect(edge).toContain("host.docker.internal:host-gateway");
    expect(edge).toContain("sub2api-edge");
    expect(edge).toContain("runtime/edge");
    expect(edge).not.toMatch(/sub2api-(blue|green)/);
    expect(edge).not.toContain("postgres");
    expect(edge).not.toContain("redis");
    expect(site).not.toContain("80:80");
    expect(site).not.toContain("443:443");
    expect(site).not.toContain("traefik");
    expect(site).toContain("SLOT_DATA_DIR:");
    expect(site).toContain("BLUE_EDGE_ALIAS");
    expect(site).toContain("GREEN_EDGE_ALIAS");
    expect(site).toContain("${SITE_RUNTIME_ROOT}/data/blue");
    expect(site).toContain("${SITE_RUNTIME_ROOT}/data/green");
  });

  it("renders Edge with only Traefik, public ports, and its named network", () => {
    const config = renderCompose(
      "sub2api-edge",
      ["compose/edge.yml"],
      ["TRAEFIK_IMAGE=traefik:v3.3.3", "CLOUDFLARE_DNS_API_TOKEN=placeholder", "ACME_EMAIL=ops@example.com"],
      [],
    );
    expect(Object.keys(config.services)).toEqual(["traefik"]);
    expect(config.services.traefik.ports).toEqual([
      { mode: "ingress", target: 80, published: "80", protocol: "tcp" },
      { mode: "ingress", target: 443, published: "443", protocol: "tcp" },
    ]);
    expect(Object.keys(config.services.traefik.networks)).toEqual(["sub2api-edge"]);
    expect(config.services.traefik.extra_hosts).toEqual(["host.docker.internal=host-gateway"]);
    expect(config.networks["sub2api-edge"].name).toBe("sub2api-edge");
  });

  it("renders code2 and code3 Site projects with distinct networks and edge aliases", () => {
    for (const siteId of ["code2", "code3"]) {
      const config = renderCompose(
        `sub2api-${siteId}`,
        ["compose/upstream.yml", "compose/site.yml"],
        siteEnv(siteId),
        ["app", "postgres", "redis"],
      );
      expect(config.services.traefik).toBeUndefined();
      expect(config.services.sub2api).toBeUndefined();
      for (const slot of ["sub2api-blue", "sub2api-green"]) {
        const service = config.services[slot];
        expect(service.profiles ?? []).not.toContain("base-disabled");
        expect(service.ports ?? []).toEqual([]);
        expect(service.restart).toBe("unless-stopped");
        expect(service.security_opt).toContain("no-new-privileges:true");
        expect(service.ulimits.nofile.soft).toBe(100000);
        expect(service.healthcheck.test.join(" ")).toContain("/health");
        expect(service.networks["sub2api-network"]).toBeDefined();
        expect(service.networks["sub2api-edge"].aliases).toEqual([`sub2api-${siteId}-${slot.replace("sub2api-", "")}`]);
        expect(service.volumes[0].source).toContain(`runtime/sites/${siteId}/data/${slot.replace("sub2api-", "")}`);
      }
      expect(config.networks["sub2api-network"].name).toBe(`sub2api-${siteId}_sub2api-network`);
      expect(config.networks["sub2api-edge"].name).toBe("sub2api-edge");
      expect(config.networks["sub2api-edge"].external).toBe(true);
    }
  });

  it("keeps application traffic behind Traefik and uses slot data", () => {
    const site = read("site.yml");
    expect(site).toContain("ports: !reset []");
    expect(site).toContain("depends_on: !reset []");
    expect(site).toContain("profiles: [postgres]");
    expect(site).toContain("profiles: [redis]");
    expect(site).toContain("external: true");
    expect(readPath("../traefik/traefik.yml")).toContain("/etc/traefik/dynamic");
    expect(readPath("../traefik/traefik.yml")).toContain("acme.json");
  });

  it("keeps upstream auto setup enabled after installation", () => {
    const deploy = readPath("../scripts/deploy-compose.sh");
    const reconcile = readPath("../scripts/infra-reconcile.sh");

    expect(deploy).not.toContain("AUTO_SETUP:-false");
    expect(reconcile).not.toContain("AUTO_SETUP=false");
    expect(reconcile).toContain("AUTO_SETUP=true");
  });

  it("allows first-boot migration to finish before health wait expires", () => {
    const site = read("site.yml");
    const upstream = read("upstream.yml");
    const deploy = readPath("../scripts/deploy-compose.sh");

    expect(site.match(/start_period: 180s/g)).toHaveLength(2);
    expect(upstream).toContain("start_period: 180s");
    expect(deploy).not.toContain("--wait-timeout 120");
    expect(deploy).toContain("--wait-timeout 300");
  });

  it("coexists sing-box passthrough with independently rendered code2/code3 route templates", () => {
    const singBox = readPath("../traefik/dynamic/sing-box.yml");
    expect(singBox).toContain('HostSNI(`${SING_BOX_SERVER_NAME}`)');
    expect(singBox).toContain("passthrough: true");
    expect(singBox).toContain('address: "${SING_BOX_TARGET}"');
    expect(singBox).not.toMatch(/sub2api-(blue|green)/);

    const template = readPath("../traefik/dynamic/site.yml");
    expect(template).toContain('${SITE_ID}');
    expect(template).toContain('${DOMAIN}');
    expect(template).toContain('${SLOT}');
    expect(template).toContain('${ACTIVE_EDGE_ALIAS}');
    expect(template).toContain("certResolver: cloudflare");

    const code2 = renderSiteRoute(template, {
      SITE_ID: "code2", DOMAIN: "code2.contextid.cn", SLOT: "green", ACTIVE_EDGE_ALIAS: "sub2api-code2-green",
    });
    const code3 = renderSiteRoute(template, {
      SITE_ID: "code3", DOMAIN: "code3.contextid.cn", SLOT: "blue", ACTIVE_EDGE_ALIAS: "sub2api-code3-blue",
    });

    expect(code2).toContain("Host(`code2.contextid.cn`)");
    expect(code2).toContain("site-code2-green:");
    expect(code2).toContain("http://sub2api-code2-green:8080");
    expect(code2).not.toContain("code3");
    expect(code2).not.toContain("HostSNI");
    expect(code3).toContain("Host(`code3.contextid.cn`)");
    expect(code3).toContain("site-code3-blue:");
    expect(code3).toContain("http://sub2api-code3-blue:8080");
    expect(code3).not.toContain("code2");
    expect(code3).not.toContain("HostSNI");
    expect(singBox).not.toContain("code2");
    expect(singBox).not.toContain("code3");
  });
});
