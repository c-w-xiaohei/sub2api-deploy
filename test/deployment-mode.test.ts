import { describe, expect, it } from "vitest";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { assertDeploymentModes } from "../scripts/deployment-mode.js";
import { adoptDeploymentModes } from "../scripts/deployment-mode.js";

describe("setup-only data modes", () => {
  it("accepts the persisted modes when the requested modes are unchanged", () => {
    expect(() => assertDeploymentModes({ postgresMode: "docker", redisMode: "upstash" }, "docker", "upstash", "runtime/sites/code2/deploy-state.json")).not.toThrow();
  });

  it("fails closed for a legacy state without persisted modes", () => {
    expect(() => assertDeploymentModes({}, "docker", "docker", "runtime/sites/code2/deploy-state.json"))
      .toThrow(/adopt runtime\/sites\/code2\/deploy-state\.json docker docker/);
  });

  it("rejects a mode change before deployment side effects", () => {
    expect(() => assertDeploymentModes({ postgresMode: "docker", redisMode: "docker" }, "neon", "docker", "runtime/sites/code2/deploy-state.json"))
      .toThrow(/requires migration/);
  });

  it("supports one-time adoption of a legacy state after placement is verified", () => {
    const directory = mkdtempSync(join(tmpdir(), "sub2api-adopt-"));
    const path = join(directory, "deploy-state.json");
    writeFileSync(path, JSON.stringify({ activeSlot: "blue", activeImage: "image@sha256:old" }));
    adoptDeploymentModes(path, "docker", "upstash");
    expect(JSON.parse(readFileSync(path, "utf8"))).toMatchObject({ postgresMode: "docker", redisMode: "upstash" });
  });

  it("adopts legacy mode metadata atomically without changing unrelated state", () => {
    const directory = mkdtempSync(join(tmpdir(), "sub2api-adopt-metadata-"));
    const path = join(directory, "deploy-state.json");
    writeFileSync(path, JSON.stringify({ activeSlot: "blue", activeImage: "image@sha256:old" }));
    adoptDeploymentModes(path, "neon", "docker");
    expect(JSON.parse(readFileSync(path, "utf8"))).toEqual({ activeSlot: "blue", activeImage: "image@sha256:old", postgresMode: "neon", redisMode: "docker" });
  });
});
