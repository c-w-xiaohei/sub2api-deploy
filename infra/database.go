package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type DatabaseConnection struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string
}
type DatabaseConnectionInputs struct {
	Host     pulumi.StringInput
	Port     pulumi.IntInput
	User     pulumi.StringInput
	Password pulumi.StringInput
	DBName   pulumi.StringInput
	SSLMode  string
}

// neonProject is registered directly because the alpha provider schema uses
// neon:provider:Project while the generated SDK wrapper uses a stale token.
type neonProject struct {
	pulumi.CustomResourceState
	Connection_uri        pulumi.StringOutput `pulumi:"connection_uri"`
	Default_endpoint_host pulumi.StringOutput `pulumi:"default_endpoint_host"`
}
type neonProjectArgs struct {
	Name   pulumi.StringPtrInput `pulumi:"name"`
	Org_id pulumi.StringPtrInput `pulumi:"org_id"`
}
type neonProjectArgsValue struct {
	Name   *string `pulumi:"name"`
	Org_id *string `pulumi:"org_id"`
}

type neonProjectOutputs struct {
	pulumi.Resource
	IDOutput            pulumi.IDOutput
	ConnectionURI       pulumi.StringOutput
	DefaultEndpointHost pulumi.StringOutput
}

func (project *neonProject) ProjectID() pulumi.IDOutput               { return project.ID() }
func (project *neonProject) ConnectionURIOutput() pulumi.StringOutput { return project.Connection_uri }
func (project *neonProject) EndpointHostOutput() pulumi.StringOutput {
	return project.Default_endpoint_host
}
func (project neonProjectOutputs) ProjectID() pulumi.IDOutput { return project.IDOutput }
func (project neonProjectOutputs) ConnectionURIOutput() pulumi.StringOutput {
	return project.ConnectionURI
}
func (project neonProjectOutputs) EndpointHostOutput() pulumi.StringOutput {
	return project.DefaultEndpointHost
}

func managedNeonProjectFromCommand(command *local.Command) neonProjectOutputs {
	parse := func(stdout, field string) string {
		if stdout == "" {
			return ""
		}
		var state map[string]string
		if err := json.Unmarshal([]byte(stdout), &state); err != nil {
			panic(fmt.Sprintf("managed Neon project output is invalid: %v", err))
		}
		if state[field] == "" {
			panic(fmt.Sprintf("managed Neon project output omitted %s", field))
		}
		return state[field]
	}
	return neonProjectOutputs{Resource: command, IDOutput: command.Stdout.ApplyT(func(stdout string) pulumi.ID { return pulumi.ID(parse(stdout, "id")) }).(pulumi.IDOutput), DefaultEndpointHost: command.Stdout.ApplyT(func(stdout string) string { return parse(stdout, "default_endpoint_host") }).(pulumi.StringOutput)}
}

// neonProvider is registered directly so the generated SDK's package default
// does not add a provider version to the persisted legacy resource shape.
type neonProvider struct {
	pulumi.ProviderResourceState
	Api_key pulumi.StringOutput `pulumi:"api_key"`
}
type neonProviderArgs struct {
	Api_key pulumi.StringInput `pulumi:"api_key"`
}
type neonProviderArgsValue struct {
	Api_key string `pulumi:"api_key"`
}

func (neonProviderArgs) ElementType() reflect.Type {
	return reflect.TypeOf((*neonProviderArgsValue)(nil)).Elem()
}

func registerNeonProvider(ctx *pulumi.Context, name string, args *neonProviderArgs, opts ...pulumi.ResourceOption) (*neonProvider, error) {
	var provider neonProvider
	if err := ctx.RegisterResource("pulumi:providers:neon", name, args, &provider, opts...); err != nil {
		return nil, err
	}
	return &provider, nil
}

func (neonProjectArgs) ElementType() reflect.Type {
	return reflect.TypeOf((*neonProjectArgsValue)(nil)).Elem()
}
func registerNeonProject(ctx *pulumi.Context, name string, args *neonProjectArgs, opts ...pulumi.ResourceOption) (*neonProject, error) {
	var project neonProject
	if err := ctx.RegisterResource("neon:provider:Project", name, args, &project, opts...); err != nil {
		return nil, err
	}
	return &project, nil
}
func ManagedNeonProjectName(namespace string) string { return namespace + "-postgres" }

func ParsePostgresDSN(dsn string) (DatabaseConnection, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN is invalid")
	}
	password, hasPassword := parsed.User.Password()
	if parsed.Hostname() == "" || parsed.User.Username() == "" || !hasPassword {
		return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN must include host, user, and password")
	}
	dbName := strings.TrimPrefix(parsed.Path, "/")
	if dbName == "" {
		return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN must include a database name")
	}
	if parsed.Query().Get("sslmode") != "" && parsed.Query().Get("sslmode") != "require" {
		return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN must use sslmode=require")
	}
	port := 5432
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil {
			return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN is invalid")
		}
	}
	return DatabaseConnection{Host: parsed.Hostname(), Port: port, User: parsed.User.Username(), Password: password, DBName: dbName, SSLMode: "require"}, nil
}

