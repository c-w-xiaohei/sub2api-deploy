import { execFileSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";
import { writeEdgeConfigAtomically } from "../scripts/render-edge-config.js";
import { renderSiteRoute, writeSiteRouteAtomically } from "../scripts/render-site-route.js";

const routeTemplate = readFileSync(new URL("../traefik/dynamic/site.yml", import.meta.url), "utf8");

describe("Site runtime layout", () => {
  it("renders code2 and code3 routes without cross-Site writes", () => {
    const hostRoot = mkdtempSync(join(tmpdir(), "sub2api-sites-"));
    const dynamicRoot = join(hostRoot, "edge", "dynamic");
    const code2Route = join(dynamicRoot, "site-code2.yml");
    const code3Route = join(dynamicRoot, "site-code3.yml");
    writeSiteRouteAtomically(routeTemplatePath, code2Route, "code2", "code2.example.test", "green", "sub2api-code2-green");
    writeSiteRouteAtomically(routeTemplatePath, code3Route, "code3", "code3.example.test", "blue", "sub2api-code3-blue");
    const code3Before = readFileSync(code3Route, "utf8");

    writeSiteRouteAtomically(routeTemplatePath, code2Route, "code2", "code2.example.test", "blue", "sub2api-code2-blue");

    expect(readFileSync(code2Route, "utf8")).toContain("http://sub2api-code2-blue:8080");
    expect(readFileSync(code2Route, "utf8")).not.toContain("code3");
    expect(readFileSync(code3Route, "utf8")).toBe(code3Before);
    expect(statSync(code2Route).mode & 0o777).toBe(0o600);
  });

  it("validates Site route inputs", () => {
    expect(() => renderSiteRoute(routeTemplate, "Code2", "code2.example.test", "blue", "sub2api-code2-blue")).toThrow(/site ID/);
    expect(() => renderSiteRoute(routeTemplate, "code2", "", "blue", "sub2api-code2-blue")).toThrow(/domain/);
    expect(() => renderSiteRoute(routeTemplate, "code2", "code2.example.test", "blue", "")).toThrow(/alias/);
    expect(() => renderSiteRoute(routeTemplate, "code2", "code2.example.test", "blue", "sub2api-code3-blue")).toThrow(/alias/);
    expect(() => renderSiteRoute(routeTemplate, "code3", "code3.example.test", "blue", "sub2api-blue")).toThrow(/alias/);
    expect(() => renderSiteRoute(routeTemplate, "code2", "code2.example.test` || true", "blue", "sub2api-code2-blue")).toThrow(/domain/);
    expect(renderSiteRoute(routeTemplate, "code2", "Code2.Example.Test", "blue", "sub2api-code2-blue")).toContain("Host(`code2.example.test`)");
    expect(() => renderSiteRoute(routeTemplate, "code2", `${"a".repeat(63)}.${"a".repeat(63)}.${"a".repeat(63)}.${"a".repeat(62)}`, "blue", "sub2api-code2-blue")).toThrow(/domain/);
  });

  it("writes Edge-owned files without replacing Site route fragments", () => {
    const hostRoot = mkdtempSync(join(tmpdir(), "sub2api-edge-"));
    const edgeRoot = join(hostRoot, "edge");
    const siteRoute = join(edgeRoot, "dynamic", "site-code3.yml");
    writeSiteRouteAtomically(routeTemplatePath, siteRoute, "code3", "code3.example.test", "blue", "sub2api-code3-blue");
    const before = readFileSync(siteRoute, "utf8");

    writeEdgeConfigAtomically(
      edgeRoot,
      new URL("../traefik/traefik.yml", import.meta.url).pathname,
      new URL("../traefik/dynamic/sing-box.yml", import.meta.url).pathname,
      "ops@example.test",
      "reality.example.test",
      "host.docker.internal:8443",
    );

    expect(readFileSync(join(edgeRoot, "traefik.yml"), "utf8")).toContain("email: ops@example.test");
    expect(readFileSync(join(edgeRoot, "dynamic", "00-sing-box.yml"), "utf8")).toContain('address: "host.docker.internal:8443"');
    expect(readFileSync(siteRoute, "utf8")).toBe(before);
    expect(statSync(join(edgeRoot, "traefik.yml")).mode & 0o777).toBe(0o600);
  });

  it("sources Site helpers with explicit, isolated runtime roots", () => {
    const hostRoot = mkdtempSync(join(tmpdir(), "sub2api-site-helper-"));
    const code2Root = join(hostRoot, "sites", "code2");
    const code3Root = join(hostRoot, "sites", "code3");
    mkdirSync(code2Root, { recursive: true });
    mkdirSync(code3Root, { recursive: true });
    writeFileSync(join(code2Root, "runtime.env"), 'POSTGRES_MODE="docker"\nREDIS_MODE="upstash"\n');
    writeFileSync(join(code3Root, "runtime.env"), 'POSTGRES_MODE="neon"\nREDIS_MODE="docker"\n', { flag: "w" });

    const source = (siteId: string, root: string) => execFileSync("bash", ["-c", "set -e; source scripts/site-compose-common.sh; printf '%s\\n' \"${SITE_RUNTIME_ROOT}\" \"${SITE_COMPOSE[@]}\""], {
      cwd: process.cwd(),
      encoding: "utf8",
      env: {
        ...process.env,
        SITE_ID: siteId,
        SITE_RUNTIME_ROOT: relative(process.cwd(), root),
        COMPOSE_PROJECT_NAME: `sub2api-${siteId}`,
        SITE_ROUTE_PATH: join(hostRoot, "edge", "dynamic", `site-${siteId}.yml`),
      },
    }).trim().split("\n");

    const code2 = source("code2", code2Root);
    const code3 = source("code3", code3Root);
    expect(code2).toContain(code2Root);
    expect(code2).toContain("--profile");
    expect(code2).toContain("postgres");
    expect(code2).not.toContain("redis");
    expect(code2).not.toContain("compose/edge.yml");
    expect(code3).toContain(code3Root);
    expect(code3).toContain("redis");
    expect(code3).not.toContain("postgres");

    expect(source("a", code2Root)).toContain("sub2api-a");
    expect(source(`a${"b".repeat(30)}c`, code2Root)).toContain(`sub2api-a${"b".repeat(30)}c`);
    expect(() => source(`a${"b".repeat(31)}c`, code2Root)).toThrow(/SITE_ID must match/);

    expect(() => execFileSync("bash", ["-c", "source scripts/site-compose-common.sh"], {
      cwd: process.cwd(),
      encoding: "utf8",
      stdio: "pipe",
      env: { PATH: process.env.PATH ?? "" },
    })).toThrow(/SITE_ID is required/);
    expect(() => execFileSync("bash", ["-c", "source scripts/site-compose-common.sh"], {
      cwd: process.cwd(),
      encoding: "utf8",
      stdio: "pipe",
      env: {
        ...process.env,
        SITE_ID: "Code2",
        SITE_RUNTIME_ROOT: code2Root,
        COMPOSE_PROJECT_NAME: "sub2api-code2",
        SITE_ROUTE_PATH: join(hostRoot, "edge", "dynamic", "site-code2.yml"),
      },
    })).toThrow(/SITE_ID must match/);

    const fakeBin = mkdtempSync(join(tmpdir(), "sub2api-fake-docker-"));
    const dockerLog = join(hostRoot, "docker.log");
    const fakeDocker = join(fakeBin, "docker");
    writeFileSync(fakeDocker, "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$DOCKER_LOG\"\n");
    chmodSync(fakeDocker, 0o755);
    execFileSync("bash", ["-c", "source scripts/site-compose-common.sh; site_stop_service sub2api-blue"], {
      cwd: process.cwd(),
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${fakeBin}:${process.env.PATH}`,
        DOCKER_LOG: dockerLog,
        SITE_ID: "code2",
        SITE_RUNTIME_ROOT: code2Root,
        COMPOSE_PROJECT_NAME: "sub2api-code2",
        SITE_ROUTE_PATH: join(hostRoot, "edge", "dynamic", "site-code2.yml"),
      },
    });
    expect(readFileSync(dockerLog, "utf8")).toContain("compose --project-name sub2api-code2");
    expect(readFileSync(dockerLog, "utf8")).toContain("stop --timeout 30 sub2api-blue");
  });

  it("keeps Site and Edge Compose helper contracts separate", () => {
    const siteHelper = readFileSync(new URL("../scripts/site-compose-common.sh", import.meta.url), "utf8");
    const edgeHelper = readFileSync(new URL("../scripts/edge-compose-common.sh", import.meta.url), "utf8");
    expect(siteHelper).toContain('EDGE_NETWORK_NAME=sub2api-edge');
    expect(siteHelper).toContain(': "${SITE_RUNTIME_ROOT:?SITE_RUNTIME_ROOT is required}"');
    expect(siteHelper).toContain(': "${COMPOSE_PROJECT_NAME:?COMPOSE_PROJECT_NAME is required}"');
    expect(siteHelper).toContain(': "${SITE_ROUTE_PATH:?SITE_ROUTE_PATH is required}"');
    expect(siteHelper).toContain('--profile app');
    expect(siteHelper).toContain('compose/upstream.yml');
    expect(siteHelper).toContain('compose/site.yml');
    expect(siteHelper).not.toContain('compose/edge.yml');
    expect(edgeHelper).toContain('--project-name sub2api-edge');
    expect(edgeHelper).toContain('--env-file "$EDGE_RUNTIME_ROOT/edge.env"');
    expect(edgeHelper).toContain('compose/edge.yml');
    expect(edgeHelper).not.toContain('upstream.yml');
    expect(edgeHelper).not.toContain('--profile');
  });
});

const routeTemplatePath = new URL("../traefik/dynamic/site.yml", import.meta.url).pathname;
