import { describe, expect, it } from "vitest";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { readDeploymentPreflight, validateDeploymentPreflight } from "../src/deployment-preflight.js";

describe("Pulumi deployment preflight", () => {
  it("allows only the initial state with neither state nor marker", () => {
    expect(validateDeploymentPreflight({ state: undefined, markerExists: false }, "docker", "docker", "runtime/sites/code2/deploy-state.json")).toBe("first-setup");
  });

  it("rejects a marker without deploy state", () => {
    expect(() => validateDeploymentPreflight({ state: undefined, markerExists: true }, "docker", "docker", "runtime/sites/code2/deploy-state.json"))
      .toThrow(/restore\/adopt state/);
  });

  it("rejects legacy state without persisted modes", () => {
    expect(() => validateDeploymentPreflight({ state: { activeSlot: "blue", activeImage: "image@sha256:old" }, markerExists: false }, "docker", "docker", "runtime/sites/code2/deploy-state.json"))
      .toThrow(/adopt|migration required/);
  });

  it("rejects a persisted mode change before resource construction", () => {
    expect(() => validateDeploymentPreflight({
      state: { activeSlot: "blue", activeImage: "image@sha256:old", postgresMode: "docker", redisMode: "docker" },
      markerExists: true,
    }, "neon", "docker", "runtime/sites/code2/deploy-state.json")).toThrow(/requires migration/);
  });

  it("allows a normal persisted state with its bootstrap marker", () => {
    expect(validateDeploymentPreflight({
      state: { activeSlot: "green", activeImage: "image@sha256:active", postgresMode: "docker", redisMode: "upstash" },
      markerExists: true,
    }, "docker", "upstash", "runtime/sites/code2/deploy-state.json")).toBe("existing");
  });

  it("allows an existing state when the marker has not yet been written", () => {
    expect(validateDeploymentPreflight({
      state: { activeSlot: "blue", activeImage: "image@sha256:active", postgresMode: "docker", redisMode: "docker" },
      markerExists: false,
    }, "docker", "docker", "runtime/sites/code2/deploy-state.json")).toBe("existing");
  });

  it("reads state and marker from explicit Site paths", () => {
    const hostRoot = mkdtempSync(join(tmpdir(), "sub2api-preflight-"));
    const code2State = join(hostRoot, "sites", "code2", "deploy-state.json");
    const code2Marker = join(hostRoot, "sites", "code2", "bootstrap.marker");
    const code3State = join(hostRoot, "sites", "code3", "deploy-state.json");
    const code3Marker = join(hostRoot, "sites", "code3", "bootstrap.marker");
    mkdirSync(join(hostRoot, "sites", "code2"), { recursive: true });
    mkdirSync(join(hostRoot, "sites", "code3"), { recursive: true });
    writeFileSync(code2State, JSON.stringify({ activeSlot: "blue", activeImage: "code2@sha256:active", postgresMode: "docker", redisMode: "docker" }));
    writeFileSync(code2Marker, "marker\n");
    writeFileSync(code3State, JSON.stringify({ activeSlot: "green", activeImage: "code3@sha256:active", postgresMode: "neon", redisMode: "upstash" }));
    writeFileSync(code3Marker, "marker\n");

    expect(readDeploymentPreflight(code2State, code2Marker, "docker", "docker")).toBe("existing");
    expect(readDeploymentPreflight(code3State, code3Marker, "neon", "upstash")).toBe("existing");
  });
});