func BuildDSNDatabaseConnection(dsn pulumi.StringInput) DatabaseConnectionInputs {
	parse := func(value string) DatabaseConnection {
		connection, err := ParsePostgresDSN(value)
		if err != nil {
			panic(err)
		}
		return connection
	}
	stringField := func(selector func(DatabaseConnection) string) pulumi.StringOutput {
		return dsn.ToStringOutput().ApplyT(func(value string) string { return selector(parse(value)) }).(pulumi.StringOutput)
	}
	intField := func(selector func(DatabaseConnection) int) pulumi.IntOutput {
		return dsn.ToStringOutput().ApplyT(func(value string) int { return selector(parse(value)) }).(pulumi.IntOutput)
	}
	return DatabaseConnectionInputs{Host: stringField(func(value DatabaseConnection) string { return value.Host }), Port: intField(func(value DatabaseConnection) int { return value.Port }), User: stringField(func(value DatabaseConnection) string { return value.User }), Password: pulumi.ToSecret(stringField(func(value DatabaseConnection) string { return value.Password })).(pulumi.StringOutput), DBName: stringField(func(value DatabaseConnection) string { return value.DBName }), SSLMode: "require"}
}

type siteDatabaseResult struct {
	Connection       DatabaseConnectionInputs
	EndpointSettings pulumi.Resource
}

func siteDatabaseInputs(ctx *pulumi.Context, site, preflight pulumi.Resource, layout SiteLayout, spec SiteSpec, secrets SiteSecrets, endpointChecksum string) (siteDatabaseResult, error) {
	siteID := layout.SiteID
	if spec.Database.Mode == "docker" {
		return siteDatabaseResult{Connection: DatabaseConnectionInputs{Host: pulumi.String("postgres"), Port: pulumi.Int(5432), User: pulumi.String("sub2api"), Password: pulumi.ToSecret(pulumi.String(secrets.Database.Password)).(pulumi.StringOutput), DBName: pulumi.String("sub2api"), SSLMode: "disable"}}, nil
	}
	if spec.Database.ResourceMode == "create" {
		apiKey := pulumi.ToSecret(pulumi.String(secrets.Database.APIToken)).(pulumi.StringOutput)
		legacy := len(legacyCode2Aliases(layout, "legacy")) != 0
		var project neonProjectLike
		var regionValidation pulumi.Resource
		var connectionURI pulumi.StringOutput
		if legacy {
			providerOptions := []pulumi.ResourceOption{pulumi.Parent(site), pulumi.Aliases(legacyCode2Aliases(layout, "neon")), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.PluginDownloadURL("https://github.com/kislerdm/pulumi-neon/releases/download/v${VERSION}")}
			provider, err := registerNeonProvider(ctx, "site-"+siteID+"-neon", &neonProviderArgs{Api_key: apiKey}, providerOptions...)
			if err != nil {
				return siteDatabaseResult{}, err
			}
			projectOptions := []pulumi.ResourceOption{pulumi.Parent(site), pulumi.Aliases(legacyCode2Aliases(layout, spec.ResourcePrefix+"-neon-project")), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Protect(true), pulumi.RetainOnDelete(true), pulumi.IgnoreChanges([]string{"org_id"})}
			nativeProject, err := registerNeonProject(ctx, "site-"+siteID+"-neon-project", &neonProjectArgs{Name: pulumi.StringPtrInput(pulumi.String(ManagedNeonProjectName(spec.ResourcePrefix)))}, projectOptions...)
			if err != nil {
				return siteDatabaseResult{}, err
			}
			project = nativeProject
			connectionURI = nativeProject.ConnectionURIOutput()
			regionValidation, err = validateNeonRegion(ctx, site, siteID, nativeProject, apiKey, spec.Database.Region, preflight, endpointChecksum)
			if err != nil {
				return siteDatabaseResult{}, err
			}
		} else {
			command, err := newCommand(ctx, "site-"+siteID+"-neon-project", "bash scripts/node-env.sh npx --no-install tsx scripts/create-neon-project.ts", pulumi.StringMap{"NEON_API_KEY": apiKey, "NEON_PROJECT_NAME": pulumi.String(ManagedNeonProjectName(spec.ResourcePrefix)), "NEON_REGION": pulumi.String(spec.Database.Region), "NEON_PROJECT_STATE_FILE": pulumi.String(layout.RuntimeRoot + "/neon-project.json")}, []string{"neon-project-v1", siteID, endpointChecksum, spec.Database.Region, ManagedNeonProjectName(spec.ResourcePrefix)}, site, preflight)
			if err != nil {
				return siteDatabaseResult{}, err
			}
			project = managedNeonProjectFromCommand(command)
			connectionCommand, err := newCommand(ctx, "site-"+siteID+"-neon-connection", "bash scripts/node-env.sh npx --no-install tsx scripts/fetch-neon-connection.ts", pulumi.StringMap{"NEON_API_KEY": apiKey, "NEON_PROJECT_ID": project.ProjectID()}, []string{"neon-connection-v1", siteID, endpointChecksum}, site, preflight, command)
			if err != nil {
				return siteDatabaseResult{}, err
			}
			connectionURI = connectionCommand.Stdout.ApplyT(func(stdout string) string { return strings.TrimSpace(stdout) }).(pulumi.StringOutput)
		}
		endpointSettings, err := reconcileNeonEndpointSettings(ctx, site, siteID, project, apiKey, spec.Database.Region, spec.Database.Compute, preflight, regionValidation, endpointChecksum)
		if err != nil {
			return siteDatabaseResult{}, err
		}
		return siteDatabaseResult{Connection: BuildDSNDatabaseConnection(pulumi.ToSecret(connectionURI).(pulumi.StringOutput)), EndpointSettings: endpointSettings}, nil
	}
	return siteDatabaseResult{Connection: BuildDSNDatabaseConnection(pulumi.ToSecret(pulumi.String(secrets.Database.DSN)).(pulumi.StringOutput))}, nil
}
