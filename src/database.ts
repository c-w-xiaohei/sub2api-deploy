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

export interface NeonResourceNames {
  projectName: string;
  branchName: string;
  roleName: string;
  databaseName: string;
}

export function managedNeonResourceNames(namespace: string): NeonResourceNames {
  return {
    projectName: `${namespace}-postgres`,
    branchName: namespace,
    roleName: namespace,
    databaseName: namespace,
  };
}

export function parsePostgresDsn(dsn: string): DatabaseConnection {
  let url: URL;
  try {
    url = new URL(dsn);
  } catch {
    throw new Error("PostgreSQL DSN is invalid");
  }
  if (url.protocol !== "postgres:" && url.protocol !== "postgresql:") {
    throw new Error("PostgreSQL DSN must use postgres or postgresql");
  }
  if (!url.hostname || !url.username || !url.password) {
    throw new Error("PostgreSQL DSN must include host, user, and password");
  }
  const dbname = decodeURIComponent(url.pathname.replace(/^\//, ""));
  if (!dbname) throw new Error("PostgreSQL DSN must include a database name");
  if ((url.searchParams.get("sslmode") ?? "require") !== "require") {
    throw new Error("PostgreSQL DSN must use sslmode=require");
  }
  return {
    host: url.hostname,
    port: Number(url.port || 5432),
    user: decodeURIComponent(url.username),
    password: decodeURIComponent(url.password),
    dbname,
    sslmode: "require",
  };
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

export function buildDsnDatabaseConnection(dsn: pulumi.Input<string>): DatabaseConnectionInputs {
  const parsed = pulumi.output(dsn).apply(parsePostgresDsn);
  return {
    host: parsed.apply((value) => value.host),
    port: parsed.apply((value) => value.port),
    user: parsed.apply((value) => value.user),
    password: pulumi.secret(parsed.apply((value) => value.password)),
    dbname: parsed.apply((value) => value.dbname),
    sslmode: "require",
  };
}

export function createNeonConnection(
  config: DeploymentConfig,
  apiToken: pulumi.Input<string>,
): DatabaseConnectionInputs {
  const provider = new neon.Provider("neon", { token: apiToken });
  const names = managedNeonResourceNames(config.resourceNamespace);
  const project = new neon.Project(`${config.resourceNamespace}-neon-project`, {
    name: names.projectName,
    orgId: config.neonOrgId,
    regionId: config.neonRegionId,
  }, { provider });
  const branch = new neon.Branch(`${config.resourceNamespace}-neon-branch`, {
    projectId: project.id,
    name: names.branchName,
  }, { provider, dependsOn: project });
  const role = new neon.Role(`${config.resourceNamespace}-neon-role`, {
    projectId: project.id,
    branchId: branch.id,
    name: names.roleName,
  }, { provider, dependsOn: branch });
  const database = new neon.Database(`${config.resourceNamespace}-neon-database`, {
    projectId: project.id,
    branchId: branch.id,
    ownerName: role.name,
    name: names.databaseName,
  }, { provider, dependsOn: role });
  const endpoint = new neon.Endpoint(`${config.resourceNamespace}-neon-endpoint`, {
    projectId: project.id,
    branchId: branch.id,
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
