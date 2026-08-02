import type { DeploymentConfig } from "./config.js";
import * as pulumi from "@pulumi/pulumi";
import * as neon from "@dkisler/pulumi-neon";

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

export function managedNeonProjectName(namespace: string): string {
  return `${namespace}-postgres`;
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
  const provider = new neon.Provider("neon", { api_key: apiToken });
  const project = new neon.Project(`${config.resourceNamespace}-neon-project`, {
    name: managedNeonProjectName(config.resourceNamespace),
    org_id: config.neonOrgId,
  }, { provider });

  return buildDsnDatabaseConnection(pulumi.secret(project.connection_uri));
}
