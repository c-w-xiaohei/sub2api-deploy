import { chmodSync, mkdirSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

export function writeBootstrapMarkerAtomically(path: string): void {
  const directory = dirname(path);
  mkdirSync(directory, { recursive: true });
  const temporary = join(directory, `.bootstrap-marker.${process.pid}.tmp`);
  try {
    writeFileSync(temporary, "sub2api-bootstrap-v1\n", { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, path);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

if (process.argv[2] === "write") {
  const path = process.argv[3];
  if (!path) throw new Error("usage: write-bootstrap-marker.ts write PATH");
  writeBootstrapMarkerAtomically(path);
}
