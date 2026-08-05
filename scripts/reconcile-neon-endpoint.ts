const apiBase = "https://console.neon.tech/api/v2";

export type Endpoint = { id?: string; host?: string };
type EndpointState = Endpoint & Record<string, unknown>;
export type NeonRequest = (path: string, init?: RequestInit) => Promise<unknown>;
export type Sleep = (milliseconds: number) => Promise<void>;

export function selectEndpoint(endpoints: Endpoint[], host: string): Endpoint {
  const matches = endpoints.filter((endpoint) => endpoint.host === host && endpoint.id);
  if (matches.length !== 1) throw new Error("Neon endpoint host did not identify exactly one endpoint");
  return matches[0];
}

export function desiredEndpointSettings(minCU: number, maxCU: number, suspendTimeoutSeconds: number) {
  return {
    autoscaling_limit_min_cu: minCU,
    autoscaling_limit_max_cu: maxCU,
    suspend_timeout_seconds: suspendTimeoutSeconds,
  };
}

function endpointSettingsMismatch(actual: EndpointState, settings: ReturnType<typeof desiredEndpointSettings>): string | null {
  for (const [key, expected] of Object.entries(settings)) {
    if (actual[key] !== expected) return `setting ${key} = ${String(actual[key])}, want ${String(expected)}`;
  }
  return null;
}

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

async function request(path: string, init?: RequestInit): Promise<unknown> {
  const response = await fetch(`${apiBase}${path}`, {
    ...init,
    headers: {
      Authorization: `Bearer ${required("NEON_API_KEY")}`,
      "Content-Type": "application/json",
      ...(init?.headers ?? {}),
    },
  });
  if (!response.ok) {
    throw Object.assign(new Error(`Neon API request failed: HTTP ${response.status}`), { status: response.status });
  }
  return response.status === 204 ? null : response.json();
}

export async function reconcileEndpointSettings(
  projectId: string,
  endpointHost: string,
  settings: ReturnType<typeof desiredEndpointSettings>,
  requestEndpoint: NeonRequest = request,
  sleep: Sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds)),
): Promise<void> {
  const endpointResponse = await requestEndpoint(`/projects/${encodeURIComponent(projectId)}/endpoints`);
  const endpoints = (endpointResponse as { endpoints?: Endpoint[] }).endpoints ?? [];
  const endpoint = selectEndpoint(endpoints, endpointHost);
  const endpointId = endpoint.id!;

  const endpointPath = `/projects/${encodeURIComponent(projectId)}/endpoints/${encodeURIComponent(endpointId)}`;
  const current = await requestEndpoint(endpointPath) as { endpoint?: EndpointState };
  if (endpointSettingsMismatch(current.endpoint ?? {}, settings) === null) return;

  await requestEndpoint(endpointPath, {
    method: "PATCH",
    body: JSON.stringify({ endpoint: settings }),
  });

  let lastMismatch = "verification did not return the requested settings";
  for (let attempt = 0; attempt < 5; attempt++) {
    try {
      const updated = await requestEndpoint(endpointPath) as {
        endpoint?: EndpointState;
      };
      lastMismatch = endpointSettingsMismatch(updated.endpoint ?? {}, settings) ?? "";
      if (!lastMismatch) return;
    } catch (error) {
      const status = (error as { status?: unknown }).status;
      if (typeof status === "number" && status !== 408 && status !== 429 && (status < 500 || status > 599)) {
        throw error;
      }
      lastMismatch = error instanceof Error ? error.message : "Neon API verification request failed";
    }
    if (attempt < 4) await sleep(2000);
  }
  throw new Error(`Neon endpoint settings did not converge: ${lastMismatch}`);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const projectId = required("NEON_PROJECT_ID");
  const endpointHost = required("NEON_ENDPOINT_HOST");
  const minCU = Number(required("NEON_AUTOSCALING_MIN_CU"));
  const maxCU = Number(required("NEON_AUTOSCALING_MAX_CU"));
  const suspendTimeoutSeconds = Number(required("NEON_SUSPEND_TIMEOUT_SECONDS"));
  if (!Number.isFinite(minCU) || !Number.isFinite(maxCU) || !Number.isInteger(suspendTimeoutSeconds)) {
    throw new Error("Neon endpoint settings are invalid");
  }

  const settings = desiredEndpointSettings(minCU, maxCU, suspendTimeoutSeconds);
  await reconcileEndpointSettings(projectId, endpointHost, settings);
  process.stdout.write("Neon endpoint settings reconciled\n");
}
