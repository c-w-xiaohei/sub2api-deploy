import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmodSync, lstatSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const root = process.cwd();
const script = join(root, "scripts", "release-bundle.sh");
const manifest = join(root, "scripts", "release-bundle-files.txt");

const targetInventory = [
  "Pulumi.production.example.yaml",
  "Pulumi.yaml",
  "README.md",
  "artifacts/sub2api-host/manifest.json",
  "artifacts/sub2api-host/sub2api-host-linux-amd64",
  "artifacts/sub2api-host/sub2api-host-linux-arm64",
  "bin/go",
  "bin/pulumi",
  "bin/pulumi-program",
  "bin/pulumi-resource-sub2api-host",
  "bin/sub2api-deploy",
  "go.mod",
  "scripts/pulumi-plugins/cloudflare/pulumi-plugin.json",
  "scripts/pulumi-plugins/upstash/pulumi-plugin.json",
].sort();

function elf(machine: "amd64" | "arm64") {
  const binary = Buffer.alloc(64);
  binary.set([0x7f, 0x45, 0x4c, 0x46, 2, 1, 1], 0);
  binary.writeUInt16LE(2, 16);
  binary.writeUInt16LE(machine === "amd64" ? 62 : 183, 18);
  return binary;
}

function sha256(contents: Buffer) {
  return createHash("sha256").update(contents).digest("hex");
}

function files(directory: string, base = directory): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    return entry.isDirectory() ? files(path, base) : [relative(base, path)];
  }).sort();
}

function fixture() {
  const temp = mkdtempSync(join(tmpdir(), "sub2api-release-bundle-"));
  const components = join(temp, "components");
  const bundle = join(temp, "bundle");
  const archive = join(temp, "bundle.tar.gz");
  const safeBin = join(temp, "safe-bin");
  mkdirSync(components);
  mkdirSync(safeBin);
  const docker = join(safeBin, "docker");
  writeFileSync(docker, "#!/bin/sh\nexit 97\n");
  chmodSync(docker, 0o755);
  for (const name of ["sub2api-deploy", "pulumi-program", "pulumi-resource-sub2api-host"]) {
    const path = join(components, name);
    writeFileSync(path, `#!/usr/bin/env bash\nprintf '%s\\n' '${name}'\n`);
    chmodSync(path, 0o755);
  }
  writeFileSync(join(components, "sub2api-host-linux-amd64"), elf("amd64"), { mode: 0o755 });
  writeFileSync(join(components, "sub2api-host-linux-arm64"), elf("arm64"), { mode: 0o755 });

  return {
    temp,
    bundle,
    assemble: () => execFileSync("bash", [script, "assemble", bundle, components, "test-release"], { stdio: "pipe" }),
    archive: () => execFileSync("tar", ["-C", temp, "-czf", archive, "bundle"], { stdio: "pipe" }),
    verify: () => execFileSync("/usr/bin/bash", [script, "verify", archive], {
      stdio: "pipe",
      env: { ...process.env, PATH: `${safeBin}:/usr/bin:/bin` },
    }),
    hostManifest: join(bundle, "artifacts", "sub2api-host", "manifest.json"),
    archivePath: archive,
  };
}

function withBundle(check: (bundle: ReturnType<typeof fixture>) => void) {
  const bundle = fixture();
  try {
    bundle.assemble();
    check(bundle);
  } finally {
    rmSync(bundle.temp, { recursive: true, force: true });
  }
}

function readManifest(path: string) {
  return JSON.parse(readFileSync(path, "utf8")) as {
    schemaVersion: number;
    release: string;
    "linux-amd64": { path: string; sha256: string; size: number };
    "linux-arm64": { path: string; sha256: string; size: number };
  };
}

function writeManifest(path: string, value: ReturnType<typeof readManifest>) {
  writeFileSync(path, `${JSON.stringify(value)}\n`);
}

