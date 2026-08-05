import { chmod, mkdir, open, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { dirname } from "node:path";

const apiBase = "https://console.neon.tech/api/v2";

export type ManagedProject = {
  id: string;
  name: string;
  region_id: string;
  default_endpoint_host: string;
};
export type NeonRequest = (path: string, init?: RequestInit) => Promise<unknown>;
type NeonError = Error & { status?: number };

export function managedProjectState(id: string, name: string, region_id: string, default_endpoint_host: string): ManagedProject {
  return { id, name, region_id, default_endpoint_host };
}

function object(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) throw new Error("Neon project response was not an object");
  return value as Record<string, unknown>;
}

export function parseManagedProjectResponse(value: unknown): ManagedProject {
  const envelope = object(value);
  const project = object(envelope.project ?? value);
  const endpoints = envelope.endpoints;
  if (!Array.isArray(endpoints)) throw new Error("Neon project response omitted authoritative endpoints");
  const candidates = endpoints.filter((endpoint) => {
    const item = object(endpoint);
    return typeof item.id === "string" && typeof item.host === "string" && (item.type === undefined || item.type === "read_write");
  });
  const readWrite = candidates.filter((endpoint) => object(endpoint).type === "read_write");
  const matches = readWrite.length === 1 ? readWrite : candidates.length === 1 ? candidates : [];
  if (matches.length !== 1) throw new Error("Neon project response did not identify exactly one default endpoint");
  const endpoint = object(matches[0]);
  if (typeof project.id !== "string" || typeof project.name !== "string" || typeof project.region_id !== "string") {
    throw new Error("Neon project response omitted required project metadata");
  }
  return { id: project.id, name: project.name, region_id: project.region_id, default_endpoint_host: endpoint.host as string };
}

function parseProjectOnly(value: unknown): Pick<ManagedProject, "id" | "name" | "region_id"> {
  const project = object(object(value).project ?? value);
  if (typeof project.id !== "string" || typeof project.name !== "string" || typeof project.region_id !== "string") throw new Error("Neon project detail omitted required project metadata");
  return { id: project.id, name: project.name, region_id: project.region_id };
}

async function detailProject(id: string, requestEndpoint: NeonRequest): Promise<ManagedProject> {
  const project = parseProjectOnly(await requestEndpoint(`/projects/${encodeURIComponent(id)}`));
  const endpoints = await requestEndpoint(`/projects/${encodeURIComponent(id)}/endpoints`);
  return parseManagedProjectResponse({ project, ...object(endpoints) });
}

async function listProjects(requestEndpoint: NeonRequest): Promise<Array<{ id: string; name: string; region_id?: string }>> {
  const projects: Array<{ id: string; name: string; region_id?: string }> = [];
  let path = "/projects";
  for (;;) {
    const response = object(await requestEndpoint(path));
    if (!Array.isArray(response.projects)) throw new Error("Neon project response did not contain a projects list");
    for (const item of response.projects) {
      const project = object(item);
      if (typeof project.id !== "string" || typeof project.name !== "string") throw new Error("Neon project list entry omitted required metadata");
      if (project.region_id !== undefined && typeof project.region_id !== "string") throw new Error("Neon project list entry has invalid region metadata");
      projects.push({ id: project.id, name: project.name, ...(project.region_id === undefined ? {} : { region_id: project.region_id }) });
    }
    const cursor = object(response.pagination ?? {}).cursor;
    if (typeof cursor !== "string" || cursor === "") return projects;
    path = `/projects?cursor=${encodeURIComponent(cursor)}`;
  }
}

async function findByName(name: string, regionId: string, requestEndpoint: NeonRequest): Promise<ManagedProject | undefined> {
  const matches = (await listProjects(requestEndpoint)).filter((project) => project.name === name);
  if (matches.length > 1) throw new Error(`Neon project name ${name} is ambiguous`);
  if (matches.length === 0) return undefined;
  if (matches[0].region_id !== undefined && matches[0].region_id !== regionId) {
    throw new Error(`Neon project region ${matches[0].region_id} does not match configured region ${regionId}`);
  }
  const metadata = parseProjectOnly(await requestEndpoint(`/projects/${encodeURIComponent(matches[0].id)}`));
  if (metadata.name !== name) throw new Error("Neon project name does not match the deterministic project");
  if (metadata.region_id !== regionId) throw new Error(`Neon project region ${metadata.region_id} does not match configured region ${regionId}`);
  const endpoints = await requestEndpoint(`/projects/${encodeURIComponent(matches[0].id)}/endpoints`);
  const project = parseManagedProjectResponse({ project: metadata, ...object(endpoints) });
  validateManagedProject(project, name, regionId);
  return project;
}

function validateManagedProject(project: ManagedProject, name: string, regionId: string): void {
  if (project.name !== name) throw new Error("Neon project name does not match the deterministic project");
  if (project.region_id !== regionId) throw new Error(`Neon project region ${project.region_id} does not match configured region ${regionId}`);
  if (!project.default_endpoint_host) throw new Error("Neon project detail omitted authoritative endpoint host");
}

