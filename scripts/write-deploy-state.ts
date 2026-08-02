import { chmodSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import type { DeployState } from "./slot-state.js";

const allowedKeys = new Set(["activeSlot", "previousSlot", "activeImage", "previousImage", "postgresMode", "redisMode"]);

export function writeDeployStateAtomically(path: string, state: DeployState): void {
  const keys = Object.keys(state);
  if (keys.some((key) => !allowedKeys.has(key))) {
    throw new Error("deployment state contains a credential or unsupported field");
  }
  if (state.activeSlot !== "blue" && state.activeSlot !== "green") {
    throw new Error("deployment state activeSlot is invalid");
  }
  for (const slot of [state.previousSlot]) {
    if (slot !== undefined && slot !== "blue" && slot !== "green") {
      throw new Error("deployment state previousSlot is invalid");
    }
  }
  if (state.postgresMode !== undefined && state.postgresMode !== "docker" && state.postgresMode !== "neon") {
    throw new Error("deployment state postgresMode is invalid");
  }
  if (state.redisMode !== undefined && state.redisMode !== "docker" && state.redisMode !== "upstash") {
    throw new Error("deployment state redisMode is invalid");
  }
  const directory = dirname(path);
  const temporary = join(directory, `.deploy-state.${process.pid}.tmp`);
  try {
    writeFileSync(temporary, `${JSON.stringify(state)}\n`, { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, path);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

if (process.argv[2] === "write") {
  const [, , , path, json] = process.argv;
  if (!path || !json) throw new Error("usage: write-deploy-state.ts write PATH JSON");
  writeDeployStateAtomically(path, JSON.parse(json) as DeployState);
}
