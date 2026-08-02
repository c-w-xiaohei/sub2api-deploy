import { chmodSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

export function renderTraefikConfig(template: string, acmeEmail: string): string {
  if (!acmeEmail || /[\r\n\0]/.test(acmeEmail)) throw new Error("acmeEmail contains an unsupported control character");
  const rendered = template.replaceAll("${ACME_EMAIL}", acmeEmail);
  if (rendered.includes("${ACME_EMAIL}")) throw new Error("ACME_EMAIL was not rendered");
  return rendered;
}

export function writeTraefikConfigAtomically(templatePath: string, outputPath: string, acmeEmail: string): void {
  const directory = dirname(outputPath);
  mkdirSync(directory, { recursive: true });
  const temporary = join(directory, `.traefik.${process.pid}.tmp`);
  try {
    writeFileSync(temporary, renderTraefikConfig(readFileSync(templatePath, "utf8"), acmeEmail), { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, outputPath);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

if (process.argv[2] === "write") {
  const [, , , templatePath, outputPath, acmeEmail] = process.argv;
  if (!templatePath || !outputPath || !acmeEmail) throw new Error("usage: render-traefik-config.ts write TEMPLATE OUTPUT EMAIL");
  writeTraefikConfigAtomically(templatePath, outputPath, acmeEmail);
}