export async function createOrFindManagedNeonProject(name: string, regionId: string, requestEndpoint: NeonRequest): Promise<ManagedProject> {
  const existing = await findByName(name, regionId, requestEndpoint);
  if (existing) return existing;
  try {
    const created = await requestEndpoint("/projects", { method: "POST", body: JSON.stringify({ project: { name, region_id: regionId } }) });
    const projectMetadata = parseProjectOnly(created);
    const project = await detailProject(projectMetadata.id, requestEndpoint);
    validateManagedProject(project, name, regionId);
    return project;
  } catch (error) {
    if ((error as NeonError).status !== 409) throw error;
    const recovered = await findByName(name, regionId, requestEndpoint);
    if (!recovered) throw new Error(`Neon project creation conflicted but project ${name} was not found`);
    return recovered;
  }
}

export async function validatePersistedManagedProject(project: ManagedProject, name: string, regionId: string, requestEndpoint: NeonRequest): Promise<ManagedProject> {
  try {
    const current = await detailProject(project.id, requestEndpoint);
    validateManagedProject(current, name, regionId);
    if (current.default_endpoint_host !== project.default_endpoint_host) throw new Error("persisted Neon endpoint host is stale");
    return current;
	} catch (error) {
		const status = (error as NeonError).status;
		if (status !== 404 && !(error instanceof Error && /stale|does not match|omitted/.test(error.message))) throw error;
		const recovered = await findByName(name, regionId, requestEndpoint);
    if (!recovered) throw new Error("persisted Neon project could not be revalidated and deterministic lookup found no project");
    return recovered;
  }
}

export async function withLock<T>(stateFile: string, action: () => Promise<T>): Promise<T> {
	const lockFile = `${stateFile}.lock`;
	await mkdir(dirname(stateFile), { recursive: true });
  let handle: Awaited<ReturnType<typeof open>> | undefined;
  let acquired = false;
  let staleLockFile: string | undefined;
  try {
    try {
      handle = await open(lockFile, "wx", 0o600);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      let lock: { pid?: unknown };
      try {
        lock = JSON.parse(await readFile(lockFile, "utf8")) as { pid?: unknown };
      } catch {
        throw new Error(`Neon project state is locked: ${lockFile}`);
      }
      if (typeof lock.pid !== "number" || !Number.isInteger(lock.pid) || lock.pid <= 0) {
        throw new Error(`Neon project state is locked: ${lockFile}`);
      }
      try {
        process.kill(lock.pid, 0);
      } catch (probeError) {
        if ((probeError as NodeJS.ErrnoException).code !== "ESRCH") throw new Error(`Neon project state is locked: ${lockFile}`);
        staleLockFile = `${lockFile}.${randomUUID()}.stale`;
        try {
          await rename(lockFile, staleLockFile);
          handle = await open(lockFile, "wx", 0o600);
        } catch (renameError) {
          if ((renameError as NodeJS.ErrnoException).code === "EEXIST") throw new Error(`Neon project state is locked: ${lockFile}`);
          throw renameError;
        }
      }
      if (!handle) {
        throw new Error(`Neon project state is locked: ${lockFile}`);
      }
    }
    acquired = true;
    await handle.writeFile(`${JSON.stringify({ pid: process.pid })}\n`);
    return await action();
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "EEXIST") throw new Error(`Neon project state is locked: ${lockFile}`);
    throw error;
  } finally {
    await handle?.close();
    if (acquired) {
      try { await unlink(lockFile); } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    }
    if (staleLockFile) {
      try { await unlink(staleLockFile); } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    }
  }
}

async function writeState(path: string, project: ManagedProject): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  const temporary = `${path}.${process.pid}.tmp`;
  await writeFile(temporary, `${JSON.stringify(project)}\n`, { mode: 0o600 });
  await rename(temporary, path);
  await chmod(path, 0o600);
}

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

async function request(path: string, init?: RequestInit): Promise<unknown> {
  const response = await fetch(`${apiBase}${path}`, { ...init, headers: { Authorization: `Bearer ${required("NEON_API_KEY")}`, "Content-Type": "application/json", ...(init?.headers ?? {}) } });
  if (!response.ok) {
    const error: NeonError = Object.assign(new Error(`Neon API request failed: HTTP ${response.status}`), { status: response.status });
    throw error;
  }
  return response.status === 204 ? null : response.json();
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const projectName = required("NEON_PROJECT_NAME");
  const regionId = required("NEON_REGION");
  const stateFile = required("NEON_PROJECT_STATE_FILE");
  await withLock(stateFile, async () => {
    let project: ManagedProject | undefined;
    try {
      project = await validatePersistedManagedProject(JSON.parse(await readFile(stateFile, "utf8")) as ManagedProject, projectName, regionId, request);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    project ??= await createOrFindManagedNeonProject(projectName, regionId, request);
    await writeState(stateFile, project);
    process.stdout.write(`${JSON.stringify(project)}\n`);
  });
}
