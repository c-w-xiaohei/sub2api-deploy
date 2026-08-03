package main

import (
	"encoding/json"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type Site struct { pulumi.ResourceState; FinalBarrier *local.Command; Status pulumi.MapOutput }

func marshalRuntimeSecrets(secrets SiteSecrets) string {
	encoded, err := json.Marshal(secrets)
	if err != nil { panic(err) }
	return string(encoded)
}

func buildSiteRuntimePayload(siteID string, spec SiteSpec, layout SiteLayout, database DatabaseConnectionInputs, redis RedisConnectionInputs, secrets SiteSecrets) pulumi.StringOutput {
	payload := pulumi.All(database.Host, database.Port, database.User, database.Password, database.DBName, redis.Host, redis.Port, redis.Username, redis.Password, redis.DB, redis.EnableTLS).ApplyT(func(values []interface{}) string {
		payload := map[string]interface{}{
			"SITE_ID": siteID, "DOMAIN": spec.Domain, "ADMIN_EMAIL": spec.AdminEmail,
			"APP_PROBE_PATH": spec.AppProbePath, "DRAIN_SECONDS": *spec.DrainSeconds, "POSTGRES_MODE": spec.Database.Mode, "REDIS_MODE": spec.Redis.Mode,
			"DATABASE_HOST": values[0], "DATABASE_PORT": values[1], "DATABASE_USER": values[2], "DATABASE_PASSWORD": values[3], "DATABASE_DBNAME": values[4], "DATABASE_SSLMODE": database.SSLMode,
			"REDIS_HOST": values[5], "REDIS_PORT": values[6], "REDIS_USERNAME": values[7], "REDIS_PASSWORD": values[8], "REDIS_DB": values[9], "REDIS_ENABLE_TLS": values[10],
			"POSTGRES_PASSWORD": postgresRuntimePassword(spec, secrets, values[3]),
			"POSTGRES_USER": values[2], "POSTGRES_DB": values[4],
			"ADMIN_PASSWORD": secrets.AdminPassword, "JWT_SECRET": secrets.JWTSecret, "TOTP_ENCRYPTION_KEY": secrets.TOTPEncryptionKey,
			"RUNTIME_ROOT": layout.RuntimeRoot, "BLUE_EDGE_ALIAS": layout.BlueAlias, "GREEN_EDGE_ALIAS": layout.GreenAlias,
			"ACTIVE_EDGE_ALIAS": layout.BlueAlias, "AUTO_SETUP": "true",
		}
		encoded, err := json.Marshal(payload); if err != nil { panic(err) }; return string(encoded)
	}).(pulumi.StringOutput)
	return pulumi.ToSecret(payload).(pulumi.StringOutput)
}

func postgresRuntimePassword(spec SiteSpec, secrets SiteSecrets, databasePassword interface{}) interface{} {
	if spec.Database.Mode == "docker" { return secrets.Database.Password }
	return databasePassword
}

func DeploySite(ctx *pulumi.Context, siteID string, spec SiteSpec, secrets SiteSecrets, layout SiteLayout, edge *Edge, preflight, previousBarrier pulumi.Resource, checksum, configuredSiteIDs string) (*Site, error) {
	site := &Site{}
	if err := ctx.RegisterComponentResource("sub2api:host:Site", "site-"+siteID, site); err != nil { return nil, err }
	dns, err := createSiteDNSRecord(ctx, site, preflight, edge.Provider, layout, spec, edge.Spec)
	if err != nil { return nil, err }
	database, err := siteDatabaseInputs(ctx, site, preflight, layout, spec, secrets); if err != nil { return nil, err }
	redis, err := siteRedisInputs(ctx, site, preflight, layout, spec, secrets); if err != nil { return nil, err }
	runtime := buildSiteRuntimePayload(siteID, spec, layout, database, redis, secrets)
	reconcileTriggers := BuildSiteReconcileTriggers(SiteTriggerInput{SiteID: siteID, Domain: spec.Domain, RuntimeRoot: layout.RuntimeRoot, ComposeProject: layout.ComposeProject, RoutePath: layout.RoutePath, SiteChecksum: checksum})
	dependencies := []pulumi.Resource{dns, edge.Reconcile, preflight}
	if previousBarrier != nil { dependencies = append(dependencies, previousBarrier) }
	environment := siteEnvironment(siteID, spec, layout, runtime, configuredSiteIDs, edge.Spec.OriginIP)
	reconcile, err := newSiteCommand(ctx, layout, "site-"+siteID+"-reconcile", "infra-reconcile", "bash scripts/reconcile-site.sh", environment, reconcileTriggers, site, dependencies...); if err != nil { return nil, err }
	releaseEnvironment := siteEnvironment(siteID, spec, layout, runtime, configuredSiteIDs, edge.Spec.OriginIP)
	releaseEnvironment["SUB2API_IMAGE"] = pulumi.String(spec.Image)
	release, err := newSiteCommand(ctx, layout, "site-"+siteID+"-release", "application-release", "bash scripts/application-release.sh", releaseEnvironment, BuildSiteReleaseTriggers(siteID, spec.Image), site, reconcile, preflight); if err != nil { return nil, err }
	readiness, err := newSiteCommand(ctx, layout, "site-"+siteID+"-strict-public-readiness", "post-strict-public-readiness", `bash scripts/probe-origin.sh "$DOMAIN" "$APP_PROBE_PATH"`, environment, reconcileTriggers, site, release, preflight); if err != nil { return nil, err }
	rollback, err := newCommand(ctx, "site-"+siteID+"-rollback-preparation", "bash -c 'exit 0'", environment, reconcileTriggers, site, readiness, preflight); if err != nil { return nil, err }
	site.FinalBarrier = rollback
	status := pulumi.Map{"domain": pulumi.String(spec.Domain), "dnsRecordId": dns.ID(), "readinessId": readiness.ID(), "deploymentId": release.ID()}
	site.Status = status.ToMapOutput()
	ctx.RegisterResourceOutputs(site, status)
	return site, nil
}
