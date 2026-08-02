import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { renderTraefikConfig } from "../scripts/render-traefik-config.js";

const read = (path: string) => readFileSync(new URL(`../${path}`, import.meta.url), "utf8");

describe("local deployment command contracts", () => {
  it("separates infrastructure reconciliation from application release triggers", () => {
    const index = read("src/index.ts");
    expect(index).toContain('new command.local.Command("infra-reconcile"');
    expect(index).toContain('new command.local.Command("application-release"');
    expect(index).toContain('bash scripts/infra-reconcile.sh');
    expect(index).toContain('bash scripts/application-release.sh');
    expect(index).toContain("infraTriggers");
    expect(index).toContain("releaseTriggers");
    expect(index).toContain("sub2apiImage");
    expect(index).toContain("POSTGRES_MODE: config.postgresMode");
    expect(index).toContain("REDIS_MODE: config.redisMode");
    const releaseStart = index.indexOf('new command.local.Command("application-release"');
    const releaseSection = index.slice(releaseStart);
    expect(releaseSection).toContain("SUB2API_IMAGE: config.sub2apiImage");
    expect(releaseSection).not.toContain("POSTGRES_MODE");
    expect(releaseSection).not.toContain("REDIS_MODE");
    expect(index).not.toContain("const deployment = new command.local.Command");
    expect(index).toContain("ignoreChanges: [\"environment.SUB2API_IMAGE\"]");
    expect(index).toContain("dependsOn: [infraReconcile, postStrictPublicReadiness]");
    expect(index).not.toContain("ignoreChanges: [\"environment.POSTGRES_MODE\", \"environment.REDIS_MODE\"]");
    expect(read("scripts/application-release.sh")).toContain("active_image");
  });

  it("enforces setup-only data modes before deployment side effects", () => {
    const state = read("scripts/slot-state.ts");
    const infra = read("scripts/infra-reconcile.sh");
    const release = read("scripts/application-release.sh");
    const switchSlot = read("scripts/switch-slot.sh");
    expect(state).toContain("postgresMode");
    expect(state).toContain("redisMode");
    expect(infra).toContain("deployment-mode.ts check");
    expect(release).toContain("deployment-mode.ts check");
    expect(switchSlot).toContain("deployment-mode.ts check");
    expect(infra.indexOf("deployment-mode.ts check")).toBeLessThan(infra.indexOf("deploy-compose.sh"));
    expect(release.indexOf("deployment-mode.ts check")).toBeLessThan(release.indexOf("switch-slot.sh"));
    expect(switchSlot.indexOf("deployment-mode.ts check")).toBeLessThan(switchSlot.indexOf("stop_service"));
  });

  it("uses bash explicitly and waits on health instead of ps --wait", () => {
    const index = read("src/index.ts");
    const deploy = read("scripts/deploy-compose.sh");
    expect(index).toContain("bash scripts/infra-reconcile.sh");
    expect(deploy).toContain("--wait");
    expect(deploy).toContain("--wait-timeout");
    expect(deploy).not.toContain("ps --wait");
    expect(deploy).not.toContain(". runtime/runtime.env");
    expect(deploy).toContain('CLOUDFLARE_DNS_API_TOKEN="${CLOUDFLARE_DNS_API_TOKEN:-$(runtime_value CLOUDFLARE_DNS_API_TOKEN)}"');
  });

  it("preflights bootstrap state before provider construction", () => {
    const index = read("src/index.ts");
    const preflight = read("src/deployment-preflight.ts");
    expect(index).toContain("readDeploymentPreflight(process.cwd()");
    expect(index.indexOf("readDeploymentPreflight(process.cwd()")).toBeLessThan(index.indexOf("const databaseInputs"));
    expect(index.indexOf("readDeploymentPreflight(process.cwd()")).toBeLessThan(index.indexOf("const domain = createDomainResources"));
    expect(preflight).toContain("bootstrap marker exists but deploy-state is missing");
  });

  it("keeps strict origin readiness before Full strict and public readiness after it", () => {
    const index = read("src/index.ts");
    const infra = read("scripts/infra-reconcile.sh");
    const deploy = read("scripts/deploy-compose.sh");
    const strict = read("scripts/probe-origin-strict.sh");
    expect(strict).toContain("PROBE_RETRIES");
    expect(infra).toContain("probe-origin-strict.sh");
    expect(infra).not.toContain("probe-origin.sh");
    expect(deploy).not.toContain("probe-origin.sh");
    expect(index).toContain('new command.local.Command("post-strict-public-readiness"');
    expect(index).toContain("dependsOn: [strictSsl]");
    expect(index).toContain("postStrictPublicReadiness");
    expect(index).toContain("triggers: infraTriggers");
  });

  it("fails closed when the bootstrap marker outlives deployment state", () => {
    const index = read("src/index.ts");
    const infra = read("scripts/infra-reconcile.sh");
    const marker = read("scripts/write-bootstrap-marker.ts");
    expect(index).toContain("readDeploymentPreflight");
    expect(infra).toContain("src/deployment-preflight.ts check");
    expect(read("src/deployment-preflight.ts")).toContain("restore/adopt state");
    expect(marker).toContain("writeBootstrapMarkerAtomically");
    expect(read("scripts/deploy-compose.sh")).toContain("write-bootstrap-marker.ts");
  });

  it("probes the rollback slot internally before changing the route", () => {
    const rollback = read("scripts/rollback-slot.sh");
    expect(rollback).toContain("internal_probe_url");
    expect(rollback).toContain("APP_PROBE_PATH");
    expect(rollback).toContain("PROBE_RETRIES");
    expect(rollback.indexOf("internal_probe_url")).toBeLessThan(rollback.indexOf("render-route.ts write"));
    expect(rollback).toContain("stop_service \"sub2api-${previous_slot}\"");
  });

  it("separates internal business probing from public probing", () => {
    const switchScript = read("scripts/switch-slot.sh");
    const publicProbe = read("scripts/probe-origin.sh");
    const strictProbe = read("scripts/probe-origin-strict.sh");
    expect(switchScript).toContain("APP_PROBE_PATH");
    expect(switchScript).toContain("127.0.0.1");
    expect(switchScript).toContain("stop_service \"sub2api-${inactive_slot}\"");
    expect(publicProbe).not.toContain("--resolve");
    expect(strictProbe).toContain("--resolve");
  });

  it("uses a shared Compose helper and atomically updates routes/state", () => {
    for (const path of ["scripts/deploy-compose.sh", "scripts/switch-slot.sh", "scripts/rollback-slot.sh"]) {
      expect(read(path)).toContain("compose-common.sh");
      expect(read(path)).toContain("${COMPOSE[@]}");
    }
    expect(read("scripts/switch-slot.sh")).toContain("write-deploy-state.ts");
    expect(read("scripts/rollback-slot.sh")).toContain("write-deploy-state.ts");
    expect(read("scripts/switch-slot.sh")).toContain("runtime/dynamic/active.yml.before-switch");
  });

  it("checks every deployment behavior directory and protects command outputs", () => {
    const index = read("src/index.ts");
    expect(index).toContain("scripts");
    expect(index).toContain("traefik");
    expect(index).toContain("additionalSecretOutputs");
    expect(index).toContain("acmeEmail");
    expect(index).toContain("drainSeconds");
  });

  it("renders the non-sensitive ACME email into the final static config", () => {
    const template = read("traefik/traefik.yml");
    const rendered = renderTraefikConfig(template, "ops@example.com");
    expect(rendered).toContain("email: ops@example.com");
    expect(rendered).not.toContain("${ACME_EMAIL}");
  });
});
