import { describe, expect, it } from "vitest";
import { renderDotenv, writeRuntimeEnvAtomically } from "../scripts/render-runtime-env.js";
import * as runtimeEnv from "../scripts/render-runtime-env.js";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

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
    const path = join(mkdtempSync(join(tmpdir(), "sub2api-runtime-env-")), "runtime.env");
    const output = execFileSync("npx", ["--no-install", "tsx", "scripts/render-runtime-env.ts", "write", path, "--slot=green", "--slot-data-dir=green"], {
      input: JSON.stringify({ SLOT: "blue", SLOT_DATA_DIR: "blue" }),
      encoding: "utf8",
    });
    expect(output).toBe("");
    expect(readFileSync(path, "utf8")).toContain('SLOT="green"');
    expect(readFileSync(path, "utf8")).toContain('SLOT_DATA_DIR="green"');
  });

  it("requires an explicit destination instead of writing runtime values to stdout", () => {
    expect(() => execFileSync("npx", ["--no-install", "tsx", "scripts/render-runtime-env.ts", "--slot=green"], {
      input: JSON.stringify({ JWT_SECRET: "secret" }),
      encoding: "utf8",
      stdio: "pipe",
    })).toThrow(/usage/);
  });

  it("writes isolated Site runtime env files and reads only the requested path", () => {
    const hostRoot = mkdtempSync(join(tmpdir(), "sub2api-runtime-"));
    const code2Path = join(hostRoot, "sites", "code2", "runtime.env");
    const code3Path = join(hostRoot, "sites", "code3", "runtime.env");
    writeRuntimeEnvAtomically(code2Path, { JWT_SECRET: "code2-secret", POSTGRES_MODE: "docker" });
    writeRuntimeEnvAtomically(code3Path, { JWT_SECRET: "code3-secret", POSTGRES_MODE: "neon" });

    expect(execFileSync("node", ["scripts/read-runtime-env.cjs", code2Path, "JWT_SECRET"], { encoding: "utf8" })).toBe("code2-secret");
    expect(execFileSync("node", ["scripts/read-runtime-env.cjs", code3Path, "JWT_SECRET"], { encoding: "utf8" })).toBe("code3-secret");
    expect(() => execFileSync("node", ["scripts/read-runtime-env.cjs", code2Path, "BAD-KEY"], { encoding: "utf8" })).toThrow();
    expect(readFileSync(code2Path, "utf8")).not.toContain("code3-secret");
    expect(statSync(code2Path).mode & 0o777).toBe(0o600);
  });

  it("writes an isolated app.env atomically with stable escaping and mode 0600", () => {
    const hostRoot = mkdtempSync(join(tmpdir(), "sub2api-app-env-"));
    const path = join(hostRoot, "sites", "code2", "app.env");
    (runtimeEnv as Record<string, unknown>).writeAppEnvAtomically(path, { SMTP_HOST: "mail.example.com", SECRET: "quoted\\value\"" });
    expect(readFileSync(path, "utf8")).toBe('SECRET="quoted\\\\value\\\""\nSMTP_HOST="mail.example.com"\n');
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(() => (runtimeEnv as Record<string, unknown>).writeAppEnvAtomically(path, { "BAD-KEY": "value" })).toThrow(/invalid environment key/);
    expect(() => (runtimeEnv as Record<string, unknown>).writeAppEnvAtomically(path, { SECRET: "line1\nline2" })).toThrow(/newline/);
    expect(() => (runtimeEnv as Record<string, unknown>).writeAppEnvAtomically(path, { SECRET: 42 })).toThrow(/must be a string/);
  });

  it("renders app.env literals safely for Compose interpolation", () => {
    const renderAppDotenv = (runtimeEnv as Record<string, unknown>).renderAppDotenv as (values: Record<string, string>) => string;
    expect(renderAppDotenv({ LITERAL: 'price $5 ${HOME} "quoted" \\path' })).toBe('LITERAL="price $$5 $${HOME} \\\"quoted\\\" \\\\path"\n');
  });
});
