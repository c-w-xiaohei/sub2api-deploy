import { execFileSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

describe("deployment preflight CLI", () => {
  it("runs without executing the imported deployment-mode CLI", () => {
    const siteRoot = mkdtempSync(join(tmpdir(), "sub2api-preflight-cli-"));
    expect(() => execFileSync("npx", ["--no-install", "tsx", "src/deployment-preflight.ts", "check", join(siteRoot, "deploy-state.json"), join(siteRoot, "bootstrap.marker"), "docker", "docker"], {
      encoding: "utf8",
      stdio: "pipe",
    })).not.toThrow();
  });
});
