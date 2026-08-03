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
set -euo pipefail
slot_code='const x=require(process.argv[1]); if(!["blue","green"].includes(x.activeSlot)) process.exit(1); process.stdout.write(x.activeSlot)'
mapping_code='const x=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")); const want=process.argv[2]; const ok=x.version===1&&x.sites?.length===1&&x.sites[0]==="code2"&&x.legacyCode2?.runtimeRoot==="runtime"&&x.legacyCode2?.composeProject==="sub2api"&&x.legacyCode2?.routeLayout==="flat"&&(want==="either"||x.legacyCode2?.handoverComplete===(want==="complete")); process.exit(ok?0:1)'
payload_code='process.stdout.write(JSON.stringify({TRAEFIK_IMAGE:process.env.TRAEFIK_IMAGE,CLOUDFLARE_DNS_API_TOKEN:process.env.CLOUDFLARE_API_TOKEN,ACME_EMAIL:process.env.ACME_EMAIL,EDGE_RUNTIME_ROOT:process.env.EDGE_RUNTIME_ROOT}))'
if [[ "$#" -eq 3 && "$1" == -e && "$2" == "$slot_code" ]]; then
  [[ -f "$3" && "$(<"$3")" == *'"activeSlot":"blue"'* ]] || exit 1
  printf blue; exit 0
fi
if [[ "$#" -eq 4 && "$1" == -e && "$2" == "$mapping_code" ]]; then
  path="$3"; want="$4"; [[ -f "$path" && "$want" =~ ^(pending|complete|either)$ ]] || exit 1
  mapping="$(<"$path")"; [[ "$mapping" == *'"version":1'* && "$mapping" == *'"sites":["code2"]'* && "$mapping" == *'"runtimeRoot":"runtime"'* && "$mapping" == *'"composeProject":"sub2api"'* && "$mapping" == *'"routeLayout":"flat"'* ]] || exit 1
  [[ "$want" == either || ( "$want" == complete && "$mapping" == *'"handoverComplete":true'* ) || ( "$want" == pending && "$mapping" == *'"handoverComplete":false'* ) ]] || exit 1
  exit 0
fi
if [[ "$#" -eq 2 && "$1" == -e && "$2" == "$payload_code" ]]; then
  for value in "$TRAEFIK_IMAGE" "$CLOUDFLARE_API_TOKEN" "$ACME_EMAIL" "$EDGE_RUNTIME_ROOT"; do [[ "$value" =~ ^[A-Za-z0-9_./:@-]+$ ]] || exit 97; done
  printf '{"TRAEFIK_IMAGE":"%s","CLOUDFLARE_DNS_API_TOKEN":"%s","ACME_EMAIL":"%s","EDGE_RUNTIME_ROOT":"%s"}' "$TRAEFIK_IMAGE" "$CLOUDFLARE_API_TOKEN" "$ACME_EMAIL" "$EDGE_RUNTIME_ROOT"
  exit 0
fi
exit 97
`);
  writeFileSync(join(bin, "npx"), `#!/bin/bash
set -euo pipefail
printf 'NPX %s\\n' "$*" >> "$FAKE_LOG"
fail() { [[ "\${FAIL_AFTER:-}" != "$1" ]] || exit 91; }
if [[ "$#" -eq 7 && "$1" == --no-install && "$2" == tsx && "$3" == scripts/write-host-state.ts && "$4" == write-legacy && "$6" == code2 && "$7" =~ ^(pending|complete)$ ]]; then
  path="$5"; handover="$7"
  complete=false; [[ "$handover" == complete ]] && complete=true
  printf '{"version":1,"sites":["code2"],"legacyCode2":{"runtimeRoot":"runtime","composeProject":"sub2api","routeLayout":"flat","handoverComplete":%s}}\\n' "$complete" > "$path"
  chmod 600 "$path"
  if [[ -f "$JOURNAL" ]]; then printf 'HOST_STATE journal-before-mutation\\n' >> "$FAKE_LOG"; else printf 'HOST_STATE preview-without-journal\\n' >> "$FAKE_LOG"; fi
  exit 0
