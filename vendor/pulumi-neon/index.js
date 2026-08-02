import * as pulumi from "@pulumi/pulumi";

const version = "0.0.1-alpha.1";
const pluginDownloadURL = "https://github.com/kislerdm/pulumi-neon/releases/download/v${VERSION}";

function resourceOptions(opts = {}) {
  return pulumi.mergeOptions({ version, pluginDownloadURL }, opts);
}

export class Provider extends pulumi.ProviderResource {
  constructor(name, args, opts) {
    super("neon", name, { api_key: args?.api_key }, resourceOptions(opts));
  }
}

export class Project extends pulumi.CustomResource {
  static get(name, id, opts) {
    return new Project(name, undefined, pulumi.mergeOptions(resourceOptions(opts), { id }));
  }

  constructor(name, args, opts) {
    const inputs = {
      name: args?.name,
      org_id: args?.org_id,
      connection_uri: undefined,
      connection_uri_pooler: undefined,
      default_branch_name: undefined,
      default_database_name: undefined,
      default_endpoint_host: undefined,
      default_endpoint_host_pooler: undefined,
      default_role_name: undefined,
      default_role_password: undefined,
      identifier: undefined,
    };
    super("neon:resource:Project", name, inputs, resourceOptions(opts));
  }
}

pulumi.runtime.registerResourcePackage("neon", {
  version,
  constructProvider: (name, type, urn) => {
    if (type !== "pulumi:providers:neon") throw new Error(`unknown provider type ${type}`);
    return new Provider(name, undefined, { urn });
  },
});
