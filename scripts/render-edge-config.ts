import { chmodSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { renderTraefikConfig } from "./render-traefik-config.js";

function renderSingBoxConfig(template: string, serverName: string, target: string): string {
  if (!serverName || /[\r\n\0]/.test(serverName)) throw new Error("sing-box server name contains an unsupported control character");
  if (!target || /[\r\n\0]/.test(target)) throw new Error("sing-box target contains an unsupported control character");
  const rendered = template.replaceAll("${SING_BOX_SERVER_NAME}", serverName).replaceAll("${SING_BOX_TARGET}", target);
  if (/\$\{[A-Z0-9_]+\}/.test(rendered)) throw new Error("sing-box template was not fully rendered");
  return rendered;
}

function writeAtomically(path: string, content: string): void {
  const temporary = join(dirname(path), `.${process.pid}.tmp`);
  try {
    writeFileSync(temporary, content, { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, path);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

export function writeEdgeConfigAtomically(
  edgeRuntimeRoot: string,
  staticTemplatePath: string,
  singBoxTemplatePath: string,
  acmeEmail: string,
  singBoxServerName: string,
  singBoxTarget: string,
): void {
  if (!edgeRuntimeRoot) throw new Error("edge runtime root is required");
  const dynamicDirectory = join(edgeRuntimeRoot, "dynamic");
  mkdirSync(dynamicDirectory, { recursive: true });

  const traefikConfigPath = join(edgeRuntimeRoot, "traefik.yml");
  const traefikTemplate = readFileSync(staticTemplatePath, "utf8");
  writeAtomically(traefikConfigPath, renderTraefikConfig(traefikTemplate, acmeEmail));

  const singBoxConfigPath = join(dynamicDirectory, "00-sing-box.yml");
  const singBoxTemplate = readFileSync(singBoxTemplatePath, "utf8");
  writeAtomically(singBoxConfigPath, renderSingBoxConfig(singBoxTemplate, singBoxServerName, singBoxTarget));
}

if (import.meta.url === `file://${process.argv[1]}` && process.argv[2] === "write") {
  const [, , , edgeRuntimeRoot, staticTemplatePath, singBoxTemplatePath, acmeEmail, singBoxServerName, singBoxTarget] = process.argv;
  if (!edgeRuntimeRoot || !staticTemplatePath || !singBoxTemplatePath || !acmeEmail || !singBoxServerName || !singBoxTarget) {
    throw new Error("usage: render-edge-config.ts write EDGE_RUNTIME_ROOT STATIC_TEMPLATE SING_BOX_TEMPLATE ACME_EMAIL SING_BOX_SERVER_NAME SING_BOX_TARGET");
  }
  writeEdgeConfigAtomically(edgeRuntimeRoot, staticTemplatePath, singBoxTemplatePath, acmeEmail, singBoxServerName, singBoxTarget);
}
