import type { DeploymentConfig } from "./config.js";
import * as pulumi from "@pulumi/pulumi";
import * as neon from "pulumi-neon/bin/index.js";

export interface DatabaseConnection {
  host: string;
  port: number;
  user: string;
  password: string;
  dbname: string;
  sslmode: "disable" | "require";
}

export interface DatabaseConnectionInputs {
  host: pulumi.Input<string>;
  port: pulumi.Input<number>;
  user: pulumi.Input<string>;
  password: pulumi.Input<string>;
  dbname: pulumi.Input<string>;
  sslmode: "disable" | "require";
}

export function buildDatabaseConnection(config: DeploymentConfig): DatabaseConnection {
  if (config.postgresMode === "docker") {
    return {
      host: "postgres",
      port: 5432,
      user: config.postgresUser,
      password: config.postgresPassword!,
      dbname: config.postgresDb,
      sslmode: "disable",
    };
  }
  return {
    host: config.neonHost!,
    port: config.neonPort,
    user: config.neonUser,
    password: config.neonPassword!,
    dbname: config.neonDb,
    sslmode: "require",
  };
}

export function createNeonConnection(
  config: DeploymentConfig,
  apiToken: pulumi.Input<string>,
): DatabaseConnectionInputs {
  const provider = new neon.Provider("neon", { token: apiToken });
  const role = new neon.Role("sub2api-neon-role", {
    projectId: config.neonProjectId!,
    branchId: config.neonBranchId!,
    name: config.neonUser,
  }, { provider });
  const database = new neon.Database("sub2api-neon-database", {
    projectId: config.neonProjectId!,
    branchId: config.neonBranchId!,
    ownerName: role.name,
    name: config.neonDb,
  }, { provider, dependsOn: role });
  const endpoint = new neon.Endpoint("sub2api-neon-endpoint", {
    projectId: config.neonProjectId!,
    branchId: config.neonBranchId!,
  }, { provider, dependsOn: database });

  return {
    host: endpoint.host,
    port: 5432,
    user: role.name,
    password: pulumi.secret(role.password),
    dbname: database.name,
    sslmode: "require",
  };
}
