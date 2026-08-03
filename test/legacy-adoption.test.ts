import { execFileSync } from "node:child_process";
import { chmodSync, existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

const script = join(process.cwd(), "scripts", "adopt-single-site-layout.sh");
const roots: string[] = [];

type Fixture = ReturnType<typeof fixture>;

function journal(root: string, state = "prepared", overrides: Record<string, string> = {}): void {
  const runtime = join(root, "runtime");
  const edge = join(runtime, "edge");
  const fields: Record<string, string> = {
    version: "1", environment: "test", site: "code2", host_state: join(runtime, "host-state.json"),
    legacy_project: "sub2api", legacy_traefik: "aaaaaaaaaaaa", active_slot: "blue", active_app: "bbbbbbbbbbbb",
    edge_project: "sub2api-edge", edge_root: edge, route: join(edge, "dynamic", "site-code2.yml"),
    route_backup: join(edge, "dynamic", "site-code2.yml.before-adoption"), route_preexisting: "false",
    route_backup_intent: "false", route_backup_created: "false", route_write_intent: "false", edge_network: "sub2api-edge",
    network_intent: "false", network_created: "false", attachment_intent: "false", attachment_created: "false",
    edge_container: "uncreated", edge_container_preexisting: "false", edge_container_intent: "false", edge_dynamic_dir_created: "false",
    edge_env_intent: "false", edge_env_created: "false", edge_static_intent: "false", edge_static_created: "false",
    edge_singbox_intent: "false", edge_singbox_created: "false", acme_destination_preexisting: "false", acme_intent: "false",
    acme_created: "false", state,
    ...overrides,
  };
  writeFileSync(join(runtime, "adopt-single-site-layout.journal"), `${Object.entries(fields).map(([key, value]) => `${key}=${value}`).join("\n")}\n`, { mode: 0o600 });
}

function hostState(path: string, handover: "pending" | "complete", sites = ["code2"]): void {
  writeFileSync(path, `${JSON.stringify({ version: 1, sites, legacyCode2: { runtimeRoot: "runtime", composeProject: "sub2api", routeLayout: "flat", handoverComplete: handover === "complete" } })}\n`, { mode: 0o600 });
}

function fixture(): { root: string; runtime: string; state: string; env: NodeJS.ProcessEnv; run: (mode?: string) => void; log: () => string } {
  const root = mkdtempSync(join(tmpdir(), "sub2api-adopt-"));
  roots.push(root);
  const runtime = join(root, "runtime");
  const bin = join(root, "bin");
  const fakeState = join(root, "fake-state");
  mkdirSync(join(runtime, "data"), { recursive: true });
  mkdirSync(bin, { recursive: true });
  mkdirSync(fakeState, { recursive: true });
  writeFileSync(join(runtime, "deploy-state.json"), '{"activeSlot":"blue"}\n');
  writeFileSync(join(runtime, "acme.json"), "legacy acme\n", { mode: 0o600 });
  writeFileSync(join(runtime, "data", "must-not-move"), "legacy data\n");

  writeFileSync(join(bin, "node"), `#!/bin/bash
if [[ "$1" == -e ]]; then
  if [[ "$2" == *activeSlot* ]]; then printf blue; exit 0; fi
  if [[ "$2" == *handoverComplete* ]]; then
    path="$3"; want="$4"; [[ -f "$path" ]] || exit 1
    state="$(<"$path")"; [[ "$state" == *'"sites":["code2"]'* && "$state" == *'"runtimeRoot":"runtime"'* && "$state" == *'"composeProject":"sub2api"'* && "$state" == *'"routeLayout":"flat"'* ]] || exit 1
    [[ "$want" == either || ( "$want" == complete && "$state" == *'"handoverComplete":true'* ) || ( "$want" != complete && "$state" == *'"handoverComplete":false'* ) ]] || exit 1
    exit 0
  fi
fi
exit 97
`);
  writeFileSync(join(bin, "npx"), `#!/bin/bash
set -euo pipefail
printf 'NPX %s\\n' "$*" >> "$FAKE_LOG"
fail() { [[ "\${FAIL_AFTER:-}" == "$1" ]] && exit 91; }
if [[ "$*" == *write-host-state.ts* ]]; then
  path="\${@: -3:1}"; handover="\${@: -1}"; [[ -f "$JOURNAL" ]] || { printf 'missing-journal-before-host-state\\n' >> "$FAKE_LOG"; exit 92; }
  complete=false; [[ "$handover" == complete ]] && complete=true
  printf '{"version":1,"sites":["code2"],"legacyCode2":{"runtimeRoot":"runtime","composeProject":"sub2api","routeLayout":"flat","handoverComplete":%s}}\\n' "$complete" > "$path"
  chmod 600 "$path"; printf 'HOST_STATE journal-before-mutation\\n' >> "$FAKE_LOG"; exit 0
fi
if [[ "$*" == *render-runtime-env.ts* ]]; then
  path="\${@: -1}"; mkdir -p "$(dirname "$path")"; printf 'TRAEFIK_IMAGE="test"\\n' > "$path"; chmod 600 "$path"; fail edge-env; exit 0
fi
if [[ "$*" == *render-edge-config.ts* ]]; then
  root="\${@: -6:1}"; mkdir -p "$root/dynamic"; printf 'static\\n' > "$root/traefik.yml"; chmod 600 "$root/traefik.yml"; fail edge-static
  printf 'singbox\\n' > "$root/dynamic/00-sing-box.yml"; chmod 600 "$root/dynamic/00-sing-box.yml"; fail edge-singbox; exit 0
fi
if [[ "$*" == *render-site-route.ts* ]]; then
  route="\${@: -5:1}"; mkdir -p "$(dirname "$route")"; printf 'route\\n' > "$route"; chmod 600 "$route"; exit 0
fi
exit 98
`);
  writeFileSync(join(bin, "docker"), `#!/bin/bash
set -euo pipefail
state="$FAKE_STATE"; log() { printf 'DOCKER %s\\n' "$*" >> "$FAKE_LOG"; }; exists() { [[ -f "$state/$1" ]]; }
args="$*"; log "$args"
[[ "$args" != *' compose down'* && "$args" != *' compose '*sub2api' '* ]] || { printf 'forbidden-compose\\n' >> "$FAKE_LOG"; exit 96; }
if [[ "$1" == ps ]]; then
  if [[ "$args" == *'com.docker.compose.project=sub2api'* && "$args" == *'com.docker.compose.service=traefik'* ]]; then printf aaaaaaaaaaaa; fi
  if [[ "$args" == *'com.docker.compose.project=sub2api'* && "$args" == *'com.docker.compose.service=sub2api-blue'* ]]; then printf bbbbbbbbbbbb; fi
  if [[ "$args" == *'com.docker.compose.project=sub2api-edge'* && "$args" == *'com.docker.compose.service=traefik'* ]] && exists edge; then printf cccccccccccc; fi
  exit 0
fi
if [[ "$1" == inspect ]]; then
  format=""; [[ "$2" == -f ]] && format="$3" && shift 2; id="$2"
  case "$id" in aaaaaaaaaaaa) labels="\${LEGACY_TRAEFIK_LABELS:-sub2api/traefik}";; bbbbbbbbbbbb) labels="\${LEGACY_APP_LABELS:-sub2api/sub2api-blue}";; cccccccccccc) exists edge || exit 1; labels="\${EDGE_LABELS:-sub2api-edge/traefik}";; sub2api-edge) exists network || exit 1; labels="\${NETWORK_LABELS:-sub2api-edge}";; *) exit 1;; esac
  [[ -z "$format" ]] && exit 0
  [[ "$format" == *'.State.Running'* ]] && { [[ "$id" == aaaaaaaaaaaa && -f "$state/legacy-stopped" ]] && printf false || printf true; exit 0; }
   [[ "$id" == bbbbbbbbbbbb && "$format" == *NetworkSettings.Networks* ]] && { exists attached && printf network-id; exit 0; }
   [[ "$id" == sub2api-edge && "$format" == *range* ]] && { exists attached && printf bbbbbbbbbbbb; exit 0; }
  printf '%s' "$labels"; exit 0
fi
if [[ "$1" == network ]]; then
  case "$2" in inspect) exists network || exit 1; [[ "$*" == *range* ]] && { exists attached && printf bbbbbbbbbbbb; } || printf '%s' "\${NETWORK_LABELS:-sub2api-edge}";;
  connect) exists network || { printf 'connection-before-network\\n' >> "$FAKE_LOG"; exit 95; }; touch "$state/attached"; [[ "\${FAIL_AFTER:-}" == attachment ]] && exit 91;;
  disconnect) rm -f "$state/attached";; rm) exists attached && exit 93; rm -f "$state/network";; esac; exit 0
fi
if [[ "$1" == compose ]]; then
  [[ "$args" == *'--project-name sub2api-edge'* && "$args" == *"--env-file $EDGE_ENV"* && "$args" == *'-f compose/edge.yml '* ]] || exit 94
  [[ -f "$EDGE_ENV" ]] || exit 94
  if [[ "$args" == *' create traefik' ]]; then touch "$state/network"; [[ "\${FAIL_AFTER:-}" == network ]] && exit 91; touch "$state/edge"; [[ "\${FAIL_AFTER:-}" == container ]] && exit 91; exit 0; fi
  [[ "$args" == *' start traefik' ]] && exists edge && exit 0
  exit 94
fi
case "$1" in stop) [[ "$2" == aaaaaaaaaaaa ]] && touch "$state/legacy-stopped" || rm -f "$state/edge";; start) rm -f "$state/legacy-stopped";; rm) rm -f "$state/edge";; esac
`);
  writeFileSync(join(bin, "cp"), `#!/bin/bash
/bin/cp "$@"
[[ "\${@: -1}" == */edge/acme.json && "\${FAIL_AFTER:-}" == acme ]] && exit 91
`);
  writeFileSync(join(bin, "bash"), `#!/bin/bash
if [[ "$1" == scripts/probe-origin-strict.sh || "$1" == scripts/probe-origin.sh ]]; then
  printf 'PROBE %s\\n' "$1" >> "$FAKE_LOG"; [[ "\${FAIL_AFTER:-}" == health ]] && exit 91; exit 0
fi
if [[ "$1" == -c ]]; then printf 'SINGBOX %s\\n' "$2" >> "$FAKE_LOG"; fi
exec /bin/bash "$@"
`);
  for (const executable of ["node", "npx", "docker", "bash", "cp"]) chmodSync(join(bin, executable), 0o755);
  const state = join(runtime, "host-state.json");
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, FAKE_STATE: fakeState, FAKE_LOG: join(root, "fake.log"), JOURNAL: join(runtime, "adopt-single-site-layout.journal"), EDGE_ENV: join(runtime, "edge", "edge.env"), TRAEFIK_IMAGE: "traefik:test", CLOUDFLARE_API_TOKEN: "token", ACME_EMAIL: "ops@example.test", SING_BOX_SERVER_NAME: "www.cloudflare.com", SING_BOX_TARGET: "host.docker.internal:8443", DOMAIN: "code2.example.test", ORIGIN_IP: "203.0.113.10", APP_PROBE_PATH: "/api/ready", SING_BOX_VERIFY_COMMAND: "true" };
  return { root, runtime, state, env, run: (mode?: string) => execFileSync("/bin/bash", [script, "--environment", "test", "--site", "code2", "--host-state", state, ...(mode ? [mode] : [])], { cwd: process.cwd(), env, stdio: "pipe" }), log: () => existsSync(env.FAKE_LOG!) ? readFileSync(env.FAKE_LOG!, "utf8") : "" };
}

