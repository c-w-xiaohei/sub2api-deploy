import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const script = new URL("../scripts/release-bundle.sh", import.meta.url).pathname;

function elf(machine: "amd64" | "arm64") {
  const binary = Buffer.alloc(64);
  binary.set([0x7f, 0x45, 0x4c, 0x46, 2, 1, 1], 0);
  binary.writeUInt16LE(2, 16);
  binary.writeUInt16LE(machine === "amd64" ? 62 : 183, 18);
  binary.writeUInt32LE(1, 20);
  return binary;
}

function sha256(contents: Buffer) {
  return createHash("sha256").update(contents).digest("hex");
}

function fixture() {
  const root = mkdtempSync(join(tmpdir(), "sub2api-release-bundle-"));
  const components = join(root, "components");
  const bundle = join(root, "bundle");
  const archive = join(root, "bundle.tar.gz");
  const fakeBin = join(root, "bin");
  const dockerLog = join(root, "docker-invoked");
  mkdirSync(components);
  mkdirSync(fakeBin);
  const fakeDocker = join(fakeBin, "docker");
  writeFileSync(fakeDocker, "#!/usr/bin/env bash\ntouch \"$FAKE_DOCKER_LOG\"\nexit 97\n");
  chmodSync(fakeDocker, 0o755);
  for (const name of ["sub2api-deploy", "pulumi-program", "pulumi-resource-sub2api-host"]) {
    const path = join(components, name);
    writeFileSync(path, `#!/usr/bin/env bash\n# ${name}\n`);
    chmodSync(path, 0o755);
  }
  writeFileSync(join(components, "sub2api-host-linux-amd64"), elf("amd64"), { mode: 0o755 });
  writeFileSync(join(components, "sub2api-host-linux-arm64"), elf("arm64"), { mode: 0o755 });

  return {
    root,
    bundle,
    assemble: () => execFileSync("bash", [script, "assemble", bundle, components, "test-release"], { stdio: "pipe" }),
    verify: () => execFileSync("bash", [script, "verify-host-artifacts", bundle], { stdio: "pipe" }),
    verifyArchive: () => {
      execFileSync("tar", ["-C", root, "-czf", archive, "bundle"], { stdio: "pipe" });
      execFileSync("bash", [script, "verify", archive], {
        stdio: "pipe",
        env: { ...process.env, FAKE_DOCKER_LOG: dockerLog, PATH: `${fakeBin}:${process.env.PATH}` },
      });
    },
    manifestPath: join(bundle, "artifacts", "sub2api-host", "manifest.json"),
    dockerLog,
  };
}

