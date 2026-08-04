import { mkdtempSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { nextSlot, transitionState, type DeployState } from "../scripts/slot-state.js";
import { writeDeployStateAtomically } from "../scripts/write-deploy-state.js";
import { writeBootstrapMarkerAtomically } from "../scripts/write-bootstrap-marker.js";

describe("slot state", () => {
  it("selects blue for the first deployment", () => {
    expect(nextSlot(undefined)).toBe("blue");
  });

  it("records a blue to green transition without credentials", () => {
    const state: DeployState = { activeSlot: "blue", activeImage: "old@sha256:old" };
    expect(transitionState(state, "green", "new@sha256:new")).toEqual({
      activeSlot: "green",
      previousSlot: "blue",
      activeImage: "new@sha256:new",
      previousImage: "old@sha256:old",
    });
  });

  it("uses the previous slot and image for rollback", () => {
    const state: DeployState = {
      activeSlot: "green",
      previousSlot: "blue",
      activeImage: "new@sha256:new",
      previousImage: "old@sha256:old",
    };
    expect(transitionState(state, state.previousSlot!, state.previousImage!)).toEqual({
      activeSlot: "blue",
      previousSlot: "green",
      activeImage: "old@sha256:old",
      previousImage: "new@sha256:new",
    });
  });

  it("validates and atomically writes state without credentials", () => {
    const path = join(mkdtempSync(join(tmpdir(), "sub2api-state-")), "site", "deploy-state.json");
    writeDeployStateAtomically(path, { activeSlot: "blue", activeImage: "image@sha256:digest" });
    expect(JSON.parse(readFileSync(path, "utf8"))).toEqual({ activeSlot: "blue", activeImage: "image@sha256:digest" });
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(() => writeDeployStateAtomically(path, { activeSlot: "blue", activeImage: "password@sha256:bad", password: "secret" } as never)).toThrow(/credential/);
  });

  it("persists setup-only data modes in first deployment state", () => {
    const path = join(mkdtempSync(join(tmpdir(), "sub2api-state-modes-")), "deploy-state.json");
    writeDeployStateAtomically(path, {
      activeSlot: "blue",
      activeImage: "image@sha256:digest",
      postgresMode: "neon",
      redisMode: "upstash",
    });
    expect(JSON.parse(readFileSync(path, "utf8"))).toMatchObject({ postgresMode: "neon", redisMode: "upstash" });
  });

  it("writes a non-secret bootstrap marker atomically", () => {
    const path = join(mkdtempSync(join(tmpdir(), "sub2api-marker-")), "runtime", "bootstrap.marker");
    writeBootstrapMarkerAtomically(path);
    expect(readFileSync(path, "utf8")).toBe("sub2api-bootstrap-v1\n");
    expect(statSync(path).mode & 0o777).toBe(0o600);
  });

  it("keeps code2 and code3 state and markers isolated", () => {
    const hostRoot = mkdtempSync(join(tmpdir(), "sub2api-two-site-state-"));
    const code2Root = join(hostRoot, "sites", "code2");
    const code3Root = join(hostRoot, "sites", "code3");
    const code2State = join(code2Root, "deploy-state.json");
    const code3State = join(code3Root, "deploy-state.json");
    const code2Marker = join(code2Root, "bootstrap.marker");
    const code3Marker = join(code3Root, "bootstrap.marker");

    writeDeployStateAtomically(code2State, { activeSlot: "blue", activeImage: "code2@sha256:one" });
    writeDeployStateAtomically(code3State, { activeSlot: "green", activeImage: "code3@sha256:one" });
    writeBootstrapMarkerAtomically(code2Marker);
    writeBootstrapMarkerAtomically(code3Marker);
    const code3StateBefore = readFileSync(code3State, "utf8");
    const code3MarkerBefore = readFileSync(code3Marker, "utf8");

    writeDeployStateAtomically(code2State, { activeSlot: "green", activeImage: "code2@sha256:two" });
    writeBootstrapMarkerAtomically(code2Marker);

    expect(readFileSync(code2State, "utf8")).toContain("code2@sha256:two");
    expect(readFileSync(code3State, "utf8")).toBe(code3StateBefore);
    expect(readFileSync(code3Marker, "utf8")).toBe(code3MarkerBefore);
  });
});
