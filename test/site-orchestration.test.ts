import { cpSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

function createShellFixture(): { root: string; log: string; environment: NodeJS.ProcessEnv } {
  const root = mkdtempSync(join(tmpdir(), "sub2api-site-orchestration-"));
  const bin = join(root, "bin");
  const log = join(root, "commands.log");
  mkdirSync(bin);
  cpSync(new URL("../scripts", import.meta.url), join(root, "scripts"), { recursive: true });
  mkdirSync(join(root, "compose"));
  mkdirSync(join(root, "traefik", "dynamic"), { recursive: true });
  mkdirSync(join(root, "runtime", "dynamic"), { recursive: true });
  writeFileSync(join(root, "runtime", "runtime.env"), "POSTGRES_MODE=neon\nREDIS_MODE=upstash\nAPP_PROBE_PATH=/health\nDRAIN_SECONDS=10\n");
  writeFileSync(join(root, "runtime", "deploy-state.json"), '{"activeSlot":"blue","activeImage":"code2@sha256:old","previousSlot":"green","previousImage":"code2@sha256:previous"}\n');
  writeFileSync(join(root, "runtime", "dynamic", "active.yml"), "old route\n");

  const fake = (name: string, body: string) => {
    const path = join(bin, name);
    writeFileSync(path, `#!/usr/bin/env bash\nset -euo pipefail\n${body}\n`);
    chmodSync(path, 0o755);
  };
  fake("docker", '{ printf "docker AUTO_SETUP=%s" "${AUTO_SETUP:-}"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"');
  fake("npx", '{ printf "npx"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"');
  fake("sleep", '{ printf "sleep"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"');
  fake("curl", '{ printf "curl"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"');
  fake("node", [
    '{ printf "node"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"',
    'case "$*" in',
    '  *activeImage*) printf "code2@sha256:old" ;;',
    '  *activeSlot*) printf "blue" ;;',
    '  *POSTGRES_MODE*) printf "neon" ;;',
    '  *REDIS_MODE*) printf "upstash" ;;',
    '  *APP_PROBE_PATH*) printf "/health" ;;',
    '  *DRAIN_SECONDS*) printf "10" ;;',
    'esac',
  ].join("\n"));

  return {
    root,
    log,
    environment: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      COMMAND_LOG: log,
      SITE_ID: "code2",
      SITE_RUNTIME_ROOT: join(root, "runtime", "sites", "code2"),
      COMPOSE_PROJECT_NAME: "sub2api-code2",
      SITE_ROUTE_PATH: join(root, "runtime", "edge", "dynamic", "site-code2.yml"),
      EDGE_NETWORK_NAME: "sub2api-edge",
      EDGE_RUNTIME_ROOT: join(root, "runtime", "edge"),
      HOST_STATE_PATH: join(root, "runtime", "host-state.json"),
      CONFIGURED_SITE_IDS: "code2,code3",
      DOMAIN: "code2.contextid.cn",
      SUB2API_IMAGE: "code2@sha256:new",
      POSTGRES_MODE: "neon",
      REDIS_MODE: "upstash",
      APP_PROBE_PATH: "/health",
      DRAIN_SECONDS: "10",
      TRAEFIK_IMAGE: "traefik@sha256:edge",
      ACME_EMAIL: "ops@example.com",
      CLOUDFLARE_DNS_API_TOKEN: "placeholder",
      ORIGIN_IP: "127.0.0.1",
      PROBE_RETRIES: "1",
      PROBE_DELAY_SECONDS: "0",
    },
  };
}

function run(root: string, environment: NodeJS.ProcessEnv, script: string, args: string[] = []) {
  return spawnSync("bash", [script, ...args], { cwd: root, env: environment, encoding: "utf8" });
}

function commands(log: string): string {
  return existsSync(log) ? readFileSync(log, "utf8") : "";
}

describe("independent Site lifecycle", () => {
  it("reconciles the public Edge lifecycle without enumerating or composing Sites", () => {
    const fixture = createShellFixture();
    const result = run(fixture.root, fixture.environment, "scripts/reconcile-edge.sh");

    expect(result.status, result.stderr ?? "").toBe(0);
    const captured = commands(fixture.log);
    expect(captured).toContain("--project-name sub2api-edge");
    expect(captured).toContain("compose/edge.yml");
    expect(captured).toContain("traefik");
    expect(captured).not.toContain("compose/upstream.yml");
    expect(captured).not.toContain("compose/site.yml");
    expect(captured).not.toMatch(/sub2api-code[0-9]|runtime\/sites|\b(stop|down)\b/);
  });

  it("reconciles a first Site only after host preflight and leaves host finalization to the success barrier", () => {
    const fixture = createShellFixture();
    const result = run(fixture.root, fixture.environment, "scripts/reconcile-site.sh");

    expect(result.status, result.stderr ?? "").toBe(0);
    const captured = commands(fixture.log);
    const preflight = captured.indexOf("scripts/host-preflight.ts check");
    const firstSideEffect = captured.search(/docker |render-site-route\.ts write/);
    expect(preflight).toBeGreaterThanOrEqual(0);
    expect(firstSideEffect).toBeGreaterThan(preflight);
    expect(captured).toContain("--project-name sub2api-code2");
    expect(captured).toContain("compose/upstream.yml");
    expect(captured).toContain("compose/site.yml");
    expect(captured).toContain("--wait-timeout 300");
    expect(captured).toContain("AUTO_SETUP=true");
    expect(captured).not.toMatch(/restart|--remove-orphans/);
    expect(captured).toContain("/health");
    expect(captured).not.toContain("compose/edge.yml");
    expect(captured).not.toContain("00-sing-box");
    expect(captured).not.toContain("sub2api-code3");
    expect(captured).not.toContain("site-code3");
    expect(captured).not.toContain("write-host-state.ts write");

    const finalize = run(fixture.root, fixture.environment, "scripts/finalize-host-state.sh");
    expect(finalize.status, finalize.stderr ?? "").toBe(0);
    const finalized = commands(fixture.log);
    expect(finalized).toContain("scripts/write-host-state.ts write");
  });

  it("releases, switches, and rolls back code2 without touching code3 or the Edge project", () => {
    const fixture = createShellFixture();

    for (const [script, args] of [
      ["scripts/application-release.sh", []],
      ["scripts/switch-slot.sh", ["code2@sha256:new", "code2.contextid.cn"]],
      ["scripts/rollback-slot.sh", ["code2.contextid.cn"]],
    ] as const) {
      const result = run(fixture.root, fixture.environment, script, args);
      expect(result.status, `${script}: ${result.stderr ?? ""}`).toBe(0);
    }

    const captured = commands(fixture.log);
    expect(captured).toContain("--project-name sub2api-code2");
    expect(captured).toContain("scripts/render-site-route.ts write");
    expect(captured).toContain("--wait-timeout 300");
    expect(captured).toContain("/health");
    expect(captured).toMatch(/sleep 10/);
    expect(captured).not.toContain("--remove-orphans");
    expect(captured).not.toContain("compose/edge.yml");
    expect(captured).not.toMatch(/runtime\/sites\/code3|sub2api-code3|code3\.contextid\.cn|site-code3/);
  });
});
