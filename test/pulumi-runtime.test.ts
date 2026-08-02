import { execFileSync } from "node:child_process";
import { mkdtempSync, symlinkSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { describe, expect, it } from "vitest";

const root = process.cwd();
const goShim = join(root, "scripts", "pulumi-go-shim.sh");
const pulumiWrapper = join(root, "scripts", "pulumi-cli-wrapper.sh");

function runGoShim(...args: string[]): string {
  return execFileSync(goShim, args, {
    cwd: root,
    env: { ...process.env, PULUMI_BUNDLE_ROOT: root },
    encoding: "utf8",
  });
}

describe("Pulumi release Go compatibility shim", () => {
  it("reports a supported Go version and the bundled module file", () => {
    expect(runGoShim("version")).toMatch(/^go version go1\.25\.11 /);
    expect(runGoShim("list", "-m", "-f", "{{.GoMod}}").trim()).toBe(join(root, "go.mod"));
  });

  it("reports provider module metadata from the bundled files", () => {
    const output = runGoShim(
      "list",
      "-m",
      "-json",
      "github.com/pulumi/pulumi-cloudflare/sdk/v6",
      "github.com/pulumi/pulumi-command/sdk",
      "github.com/upstash/pulumi-upstash/sdk",
      "github.com/kislerdm/pulumi-sdk-neon",
    );
    const modules = output.trim().split("\n").map((line) => JSON.parse(line) as { Path: string; Dir: string });

    expect(modules).toEqual(expect.arrayContaining([
      expect.objectContaining({
        Path: "github.com/pulumi/pulumi-cloudflare/sdk/v6",
        Dir: join(root, "scripts", "pulumi-plugins", "cloudflare"),
      }),
      expect.objectContaining({
        Path: "github.com/pulumi/pulumi-command/sdk",
        Dir: join(root, "scripts", "pulumi-plugins", "command"),
      }),
      expect.objectContaining({
        Path: "github.com/upstash/pulumi-upstash/sdk",
        Dir: join(root, "scripts", "pulumi-plugins", "upstash"),
      }),
      expect.objectContaining({
        Path: "github.com/kislerdm/pulumi-sdk-neon",
        Dir: join(root, "scripts", "pulumi-plugins", "neon"),
      }),
    ]));
  });

  it("delegates to a Pulumi CLI outside the bundle", () => {
    const bundleDirectory = mkdtempSync(join(tmpdir(), "sub2api-pulumi-bundle-"));
    const cliDirectory = mkdtempSync(join(tmpdir(), "sub2api-pulumi-cli-"));
    symlinkSync("/bin/true", join(cliDirectory, "pulumi"));

    expect(() => execFileSync(pulumiWrapper, ["--version"], {
      cwd: root,
      env: {
        ...process.env,
        PATH: `${cliDirectory}:${process.env.PATH ?? ""}`,
        PULUMI_BUNDLE_BIN: bundleDirectory,
      },
      stdio: "pipe",
    })).not.toThrow();
  });
});
