package main

import "testing"

func TestManagedNeonProjectName(t *testing.T) {
	if got := ManagedNeonProjectName("tenant-a"); got != "tenant-a-postgres" {
		t.Fatalf("ManagedNeonProjectName() = %q, want tenant-a-postgres", got)
	}
}

func TestParsePostgresDSN(t *testing.T) {
	got, err := ParsePostgresDSN("postgresql://sub2api:p%40ss@ep.example.neon.tech:5432/sub2api?sslmode=require")
	if err != nil {
		t.Fatalf("ParsePostgresDSN() error = %v", err)
	}
	want := DatabaseConnection{Host: "ep.example.neon.tech", Port: 5432, User: "sub2api", Password: "p@ss", DBName: "sub2api", SSLMode: "require"}
	if got != want {
		t.Fatalf("ParsePostgresDSN() = %+v, want %+v", got, want)
	}
}

func TestParsePostgresDSNRequiresDatabaseAndTLS(t *testing.T) {
	for _, dsn := range []string{
		"postgresql://user:pass@host/",
		"postgresql://user:pass@host/db?sslmode=disable",
	} {
		if _, err := ParsePostgresDSN(dsn); err == nil {
			t.Fatalf("ParsePostgresDSN(%q) succeeded, want error", dsn)
		}
	}
}

func TestBuildDatabaseConnection(t *testing.T) {
	input := validDeploymentInput()
	config, err := ValidateDeploymentConfig(input)
	if err != nil {
		t.Fatalf("ValidateDeploymentConfig() error = %v", err)
	}
	docker := BuildDatabaseConnection(config)
	if docker != (DatabaseConnection{Host: "postgres", Port: 5432, User: "sub2api", Password: "postgres-secret", DBName: "sub2api", SSLMode: "disable"}) {
		t.Fatalf("docker connection = %+v", docker)
	}

	input.PostgresMode = "neon"
	input.PostgresPassword = ""
	input.NeonHost = "ep.example.neon.tech"
	input.NeonPassword = "neon-secret"
	config, err = ValidateDeploymentConfig(input)
	if err != nil {
		t.Fatalf("ValidateDeploymentConfig(neon) error = %v", err)
	}
	neon := BuildDatabaseConnection(config)
	if neon.Host != "ep.example.neon.tech" || neon.Port != 5432 || neon.Password != "neon-secret" || neon.SSLMode != "require" {
		t.Fatalf("neon connection = %+v", neon)
	}
}

func TestBuildConnectionsCoverAllDataModeCombinations(t *testing.T) {
	cases := []struct {
		postgresMode string
		redisMode    string
		databaseHost string
		redisHost    string
		sslMode      string
		redisTLS     bool
	}{
		{"docker", "docker", "postgres", "redis", "disable", false},
		{"neon", "docker", "ep.example.neon.tech", "redis", "require", false},
		{"docker", "upstash", "postgres", "upstash.example.com", "disable", true},
		{"neon", "upstash", "ep.example.neon.tech", "upstash.example.com", "require", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.postgresMode+"/"+testCase.redisMode, func(t *testing.T) {
			input := validDeploymentInput()
			input.PostgresMode = testCase.postgresMode
			input.RedisMode = testCase.redisMode
			if input.PostgresMode == "neon" {
				input.PostgresPassword = ""
				input.NeonHost = "ep.example.neon.tech"
				input.NeonPassword = "neon-secret"
			}
			if input.RedisMode == "upstash" {
				input.RedisPassword = ""
				input.UpstashHost = "upstash.example.com"
				input.UpstashPort = intPointer(6380)
				input.UpstashPassword = "upstash-secret"
			}
			config, err := ValidateDeploymentConfig(input)
			if err != nil {
				t.Fatalf("ValidateDeploymentConfig() error = %v", err)
			}
			database := BuildDatabaseConnection(config)
			redis := BuildRedisConnection(config)
			if database.Host != testCase.databaseHost || database.SSLMode != testCase.sslMode {
				t.Fatalf("database = %+v", database)
			}
			if redis.Host != testCase.redisHost || redis.EnableTLS != testCase.redisTLS {
				t.Fatalf("redis = %+v", redis)
			}
		})
	}
}
