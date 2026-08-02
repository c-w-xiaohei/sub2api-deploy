import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
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
): DeploymentPreflightResult {
  if (!input.state && !input.markerExists) return "first-setup";
  if (input.markerExists && !input.state) {
    throw new Error("bootstrap marker exists but deploy-state is missing; restore/adopt state before running pulumi up");
  }
  assertDeploymentModes(input.state!, postgresMode, redisMode);
  return "existing";
}

export function readDeploymentPreflight(
  rootDirectory: string,
  postgresMode: string,
  redisMode: string,
): DeploymentPreflightResult {
  const statePath = join(rootDirectory, "runtime", "deploy-state.json");
  const markerPath = join(rootDirectory, "runtime", "bootstrap.marker");
  const state = existsSync(statePath) ? JSON.parse(readFileSync(statePath, "utf8")) as DeployState : undefined;
  return validateDeploymentPreflight({ state, markerExists: existsSync(markerPath) }, postgresMode, redisMode);
}

if (process.argv[2] === "check") {
  const [, , , rootDirectory, postgresMode, redisMode] = process.argv;
  if (!rootDirectory || !postgresMode || !redisMode) throw new Error("usage: deployment-preflight.ts check ROOT POSTGRES_MODE REDIS_MODE");
  readDeploymentPreflight(rootDirectory, postgresMode, redisMode);
}
