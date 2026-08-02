import { readFileSync } from "node:fs";
import { writeDeployStateAtomically } from "./write-deploy-state.js";
import type { DeployState } from "./slot-state.js";

export type PersistedDeploymentModes = Pick<DeployState, "postgresMode" | "redisMode">;

function validPostgresMode(value: string): value is "docker" | "neon" {
  return value === "docker" || value === "neon";
}

function validRedisMode(value: string): value is "docker" | "upstash" {
  return value === "docker" || value === "upstash";
}

export function assertDeploymentModes(
  state: PersistedDeploymentModes,
  postgresMode: string,
  redisMode: string,
): void {
  if (!state.postgresMode || !state.redisMode) {
    throw new Error(
      "deployment state has no persisted postgresMode/redisMode; migration required: "
      + "verify the existing data placement, then run "
      + `npx --no-install tsx scripts/deployment-mode.ts adopt runtime/deploy-state.json ${postgresMode} ${redisMode}`,
    );
  }
  if (state.postgresMode !== postgresMode) {
    throw new Error(
      `postgresMode change from ${state.postgresMode} to ${postgresMode} requires migration; `
      + "ordinary pulumi up does not migrate PostgreSQL data",
    );
  }
  if (state.redisMode !== redisMode) {
    throw new Error(
      `redisMode change from ${state.redisMode} to ${redisMode} requires migration; `
      + "ordinary pulumi up does not migrate Redis data",
    );
  }
}

function readState(path: string): DeployState {
  return JSON.parse(readFileSync(path, "utf8")) as DeployState;
}

export function adoptDeploymentModes(path: string, postgresMode: string, redisMode: string): void {
  if (!validPostgresMode(postgresMode) || !validRedisMode(redisMode)) {
    throw new Error("adopt requires postgresMode docker|neon and redisMode docker|upstash");
  }
  const state = readState(path);
  if (state.postgresMode || state.redisMode) {
    throw new Error("deployment state already records data modes; use migration instead of adopt");
  }
  writeDeployStateAtomically(path, { ...state, postgresMode, redisMode });
}

const isMain = import.meta.url === `file://${process.argv[1]}`;
if (isMain && process.argv[2] === "check") {
  const [, , , path, postgresMode, redisMode] = process.argv;
  if (!path || !postgresMode || !redisMode) throw new Error("usage: deployment-mode.ts check PATH POSTGRES_MODE REDIS_MODE");
  try {
    assertDeploymentModes(readState(path), postgresMode, redisMode);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") process.exit(0);
    throw error;
  }
}

if (isMain && process.argv[2] === "adopt") {
  const [, , , path, postgresMode, redisMode] = process.argv;
  if (!path || !postgresMode || !redisMode) throw new Error("usage: deployment-mode.ts adopt PATH POSTGRES_MODE REDIS_MODE");
  adoptDeploymentModes(path, postgresMode, redisMode);
}
