import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const root = process.cwd();

function section(document: string, heading: string) {
  const start = document.indexOf(`## ${heading}\n`);
  expect(start).toBeGreaterThanOrEqual(0);
  const next = document.indexOf("\n## ", start + heading.length + 4);
  return document.slice(start, next === -1 ? undefined : next);
}

const targetProductionExample = `config:
  sub2api-environment:environmentConfig: |
    version: 1
    reverseProxy:
      image: traefik@sha256:1111111111111111111111111111111111111111111111111111111111111111
      acmeEmail: ops@example.com
    servers:
      edge:
        sshAlias: production-edge
        addresses:
          public:
            ipv4: 203.0.113.10
    postgres:
      app:
        type: docker
        server: edge
    redis:
      app:
        type: docker
        server: edge
      cache:
        type: upstash
        region: us-east-1
    apps:
      api:
        hostname: api.example.com
        image: ghcr.io/example/sub2api@sha256:2222222222222222222222222222222222222222222222222222222222222222
        initialAdminEmail: admin@example.com
        readinessPath: /healthz
        drainTimeout: 30s
        servers:
          - edge
        postgres:
          name: app
          database: sub2api
        redis:
          name: app
          database: 0
        publicAccess:
          type: cloudflare
          servers:
            - edge
          cloudflare:
            mode: dns
            connectBy: publicAddress
    cloudflare:
      zoneId: replace-with-zone-id
  sub2api-environment:environmentSecrets:
    secure: replace-with-encrypted-environment-secrets
`;

describe("target Pulumi runtime", () => {
  it("discovers only the bundled Environment program and Host provider", () => {
    const manifest = readFileSync(join(root, "Pulumi.yaml"), "utf8");
    const wrapper = readFileSync(join(root, "scripts", "pulumi-cli-wrapper.sh"), "utf8");
    const shim = readFileSync(join(root, "scripts", "pulumi-go-shim.sh"), "utf8");

    expect(manifest).toMatch(/^name:\s*sub2api-environment\s*$/m);
    expect(manifest).toMatch(/^\s*binary:\s*\.\/bin\/pulumi-program\s*$/m);
    expect(manifest).not.toMatch(/infra\/|main:\s*\.\/infra|command\.local\.Command/i);
    expect(wrapper).toContain("PULUMI_BUNDLE_BIN");
    expect(wrapper).not.toMatch(/infra\/|go build|pulumi-command|command\.local\.Command/i);
    expect(shim).toContain("github.com/upstash/pulumi-upstash/sdk");
    expect(shim).not.toMatch(/pulumi-command|pulumi-sdk-neon/i);
    const go = join(root, "scripts", "pulumi-go-shim.sh");
    const environment = { ...process.env, PULUMI_BUNDLE_ROOT: root };
    expect(execFileSync("bash", [go, "list", "-m", "-json", "github.com/pulumi/pulumi-cloudflare/sdk/v6"], { encoding: "utf8", env: environment })).toContain('"Dir":"' + join(root, "scripts", "pulumi-plugins", "cloudflare") + '"');
    expect(execFileSync("bash", [go, "mod", "download", "-json", "github.com/upstash/pulumi-upstash/sdk"], { encoding: "utf8", env: environment })).toContain('"Dir":"' + join(root, "scripts", "pulumi-plugins", "upstash") + '"');
    const modules = execFileSync("bash", [go, "list", "-m", "-json", "github.com/pulumi/pulumi-command/sdk", "github.com/pulumi/pulumi-go-provider", "github.com/pkg/term", "golang.org/x/sys", "google.golang.org/protobuf", "gopkg.in/yaml.v3", "github.com/pulumi/pulumi-cloudflare/sdk/v6", "github.com/upstash/pulumi-upstash/sdk"], { encoding: "utf8", env: environment })
      .trim().split("\n").map((line) => JSON.parse(line) as { Path: string; Dir: string });
    expect(modules).toEqual([
      { Path: "github.com/pulumi/pulumi-cloudflare/sdk/v6", Dir: join(root, "scripts", "pulumi-plugins", "cloudflare") },
      { Path: "github.com/upstash/pulumi-upstash/sdk", Dir: join(root, "scripts", "pulumi-plugins", "upstash") },
    ].map((module) => expect.objectContaining(module)));
    expect(modules.map((module) => module.Path)).not.toContain("github.com/pulumi/pulumi-command/sdk");
    expect(modules.map((module) => module.Path)).not.toContain("github.com/kislerdm/pulumi-sdk-neon");
    expect(execFileSync("bash", [go, "mod", "download", "-json", "github.com/kislerdm/pulumi-sdk-neon"], { encoding: "utf8", env: environment })).toBe("");
    expect(() => execFileSync("bash", [go, "build"], { encoding: "utf8", env: environment })).toThrow();
  });

  it("defines the Environment-only public example contract", () => {
    const example = readFileSync(join(root, "Pulumi.production.example.yaml"), "utf8");

    // internal/environment tests own YAML parsing and validation semantics; this is the stable public example.
    expect(example).toBe(targetProductionExample);
    expect(example).toContain("type: upstash");
    expect(example).not.toMatch(/revisionKey|password|token|passphrase|singbox|neon|cross[- ]?host|microsocks|tunnel|connector|migration|adoption|sub2api-vps-deploy:/i);
  });

  it("documents the controller-side public CLI and capability boundary", () => {
    const readme = readFileSync(join(root, "README.md"), "utf8");

    const interfaceSection = section(readme, "Controller Interface");
    const boundarySection = section(readme, "Capability Boundary");
    expect(interfaceSection).toContain("controller-side `sub2api-deploy`");
    expect(interfaceSection).toContain("Environment configuration and secrets");
    expect(interfaceSection).toContain("Host server model");
    expect(interfaceSection).toContain("managed import command");
    expect(interfaceSection).toContain("managed Upstash");
    expect(interfaceSection).toContain("sub2api-deploy pulumi production import sub2api-host:index:Host host-edge edge");
    expect(boundarySection).toContain("Neon is blocked.");
    expect(boundarySection).toContain("Multi-Host app placement is supported.");
    expect(boundarySection).toContain("Cross-Host local Docker data and allowlist connectivity are blocked.");
    expect(boundarySection).toContain("MicroSocks is blocked.");
    expect(boundarySection).toContain("Tunnel support is blocked.");
    expect(boundarySection).toContain("Migration is not performed by this release.");
    expect(readme).not.toMatch(/one Host per Pulumi Stack|run on the VPS itself|local Docker Compose|sing-box|one-stack-per-VPS/i);
  });
});
