import { readFileSync } from "node:fs";

function parseLegacyDotenv(contents: string): Record<string, string> {
  const values: Record<string, string> = {};
  for (const line of contents.split(/\n/)) {
    if (line === "") continue;
    if (line.includes("\r") || line.startsWith("#")) throw new Error("legacy oidc.env is malformed");
    const match = /^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/.exec(line);
    if (!match || match[2].includes("\0") || match[2].includes("\n")) throw new Error("legacy oidc.env is malformed or unsafe");
    if (match[1] in values) throw new Error("legacy oidc.env contains duplicate keys");
    let value = match[2];
    if (value.startsWith('"') && value.endsWith('"')) value = value.slice(1, -1).replace(/\\([\\"])/g, "$1");
    else if (/[\s#$`;'"\\&|<>]/.test(value)) throw new Error("legacy oidc.env is malformed or unsafe");
    values[match[1]] = value;
  }
  return values;
}

export function assertLegacyAppEnvMatches(path: string, appEnvJson: string, configured: string): void {
  if (configured !== "true") throw new Error("legacy oidc.env requires an explicitly configured siteSecrets.appEnv");
  const appEnv = JSON.parse(appEnvJson) as Record<string, unknown>;
  if (!appEnv || Array.isArray(appEnv) || Object.values(appEnv).some((value) => typeof value !== "string")) throw new Error("appEnv must be a string object");
  const legacy = parseLegacyDotenv(readFileSync(path, "utf8"));
  for (const [key, value] of Object.entries(legacy)) if (appEnv[key] !== value) throw new Error(`siteSecrets.appEnv does not match legacy oidc.env key ${key}`);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const [, , path, configured] = process.argv;
  if (!path || !configured) throw new Error("usage: verify-legacy-app-env.ts PATH CONFIGURED");
  let input = "";
  process.stdin.setEncoding("utf8");
  process.stdin.on("data", (chunk) => { input += chunk; });
  process.stdin.on("end", () => assertLegacyAppEnvMatches(path, input, configured));
}
