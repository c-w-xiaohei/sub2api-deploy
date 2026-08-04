#!/usr/bin/env node

const { chmodSync, existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } = require("node:fs");
const { dirname, join } = require("node:path");

const [command, path, siteList, handover] = process.argv.slice(2);

function parseSiteIds(value) {
  const siteIds = value.split(",").map((siteId) => siteId.trim());
  if (siteIds.length === 0 || siteIds.some((siteId) => !/^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/.test(siteId) || siteId === "edge")) {
    throw new Error("site IDs must be non-empty lowercase identifiers and cannot be edge");
  }
  if (new Set(siteIds).size !== siteIds.length) throw new Error("site IDs must be unique");
  return siteIds.sort();
}

function existingLegacy(path) {
  if (!existsSync(path)) return undefined;
  const legacy = JSON.parse(readFileSync(path, "utf8")).legacyCode2;
  if (!legacy) return undefined;
  if (legacy.runtimeRoot !== "runtime" || legacy.composeProject !== "sub2api" || legacy.routeLayout !== "flat" || typeof legacy.handoverComplete !== "boolean") {
    throw new Error("existing host state has an invalid legacy code2 layout");
  }
  return legacy;
}

function writeState(path, siteIds, legacyCode2) {
  const directory = dirname(path);
  const temporary = join(directory, `.host-state.${process.pid}.tmp`);
  mkdirSync(directory, { recursive: true });
  try {
    writeFileSync(temporary, `${JSON.stringify(legacyCode2 ? { version: 1, sites: siteIds, legacyCode2 } : { version: 1, sites: siteIds })}\n`, { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, path);
  } finally {
    try { unlinkSync(temporary); } catch { /* renamed */ }
  }
}

if (!path || !siteList || (command !== "write" && command !== "write-legacy")) {
  throw new Error("usage: write-host-state.cjs write|write-legacy HOST_STATE_PATH SITE_IDS [pending|complete]");
}

const siteIds = parseSiteIds(siteList);
if (command === "write") {
  writeState(path, siteIds, existingLegacy(path));
} else {
  if (siteIds.length !== 1 || siteIds[0] !== "code2" || (handover !== "pending" && handover !== "complete")) {
    throw new Error("write-legacy requires only code2 and pending or complete handover state");
  }
  writeState(path, siteIds, { runtimeRoot: "runtime", composeProject: "sub2api", routeLayout: "flat", handoverComplete: handover === "complete" });
}