afterEach(() => { for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true }); });

describe("legacy single-site adoption", () => {
  it("dry-run leaves the legacy host and Docker untouched", () => {
    const f = fixture();
    f.run();
    expect(existsSync(f.state)).toBe(false);
    expect(f.log()).toBe("");
    expect(readFileSync(join(f.runtime, "data", "must-not-move"), "utf8")).toBe("legacy data\n");
    expect(existsSync(join(f.runtime, "edge"))).toBe(false);
  });

  it("hands over only after journaling, validating every fake health check, and preserves legacy data", () => {
    const f = fixture();
    f.run("--apply");
    const content = readFileSync(join(f.runtime, "adopt-single-site-layout.journal"), "utf8");
    expect(statSync(join(f.runtime, "adopt-single-site-layout.journal")).mode & 0o777).toBe(0o600);
    expect(content).toContain("state=complete");
    expect(readFileSync(f.state, "utf8")).toContain('"handoverComplete":true');
    expect(readFileSync(join(f.runtime, "edge", "edge.env"), "utf8")).toContain('TRAEFIK_IMAGE="test"');
    expect(existsSync(join(f.runtime, "edge", "dynamic", "site-code2.yml"))).toBe(true);
    expect(readFileSync(join(f.runtime, "data", "must-not-move"), "utf8")).toBe("legacy data\n");
    expect(f.log()).toMatch(/HOST_STATE journal-before-mutation[\s\S]*network connect --alias sub2api-blue sub2api-edge bbbbbbbbbbbb[\s\S]*PROBE scripts\/probe-origin-strict.sh[\s\S]*PROBE scripts\/probe-origin.sh[\s\S]*SINGBOX true/);
    expect(f.log()).toMatch(/compose --project-name sub2api-edge --env-file .*runtime\/edge\/edge.env -f compose\/edge.yml create traefik/);
    expect(f.log()).not.toMatch(/compose down|volume|redis|sql|forbidden-compose|connection-before-network/);
  });

  it("rolls back a failed health handover, restoring legacy Traefik and retaining its pending journal", () => {
    const f = fixture();
    f.env.FAIL_AFTER = "health";
    expect(() => f.run("--apply")).toThrow();
    expect(existsSync(f.state)).toBe(false);
    expect(readFileSync(join(f.runtime, "adopt-single-site-layout.journal"), "utf8")).toContain("state=pending");
    expect(existsSync(join(f.runtime, "edge", "edge.env"))).toBe(false);
    expect(existsSync(join(f.runtime, "edge", "dynamic", "site-code2.yml"))).toBe(false);
    expect(f.log()).toMatch(/stop aaaaaaaaaaaa[\s\S]*compose --project-name sub2api-edge.* start traefik[\s\S]*stop cccccccccccc[\s\S]*start aaaaaaaaaaaa[\s\S]*network disconnect sub2api-edge bbbbbbbbbbbb[\s\S]*network rm sub2api-edge/);
  });

  it("rejects malformed or unsafe journals before destructive recovery", () => {
    for (const setup of ["malformed", "labels", "backup"] as const) {
      const f = fixture();
      if (setup === "malformed") writeFileSync(join(f.runtime, "adopt-single-site-layout.journal"), "version=1\n");
      else if (setup === "labels") journal(f.root, "prepared");
      else { journal(f.root, "prepared", { route_preexisting: "true", route_backup_intent: "true" }); mkdirSync(join(f.runtime, "edge", "dynamic"), { recursive: true }); }
      if (setup === "labels") f.env.LEGACY_TRAEFIK_LABELS = "other/traefik";
      expect(() => f.run("--rollback")).toThrow();
      expect(f.log()).not.toMatch(/DOCKER (stop|start|rm|network disconnect|network rm)/);
      expect(existsSync(f.state)).toBe(false);
    }
  });

  it("accepts only recoverable journal and host-state pairs", () => {
    const accepted: Array<[string, "pending" | "complete" | undefined]> = [["prepared", undefined], ["prepared", "pending"], ["pending", "pending"], ["completing", "pending"], ["completing", "complete"], ["complete", "complete"]];
    for (const [state, handover] of accepted) {
      const f = fixture(); journal(f.root, state); if (handover) hostState(f.state, handover); expect(() => f.run("--rollback")).not.toThrow();
    }
    const rejected: Array<[string, "pending" | "complete" | undefined, string[]?]> = [["pending", undefined], ["complete", "pending"], ["prepared", "complete"], ["complete", "complete", ["code2", "code3"]]];
    for (const [state, handover, sites] of rejected) {
      const f = fixture(); journal(f.root, state); if (handover) hostState(f.state, handover, sites); expect(() => f.run("--rollback")).toThrow(); expect(f.log()).not.toMatch(/DOCKER (stop|start|rm|network disconnect|network rm)/);
    }
  });

  it("uses intent-only cleanup after every interruptible writer and Docker mutation, then allows retry", () => {
    for (const point of ["edge-env", "edge-static", "edge-singbox", "acme", "network", "container", "attachment"] as const) {
      const f = fixture();
      f.env.FAIL_AFTER = point;
      expect(() => f.run("--apply")).toThrow();
      expect(existsSync(f.state)).toBe(false);
      expect(existsSync(join(f.root, "fake-state", "network"))).toBe(false);
      expect(existsSync(join(f.root, "fake-state", "edge"))).toBe(false);
      expect(existsSync(join(f.root, "fake-state", "attached"))).toBe(false);
      expect(existsSync(join(f.runtime, "edge", "edge.env"))).toBe(false);
      expect(existsSync(join(f.runtime, "edge", "traefik.yml"))).toBe(false);
      expect(existsSync(join(f.runtime, "edge", "dynamic", "00-sing-box.yml"))).toBe(false);
      f.env.FAIL_AFTER = "";
      expect(() => f.run("--apply")).not.toThrow();
      expect(readFileSync(join(f.runtime, "adopt-single-site-layout.journal"), "utf8")).toContain("state=complete");
    }
  });

  it("accepts an attachment intent with no attachment after connect failure and retries cleanly", () => {
    const f = fixture();
    f.env.FAIL_AFTER = "attachment";
    expect(() => f.run("--apply")).toThrow();
    expect(f.log()).toMatch(/network connect --alias sub2api-blue sub2api-edge bbbbbbbbbbbb/);
    expect(existsSync(join(f.root, "fake-state", "attached"))).toBe(false);
    expect(existsSync(join(f.root, "fake-state", "network"))).toBe(false);
    f.env.FAIL_AFTER = "";
    expect(() => f.run("--apply")).not.toThrow();
  });

  it("retires only a completed code2-only journal", () => {
    const f = fixture();
    journal(f.root, "complete"); hostState(f.state, "complete");
    expect(() => f.run("--retire-journal")).not.toThrow();
    expect(existsSync(join(f.runtime, "adopt-single-site-layout.journal.retired"))).toBe(true);
    const unsafe = fixture(); journal(unsafe.root, "complete"); hostState(unsafe.state, "complete", ["code2", "code3"]);
    expect(() => unsafe.run("--retire-journal")).toThrow();
  });
});
