import { describe, expect, it } from "vitest";
import { validateDeploymentPreflight } from "../src/deployment-preflight.js";

describe("Pulumi deployment preflight", () => {
  it("allows only the initial state with neither state nor marker", () => {
    expect(validateDeploymentPreflight({ state: undefined, markerExists: false }, "docker", "docker")).toBe("first-setup");
  });

  it("rejects a marker without deploy state", () => {
    expect(() => validateDeploymentPreflight({ state: undefined, markerExists: true }, "docker", "docker"))
      .toThrow(/restore\/adopt state/);
  });

  it("rejects legacy state without persisted modes", () => {
    expect(() => validateDeploymentPreflight({ state: { activeSlot: "blue", activeImage: "image@sha256:old" }, markerExists: false }, "docker", "docker"))
      .toThrow(/adopt|migration required/);
  });

  it("rejects a persisted mode change before resource construction", () => {
    expect(() => validateDeploymentPreflight({
      state: { activeSlot: "blue", activeImage: "image@sha256:old", postgresMode: "docker", redisMode: "docker" },
      markerExists: true,
    }, "neon", "docker")).toThrow(/requires migration/);
  });

  it("allows a normal persisted state with its bootstrap marker", () => {
    expect(validateDeploymentPreflight({
      state: { activeSlot: "green", activeImage: "image@sha256:active", postgresMode: "docker", redisMode: "upstash" },
      markerExists: true,
    }, "docker", "upstash")).toBe("existing");
  });

  it("allows an existing state when the marker has not yet been written", () => {
    expect(validateDeploymentPreflight({
      state: { activeSlot: "blue", activeImage: "image@sha256:active", postgresMode: "docker", redisMode: "docker" },
      markerExists: false,
    }, "docker", "docker")).toBe("existing");
  });
});
