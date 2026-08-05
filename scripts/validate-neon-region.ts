const apiBase = "https://console.neon.tech/api/v2";

export type RegionEndpoint = { id?: string; host?: string; region_id?: string };
export type NeonRequest = (path: string, init?: RequestInit) => Promise<unknown>;

export function selectRegionEndpoint(endpoints: RegionEndpoint[], host: string): RegionEndpoint {
  const matches = endpoints.filter((endpoint) => endpoint.host === host && endpoint.id);
  if (matches.length !== 1) throw new Error("Neon endpoint host did not identify exactly one endpoint");
  if (!matches[0].region_id) throw new Error("Neon endpoint region_id is missing or ambiguous");
  return matches[0];
}

export async function validateManagedNeonRegion(projectId: string, endpointHost: string, configuredRegion: string, requestEndpoint: NeonRequest = request): Promise<void> {
  if (!configuredRegion.trim()) throw new Error("NEON_REGION is required");
  const response = await requestEndpoint(`/projects/${encodeURIComponent(projectId)}/endpoints`);
  const endpoints = (response as { endpoints?: RegionEndpoint[] }).endpoints;
  if (!Array.isArray(endpoints)) throw new Error("Neon endpoint response did not contain an endpoints list");
  const endpoint = selectRegionEndpoint(endpoints, endpointHost);
  if (endpoint.region_id !== configuredRegion) throw new Error(`Neon endpoint region ${String(endpoint.region_id)} does not match configured region ${configuredRegion}`);
}

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

async function request(path: string, init?: RequestInit): Promise<unknown> {
  const response = await fetch(`${apiBase}${path}`, {
    ...init,
    headers: { Authorization: `Bearer ${required("NEON_API_KEY")}`, "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });
  if (!response.ok) throw new Error(`Neon API request failed: HTTP ${response.status}`);
  return response.json();
}

if (import.meta.url === `file://${process.argv[1]}`) {
  await validateManagedNeonRegion(required("NEON_PROJECT_ID"), required("NEON_ENDPOINT_HOST"), required("NEON_REGION"));
  process.stdout.write("Neon project region validated\n");
}
