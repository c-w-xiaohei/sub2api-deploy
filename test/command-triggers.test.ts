import { describe, expect, it } from "vitest";
import { buildInfraTriggers, buildReleaseTriggers } from "../src/command-triggers.js";

const infra = () => buildInfraTriggers({
  resourceNamespace: "sub2api",
  domain: "sub2api.example.com",
  originIp: "203.0.113.10",
  postgresMode: "docker",
  redisMode: "docker",
  traefikImage: "traefik:v3.3.3",
  acmeEmail: "ops@example.com",
  appProbePath: "/api/ready",
  drainSeconds: 10,
  composeChecksum: "compose-v1",
  resourceModes: "existing/existing",
});

describe("Pulumi command trigger contracts", () => {
  it("keeps image-only release changes out of infrastructure triggers", () => {
    expect(infra()).not.toContain("image@sha256:new");
    expect(buildReleaseTriggers("image@sha256:new")).toEqual(["image@sha256:new"]);
  });

  it("keeps infrastructure changes out of application release triggers", () => {
    expect(buildReleaseTriggers("image@sha256:same")).not.toContain("sub2api.example.net");
    expect(infra()).toEqual(expect.arrayContaining(["sub2api.example.com", "203.0.113.10", "docker"]));
  });
});
