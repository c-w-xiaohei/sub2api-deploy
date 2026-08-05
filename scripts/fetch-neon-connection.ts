const apiBase = "https://console.neon.tech/api/v2";

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const projectId = required("NEON_PROJECT_ID");
const databaseName = process.env.NEON_DATABASE_NAME || "neondb";
const roleName = process.env.NEON_ROLE_NAME || "neondb_owner";
const query = new URLSearchParams({ database_name: databaseName, role_name: roleName });
const response = await fetch(`${apiBase}/projects/${encodeURIComponent(projectId)}/connection_uri?${query}`, {
  headers: { Authorization: `Bearer ${required("NEON_API_KEY")}`, "Content-Type": "application/json" },
});
if (!response.ok) throw new Error(`Neon connection URI request failed: HTTP ${response.status}`);
const body = await response.json() as { uri?: string };
if (!body.uri) throw new Error("Neon connection URI response omitted uri");
process.stdout.write(`${body.uri}\n`);
