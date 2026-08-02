import { readFileSync } from "node:fs";

export function renderDotenv(values: Record<string, unknown>): string {
  for (const [key, value] of Object.entries(values)) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      throw new Error(`invalid environment key: ${key}`);
    }
    if (typeof value === "string" && value.includes("\0")) {
      throw new Error(`${key} contains NUL`);
    }
    if (typeof value === "string" && /\r?\n/.test(value)) {
      throw new Error(`${key} contains newline; multiline secrets are unsupported`);
    }
  }
  return Object.entries(values)
    .filter(([, value]) => value !== undefined && value !== null)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, value]) => `${key}="${String(value).replace(/\\/g, "\\\\").replace(/"/g, '\\"') }"`)
    .join("\n") + "\n";
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const input = readFileSync(0, "utf8");
  const values = JSON.parse(input) as Record<string, unknown>;
  if (process.argv.includes("--auto-setup=false")) values.AUTO_SETUP = "false";
  const slot = process.argv.find((argument) => argument.startsWith("--slot="))?.slice("--slot=".length);
  const slotDataDir = process.argv.find((argument) => argument.startsWith("--slot-data-dir="))?.slice("--slot-data-dir=".length);
  if (slot) values.SLOT = slot;
  if (slotDataDir) values.SLOT_DATA_DIR = slotDataDir;
  process.stdout.write(renderDotenv(values));
}
