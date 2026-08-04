import { chmodSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

const siteIdPattern = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;
const domainPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/;

export function renderSiteRoute(
  template: string,
  siteId: string,
  domain: string,
  slot: "blue" | "green",
  activeEdgeAlias: string,
): string {
  if (!siteIdPattern.test(siteId)) throw new Error("site ID is invalid");
  const normalizedDomain = domain.toLowerCase();
  if (normalizedDomain.length > 253 || !domainPattern.test(normalizedDomain)) throw new Error("domain is invalid");
  if (slot !== "blue" && slot !== "green") throw new Error("slot must be blue or green");
  const siteSlotAlias = `sub2api-${siteId}-${slot}`;
  const allowedAliases = [siteSlotAlias];
  if (siteId === "code2") allowedAliases.push(`sub2api-${slot}`);
  if (!allowedAliases.includes(activeEdgeAlias)) {
    throw new Error("active edge alias does not belong to this Site and slot");
  }
  const rendered = template
    .replaceAll("${SITE_ID}", siteId)
    .replaceAll("${DOMAIN}", normalizedDomain)
    .replaceAll("${SLOT}", slot)
    .replaceAll("${ACTIVE_EDGE_ALIAS}", activeEdgeAlias);
  if (/\$\{[A-Z0-9_]+\}/.test(rendered)) throw new Error("site route template was not fully rendered");
  return rendered;
}

export function writeSiteRouteAtomically(
  templatePath: string,
  destination: string,
  siteId: string,
  domain: string,
  slot: "blue" | "green",
  activeEdgeAlias: string,
): void {
  const directory = dirname(destination);
  mkdirSync(directory, { recursive: true });
  const temporary = join(directory, `.site-route.${process.pid}.tmp`);
  try {
    const template = readFileSync(templatePath, "utf8");
    const route = renderSiteRoute(template, siteId, domain, slot, activeEdgeAlias);
    writeFileSync(temporary, route, { mode: 0o600 });
    chmodSync(temporary, 0o600);
    renameSync(temporary, destination);
  } finally {
    try { unlinkSync(temporary); } catch { /* already renamed */ }
  }
}

if (import.meta.url === `file://${process.argv[1]}` && process.argv[2] === "write") {
  const [, , , templatePath, destination, siteId, domain, slot, activeEdgeAlias] = process.argv;
  if (!templatePath || !destination || !siteId || !domain || !activeEdgeAlias || (slot !== "blue" && slot !== "green")) {
    throw new Error("usage: render-site-route.ts write TEMPLATE DESTINATION SITE_ID DOMAIN SLOT ACTIVE_EDGE_ALIAS");
  }
  writeSiteRouteAtomically(templatePath, destination, siteId, domain, slot, activeEdgeAlias);
}
