import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";

export const HOST_STATE_VERSION = 1;
export const SITE_ID_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;

export interface HostState {
  version: typeof HOST_STATE_VERSION;
  sites: string[];
  legacyCode2?: LegacyCode2Layout;
}

// This is deliberately internal state, not part of edge/sites configuration.
// It records only layout facts needed to identify the pre-multi-site code2 host.
export interface LegacyCode2Layout {
  runtimeRoot: "runtime";
  composeProject: "sub2api";
  routeLayout: "flat";
  handoverComplete: boolean;
}

export function parseSiteIds(siteIds: string[]): string[] {
  const seen = new Set<string>();
  for (const id of siteIds) {
    if (!SITE_ID_PATTERN.test(id)) {
      throw new Error(`invalid Site ID "${id}"; expected ${SITE_ID_PATTERN}`);
    }
    if (seen.has(id)) {
      throw new Error(`duplicate Site ID "${id}"`);
    }
    seen.add(id);
  }
  return [...siteIds];
}

export function parseSiteIdList(input: string): string[] {
  const ids = input.split(",").map((part) => part.trim());
  if (ids.some((id) => id.length === 0)) {
    throw new Error("configured Site IDs must be a comma-separated list of non-empty Site IDs");
  }
  return parseSiteIds(ids);
}

export function parseHostState(json: string): HostState {
  let parsed: unknown;
  try {
    parsed = JSON.parse(json);
  } catch {
    throw new Error("host state is not valid JSON; inspect and repair it before proceeding");
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error("host state is malformed: expected a JSON object");
  }
  const record = parsed as Record<string, unknown>;
  const allowedKeys = new Set(["version", "sites", "legacyCode2"]);
  if (Object.keys(record).some((key) => !allowedKeys.has(key))) {
    throw new Error("host state contains a credential or unsupported field; only non-secret Site registry metadata is allowed");
  }
  if (record.version !== HOST_STATE_VERSION) {
    throw new Error(`host state version ${JSON.stringify(record.version)} is unsupported; expected ${HOST_STATE_VERSION}`);
  }
  if (!Array.isArray(record.sites)) {
    throw new Error("host state is malformed: sites must be an array of Site ID strings");
  }
  if (record.sites.some((id) => typeof id !== "string")) {
    throw new Error("host state is malformed: sites must contain only Site ID strings");
  }
  const sites = parseSiteIds(record.sites as string[]);
  if (record.legacyCode2 === undefined) return { version: HOST_STATE_VERSION, sites };
  if (typeof record.legacyCode2 !== "object" || record.legacyCode2 === null || Array.isArray(record.legacyCode2)) {
    throw new Error("host state is malformed: legacyCode2 must be a layout object");
  }
  const legacy = record.legacyCode2 as Record<string, unknown>;
  const legacyKeys = new Set(["runtimeRoot", "composeProject", "routeLayout", "handoverComplete"]);
  if (Object.keys(legacy).some((key) => !legacyKeys.has(key))) {
    throw new Error("host state legacy mapping contains a credential or unsupported field");
  }
  if (!sites.includes("code2") || legacy.runtimeRoot !== "runtime" || legacy.composeProject !== "sub2api"
    || legacy.routeLayout !== "flat" || typeof legacy.handoverComplete !== "boolean") {
    throw new Error("host state legacy mapping must describe the code2 runtime/sub2api flat layout and handover status");
  }
  return { version: HOST_STATE_VERSION, sites, legacyCode2: { runtimeRoot: "runtime", composeProject: "sub2api", routeLayout: "flat", handoverComplete: legacy.handoverComplete } };
}

function deployStatePath(hostStatePath: string, siteID: string, legacy: boolean): string {
  const runtime = dirname(hostStatePath);
  return legacy ? join(runtime, "deploy-state.json") : join(runtime, "sites", siteID, "deploy-state.json");
}

export function checkHostPreflight(configuredSiteIds: string[], state: HostState | undefined, hostStatePath?: string, allowPendingPreview = false): string[] {
  const configured = parseSiteIds(configuredSiteIds);
  if (configured.length === 0) {
    throw new Error("host preflight requires at least one configured Site ID");
  }
  if (state) {
    const missing = state.sites.filter((id) => !configured.includes(id));
    if (missing.length > 0) {
      throw new Error(
        `host registry records Site ID(s) no longer configured: ${missing.join(", ")}\n`
        + "removing or renaming a Site key cannot silently stop routing or destroy provider resources;\n"
        + "run the explicit Site retirement workflow before removing the Site, or restore the Site key and re-run",
      );
    }
    if (hostStatePath) {
      for (const siteID of state.sites) {
        const isLegacy = state.legacyCode2 !== undefined && siteID === "code2";
        if (!existsSync(deployStatePath(hostStatePath, siteID, isLegacy))) {
          throw new Error(`host registry records Site ${siteID} but its deploy state is missing; restore it or use the explicit retirement workflow`);
        }
      }
    }
    if (state.legacyCode2 && !state.legacyCode2.handoverComplete) {
      if (configured.length !== 1 || configured[0] !== "code2") {
        throw new Error("pending legacy code2 adoption requires exactly one configured Site ID: code2");
      }
      if (!allowPendingPreview) throw new Error("legacy code2 layout is recorded; ordinary Pulumi operations are blocked until the explicitly approved maintenance-window handover completes");
    }
  } else if (hostStatePath) {
    const runtime = dirname(hostStatePath);
    if (existsSync(join(runtime, "deploy-state.json"))) {
      if (configured.length !== 1 || configured[0] !== "code2") {
        throw new Error("legacy runtime/deploy-state.json requires exactly one configured Site ID: code2; it cannot be inferred for another Site");
      }
      throw new Error("legacy code2 runtime detected; run the explicitly approved adopt-single-site-layout.sh maintenance-window procedure before ordinary Pulumi operations");
    }
  }
  return [...configured].sort();
}

export function readHostState(path: string): HostState | undefined {
  let content: string;
  try {
    content = readFileSync(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }
  return parseHostState(content);
}

if (process.argv[2] === "check") {
  const [, , , siteIds, path, pendingPreview] = process.argv;
  if (!siteIds || !path) throw new Error("usage: host-preflight.ts check CONFIGURED_SITE_IDS HOST_STATE_PATH");
  checkHostPreflight(parseSiteIdList(siteIds), readHostState(path), path, pendingPreview === "true");
}