fi
if [[ "$#" -eq 5 && "$1" == --no-install && "$2" == tsx && "$3" == scripts/render-runtime-env.ts && "$4" == write ]]; then
  path="$5"; edge_root="$(dirname "$path")"; payload="$(</dev/stdin)"
  expected="{\"TRAEFIK_IMAGE\":\"$TRAEFIK_IMAGE\",\"CLOUDFLARE_DNS_API_TOKEN\":\"$CLOUDFLARE_API_TOKEN\",\"ACME_EMAIL\":\"$ACME_EMAIL\",\"EDGE_RUNTIME_ROOT\":\"$edge_root\"}"
  [[ "$payload" == "$expected" ]] || exit 99
  mkdir -p "$edge_root"; printf 'TRAEFIK_IMAGE="test"\\n' > "$path"; chmod 600 "$path"; fail edge-env; exit 0
fi
if [[ "$#" -eq 10 && "$1" == --no-install && "$2" == tsx && "$3" == scripts/render-edge-config.ts && "$4" == write && "$6" == traefik/traefik.yml && "$7" == traefik/dynamic/sing-box.yml && "$8" == "$ACME_EMAIL" && "$9" == "$SING_BOX_SERVER_NAME" && "\${10}" == "$SING_BOX_TARGET" ]]; then
  root="$5"; mkdir -p "$root/dynamic"; printf 'static\\n' > "$root/traefik.yml"; chmod 600 "$root/traefik.yml"; fail edge-static
  printf 'singbox\\n' > "$root/dynamic/00-sing-box.yml"; chmod 600 "$root/dynamic/00-sing-box.yml"; fail edge-singbox; exit 0
fi
if [[ "$#" -eq 10 && "$1" == --no-install && "$2" == tsx && "$3" == scripts/render-site-route.ts && "$4" == write && "$5" == traefik/dynamic/site.yml && "$7" == code2 && "$8" == "$DOMAIN" && "$9" == blue && "\${10}" == sub2api-blue ]]; then
  route="$6"; mkdir -p "$(dirname "$route")"; printf 'route\\n' > "$route"; chmod 600 "$route"; exit 0
fi
exit 98
`);
  writeFileSync(join(bin, "docker"), `#!/bin/bash
set -euo pipefail
state="$FAKE_STATE"; log() { printf 'DOCKER %s\\n' "$*" >> "$FAKE_LOG"; }; exists() { [[ -f "$state/$1" ]]; }
log "$@"
if [[ "$1" == ps ]]; then
  if [[ "$#" -eq 8 && "$2" == -q && "$3" == --filter && "$4" == label=com.docker.compose.project=sub2api && "$5" == --filter && "$7" == --filter && "$8" == status=running ]]; then
    [[ "$6" == label=com.docker.compose.service=traefik ]] && printf 'aaaaaaaaaaaa\\n'
    [[ "$6" == label=com.docker.compose.service=sub2api-blue ]] && printf 'bbbbbbbbbbbb\\n'
  elif [[ "$#" -eq 6 && "$2" == -aq && "$3" == --filter && "$4" == label=com.docker.compose.project=sub2api-edge && "$5" == --filter && "$6" == label=com.docker.compose.service=traefik ]]; then
    exists edge && printf 'cccccccccccc\\n'
  fi
  exit 0
