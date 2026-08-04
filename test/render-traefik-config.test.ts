import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { renderTraefikConfig } from "../scripts/render-traefik-config.js";

describe("renderTraefikConfig", () => {
  it("renders the ACME email", () => {
    const template = "certificatesResolvers:\n  letsencrypt:\n    acme:\n      email: ${ACME_EMAIL}\n";

    expect(renderTraefikConfig(template, "ops@example.com")).toBe(
      "certificatesResolvers:\n  letsencrypt:\n    acme:\n      email: ops@example.com\n",
    );
  });

  it("does not execute its CLI when imported by the Edge renderer", () => {
    const source = readFileSync(new URL("../scripts/render-traefik-config.ts", import.meta.url), "utf8");
    expect(source).toContain('import.meta.url === `file://${process.argv[1]}` && process.argv[2] === "write"');
  });
});
