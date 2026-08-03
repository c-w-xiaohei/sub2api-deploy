import { chmodSync, existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { HOST_STATE_VERSION, parseHostState, parseSiteIdList, parseSiteIds, type HostState } from "./host-preflight.js";

export function writeHostStateAtomically(path: string, siteIds: string[], legacyCode2?: HostState["legacyCode2"]): void {
  const validated = parseSiteIds(siteIds);
  if (validated.length === 0) {
    throw new Error("host state requires at least one Site ID; removing the last Site requires the explicit retirement workflow");
  }
  const existing = legacyCode2 === undefined && existsSync(path) ? parseHostState(readFileSync(path, "utf8")) : undefined;
  const preservedLegacy = legacyCode2 ?? existing?.legacyCode2;
  const state: HostState = preservedLegacy
    ? parseHostState(JSON.stringify({ version: HOST_STATE_VERSION, sites: [...validated].sort(), legacyCode2: preservedLegacy }))
    : { version: HOST_STATE_VERSION, sites: [...validated].sort() };
  const directory = dirname(path);
  mkdirSync(directory, { recursive: true });
  const temporary = join(directory, `.host-state.${process.pid}.tmp`);
  try {
    writeFileSync(temporary, `${JSON.stringify(state)}\n`, { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, path);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

if (process.argv[2] === "write") {
  const [, , , path, siteIds] = process.argv;
  if (!path || !siteIds) throw new Error("usage: write-host-state.ts write HOST_STATE_PATH SITE_IDS");
  writeHostStateAtomically(path, parseSiteIdList(siteIds));
}

if (process.argv[2] === "write-legacy") {
  const [, , , path, siteIds, handover] = process.argv;
  if (!path || !siteIds || (handover !== "pending" && handover !== "complete")) {
    throw new Error("usage: write-host-state.ts write-legacy HOST_STATE_PATH SITE_IDS pending|complete");
  }
  writeHostStateAtomically(path, parseSiteIdList(siteIds), {
    runtimeRoot: "runtime", composeProject: "sub2api", routeLayout: "flat", handoverComplete: handover === "complete",
  });
}
