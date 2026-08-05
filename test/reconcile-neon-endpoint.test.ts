import { describe, expect, it } from "vitest";
import {
  desiredEndpointSettings,
  reconcileEndpointSettings,
  selectEndpoint,
} from "../scripts/reconcile-neon-endpoint.js";

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

  it("builds the requested 0.25-1 CU and three-minute policy", () => {
    expect(desiredEndpointSettings(0.25, 1, 180)).toEqual({
      autoscaling_limit_min_cu: 0.25,
      autoscaling_limit_max_cu: 1,
      suspend_timeout_seconds: 180,
    });
  });

  it("retries transient GET failures after PATCH before declaring convergence", async () => {
    const settings = desiredEndpointSettings(0.25, 1, 180);
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
    const settings = desiredEndpointSettings(0.25, 1, 180);
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
    const settings = desiredEndpointSettings(0.25, 1, 180);
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
