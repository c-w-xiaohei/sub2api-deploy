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
// neon:provider:Project while the generated SDK wrapper uses the stale
// neon:resource:Project token.
type neonProject struct {
	pulumi.CustomResourceState
	Connection_uri pulumi.StringOutput `pulumi:"connection_uri"`
}

type neonProjectArgs struct {
	Name   pulumi.StringPtrInput `pulumi:"name"`
	Org_id pulumi.StringPtrInput `pulumi:"org_id"`
}

func (neonProjectArgs) ElementType() reflect.Type {
	return reflect.TypeOf((*neonProjectArgs)(nil)).Elem()
}

func registerNeonProject(ctx *pulumi.Context, name string, args *neonProjectArgs, opts ...pulumi.ResourceOption) (*neonProject, error) {
	var project neonProject
	if err := ctx.RegisterResource("neon:provider:Project", name, args, &project, opts...); err != nil {
		return nil, err
	}
	return &project, nil
}

func ManagedNeonProjectName(namespace string) string {
	return namespace + "-postgres"
}

func ParsePostgresDSN(dsn string) (DatabaseConnection, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN is invalid")
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return DatabaseConnection{}, fmt.Errorf("PostgreSQL DSN must use postgres or postgresql")
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
	return DatabaseConnection{
		Host:     parsed.Hostname(),
		Port:     port,
		User:     parsed.User.Username(),
		Password: password,
		DBName:   dbName,
		SSLMode:  "require",
	}, nil
}

func BuildDatabaseConnection(config DeploymentConfig) DatabaseConnection {
	if config.PostgresMode == "docker" {
		return DatabaseConnection{
			Host:     "postgres",
			Port:     5432,
			User:     config.PostgresUser,
			Password: config.PostgresPassword,
			DBName:   config.PostgresDB,
			SSLMode:  "disable",
		}
	}
	return DatabaseConnection{
		Host:     config.NeonHost,
		Port:     config.NeonPort,
		User:     config.NeonUser,
		Password: config.NeonPassword,
		DBName:   config.NeonDB,
		SSLMode:  "require",
	}
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
	return DatabaseConnectionInputs{
		Host:     stringField(func(value DatabaseConnection) string { return value.Host }),
		Port:     intField(func(value DatabaseConnection) int { return value.Port }),
		User:     stringField(func(value DatabaseConnection) string { return value.User }),
		Password: pulumi.ToSecret(stringField(func(value DatabaseConnection) string { return value.Password })).(pulumi.StringOutput),
		DBName:   stringField(func(value DatabaseConnection) string { return value.DBName }),
		SSLMode:  "require",
	}
}

func CreateNeonConnection(ctx *pulumi.Context, config DeploymentConfig, apiToken pulumi.StringInput) (DatabaseConnectionInputs, error) {
	provider, err := neon.NewProvider(ctx, "neon", &neon.ProviderArgs{Api_key: apiToken}, pulumi.Version("0.0.1-alpha.1"))
	if err != nil {
		return DatabaseConnectionInputs{}, err
	}
	args := &neonProjectArgs{
		Name: pulumi.StringPtr(ManagedNeonProjectName(config.ResourceNamespace)),
	}
	if config.NeonOrgID != "" {
		args.Org_id = pulumi.StringPtr(config.NeonOrgID)
	}
	project, err := registerNeonProject(ctx, config.ResourceNamespace+"-neon-project", args, pulumi.Provider(provider), pulumi.Version("0.0.1-alpha.1"))
	if err != nil {
		return DatabaseConnectionInputs{}, err
	}
	return BuildDSNDatabaseConnection(pulumi.ToSecret(project.Connection_uri).(pulumi.StringOutput)), nil
}
