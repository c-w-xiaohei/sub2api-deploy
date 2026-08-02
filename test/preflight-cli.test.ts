import { execFileSync } from "node:child_process";
import { describe, expect, it } from "vitest";

describe("deployment preflight CLI", () => {
  it("runs without executing the imported deployment-mode CLI", () => {
    expect(() => execFileSync("npx", ["--no-install", "tsx", "src/deployment-preflight.ts", "check", process.cwd(), "docker", "docker"], {
      encoding: "utf8",
      stdio: "pipe",
    })).not.toThrow();
  });
});
