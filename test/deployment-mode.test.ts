import { describe, expect, it } from "vitest";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { assertDeploymentModes } from "../scripts/deployment-mode.js";
import { adoptDeploymentModes } from "../scripts/deployment-mode.js";

describe("setup-only data modes", () => {
  it("accepts the persisted modes when the requested modes are unchanged", () => {
    expect(() => assertDeploymentModes({ postgresMode: "docker", redisMode: "upstash" }, "docker", "upstash")).not.toThrow();
  });

  it("fails closed for a legacy state without persisted modes", () => {
    expect(() => assertDeploymentModes({}, "docker", "docker")).toThrow(/adopt|migration required/);
  });

  it("rejects a mode change before deployment side effects", () => {
    expect(() => assertDeploymentModes({ postgresMode: "docker", redisMode: "docker" }, "neon", "docker"))
      .toThrow(/requires migration/);
  });

  it("supports one-time adoption of a legacy state after placement is verified", () => {
    const directory = mkdtempSync(join(tmpdir(), "sub2api-adopt-"));
    const path = join(directory, "deploy-state.json");
    writeFileSync(path, JSON.stringify({ activeSlot: "blue", activeImage: "image@sha256:old" }));
    adoptDeploymentModes(path, "docker", "upstash");
    expect(JSON.parse(readFileSync(path, "utf8"))).toMatchObject({ postgresMode: "docker", redisMode: "upstash" });
  });
});
