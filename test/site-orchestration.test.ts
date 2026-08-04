import { cpSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

function createShellFixture(existing = true): { root: string; log: string; environment: NodeJS.ProcessEnv } {
  const root = mkdtempSync(join(tmpdir(), "sub2api-site-orchestration-"));
  const bin = join(root, "bin");
  const log = join(root, "commands.log");
  mkdirSync(bin);
  cpSync(new URL("../scripts", import.meta.url), join(root, "scripts"), { recursive: true });
  mkdirSync(join(root, "compose"));
  mkdirSync(join(root, "traefik", "dynamic"), { recursive: true });
  mkdirSync(join(root, "runtime", "sites", "code2"), { recursive: true });
  mkdirSync(join(root, "runtime", "sites", "code3"), { recursive: true });
  if (existing) writeFileSync(join(root, "runtime", "sites", "code2", "deploy-state.json"), '{"activeSlot":"blue","activeImage":"code2@sha256:old","previousSlot":"green","previousImage":"code2@sha256:previous","postgresMode":"neon","redisMode":"upstash"}\n');
  writeFileSync(join(root, "runtime", "sites", "code3", "deploy-state.json"), '{"activeSlot":"blue","activeImage":"code3@sha256:old","postgresMode":"neon","redisMode":"upstash"}\n');

  const fake = (name: string, body: string) => {
    const path = join(bin, name);
    writeFileSync(path, `#!/usr/bin/env bash\nset -euo pipefail\n${body}\n`);
    chmodSync(path, 0o755);
  };
  fake("docker", '{ printf "docker AUTO_SETUP=%s" "${AUTO_SETUP:-}"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"');
  fake("npx", [
    '{ printf "npx"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"',
    'case "$3:$4" in',
    '  scripts/render-runtime-env.ts:write|scripts/render-runtime-env.ts:write-app) cat >/dev/null; mkdir -p "$(dirname "$5")"; printf "POSTGRES_MODE=neon\\nREDIS_MODE=upstash\\n" > "$5" ;;',
    '  scripts/render-site-route.ts:write) mkdir -p "$(dirname "$6")"; printf "route\\n" > "$6" ;;',
    '  scripts/write-deploy-state.ts:write) mkdir -p "$(dirname "$5")"; printf "%s\\n" "$6" > "$5" ;;',
    '  scripts/write-bootstrap-marker.ts:write) mkdir -p "$(dirname "$5")"; printf "marker\\n" > "$5" ;;',
    'esac',
  ].join("\n"));
  fake("sleep", '{ printf "sleep"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"');
  fake("curl", '{ printf "curl"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"; [[ "${FAIL_CURL:-}" != "true" ]]');
  fake("node", [
    '{ printf "node"; printf " %s" "$@"; printf "\\n"; } >> "$COMMAND_LOG"',
    'field() { local key="$1" marker value; marker="\\\"${key}\\\":\\\""; value="${content#*"$marker"}"; value="${value%%\\\"*}"; printf "%s" "$value"; }',
    'if [[ "$1" == "scripts/read-runtime-env.cjs" ]]; then',
    '  case "$3" in POSTGRES_MODE) printf "neon" ;; REDIS_MODE) printf "upstash" ;; APP_PROBE_PATH) printf "/health" ;; DRAIN_SECONDS) printf "10" ;; esac',
    '  exit 0',
    'fi',
    'if [[ "$1" == "scripts/write-host-state.cjs" && "$2" == "write" ]]; then mkdir -p "$(dirname "$3")"; printf "{\\\"sites\\\":[\\\"code2\\\",\\\"code3\\\"]}\\n" > "$3"; exit 0; fi',
    'if [[ "$1" == "-e" ]]; then',
    '  code="$2"; state="${3:-}"; content=""; [[ -n "$state" && -f "$state" ]] && content="$(<"$state")"',
    '  if [[ "$code" == *value.serverName* ]]; then printf "www.cloudflare.com"; exit 0; fi',
    '  if [[ "$code" == *value.target* ]]; then printf "host.docker.internal:8443"; exit 0; fi',
    '  if [[ "$code" == *CLOUDFLARE_DNS_API_TOKEN* ]]; then printf "{\\\"TRAEFIK_IMAGE\\\":\\\"traefik@sha256:edge\\\",\\\"ACME_EMAIL\\\":\\\"ops@example.com\\\",\\\"CLOUDFLARE_DNS_API_TOKEN\\\":\\\"placeholder\\\"}"; exit 0; fi',
    '  active_slot="$(field activeSlot)"; active_image="$(field activeImage)"; previous_slot="$(field previousSlot)"; previous_image="$(field previousImage)"',
    '  if [[ "$code" == *JSON.stringify* && "$code" == *previousSlot=s.activeSlot* ]]; then printf "{\\\"activeSlot\\\":\\\"%s\\\",\\\"activeImage\\\":\\\"%s\\\",\\\"previousSlot\\\":\\\"%s\\\",\\\"previousImage\\\":\\\"%s\\\",\\\"postgresMode\\\":\\\"neon\\\",\\\"redisMode\\\":\\\"upstash\\\"}" "$4" "$5" "$active_slot" "$active_image"; exit 0; fi',
    '  if [[ "$code" == *JSON.stringify* ]]; then printf "{\\\"activeSlot\\\":\\\"%s\\\",\\\"activeImage\\\":\\\"%s\\\",\\\"previousSlot\\\":\\\"%s\\\",\\\"previousImage\\\":\\\"%s\\\",\\\"postgresMode\\\":\\\"neon\\\",\\\"redisMode\\\":\\\"upstash\\\"}" "$previous_slot" "$previous_image" "$active_slot" "$active_image"; exit 0; fi',
    '  if [[ "$code" == *activeSlot* ]]; then printf "%s" "$active_slot"; exit 0; fi',
    '  if [[ "$code" == *previousSlot* ]]; then printf "%s" "$previous_slot"; exit 0; fi',
    '  if [[ "$code" == *previousImage* ]]; then printf "%s" "$previous_image"; exit 0; fi',
    '  if [[ "$code" == *activeImage* ]]; then printf "%s" "$active_image"; exit 0; fi',
    'fi',
    'case "$*" in',
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
      SITE_RUNTIME_ENV_PATH: join(root, "runtime", "sites", "code2", "runtime.env"),
      SITE_APP_ENV_PATH: join(root, "runtime", "sites", "code2", "app.env"),
      SITE_DEPLOY_STATE_PATH: join(root, "runtime", "sites", "code2", "deploy-state.json"),
      SITE_BOOTSTRAP_MARKER_PATH: join(root, "runtime", "sites", "code2", "bootstrap.marker"),
      BLUE_DATA_PATH: join(root, "runtime", "sites", "code2", "data", "blue"),
      GREEN_DATA_PATH: join(root, "runtime", "sites", "code2", "data", "green"),
      EDGE_NETWORK_NAME: "sub2api-edge",
      BLUE_EDGE_ALIAS: "sub2api-code2-blue",
      GREEN_EDGE_ALIAS: "sub2api-code2-green",
      EDGE_RUNTIME_ROOT: join(root, "runtime", "edge"),
      HOST_STATE_PATH: join(root, "runtime", "host-state.json"),
      CONFIGURED_SITE_IDS: "code2,code3",
      SITE_DEPLOY_STATE_PATHS: `${join(root, "runtime", "sites", "code2", "deploy-state.json")},${join(root, "runtime", "sites", "code3", "deploy-state.json")}`,
      DOMAIN: "code2.contextid.cn",
      SUB2API_IMAGE: "code2@sha256:new",
      POSTGRES_MODE: "neon",
      REDIS_MODE: "upstash",
      APP_PROBE_PATH: "/health",
      DRAIN_SECONDS: "10",
      TRAEFIK_IMAGE: "traefik@sha256:edge",
      ACME_EMAIL: "ops@example.com",
      CLOUDFLARE_API_TOKEN: "placeholder",
      SING_BOX_CONFIG: JSON.stringify({ serverName: "www.cloudflare.com", target: "host.docker.internal:8443" }),
      ORIGIN_IP: "127.0.0.1",
      PROBE_RETRIES: "1",
      PROBE_DELAY_SECONDS: "0",
      RUNTIME_JSON: JSON.stringify({ SITE_ID: "code2", POSTGRES_MODE: "neon", REDIS_MODE: "upstash", APP_PROBE_PATH: "/health", DRAIN_SECONDS: "10" }),
      APP_ENV_JSON: "{}",
      APP_ENV_CONFIGURED: "false",
    },
  };
}

function run(root: string, environment: NodeJS.ProcessEnv, script: string, args: readonly string[] = []) {
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

  it("bootstraps a first Site through release only after host preflight and leaves host finalization to the success barrier", () => {
    const fixture = createShellFixture(false);
    const result = run(fixture.root, fixture.environment, "scripts/application-release.sh");

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
    expect(captured).not.toContain("write-host-state.cjs write");
    expect(existsSync(fixture.environment.SITE_DEPLOY_STATE_PATH!)).toBe(true);
    expect(existsSync(fixture.environment.SITE_BOOTSTRAP_MARKER_PATH!)).toBe(true);
    expect(existsSync(fixture.environment.SITE_ROUTE_PATH!)).toBe(true);
    expect(readFileSync(fixture.environment.SITE_DEPLOY_STATE_PATH!, "utf8")).toContain("code2@sha256:new");

    const finalize = run(fixture.root, fixture.environment, "scripts/finalize-host-state.sh");
    expect(finalize.status, finalize.stderr ?? "").toBe(0);
    const finalized = commands(fixture.log);
    expect(finalized).toContain("scripts/write-host-state.cjs write");
    expect(existsSync(fixture.environment.HOST_STATE_PATH!)).toBe(true);
  });

  it("releases, switches, and rolls back code2 without touching code3 or the Edge project", () => {
    const fixture = createShellFixture();

    for (const [script, args] of [["scripts/application-release.sh", []]] as const) {
      const result = run(fixture.root, fixture.environment, script, args);
      expect(result.status, `${script}: ${result.stderr ?? ""}`).toBe(0);
    }
    const switched = JSON.parse(readFileSync(fixture.environment.SITE_DEPLOY_STATE_PATH!, "utf8"));
    expect(switched).toMatchObject({ activeSlot: "green", activeImage: "code2@sha256:new", previousSlot: "blue", previousImage: "code2@sha256:old" });

    const rollback = run(fixture.root, fixture.environment, "scripts/rollback-slot.sh", ["code2.contextid.cn"]);
    expect(rollback.status, rollback.stderr ?? "").toBe(0);
    const rolledBack = JSON.parse(readFileSync(fixture.environment.SITE_DEPLOY_STATE_PATH!, "utf8"));
    expect(rolledBack).toMatchObject({ activeSlot: "blue", activeImage: "code2@sha256:old", previousSlot: "green", previousImage: "code2@sha256:new" });

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

  it("restores only its prior route, or removes a newly created route, when public readiness fails", () => {
    const fixture = createShellFixture();
    const route = fixture.environment.SITE_ROUTE_PATH!;
    mkdirSync(join(fixture.root, "runtime", "edge", "dynamic"), { recursive: true });
    writeFileSync(route, "code2 prior route\n");
    fixture.environment.FAIL_CURL = "true";

    const restored = run(fixture.root, fixture.environment, "scripts/switch-slot.sh", ["code2@sha256:new", "code2.contextid.cn"]);
    expect(restored.status).not.toBe(0);
    expect(readFileSync(route, "utf8")).toBe("code2 prior route\n");
    expect(existsSync(join(fixture.root, "runtime", "edge", "dynamic", "site-code3.yml"))).toBe(false);

    writeFileSync(join(fixture.root, "runtime", "sites", "code2", "deploy-state.json"), '{"activeSlot":"blue","activeImage":"code2@sha256:old","previousSlot":"green","previousImage":"code2@sha256:previous","postgresMode":"neon","redisMode":"upstash"}\n');
    rmSync(route);
    const removed = run(fixture.root, fixture.environment, "scripts/switch-slot.sh", ["code2@sha256:new", "code2.contextid.cn"]);
    expect(removed.status).not.toBe(0);
    expect(existsSync(route)).toBe(false);
  });
});