function withBundle(check: (bundle: ReturnType<typeof fixture>) => void) {
  const bundle = fixture();
  try {
    bundle.assemble();
    check(bundle);
  } finally {
    rmSync(bundle.root, { recursive: true, force: true });
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

function writeManifest(path: string, manifest: ReturnType<typeof readManifest>) {
  writeFileSync(path, `${JSON.stringify(manifest)}\n`);
}

function failureStderr(action: () => void) {
  try {
    action();
  } catch (error) {
    return (error as { stderr?: Buffer }).stderr?.toString() ?? "";
  }
  throw new Error("expected command to fail");
}

function workflowJobs(workflow: string) {
  return [...workflow.matchAll(/^  ([A-Za-z0-9_-]+):\n([\s\S]*?)(?=^  [A-Za-z0-9_-]+:\n|(?![\s\S]))/gm)]
    .map(([, name, body]) => ({ name, body }));
}

function needsJob(body: string, job: string) {
  const escaped = job.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(
    `^ {4}needs:\\s*(?:${escaped}\\s*$|\\[[^\\]]*\\b${escaped}\\b[^\\]]*\\])|^ {4}needs:\\s*\\n(?: {6}- [^\\n]+\\n)* {6}- ${escaped}\\s*$`,
    "m",
  ).test(body);
}

function hostEntry(architecture: "amd64" | "arm64") {
  return architecture === "amd64" ? "linux-amd64" : "linux-arm64";
}

function hostBinary(architecture: "amd64" | "arm64") {
  return `sub2api-host-linux-${architecture}`;
}

function producesHostArchitecture(body: string, architecture: "amd64" | "arm64") {
  const namedBinary = new RegExp(`sub2api-host[^\\n]*${architecture}|${architecture}[^\\n]*sub2api-host`);
  const matrixArchitecture = new RegExp(`matrix:[\\s\\S]*?arch:[\\s\\S]*?- ${architecture}`);
  return namedBinary.test(body) || (body.includes("sub2api-host") && matrixArchitecture.test(body));
}

function artifactSteps(body: string, verb: "upload" | "download") {
  const step = new RegExp(`uses:\\s*[^\\n]*${verb}-artifact[^\\n]*\\n([\\s\\S]*?)(?=^ {6}- name:|^ {4}- name:|$)`, "gmi");
  return [...body.matchAll(step)].map((match) => ({
    index: match.index!,
    selectors: [...match[1].matchAll(/^ {10}(?:name|pattern):\s*(.+?)(?:\s+#.*)?$/gm)].map(([, selector]) => selector.trim()),
    paths: [...match[1].matchAll(/^ {10}path:\s*(.+?)(?:\s+#.*)?$/gm)].map(([, path]) => path.trim()),
  }));
}

function selectorMatchesArtifact(selector: string, artifact: string) {
  const expression = /\$\{\{[^}]+\}\}/g;
  const pattern = selector.replace(expression, "*").replace(/[.+^${}()|[\]\\]/g, "\\$&").replaceAll("*", ".*");
  return new RegExp(`^${pattern}$`).test(artifact);
}

function hostBuildOutputs(body: string, architecture: "amd64" | "arm64") {
  const builds = [...body.matchAll(/\bgo\s+build\b[^\n]*?\s-o\s+["']?([^\s"']+)/gi)];
  return builds.flatMap((match) => {
    const output = match[1];
    const exact = output.toLowerCase().includes(hostBinary(architecture));
    const loopArchitectures = /for\s+(?:arch|goarch)\s+in\s+[^\n]*\bamd64\b[^\n]*\barm64\b[^\n]*;?\s*do/i.test(body);
    const variableArchitecture = /sub2api-host-linux-\$\{?(?:goarch|arch)\}?/i.test(output) && loopArchitectures;
    return exact || variableArchitecture ? [{ index: match.index!, output }] : [];
  });
}

function uploadCoversOutput(paths: string[], output: string) {
  const directory = output.slice(0, output.lastIndexOf("/") + 1);
  return paths.some((path) => path.includes(output) || (directory !== "" && path.includes(directory)));
}

function makesHostBinaryAvailable(job: string, source: string, sourceIndex: number, componentDirectory: string | undefined, architecture: "amd64" | "arm64", assembleAt: number) {
  if (componentDirectory === undefined) return false;
  const expected = `${componentDirectory}/${hostBinary(architecture)}`;
  const resolvedSource = source.replace(/\$\{?(?:goarch|arch)\}?/gi, architecture);
  if (resolvedSource === expected) return sourceIndex < assembleAt;
  const beforeAssembly = job.slice(0, assembleAt);
  const escapedSource = resolvedSource.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const escapedExpected = expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const transfer = new RegExp(`\\b(?:cp|install|mv)\\b[^\\n]*${escapedSource}[^\\n]*${escapedExpected}`, "g");
  return [...beforeAssembly.matchAll(transfer)].some((match) => sourceIndex < match.index! && match.index! < assembleAt);
}

function assemblyInvocations(body: string) {
  return [...body.matchAll(/release-bundle\.sh\s+assemble\s+[^\s]+\s+([^\s]+)/g)].map((match) => ({
    index: match.index!,
    componentDirectory: match[1].replace(/^(["'])(.*)\1$/, "$2"),
  }));
}

describe("release bundle verification fixtures", () => {
  it("supplies an existing per-Site app env file to Site Compose verification", () => {
    const contents = readFileSync(script, "utf8");
    expect(contents).toContain('app_env="$verification_dir/sites/$site_id.app.env"');
    expect(contents).toContain('SITE_APP_ENV_PATH=$app_env');
    expect(contents).toContain('> "$app_env"');
  });

  it("assembles the control plane and a strict, verifiable Host artifact manifest", () => {
    withBundle((bundle) => {
      for (const path of [
        "bin/sub2api-deploy",
        "bin/pulumi-program",
        "bin/pulumi-resource-sub2api-host",
        "artifacts/sub2api-host/sub2api-host-linux-amd64",
        "artifacts/sub2api-host/sub2api-host-linux-arm64",
      ]) {
        expect(existsSync(join(bundle.bundle, path))).toBe(true);
      }
      for (const path of ["bin/sub2api-deploy", "bin/pulumi-program", "bin/pulumi-resource-sub2api-host"]) {
        expect(statSync(join(bundle.bundle, path)).mode & 0o111).not.toBe(0);
      }

      const manifest = readManifest(bundle.manifestPath);
      expect(manifest.schemaVersion).toBe(1);
      expect(manifest.release).toBe("test-release");
      for (const architecture of ["amd64", "arm64"] as const) {
        const name = hostBinary(architecture);
        const contents = readFileSync(join(bundle.bundle, "artifacts", "sub2api-host", name));
        expect(manifest[hostEntry(architecture)]).toEqual({ path: name, sha256: sha256(contents), size: contents.length });
      }
      bundle.verify();
    });
  });

  it.each(["amd64", "arm64"] as const)("rejects %s Host content tampering while ELF metadata and size remain valid", (architecture) => {
    withBundle((bundle) => {
      const path = join(bundle.bundle, "artifacts", "sub2api-host", hostBinary(architecture));
      const contents = readFileSync(path);
      contents[contents.length - 1] ^= 0xff;
      writeFileSync(path, contents, { mode: 0o755 });
      expect(bundle.verify).toThrow();
    });
  });

  it.each(["amd64", "arm64"] as const)("rejects a false declared size for the %s Host binary", (architecture) => {
    withBundle((bundle) => {
      const manifest = readManifest(bundle.manifestPath);
      manifest[hostEntry(architecture)].size += 1;
      writeManifest(bundle.manifestPath, manifest);
      expect(bundle.verify).toThrow();
    });
  });

  it.each([
    ["an empty release", (manifest: ReturnType<typeof readManifest>) => { manifest.release = ""; }],
    ["a Host size above 64 MiB", (manifest: ReturnType<typeof readManifest>) => { manifest["linux-amd64"].size = 64 * 1024 * 1024 + 1; }],
  ])("rejects %s as an invalid Host manifest before reading its payload", (_description, mutate) => {
    withBundle((bundle) => {
      const manifest = readManifest(bundle.manifestPath);
      mutate(manifest);
      writeManifest(bundle.manifestPath, manifest);
      const stderr = failureStderr(bundle.verify);
      expect(stderr).toMatch(/Host artifact manifest has an invalid schema/i);
      expect(stderr).not.toMatch(/Host artifact size mismatch/i);
    });
  });

  it.each(["amd64", "arm64"] as const)("rejects a %s Host binary with the other ELF architecture even when its manifest entry is updated", (architecture) => {
    withBundle((bundle) => {
      const path = join(bundle.bundle, "artifacts", "sub2api-host", hostBinary(architecture));
      const contents = elf(architecture === "amd64" ? "arm64" : "amd64");
      writeFileSync(path, contents, { mode: 0o755 });
      const manifest = readManifest(bundle.manifestPath);
      manifest[hostEntry(architecture)].sha256 = sha256(contents);
      manifest[hostEntry(architecture)].size = contents.length;
      writeManifest(bundle.manifestPath, manifest);
      expect(bundle.verify).toThrow();
    });
  });

  it("rejects missing, malformed, obsolete, and incomplete Host artifact manifests", () => {
    withBundle((bundle) => {
      const valid = readManifest(bundle.manifestPath);
      rmSync(bundle.manifestPath);
      expect(bundle.verify).toThrow();
      writeFileSync(bundle.manifestPath, "not json\n");
      expect(bundle.verify).toThrow();

      for (const invalid of [
        { version: 1, artifacts: {} },
        { ...valid, unexpected: true },
        (() => { const manifest = { ...valid }; delete (manifest as Partial<typeof valid>).release; return manifest; })(),
        (() => { const manifest = { ...valid }; delete (manifest as Partial<typeof valid>)["linux-arm64"]; return manifest; })(),
        (() => {
          const { sha256, ...entry } = valid["linux-amd64"];
          return { ...valid, "linux-amd64": entry };
        })(),
      ]) {
        writeManifest(bundle.manifestPath, invalid as ReturnType<typeof readManifest>);
        expect(bundle.verify).toThrow();
      }
    });
  });

  it.each([
    ["control executable", "bin/sub2api-deploy"],
    ["manifest-listed Host payload", "artifacts/sub2api-host/sub2api-host-linux-amd64"],
    ["ordinary manifest-listed payload", "Pulumi.yaml"],
  ])("rejects a full archive containing a %s symlink", (_description, relativePath) => {
    withBundle((bundle) => {
      const path = join(bundle.bundle, relativePath);
      rmSync(path);
      symlinkSync("pulumi-program", path);
      expect(bundle.verifyArchive).toThrow();
      expect(existsSync(bundle.dockerLog)).toBe(false);
    });
  });

  it.each([
    ["control executable", "bin/sub2api-deploy"],
    ["manifest-listed Host payload", "artifacts/sub2api-host/sub2api-host-linux-amd64"],
    ["ordinary manifest-listed payload", "Pulumi.yaml"],
  ])("rejects a full archive containing a %s directory", (_description, relativePath) => {
    withBundle((bundle) => {
      const path = join(bundle.bundle, relativePath);
      rmSync(path);
      mkdirSync(path);
      expect(bundle.verifyArchive).toThrow();
      expect(existsSync(bundle.dockerLog)).toBe(false);
    });
  });

  it("reaches content verification for an intact archive", () => {
    withBundle((bundle) => {
      expect(bundle.verifyArchive).toThrow();
      expect(existsSync(bundle.dockerLog)).toBe(true);
    });
  });

  it("routes archive verification through Host artifact verification", () => {
    const contents = readFileSync(script, "utf8");
    const verifyContent = contents.slice(contents.indexOf("verify_content()"), contents.indexOf("verify()"));
    expect(verifyContent).toMatch(/^\s*(?!#)verify(?:_|-)host(?:_|-)artifacts\b[^\n]*"\$bundle_root"/m);
  });

  it("transfers both Host architectures into every release assembly job", () => {
    const workflow = readFileSync(new URL("../.github/workflows/release.yml", import.meta.url), "utf8");
    const jobs = workflowJobs(workflow);
    const assemblies = jobs.flatMap((job) => assemblyInvocations(job.body).map((invocation) => ({ ...job, ...invocation })));

    expect(assemblies).not.toEqual([]);
    for (const assembly of assemblies) {
      const assembleAt = assembly.index;
      const componentDirectory = assembly.componentDirectory;
      const buildsBothHere = (["amd64", "arm64"] as const).every((architecture) =>
        hostBuildOutputs(assembly.body, architecture).some(({ index, output }) =>
          makesHostBinaryAvailable(assembly.body, output, index, componentDirectory, architecture, assembleAt),
        ),
      );
      const downloads = artifactSteps(assembly.body, "download");
      const receivesBoth = (["amd64", "arm64"] as const).every((architecture) =>
        jobs.some((producer) => {
          const build = hostBuildOutputs(producer.body, architecture).find(({ index }) => index >= 0);
          if (!build || !needsJob(assembly.body, producer.name)) return false;
          return artifactSteps(producer.body, "upload").some((upload) =>
            upload.index > build.index
            && uploadCoversOutput(upload.paths, build.output)
            && upload.selectors.some((name) => downloads.some((download) =>
              download.index < assembleAt
              && download.selectors.some((selector) => selectorMatchesArtifact(selector, name))
              && download.paths.some((path) => makesHostBinaryAvailable(assembly.body, `${path}/${hostBinary(architecture)}`, download.index, componentDirectory, architecture, assembleAt)),
            )),
          );
        }),
      );
      expect(buildsBothHere || receivesBoth).toBe(true);
    }
  });
});
