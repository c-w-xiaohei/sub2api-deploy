import { describe, expect, it } from "vitest";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { assertLegacyAppEnvMatches } from "../scripts/verify-legacy-app-env.js";

function legacyFile(contents: string): string {
  const path = join(mkdtempSync(join(tmpdir(), "sub2api-legacy-env-")), "oidc.env");
  writeFileSync(path, contents, { mode: 0o600 });
  return path;
}

describe("legacy application environment handover", () => {
  it("requires an explicit appEnv and strictly matches legacy dotenv values", () => {
    const path = legacyFile('OIDC_CLIENT_ID="client id"\nOIDC_CLIENT_SECRET=secret\n');
    expect(() => assertLegacyAppEnvMatches(path, JSON.stringify({ OIDC_CLIENT_ID: "client id", OIDC_CLIENT_SECRET: "secret" }), "true")).not.toThrow();
    expect(() => assertLegacyAppEnvMatches(path, JSON.stringify({ OIDC_CLIENT_ID: "wrong", OIDC_CLIENT_SECRET: "secret" }), "true")).toThrow(/does not match/);
    expect(() => assertLegacyAppEnvMatches(path, JSON.stringify({ OIDC_CLIENT_ID: "client id", OIDC_CLIENT_SECRET: "secret" }), "false")).toThrow(/explicitly configured/);
    expect(readFileSync(path, "utf8")).toContain("OIDC_CLIENT_SECRET=secret");
  });

  it("rejects unsafe dotenv syntax without shell evaluation", () => {
    for (const contents of ["KEY=$(touch /tmp/should-not-exist)\n", "KEY=`touch /tmp/should-not-exist`\n", "KEY=value;touch /tmp/should-not-exist\n", "KEY=value&touch /tmp/should-not-exist\n", "KEY=value|touch /tmp/should-not-exist\n", "KEY=value>file\n", "KEY=value<file\n", "KEY=value\nKEY=other\n", "KEY=bad value\n", "KEY=bad\r\n"]) {
      expect(() => assertLegacyAppEnvMatches(legacyFile(contents), JSON.stringify({ KEY: "value" }), "true")).toThrow(/legacy oidc.env/);
    }
  });
});