function loadProviderReleaseBundle(providerPath: string, architecture: "amd64" | "arm64") {
  const root = join(dirname(dirname(providerPath)), "artifacts", "sub2api-host");
  const manifestPath = join(root, "manifest.json");
  expect(lstatSync(providerPath).isSymbolicLink()).toBe(false);
  expect(statSync(providerPath).isFile()).toBe(true);
  expect(statSync(providerPath).mode & 0o111).not.toBe(0);
  expect(lstatSync(root).isSymbolicLink()).toBe(false);
  expect(lstatSync(manifestPath).isSymbolicLink()).toBe(false);

  const value = JSON.parse(readFileSync(manifestPath, "utf8")) as Record<string, unknown>;
  expect(Object.keys(value).sort()).toEqual(["linux-amd64", "linux-arm64", "release", "schemaVersion"]);
  expect(value.schemaVersion).toBe(1);
  expect(typeof value.release).toBe("string");
  expect(value.release).not.toBe("");
  const entry = value[`linux-${architecture}`] as Record<string, unknown>;
  expect(Object.keys(entry).sort()).toEqual(["path", "sha256", "size"]);
  expect(entry.path).toBe(`sub2api-host-linux-${architecture}`);
  expect(basename(entry.path as string)).toBe(entry.path);
  expect(typeof entry.sha256).toBe("string");
  expect(entry.sha256).toMatch(/^[0-9a-f]{64}$/);
  expect(Number.isSafeInteger(entry.size)).toBe(true);
  expect(entry.size).toBeGreaterThanOrEqual(0);

  const artifactPath = join(root, entry.path as string);
  expect(lstatSync(artifactPath).isSymbolicLink()).toBe(false);
  const artifact = readFileSync(artifactPath);
  expect(statSync(artifactPath).isFile()).toBe(true);
  expect(statSync(artifactPath).mode & 0o111).not.toBe(0);
  expect(artifact.length).toBe(entry.size);
  expect(sha256(artifact)).toBe(entry.sha256);
  expect(artifact.subarray(0, 6)).toEqual(Buffer.from([0x7f, 0x45, 0x4c, 0x46, 2, 1]));
  expect(artifact.readUInt16LE(18)).toBe(architecture === "amd64" ? 62 : 183);
  return { root, release: value.release as string, artifactPath };
}

