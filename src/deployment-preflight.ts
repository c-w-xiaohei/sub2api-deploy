import { existsSync, readFileSync } from "node:fs";
import type { DeployState } from "../scripts/slot-state.js";
import { assertDeploymentModes } from "../scripts/deployment-mode.js";

export type DeploymentPreflightResult = "first-setup" | "existing";

export interface DeploymentPreflightInput {
  state: DeployState | undefined;
  markerExists: boolean;
}

export function validateDeploymentPreflight(
  input: DeploymentPreflightInput,
  postgresMode: string,
  redisMode: string,
  statePath: string,
): DeploymentPreflightResult {
  const { markerExists, state } = input;
  if (!state) {
    if (!markerExists) return "first-setup";
    throw new Error("bootstrap marker exists but deploy-state is missing; restore/adopt state before running pulumi up");
  }
  assertDeploymentModes(state, postgresMode, redisMode, statePath);
  return "existing";
}

export function readDeploymentPreflight(
  statePath: string,
  markerPath: string,
  postgresMode: string,
  redisMode: string,
): DeploymentPreflightResult {
  const state = existsSync(statePath) ? JSON.parse(readFileSync(statePath, "utf8")) as DeployState : undefined;
  const markerExists = existsSync(markerPath);
  return validateDeploymentPreflight({ state, markerExists }, postgresMode, redisMode, statePath);
}

if (process.argv[2] === "check") {
  const [, , , statePath, markerPath, postgresMode, redisMode] = process.argv;
  if (!statePath || !markerPath || !postgresMode || !redisMode) throw new Error("usage: deployment-preflight.ts check DEPLOY_STATE_PATH BOOTSTRAP_MARKER_PATH POSTGRES_MODE REDIS_MODE");
  readDeploymentPreflight(statePath, markerPath, postgresMode, redisMode);
}
