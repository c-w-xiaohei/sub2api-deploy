import { chmodSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

export function renderDotenv(values: Record<string, unknown>): string {
  for (const [key, value] of Object.entries(values)) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      throw new Error(`invalid environment key: ${key}`);
    }
    if (typeof value !== "string") continue;
    if (value.includes("\0")) {
      throw new Error(`${key} contains NUL`);
    }
    if (/\r?\n/.test(value)) {
      throw new Error(`${key} contains newline; multiline secrets are unsupported`);
    }
  }
  return Object.entries(values)
    .filter(([, value]) => value !== undefined && value !== null)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, value]) => `${key}="${String(value).replace(/\\/g, "\\\\").replace(/"/g, '\\"') }"`)
    .join("\n") + "\n";
}

export function writeRuntimeEnvAtomically(path: string, values: Record<string, unknown>): void {
  const directory = dirname(path);
  mkdirSync(directory, { recursive: true });
  const temporary = join(directory, `.runtime-env.${process.pid}.tmp`);
  try {
    writeFileSync(temporary, renderDotenv(values), { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, path);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const [, , command, outputPath] = process.argv;
  if (command !== "write" || !outputPath) {
    throw new Error("usage: render-runtime-env.ts write PATH [--auto-setup=false] [--slot=SLOT] [--slot-data-dir=PATH]");
  }
  const input = readFileSync(0, "utf8");
  const values = JSON.parse(input) as Record<string, unknown>;
  if (process.argv.includes("--auto-setup=false")) values.AUTO_SETUP = "false";
  const slot = process.argv.find((argument) => argument.startsWith("--slot="))?.slice("--slot=".length);
  const slotDataDir = process.argv.find((argument) => argument.startsWith("--slot-data-dir="))?.slice("--slot-data-dir=".length);
  if (slot) values.SLOT = slot;
  if (slotDataDir) values.SLOT_DATA_DIR = slotDataDir;
  writeRuntimeEnvAtomically(outputPath, values);
}
