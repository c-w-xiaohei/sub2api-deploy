import { describe, expect, it } from "vitest";
import { renderTraefikConfig } from "../scripts/render-traefik-config.js";

describe("renderTraefikConfig", () => {
  it("renders the ACME email", () => {
    const template = "certificatesResolvers:\n  letsencrypt:\n    acme:\n      email: ${ACME_EMAIL}\n";

    expect(renderTraefikConfig(template, "ops@example.com")).toBe(
      "certificatesResolvers:\n  letsencrypt:\n    acme:\n      email: ops@example.com\n",
    );
  });
});
