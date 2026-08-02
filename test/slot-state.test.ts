import { mkdtempSync, readFileSync } from "node:fs";
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
    const path = join(mkdtempSync(join(tmpdir(), "sub2api-state-")), "deploy-state.json");
    writeDeployStateAtomically(path, { activeSlot: "blue", activeImage: "image@sha256:digest" });
    expect(JSON.parse(readFileSync(path, "utf8"))).toEqual({ activeSlot: "blue", activeImage: "image@sha256:digest" });
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
  });
});
