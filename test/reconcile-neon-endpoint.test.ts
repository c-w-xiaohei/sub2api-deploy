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
