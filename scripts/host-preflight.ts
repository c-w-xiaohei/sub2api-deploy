import { readFileSync } from "node:fs";

export const HOST_STATE_VERSION = 1;
export const SITE_ID_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;

export interface HostState {
  version: typeof HOST_STATE_VERSION;
  sites: string[];
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
  const allowedKeys = new Set(["version", "sites"]);
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
  return { version: HOST_STATE_VERSION, sites: parseSiteIds(record.sites as string[]) };
}

export function checkHostPreflight(configuredSiteIds: string[], state: HostState | undefined): string[] {
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
  const [, , , siteIds, path] = process.argv;
  if (!siteIds || !path) throw new Error("usage: host-preflight.ts check CONFIGURED_SITE_IDS HOST_STATE_PATH");
  checkHostPreflight(parseSiteIdList(siteIds), readHostState(path));
}
