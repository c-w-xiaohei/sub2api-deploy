import * as pulumi from "@pulumi/pulumi";

export interface ProviderArgs {
  api_key: pulumi.Input<string>;
}

export class Provider extends pulumi.ProviderResource {
  constructor(name: string, args: ProviderArgs, opts?: pulumi.ResourceOptions);
}

export interface ProjectArgs {
  name?: pulumi.Input<string>;
  org_id?: pulumi.Input<string>;
}

export class Project extends pulumi.CustomResource {
  readonly connection_uri: pulumi.Output<string>;
  readonly connection_uri_pooler: pulumi.Output<string>;
  readonly default_branch_name: pulumi.Output<string>;
  readonly default_database_name: pulumi.Output<string>;
  readonly default_endpoint_host: pulumi.Output<string>;
  readonly default_endpoint_host_pooler: pulumi.Output<string>;
  readonly default_role_name: pulumi.Output<string>;
  readonly default_role_password: pulumi.Output<string>;
  readonly identifier: pulumi.Output<string>;
  readonly name: pulumi.Output<string | undefined>;
  readonly org_id: pulumi.Output<string | undefined>;

  constructor(name: string, args: ProjectArgs, opts?: pulumi.CustomResourceOptions);
  static get(name: string, id: pulumi.Input<pulumi.ID>, opts?: pulumi.CustomResourceOptions): Project;
}
