import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

describe("operator documentation contract", () => {
  it("documents local Pulumi, all data combinations, and destroy limitations", () => {
    const readme = readFileSync(new URL("../README.md", import.meta.url), "utf8");
    expect(readme).toContain("pulumi up");
    expect(readme).toContain("runs on the VPS");
    expect(readme).toContain("Docker Engine");
    expect(readme).toContain("docker/docker");
    expect(readme).toContain("neon/upstash");
    expect(readme).toContain("pulumi destroy");
    expect(readme).toContain("SSE");
    expect(readme).toContain("worker");
    expect(readme).toContain("Basic setup");
    expect(readme).toContain("Advanced configuration");
    expect(readme).toContain("does not migrate data");
  });
});
