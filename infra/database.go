package main

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	neon "github.com/kislerdm/pulumi-sdk-neon"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type DatabaseConnection struct { Host string; Port int; User string; Password string; DBName string; SSLMode string }
type DatabaseConnectionInputs struct { Host pulumi.StringInput; Port pulumi.IntInput; User pulumi.StringInput; Password pulumi.StringInput; DBName pulumi.StringInput; SSLMode string }

// neonProject is registered directly because the alpha provider schema uses
// neon:provider:Project while the generated SDK wrapper uses a stale token.
type neonProject struct { pulumi.CustomResourceState; Connection_uri pulumi.StringOutput `pulumi:"connection_uri"` }
type neonProjectArgs struct { Name pulumi.StringPtrInput `pulumi:"name"`; Org_id pulumi.StringPtrInput `pulumi:"org_id"` }
type neonProjectArgsValue struct { Name *string `pulumi:"name"`; Org_id *string `pulumi:"org_id"` }
func (neonProjectArgs) ElementType() reflect.Type { return reflect.TypeOf((*neonProjectArgsValue)(nil)).Elem() }
func registerNeonProject(ctx *pulumi.Context, name string, args *neonProjectArgs, opts ...pulumi.ResourceOption) (*neonProject, error) { var project neonProject; if err := ctx.RegisterResource("neon:provider:Project", name, args, &project, opts...); err != nil { return nil, err }; return &project, nil }
func ManagedNeonProjectName(namespace string) string { return namespace + "-postgres" }

func legacyNeonProvider(ctx *pulumi.Context, siteID string, apiKey pulumi.StringInput, opts ...pulumi.ResourceOption) (*neon.Provider, error) {
	var provider neon.Provider
	if err := ctx.RegisterResource("pulumi:providers:neon", "site-"+siteID+"-neon", &neon.ProviderArgs{Api_key: apiKey}, &provider, opts...); err != nil { return nil, err }
	return &provider, nil
}

func ParsePostgresDSN(dsn string) (DatabaseConnection, error) {
	parsed, err := url.Parse(dsn); if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") { return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN is invalid") }
	password, hasPassword := parsed.User.Password(); if parsed.Hostname() == "" || parsed.User.Username() == "" || !hasPassword { return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN must include host, user, and password") }
	dbName := strings.TrimPrefix(parsed.Path, "/"); if dbName == "" { return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN must include a database name") }; if parsed.Query().Get("sslmode") != "" && parsed.Query().Get("sslmode") != "require" { return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN must use sslmode=require") }
	port := 5432; if parsed.Port() != "" { port, err = strconv.Atoi(parsed.Port()); if err != nil { return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN is invalid") } }
	return DatabaseConnection{Host: parsed.Hostname(), Port: port, User: parsed.User.Username(), Password: password, DBName: dbName, SSLMode: "require"}, nil
}

func BuildDSNDatabaseConnection(dsn pulumi.StringInput) DatabaseConnectionInputs {
	parse := func(value string) DatabaseConnection { connection, err := ParsePostgresDSN(value); if err != nil { panic(err) }; return connection }
	stringField := func(selector func(DatabaseConnection) string) pulumi.StringOutput { return dsn.ToStringOutput().ApplyT(func(value string) string { return selector(parse(value)) }).(pulumi.StringOutput) }
	intField := func(selector func(DatabaseConnection) int) pulumi.IntOutput { return dsn.ToStringOutput().ApplyT(func(value string) int { return selector(parse(value)) }).(pulumi.IntOutput) }
	return DatabaseConnectionInputs{Host: stringField(func(value DatabaseConnection) string { return value.Host }), Port: intField(func(value DatabaseConnection) int { return value.Port }), User: stringField(func(value DatabaseConnection) string { return value.User }), Password: pulumi.ToSecret(stringField(func(value DatabaseConnection) string { return value.Password })).(pulumi.StringOutput), DBName: stringField(func(value DatabaseConnection) string { return value.DBName }), SSLMode: "require"}
}

func siteDatabaseInputs(ctx *pulumi.Context, site, preflight pulumi.Resource, layout SiteLayout, spec SiteSpec, secrets SiteSecrets) (DatabaseConnectionInputs, error) {
	siteID := layout.SiteID
	if spec.Database.Mode == "docker" { return DatabaseConnectionInputs{Host: pulumi.String("postgres"), Port: pulumi.Int(5432), User: pulumi.String("sub2api"), Password: pulumi.ToSecret(pulumi.String(secrets.Database.Password)).(pulumi.StringOutput), DBName: pulumi.String("sub2api"), SSLMode: "disable"}, nil }
	if spec.Database.ResourceMode == "create" {
		providerOptions := []pulumi.ResourceOption{pulumi.Parent(site), pulumi.Aliases(legacyCode2Aliases(layout, "neon")), pulumi.DependsOn([]pulumi.Resource{preflight})}
		apiKey := pulumi.ToSecret(pulumi.String(secrets.Database.APIToken)).(pulumi.StringOutput)
		legacy := len(legacyCode2Aliases(layout, "legacy")) != 0
		var provider *neon.Provider
		var err error
		if legacy { provider, err = legacyNeonProvider(ctx, siteID, apiKey, providerOptions...) } else { provider, err = neon.NewProvider(ctx, "site-"+siteID+"-neon", &neon.ProviderArgs{Api_key: apiKey}, append(providerOptions, pulumi.Version("0.0.1-alpha.1"))...) }
		if err != nil { return DatabaseConnectionInputs{}, err }
		// Retirement must explicitly unprotect this persistent project first.
		projectOptions := []pulumi.ResourceOption{pulumi.Parent(site), pulumi.Aliases(legacyCode2Aliases(layout, spec.ResourcePrefix+"-neon-project")), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Version("0.0.1-alpha.1"), pulumi.Protect(true), pulumi.RetainOnDelete(true)}; if len(legacyCode2Aliases(layout, "legacy")) != 0 { projectOptions = append(projectOptions, pulumi.IgnoreChanges([]string{"org_id"})) }
		project, err := registerNeonProject(ctx, "site-"+siteID+"-neon-project", &neonProjectArgs{Name: pulumi.StringPtr(ManagedNeonProjectName(spec.ResourcePrefix))}, projectOptions...); if err != nil { return DatabaseConnectionInputs{}, err }
		return BuildDSNDatabaseConnection(pulumi.ToSecret(project.Connection_uri).(pulumi.StringOutput)), nil
	}
	return BuildDSNDatabaseConnection(pulumi.ToSecret(pulumi.String(secrets.Database.DSN)).(pulumi.StringOutput)), nil
}