fi
if [[ "$1" == inspect ]]; then
  if [[ "$#" -eq 2 ]]; then
    [[ "$2" == aaaaaaaaaaaa || "$2" == bbbbbbbbbbbb ]] && exit 0
    [[ "$2" == cccccccccccc ]] && exists edge && exit 0
    exit 1
  fi
  [[ "$#" -eq 4 && "$2" == -f ]] || exit 94
  format="$3"; id="$4"
  if [[ "$format" == '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}' ]]; then
    case "$id" in
      aaaaaaaaaaaa) printf '%s' "\${LEGACY_TRAEFIK_LABELS:-sub2api/traefik}" ;;
      bbbbbbbbbbbb) printf '%s' "\${LEGACY_APP_LABELS:-sub2api/sub2api-blue}" ;;
      cccccccccccc) exists edge || exit 1; printf '%s' "\${EDGE_LABELS:-sub2api-edge/traefik}" ;;
      *) exit 1 ;;
    esac
    exit 0
  fi
  if [[ "$format" == '{{.State.Running}}' ]]; then
    case "$id" in
      aaaaaaaaaaaa) exists legacy-stopped && printf false || printf true ;;
      bbbbbbbbbbbb) printf true ;;
      cccccccccccc) exists edge || exit 1; exists edge-stopped && printf false || printf true ;;
      *) exit 1 ;;
    esac
    exit 0
  fi
  if [[ "$format" == '{{with index .NetworkSettings.Networks "sub2api-edge"}}{{.NetworkID}}{{end}}' && "$id" == bbbbbbbbbbbb ]]; then
    exists attached && printf network-id
    exit 0
  fi
  exit 94
fi
if [[ "$1" == network ]]; then
  if [[ "$#" -eq 3 && "$2" == inspect && "$3" == sub2api-edge ]]; then exists network; exit; fi
  if [[ "$#" -eq 5 && "$2" == inspect && "$3" == -f && "$5" == sub2api-edge && "$4" == '{{index .Labels "com.docker.compose.project"}}' ]]; then exists network || exit 1; printf '%s' "\${NETWORK_LABELS:-sub2api-edge}"; exit 0; fi
  if [[ "$#" -eq 6 && "$2" == connect && "$3" == --alias && "$4" == sub2api-blue && "$5" == sub2api-edge && "$6" == bbbbbbbbbbbb ]]; then
    exists network || { printf 'connection-before-network\\n' >> "$FAKE_LOG"; exit 95; }
    [[ "\${FAIL_AFTER:-}" == attachment ]] && exit 91
    touch "$state/attached"; exit 0
  fi
  if [[ "$#" -eq 4 && "$2" == disconnect && "$3" == sub2api-edge && "$4" == bbbbbbbbbbbb ]]; then exists attached || exit 1; rm -f "$state/attached"; exit 0; fi
  if [[ "$#" -eq 3 && "$2" == rm && "$3" == sub2api-edge ]]; then exists network || exit 1; exists attached && exit 93; rm -f "$state/network"; exit 0; fi
  exit 94
fi
if [[ "$1" == compose ]]; then
  [[ "$#" -eq 9 && "$2" == --project-name && "$3" == sub2api-edge && "$4" == --env-file && "$5" == "$EDGE_ENV" && "$6" == -f && "$7" == compose/edge.yml && "$9" == traefik ]] || exit 94
  [[ -f "$EDGE_ENV" ]] || exit 94
  if [[ "$8" == create ]]; then touch "$state/network"; [[ "\${FAIL_AFTER:-}" == network ]] && exit 91; touch "$state/edge" "$state/edge-stopped"; [[ "\${FAIL_AFTER:-}" == container ]] && exit 91; exit 0; fi
  if [[ "$8" == start ]]; then exists edge || exit 1; rm -f "$state/edge-stopped"; exit 0; fi
  exit 94
fi
if [[ "$#" -eq 2 && "$1" == stop && "$2" == aaaaaaaaaaaa ]]; then touch "$state/legacy-stopped"; exit 0; fi
if [[ "$#" -eq 2 && "$1" == stop && "$2" == cccccccccccc ]]; then exists edge || exit 1; touch "$state/edge-stopped"; exit 0; fi
if [[ "$#" -eq 2 && "$1" == start && "$2" == aaaaaaaaaaaa ]]; then rm -f "$state/legacy-stopped"; exit 0; fi
if [[ "$#" -eq 2 && "$1" == rm && "$2" == cccccccccccc ]]; then exists edge || exit 1; rm -f "$state/edge" "$state/edge-stopped"; exit 0; fi
exit 94
`);
  writeFileSync(join(bin, "cp"), `#!/bin/bash
