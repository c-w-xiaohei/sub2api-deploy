import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

describe("release bundle verification fixtures", () => {
  it("supplies an existing per-Site app env file to Site Compose verification", () => {
    const script = readFileSync(new URL("../scripts/release-bundle.sh", import.meta.url), "utf8");
    expect(script).toContain('app_env="$verification_dir/sites/$site_id.app.env"');
    expect(script).toContain('SITE_APP_ENV_PATH=$app_env');
    expect(script).toContain('> "$app_env"');
  });
});
