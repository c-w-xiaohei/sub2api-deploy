import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { describe, expect, it } from "vitest";
import { checkHostPreflight, parseHostState, readHostState, type HostState } from "../scripts/host-preflight.js";
import { writeHostStateAtomically } from "../scripts/write-host-state.js";

function tempStatePath(): string {
  return join(mkdtempSync(join(tmpdir(), "sub2api-host-state-")), "host-state.json");
}

describe("host preflight", () => {
  it("permits the first deployment when host state is missing", () => {
    expect(checkHostPreflight(["code2"], undefined)).toEqual(["code2"]);
  });

  it("permits the first deployment when host state records no Sites", () => {
    const state: HostState = { version: 1, sites: [] };
    expect(checkHostPreflight(["code2", "code3"], state)).toEqual(["code2", "code3"]);
  });

  it("permits retaining existing Sites and adding new Sites", () => {
    const state: HostState = { version: 1, sites: ["code2"] };
    expect(checkHostPreflight(["code3", "code2"], state)).toEqual(["code2", "code3"]);
  });

  it("rejects removing a recorded Site with a retirement-required message", () => {
    const state: HostState = { version: 1, sites: ["code2", "code3"] };
    expect(() => checkHostPreflight(["code3"], state)).toThrow(/no longer configured: code2/);
    expect(() => checkHostPreflight(["code3"], state)).toThrow(/retirement/);
  });

  it("rejects renaming a recorded Site as an unretired removal", () => {
    const state: HostState = { version: 1, sites: ["code2"] };
    expect(() => checkHostPreflight(["code4"], state)).toThrow(/no longer configured: code2/);
  });

  it("rejects duplicate requested Site IDs", () => {
    expect(() => checkHostPreflight(["code2", "code2"], undefined)).toThrow(/duplicate Site ID "code2"/);
  });

  it("rejects invalid or empty requested Site IDs", () => {
    expect(() => checkHostPreflight(["Code2"], undefined)).toThrow(/invalid Site ID/);
    expect(() => checkHostPreflight([], undefined)).toThrow(/at least one configured Site ID/);
    expect(() => checkHostPreflight(["code2,"], undefined)).toThrow(/invalid Site ID/);
  });

  it("fails closed on malformed host state", () => {
    expect(() => parseHostState("{not json")).toThrow(/not valid JSON/);
    expect(() => parseHostState('{"version":2,"sites":[]}')).toThrow(/unsupported/);
    expect(() => parseHostState('{"version":1,"sites":"code2"}')).toThrow(/sites must be an array/);
    expect(() => parseHostState('{"version":1,"sites":[1]}')).toThrow(/only Site ID strings/);
    expect(() => parseHostState('{"version":1,"sites":["code2","code2"]}')).toThrow(/duplicate Site ID/);
    expect(() => parseHostState('{"version":1,"sites":["Code2"]}')).toThrow(/invalid Site ID/);
    expect(() => parseHostState("[]")).toThrow(/expected a JSON object/);
  });

  it("rejects host state carrying credentials or unsupported fields", () => {
    expect(() => parseHostState('{"version":1,"sites":["code2"],"databaseDsn":"postgres://secret"}'))
      .toThrow(/credential or unsupported field/);
  });

  it("treats a missing host state file as empty and a corrupt file as malformed", () => {
    expect(readHostState(tempStatePath())).toBeUndefined();
    const path = tempStatePath();
    writeFileSync(path, "garbage", "utf8");
    expect(() => readHostState(path)).toThrow(/not valid JSON/);
  });
});

describe("host state write", () => {
  it("writes host state atomically with mode 0600 and no temp residue", () => {
    const path = join(mkdtempSync(join(tmpdir(), "sub2api-host-write-")), "runtime", "host-state.json");
    writeHostStateAtomically(path, ["code3", "code2"]);
    const persisted = JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
    expect(persisted).toEqual({ version: 1, sites: ["code2", "code3"] });
    expect(Object.keys(persisted)).toEqual(["version", "sites"]);
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(readdirSync(dirname(path))).toEqual(["host-state.json"]);
  });

  it("round-trips through the strict parser without secrets", () => {
    const path = tempStatePath();
    writeHostStateAtomically(path, ["code2", "code3"]);
    expect(readHostState(path)).toEqual({ version: 1, sites: ["code2", "code3"] });
  });

  it("rejects writing duplicate or empty Site IDs", () => {
    const path = tempStatePath();
    expect(() => writeHostStateAtomically(path, ["code2", "code2"])).toThrow(/duplicate Site ID/);
    expect(() => writeHostStateAtomically(path, [])).toThrow(/at least one Site ID/);
  });

  it("runs the check CLI against missing host state", () => {
    expect(() => execFileSync("npx", ["--no-install", "tsx", "scripts/host-preflight.ts", "check", "code2,code3", tempStatePath()], {
      encoding: "utf8",
      stdio: "pipe",
    })).not.toThrow();
  });
});
