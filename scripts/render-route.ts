import { chmodSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

export function renderActiveRoute(template: string, domain: string, slot: "blue" | "green"): string {
  const rendered = template.replaceAll("${DOMAIN}", domain).replaceAll("${SLOT}", slot);
  if (rendered.includes("${DOMAIN}") || rendered.includes("${SLOT}")) throw new Error("route template was not fully rendered");
  return rendered;
}

export function writeActiveRouteAtomically(templatePath: string, outputPath: string, domain: string, slot: "blue" | "green"): void {
  const directory = dirname(outputPath);
  mkdirSync(directory, { recursive: true });
  const temporary = join(directory, `.active.${process.pid}.tmp`);
  try {
    writeFileSync(temporary, renderActiveRoute(readFileSync(templatePath, "utf8"), domain, slot), { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, outputPath);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

if (process.argv[2] === "write") {
  const [, , , templatePath, outputPath, domain, slot] = process.argv;
  if (!templatePath || !outputPath || !domain || (slot !== "blue" && slot !== "green")) throw new Error("invalid route arguments");
  writeActiveRouteAtomically(templatePath, outputPath, domain, slot);
}
