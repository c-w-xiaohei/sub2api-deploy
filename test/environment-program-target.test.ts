import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const pulumiYaml = new URL("../Pulumi.yaml", import.meta.url);
const buildScript = new URL("../scripts/build-pulumi-release.sh", import.meta.url);
const workflow = new URL("../.github/workflows/ci.yml", import.meta.url);

describe("environment Pulumi program target", () => {
  it("targets the environment program without an infra fallback", () => {
    const manifest = readFileSync(pulumiYaml, "utf8");
    const build = readFileSync(buildScript, "utf8");
    const ci = readFileSync(workflow, "utf8");

    expect(manifest).toMatch(/^name:\s*sub2api-environment\s*$/m);
    expect(manifest).toMatch(/^\s*binary:\s*\.\/bin\/pulumi-program\s*$/m);
    expect(build).toMatch(/\bgo\s+build\b[^\n]*\s\.\/cmd\/sub2api-environment\s*$/m);
    expect(build).not.toMatch(/\.\/infra(?:\s|$)/);
    expect(ci).toContain("npx vitest run test/environment-program-target.test.ts test/controller-ci.test.ts test/release-ci.test.ts");
  });
});
