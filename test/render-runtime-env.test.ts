import { describe, expect, it } from "vitest";
import { renderDotenv } from "../scripts/render-runtime-env.js";
import { execFileSync } from "node:child_process";

describe("renderDotenv", () => {
  it("serializes dotenv values and does not log secret values", () => {
    const payload = { DATABASE_PASSWORD: "p@ss\\word'quoted", JWT_SECRET: "jwt-secret", SLOT: "blue" };
    const rendered = renderDotenv(payload);
    expect(rendered).toContain('DATABASE_PASSWORD="p@ss\\\\word\'quoted"');
    expect(rendered).toContain('JWT_SECRET="jwt-secret"');
    expect(rendered).toContain('SLOT="blue"');
    expect(rendered).not.toContain("runtime secret");
  });

  it("rejects newline and NUL values instead of changing the Compose env file", () => {
    expect(() => renderDotenv({ SECRET: "line1\nline2" })).toThrow(/newline/);
    expect(() => renderDotenv({ SECRET: "bad\0value" })).toThrow(/NUL/);
  });

  it("allows infra reconciliation to preserve the active slot data directory", () => {
    const output = execFileSync("npx", ["--no-install", "tsx", "scripts/render-runtime-env.ts", "--slot=green", "--slot-data-dir=green"], {
      input: JSON.stringify({ SLOT: "blue", SLOT_DATA_DIR: "blue" }),
      encoding: "utf8",
    });
    expect(output).toContain('SLOT="green"');
    expect(output).toContain('SLOT_DATA_DIR="green"');
  });
});
