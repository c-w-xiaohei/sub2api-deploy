import type { DeploymentConfig } from "./config.js";
import * as pulumi from "@pulumi/pulumi";
import * as upstash from "@upstash/pulumi";

export interface RedisConnection {
  host: string;
  port: number;
  username: string;
  password: string;
  db: number;
  enableTls: boolean;
}

export interface RedisConnectionInputs {
  host: pulumi.Input<string>;
  port: pulumi.Input<number>;
  username: pulumi.Input<string>;
  password: pulumi.Input<string>;
  db: pulumi.Input<number>;
  enableTls: pulumi.Input<boolean>;
}

export function buildRedisConnection(config: DeploymentConfig): RedisConnection {
  if (config.redisMode === "docker") {
    return {
      host: "redis",
      port: 6379,
      username: config.redisUsername,
      password: config.redisPassword!,
      db: 0,
      enableTls: false,
    };
  }
  return {
    host: config.upstashHost!,
    port: config.upstashPort,
    username: config.upstashUsername,
    password: config.upstashPassword!,
    db: 0,
    enableTls: true,
  };
}

export function createUpstashConnection(
  config: DeploymentConfig,
  apiKey: pulumi.Input<string>,
): RedisConnectionInputs {
  const provider = new upstash.Provider("upstash", {
    apiKey,
    email: config.upstashEmail!,
  });
  const database = new upstash.RedisDatabase("sub2api-upstash-redis", {
    databaseName: config.upstashDatabaseName!,
    region: config.upstashRegion!,
    tls: true,
  }, { provider });

  return {
    host: database.endpoint,
    port: database.port,
    username: "default",
    password: pulumi.secret(database.password),
    db: 0,
    enableTls: true,
  };
}