/bin/cp "$@"
exit 0
`);
  writeFileSync(join(bin, "chmod"), `#!/bin/bash
/bin/chmod "$@"
[[ "\${@: -1}" == */edge/acme.json && "\${FAIL_AFTER:-}" == acme ]] && exit 91
exit 0
`);
  writeFileSync(join(bin, "bash"), `#!/bin/bash
if [[ "$1" == scripts/probe-origin-strict.sh || "$1" == scripts/probe-origin.sh ]]; then
  printf 'PROBE %s\\n' "$1" >> "$FAKE_LOG"; [[ "\${FAIL_AFTER:-}" == health ]] && exit 91; exit 0
fi
if [[ "$1" == -c ]]; then printf 'SINGBOX %s\\n' "$2" >> "$FAKE_LOG"; fi
exec /bin/bash "$@"
`);
  for (const executable of ["node", "npx", "docker", "bash", "cp", "chmod"]) chmodSync(join(bin, executable), 0o755);
  const state = join(runtime, "host-state.json");
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, FAKE_STATE: fakeState, FAKE_LOG: join(root, "fake.log"), JOURNAL: join(runtime, "adopt-single-site-layout.journal"), EDGE_ENV: join(runtime, "edge", "edge.env"), TRAEFIK_IMAGE: "traefik:test", CLOUDFLARE_API_TOKEN: "token", ACME_EMAIL: "ops@example.test", SING_BOX_SERVER_NAME: "www.cloudflare.com", SING_BOX_TARGET: "host.docker.internal:8443", DOMAIN: "code2.example.test", ORIGIN_IP: "203.0.113.10", APP_PROBE_PATH: "/api/ready", SING_BOX_VERIFY_COMMAND: "true" };
  return { root, runtime, state, env, run: (mode?: string) => execFileSync("/bin/bash", [script, "--environment", "test", "--site", "code2", "--host-state", state, ...(mode ? [mode] : [])], { cwd: process.cwd(), env, stdio: "pipe" }), log: () => existsSync(env.FAKE_LOG!) ? readFileSync(env.FAKE_LOG!, "utf8") : "" };
}

function validRecoveryState(f: Fixture, state: string, handover?: "pending" | "complete"): void {
  const edge = join(f.runtime, "edge");
  const dynamic = join(edge, "dynamic");
  mkdirSync(dynamic, { recursive: true });
  writeFileSync(join(dynamic, "site-code2.yml"), "adopted route\n");
  writeFileSync(join(dynamic, "site-code2.yml.before-adoption"), "legacy route\n");
  for (const file of ["edge.env", "traefik.yml", "acme.json", join("dynamic", "00-sing-box.yml")]) writeFileSync(join(edge, file), "owned\n");
  journal(f.root, state, {
    route_preexisting: "true", route_backup_intent: "true", route_backup_created: "true", route_write_intent: "true",
    network_intent: "true", network_created: "true", attachment_intent: "true", attachment_created: "true",
    edge_container: "cccccccccccc", edge_container_intent: "true", edge_env_intent: "true", edge_env_created: "true",
    edge_static_intent: "true", edge_static_created: "true", edge_singbox_intent: "true", edge_singbox_created: "true",
    acme_intent: "true", acme_created: "true",
  });
  for (const artifact of ["network", "edge", "edge-stopped", "attached"]) writeFileSync(join(f.root, "fake-state", artifact), "");
  if (handover) hostState(f.state, handover);
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
    expect(f.log()).not.toMatch(/compose down|volume|redis|sql|connection-before-network/);
  });

  it("rolls back a failed health handover, removing the pending mapping and retaining its journal", () => {
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
      const f = fixture();
      validRecoveryState(f, state, handover);
      expect(() => f.run("--rollback")).not.toThrow();
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
      expect(readFileSync(join(f.runtime, "adopt-single-site-layout.journal"), "utf8")).toContain("state=pending");
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
    expect(existsSync(f.state)).toBe(false);
    expect(readFileSync(join(f.runtime, "adopt-single-site-layout.journal"), "utf8")).toContain("state=pending");
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
