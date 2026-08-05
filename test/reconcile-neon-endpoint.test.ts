import { describe, expect, it } from "vitest";
import {
  desiredEndpointSettings,
  reconcileEndpointSettings,
  selectEndpoint,
} from "../scripts/reconcile-neon-endpoint.js";
import {
  selectRegionEndpoint,
  validateManagedNeonRegion,
} from "../scripts/validate-neon-region.js";
import {
  createOrFindManagedNeonProject,
  managedProjectState,
  parseManagedProjectResponse,
  validatePersistedManagedProject,
  withLock,
} from "../scripts/create-neon-project.js";
import { mkdirSync, writeFileSync } from "node:fs";
import { mkdtemp, readFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";

describe("Neon endpoint settings", () => {
  it("selects exactly the endpoint matching the project default host", () => {
    expect(selectEndpoint([{ id: "ep-a", host: "ep-a.us-east-2.aws.neon.tech" }], "ep-a.us-east-2.aws.neon.tech")).toEqual({
      id: "ep-a",
      host: "ep-a.us-east-2.aws.neon.tech",
    });
  });

  it("rejects ambiguous or missing endpoint hosts", () => {
    expect(() => selectEndpoint([], "ep-a.neon.tech")).toThrow("exactly one");
    expect(() => selectEndpoint([
      { id: "ep-a", host: "ep-a.neon.tech" },
      { id: "ep-b", host: "ep-a.neon.tech" },
    ], "ep-a.neon.tech")).toThrow("exactly one");
  });

  it("builds the requested 0.25 CU and five-minute policy", () => {
    expect(desiredEndpointSettings(0.25, 0.25, 300)).toEqual({
      autoscaling_limit_min_cu: 0.25,
      autoscaling_limit_max_cu: 0.25,
      suspend_timeout_seconds: 300,
    });
  });

  it("retries transient GET failures after PATCH before declaring convergence", async () => {
    const settings = desiredEndpointSettings(0.25, 0.25, 300);
    const requests: Array<{ path: string; method: string }> = [];
    const sleeps: number[] = [];
    let verificationAttempts = 0;
    const request = async (path: string, init?: RequestInit): Promise<unknown> => {
      requests.push({ path, method: init?.method ?? "GET" });
      if (path === "/projects/project-id/endpoints") {
        return { endpoints: [{ id: "endpoint-id", host: "endpoint.neon.tech" }] };
      }
      if (init?.method === "PATCH") return {};
      verificationAttempts += 1;
      if (verificationAttempts === 1) return { endpoint: { ...settings, autoscaling_limit_max_cu: 2 } };
      if (verificationAttempts < 4) {
        throw Object.assign(new Error("temporary Neon API failure"), { status: 503 });
      }
      return { endpoint: settings };
    };

    await reconcileEndpointSettings(
      "project-id",
      "endpoint.neon.tech",
      settings,
      request,
      async (milliseconds) => { sleeps.push(milliseconds); },
    );

    expect(requests.map(({ method }) => method)).toEqual(["GET", "GET", "PATCH", "GET", "GET", "GET"]);
    expect(sleeps).toEqual([2000, 2000]);
  });

  it("retries a transient PATCH precondition failure", async () => {
    const settings = desiredEndpointSettings(0.25, 0.25, 300);
    const methods: string[] = [];
    const sleeps: number[] = [];
    let patches = 0;
    let reads = 0;
    await reconcileEndpointSettings(
      "project-id",
      "endpoint.neon.tech",
      settings,
      async (path, init) => {
        methods.push(init?.method ?? "GET");
        if (path === "/projects/project-id/endpoints") {
          return { endpoints: [{ id: "endpoint-id", host: "endpoint.neon.tech" }] };
        }
        if (init?.method === "PATCH") {
          patches += 1;
          if (patches === 1) throw Object.assign(new Error("endpoint busy"), { status: 412 });
          return {};
        }
        reads += 1;
        if (reads === 1) return { endpoint: { ...settings, autoscaling_limit_max_cu: 2 } };
        return { endpoint: settings };
      },
      async (milliseconds) => { sleeps.push(milliseconds); },
    );

    expect(methods).toEqual(["GET", "GET", "PATCH", "PATCH", "GET"]);
    expect(sleeps).toEqual([2000]);
  });

  it("does not PATCH an endpoint that already has the requested settings", async () => {
    const settings = desiredEndpointSettings(0.25, 0.25, 300);
    const methods: string[] = [];
    await reconcileEndpointSettings(
      "project-id",
      "endpoint.neon.tech",
      settings,
      async (path, init) => {
        methods.push(init?.method ?? "GET");
        if (path === "/projects/project-id/endpoints") {
          return { endpoints: [{ id: "endpoint-id", host: "endpoint.neon.tech" }] };
        }
        return { endpoint: settings };
      },
      async () => {},
    );

    expect(methods).toEqual(["GET", "GET"]);
  });
});

describe("managed Neon region validation", () => {
  it("selects the one default endpoint with an authoritative region", () => {
    expect(selectRegionEndpoint([
      { id: "ep-a", host: "ep-a.neon.tech", region_id: "aws-us-east-1" },
    ], "ep-a.neon.tech")).toEqual({
      id: "ep-a",
      host: "ep-a.neon.tech",
      region_id: "aws-us-east-1",
    });
  });

  it.each([
    ["missing endpoint", [], "exactly one"],
    ["ambiguous endpoint", [
      { id: "ep-a", host: "ep-a.neon.tech", region_id: "aws-us-east-1" },
      { id: "ep-b", host: "ep-a.neon.tech", region_id: "aws-us-east-1" },
    ], "exactly one"],
    ["missing region", [{ id: "ep-a", host: "ep-a.neon.tech" }], "region_id"],
  ])("fails closed for %s", (_name, endpoints, message) => {
    expect(() => selectRegionEndpoint(endpoints, "ep-a.neon.tech")).toThrow(message);
  });

  it("accepts a matching region and performs no mutation", async () => {
    const methods: string[] = [];
    await validateManagedNeonRegion("project-id", "ep-a.neon.tech", "aws-us-east-1", async (_path, init) => {
      methods.push(init?.method ?? "GET");
      return { endpoints: [{ id: "ep-a", host: "ep-a.neon.tech", region_id: "aws-us-east-1" }] };
    });
    expect(methods).toEqual(["GET"]);
  });

  it("rejects a mismatched region without attempting a mutation", async () => {
    const methods: string[] = [];
    await expect(validateManagedNeonRegion("project-id", "ep-a.neon.tech", "aws-us-west-2", async (_path, init) => {
      methods.push(init?.method ?? "GET");
      return { endpoints: [{ id: "ep-a", host: "ep-a.neon.tech", region_id: "aws-us-east-1" }] };
    })).rejects.toThrow("region");
    expect(methods).toEqual(["GET"]);
  });
});

describe("managed Neon project creation", () => {
  const projectEnvelope = {
    project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-west-2" },
    endpoints: [{ id: "ep-id", host: "ep-id.aws-us-west-2.aws.neon.tech", branch_id: "br-main", type: "read_write" }],
    connection_uris: [{ connection_uri: "postgresql://owner:secret@ep-id.aws-us-west-2.aws.neon.tech/neondb?sslmode=require", connection_parameters: { host: "ep-id.aws-us-west-2.aws.neon.tech", database: "neondb", role: "owner", password: "secret" } }],
  };

  it("parses authoritative create envelope fields without requiring secrets in project", () => {
    expect(parseManagedProjectResponse(projectEnvelope)).toEqual({
      id: "project-id", name: "tenant-postgres", region_id: "aws-us-west-2", default_endpoint_host: "ep-id.aws-us-west-2.aws.neon.tech",
    });
  });

  it("includes region_id in the Neon project creation request", async () => {
    const requests: Array<{ path: string; method: string; body?: string }> = [];
    const state = await createOrFindManagedNeonProject("tenant-postgres", "aws-us-west-2", async (path, init) => {
      requests.push({ path, method: init?.method ?? "GET", body: init?.body as string | undefined });
      if (init?.method === "POST") return { project: projectEnvelope.project };
      if (path === "/projects/project-id") return { project: projectEnvelope.project };
      if (path === "/projects/project-id/endpoints") return { endpoints: projectEnvelope.endpoints };
      return { projects: [] };
    });

    expect(JSON.parse(requests[1].body!)).toEqual({ project: { name: "tenant-postgres", region_id: "aws-us-west-2" } });
    expect(state.id).toBe("project-id");
  });

  it("finds an existing deterministic project without POSTing", async () => {
    const methods: string[] = [];
    const state = await createOrFindManagedNeonProject("tenant-postgres", "aws-us-east-1", async (_path, init) => {
      methods.push(init?.method ?? "GET");
      return methods.length === 1 ? { projects: [{ id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" }] } : { project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" }, endpoints: [{ id: "ep-id", host: "ep-id.neon.tech", branch_id: "br-main", type: "read_write" }] };
    });

    expect(methods).toEqual(["GET", "GET", "GET"]);
    expect(state.id).toBe("project-id");
  });

  it("uses project detail as the authority when the list omits region_id", async () => {
    const paths: string[] = [];
    const state = await createOrFindManagedNeonProject("tenant-postgres", "aws-us-east-1", async (path) => {
      paths.push(path);
      if (path === "/projects") return { projects: [{ id: "project-id", name: "tenant-postgres" }] };
      if (path === "/projects/project-id") return { project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" } };
      return { endpoints: [{ id: "ep-id", host: "ep-id.neon.tech", type: "read_write" }] };
    });

    expect(state.id).toBe("project-id");
    expect(paths).toEqual(["/projects", "/projects/project-id", "/projects/project-id/endpoints"]);
  });

  it("still fails closed on a detail region mismatch when the list omits region_id", async () => {
    await expect(createOrFindManagedNeonProject("tenant-postgres", "aws-us-west-2", async (path) => {
      if (path === "/projects") return { projects: [{ id: "project-id", name: "tenant-postgres" }] };
      return { project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" }, endpoints: [] };
    })).rejects.toThrow("does not match");
  });

  it("fails closed when the deterministic project has another region", async () => {
    const methods: string[] = [];
    await expect(createOrFindManagedNeonProject("tenant-postgres", "aws-us-west-2", async (_path, init) => {
      methods.push(init?.method ?? "GET");
      return { projects: [managedProjectState("project-id", "tenant-postgres", "aws-us-east-1", "ep-id.neon.tech")] };
    })).rejects.toThrow("does not match");
    expect(methods).toEqual(["GET"]);
  });

  it("fails closed when persisted state has another region", async () => {
    await expect(validatePersistedManagedProject(managedProjectState("project-id", "tenant-postgres", "aws-us-east-1", "ep-id.neon.tech"), "tenant-postgres", "aws-us-west-2", async (path) => {
      if (path === "/projects/project-id") return { project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" } };
      if (path === "/projects/project-id/endpoints") return { endpoints: [{ id: "ep-id", host: "ep-id.neon.tech", type: "read_write" }] };
      return { projects: [{ id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" }] };
    })).rejects.toThrow("does not match");
  });

  it("revalidates stale persisted endpoint metadata through deterministic lookup", async () => {
    const paths: string[] = [];
    const state = await validatePersistedManagedProject(managedProjectState("old-id", "tenant-postgres", "aws-us-east-1", "old.neon.tech"), "tenant-postgres", "aws-us-east-1", async (path) => {
      paths.push(path);
      if (path === "/projects/old-id") return { project: { id: "old-id", name: "renamed", region_id: "aws-us-east-1" } };
      if (path === "/projects") return { projects: [{ id: "new-id", name: "tenant-postgres", region_id: "aws-us-east-1" }] };
      if (path === "/projects/new-id") return { project: { id: "new-id", name: "tenant-postgres", region_id: "aws-us-east-1" } };
      return { endpoints: [{ id: "new-ep", host: "new.neon.tech", type: "read_write" }] };
    });
    expect(state).toEqual(managedProjectState("new-id", "tenant-postgres", "aws-us-east-1", "new.neon.tech"));
    expect(paths).toEqual(["/projects/old-id", "/projects/old-id/endpoints", "/projects", "/projects/new-id", "/projects/new-id/endpoints"]);
  });

  it("follows cursor pagination before creating", async () => {
    const paths: string[] = [];
    await createOrFindManagedNeonProject("tenant-postgres", "aws-us-east-1", async (path, init) => {
      paths.push(path);
      if (init?.method === "POST") return { project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" }, endpoints: [{ id: "ep-id", host: "ep-id.neon.tech", branch_id: "br-main", type: "read_write" }] };
      if (path === "/projects") return { projects: [], pagination: { cursor: "next" } };
      if (path === "/projects?cursor=next") return { projects: [] };
      if (path === "/projects/project-id") return { project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" } };
      if (path === "/projects/project-id/endpoints") return { endpoints: [{ id: "ep-id", host: "ep-id.neon.tech", type: "read_write" }] };
      throw new Error(`unexpected ${path}`);
    });
    expect(paths).toEqual(["/projects", "/projects?cursor=next", "/projects", "/projects/project-id", "/projects/project-id/endpoints"]);
  });

  it("recovers a POST conflict by validating the project found afterward", async () => {
    const methods: string[] = [];
    const state = await createOrFindManagedNeonProject("tenant-postgres", "aws-us-east-1", async (path, init) => {
      methods.push(`${init?.method ?? "GET"} ${path}`);
      if (init?.method === "POST") throw Object.assign(new Error("already exists"), { status: 409 });
      if (path === "/projects") return { projects: [{ id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" }] };
      if (path === "/projects/project-id") return { project: { id: "project-id", name: "tenant-postgres", region_id: "aws-us-east-1" } };
      return { endpoints: [{ id: "ep-id", host: "ep-id.neon.tech", branch_id: "br-main", type: "read_write" }] };
    });
    expect(state.id).toBe("project-id");
    expect(methods.at(-1)).toBe("GET /projects/project-id/endpoints");
  });

  it("does not include a connection URI in persisted metadata", () => {
    const state = JSON.stringify(managedProjectState("project-id", "tenant-postgres", "aws-us-east-1", "ep-id.neon.tech"));
    expect(state).not.toContain("connection_uri");
    expect(state).not.toContain("postgresql://");
  });

  it("recovers a lock left by a dead process but refuses a live lock", async () => {
    const root = await mkdtemp(join(tmpdir(), "sub2api-neon-lock-"));
    const stateFile = join(root, "project.json");
    mkdirSync(root, { recursive: true });
    writeFileSync(`${stateFile}.lock`, '{"pid":999999999}\n');

    await expect(withLock(stateFile, async () => "recovered")).resolves.toBe("recovered");
    await expect(readFile(`${stateFile}.lock`, "utf8")).rejects.toMatchObject({ code: "ENOENT" });

    writeFileSync(`${stateFile}.lock`, JSON.stringify({ pid: process.pid }));
    await expect(withLock(stateFile, async () => "blocked")).rejects.toThrow("locked");
  });
});