describe("target release bundle", () => {
  it("ships only target controller files and runtime metadata", () => {
    const activePaths = readFileSync(manifest, "utf8").split("\n").filter((line) => line && !line.startsWith("#"));
    expect(activePaths).toEqual([
      "Pulumi.yaml",
      "Pulumi.production.example.yaml",
      "README.md",
      "go.mod",
      "scripts/pulumi-plugins/cloudflare/pulumi-plugin.json",
      "scripts/pulumi-plugins/upstash/pulumi-plugin.json",
    ]);
    expect(activePaths).not.toContain(expect.stringMatching(/^(infra|compose|traefik)\//));
    expect(activePaths).not.toContain(expect.stringMatching(/command|neon|sing-box|migration|adopt/i));
  });

  it("assembles the exact target inventory with executable, regular Host artifacts", () => {
    withBundle((bundle) => {
      expect(files(bundle.bundle)).toEqual(targetInventory);
      for (const path of targetInventory) {
        expect(statSync(join(bundle.bundle, path)).isSymbolicLink()).toBe(false);
      }
      for (const path of [
        "bin/go", "bin/pulumi", "bin/sub2api-deploy", "bin/pulumi-program", "bin/pulumi-resource-sub2api-host",
        "artifacts/sub2api-host/sub2api-host-linux-amd64", "artifacts/sub2api-host/sub2api-host-linux-arm64",
      ]) {
        expect(statSync(join(bundle.bundle, path)).mode & 0o111).not.toBe(0);
      }
    });
  });

  it("writes a strict dual-architecture Host manifest with exact hashes, sizes, and ELF machines", () => {
    withBundle((bundle) => {
      const contents = readManifest(bundle.hostManifest);
      expect(contents.schemaVersion).toBe(1);
      expect(contents.release).toBe("test-release");
      for (const architecture of ["amd64", "arm64"] as const) {
        const name = `sub2api-host-linux-${architecture}`;
        const artifact = readFileSync(join(bundle.bundle, "artifacts", "sub2api-host", name));
        expect(contents[`linux-${architecture}`]).toEqual({ path: name, sha256: sha256(artifact), size: artifact.length });
        expect(artifact.subarray(0, 4)).toEqual(Buffer.from([0x7f, 0x45, 0x4c, 0x46]));
        expect(artifact.readUInt16LE(18)).toBe(architecture === "amd64" ? 62 : 183);
      }
    });
  });

  it("TestTargetSupportedReleaseBundleIsConsumableByProviderCreate", () => {
    withBundle((bundle) => {
      bundle.archive();
      const consumer = mkdtempSync(join(tmpdir(), "sub2api-target-consumer-"));
      try {
        execFileSync("tar", ["-xzf", bundle.archivePath, "-C", consumer], { stdio: "pipe" });
        const extracted = join(consumer, "bundle");
        const safeBin = join(consumer, "safe-bin");
        mkdirSync(safeBin);
        const bash = join(safeBin, "bash");
        writeFileSync(bash, "#!/bin/sh\nexec /usr/bin/bash \"$@\"\n");
        chmodSync(bash, 0o755);
        const dirname = join(safeBin, "dirname");
        writeFileSync(dirname, "#!/bin/sh\nexec /usr/bin/dirname \"$@\"\n");
        chmodSync(dirname, 0o755);
        const fakePulumi = join(safeBin, "pulumi");
        writeFileSync(fakePulumi, "#!/usr/bin/env bash\n[[ \"${PATH%%:*}\" == \"$EXPECTED_BUNDLE_BIN\" ]]\nprintf 'safe-pulumi\\n'\n");
        chmodSync(fakePulumi, 0o755);
        const environment = { PATH: `${join(extracted, "bin")}:${safeBin}`, EXPECTED_BUNDLE_BIN: join(extracted, "bin") };

        // This is an isolated release consumer plus the lower-level Create locator contract,
        // not a claim that a provider RPC Create has been executed.
        expect(execFileSync("/usr/bin/bash", [join(extracted, "bin", "pulumi")], { cwd: consumer, env: environment, encoding: "utf8" }).trim()).toBe("safe-pulumi");
        const provider = join(extracted, "bin", "pulumi-resource-sub2api-host");
        expect(loadProviderReleaseBundle(provider, "amd64")).toEqual({
          root: join(extracted, "artifacts", "sub2api-host"),
          release: "test-release",
          artifactPath: join(extracted, "artifacts", "sub2api-host", "sub2api-host-linux-amd64"),
        });
        for (const [name, version, server] of [
          ["cloudflare", "6.18.0", undefined],
          ["upstash", undefined, "github://api.github.com/upstash/pulumi-upstash"],
        ]) {
          const metadataPath = join(extracted, "scripts", "pulumi-plugins", name, "pulumi-plugin.json");
          expect(lstatSync(metadataPath).isSymbolicLink()).toBe(false);
          expect(JSON.parse(readFileSync(metadataPath, "utf8"))).toEqual({ resource: true, name, ...(version ? { version } : {}), ...(server ? { server } : {}) });
        }
        const locator = readFileSync(join(root, "internal", "hostprovider", "provider.go"), "utf8");
        expect(locator).toContain("func loadReleaseBundle(providerPath string)");
        expect(locator).toContain('filepath.Join(filepath.Dir(filepath.Dir(providerPath)), "artifacts", "sub2api-host")');
        expect(locator).toContain("artifact.LoadBundle(root)");
      } finally {
        rmSync(consumer, { recursive: true, force: true });
      }
    });
  });

  it("verifies the target archive shape without Docker or legacy Compose validation", () => {
    withBundle((bundle) => {
      bundle.archive();
      expect(bundle.verify).not.toThrow();
    });
  });

  it("rejects a manifest source symlink", () => {
    const sourceRoot = mkdtempSync(join(tmpdir(), "sub2api-release-source-"));
    const sourceScripts = join(sourceRoot, "scripts");
    const source = join(sourceRoot, "README.md");
    const external = join(sourceRoot, "external.md");
    mkdirSync(sourceScripts);
    writeFileSync(join(sourceScripts, "release-bundle.sh"), readFileSync(script), { mode: 0o755 });
    writeFileSync(join(sourceScripts, "release-bundle-files.txt"), "README.md\n");
    writeFileSync(external, "public metadata\n");
    symlinkSync(external, source);
    try {
      expect(() => execFileSync("bash", [join(sourceScripts, "release-bundle.sh"), "assemble", join(sourceRoot, "bundle"), join(sourceRoot, "components"), "test-release"], { stdio: "pipe" })).toThrow();
    } finally {
      rmSync(sourceRoot, { recursive: true, force: true });
    }
  });

  it("rejects a manifest source with a symlink ancestor", () => {
    const sourceRoot = mkdtempSync(join(tmpdir(), "sub2api-release-source-"));
    const sourceScripts = join(sourceRoot, "scripts");
    const external = join(sourceRoot, "external");
    mkdirSync(sourceScripts);
    mkdirSync(external);
    mkdirSync(join(sourceRoot, "components"));
    writeFileSync(join(sourceScripts, "release-bundle.sh"), readFileSync(script), { mode: 0o755 });
    writeFileSync(join(sourceScripts, "release-bundle-files.txt"), "metadata/README.md\n");
    writeFileSync(join(external, "README.md"), "public metadata\n");
    symlinkSync(external, join(sourceRoot, "metadata"));
    try {
      expect(() => execFileSync("bash", [join(sourceScripts, "release-bundle.sh"), "assemble", join(sourceRoot, "bundle"), join(sourceRoot, "components"), "test-release"], { encoding: "utf8", stdio: "pipe" })).toThrow(/symlink/);
    } finally {
      rmSync(sourceRoot, { recursive: true, force: true });
    }
  });

  it("rejects a component source symlink", () => {
    const bundle = fixture();
    const component = join(bundle.temp, "components", "pulumi-program");
    const external = join(mkdtempSync(join(tmpdir(), "sub2api-external-component-")), "pulumi-program");
    writeFileSync(external, readFileSync(component), { mode: 0o755 });
    rmSync(component);
    symlinkSync(external, component);
    try {
      expect(bundle.assemble).toThrow();
    } finally {
      rmSync(bundle.temp, { recursive: true, force: true });
      rmSync(dirname(external), { recursive: true, force: true });
    }
  });

  it("rejects a component directory symlink", () => {
    const bundle = fixture();
    const componentLink = join(bundle.temp, "components-link");
    symlinkSync(join(bundle.temp, "components"), componentLink);
    try {
      expect(() => execFileSync("bash", [script, "assemble", bundle.bundle, componentLink, "test-release"], { encoding: "utf8", stdio: "pipe" })).toThrow(/component directory.*symlink/);
    } finally {
      rmSync(bundle.temp, { recursive: true, force: true });
    }
  });

  it.each(["amd64", "arm64"] as const)("rejects %s Host content tampering while ELF metadata and size remain valid", (architecture) => {
    withBundle((bundle) => {
      const path = join(bundle.bundle, "artifacts", "sub2api-host", `sub2api-host-linux-${architecture}`);
      const contents = readFileSync(path);
      contents[contents.length - 1] ^= 0xff;
      writeFileSync(path, contents, { mode: 0o755 });
      expect(() => execFileSync("bash", [script, "verify-host-artifacts", bundle.bundle], { stdio: "pipe" })).toThrow();
    });
  });

  it("rejects a malformed Host manifest before reading its payload", () => {
    withBundle((bundle) => {
      const contents = readManifest(bundle.hostManifest);
      contents.release = "";
      writeManifest(bundle.hostManifest, contents);
      expect(() => execFileSync("bash", [script, "verify-host-artifacts", bundle.bundle], { stdio: "pipe" })).toThrow();
    });
  });

  it.each(["bin/sub2api-deploy", "artifacts/sub2api-host/sub2api-host-linux-amd64", "Pulumi.yaml"])("rejects an archive containing a symlink at %s", (path) => {
    withBundle((bundle) => {
      rmSync(join(bundle.bundle, path));
      symlinkSync("pulumi-program", join(bundle.bundle, path));
      bundle.archive();
      expect(bundle.verify).toThrow();
    });
  });

  it.each(["bin/sub2api-deploy", "artifacts/sub2api-host/sub2api-host-linux-amd64", "Pulumi.yaml"])("rejects an archive containing a directory at %s", (path) => {
    withBundle((bundle) => {
      rmSync(join(bundle.bundle, path));
      mkdirSync(join(bundle.bundle, path));
      bundle.archive();
      expect(bundle.verify).toThrow();
    });
  });
});
