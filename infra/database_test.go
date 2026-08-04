package main

import "testing"

func TestManagedNeonProjectName(t *testing.T) {
	if got := ManagedNeonProjectName("tenant-a"); got != "tenant-a-postgres" { t.Fatalf("ManagedNeonProjectName() = %q", got) }
}

func TestParsePostgresDSN(t *testing.T) {
	got, err := ParsePostgresDSN("postgresql://sub2api:p%40ss@ep.example.neon.tech:5432/sub2api?sslmode=require")
	if err != nil { t.Fatalf("ParsePostgresDSN() error = %v", err) }
	want := DatabaseConnection{Host: "ep.example.neon.tech", Port: 5432, User: "sub2api", Password: "p@ss", DBName: "sub2api", SSLMode: "require"}
	if got != want { t.Fatalf("ParsePostgresDSN() = %+v, want %+v", got, want) }
}

func TestParsePostgresDSNRequiresDatabaseAndTLS(t *testing.T) {
	for _, dsn := range []string{"postgresql://user:pass@host/", "postgresql://user:pass@host/db?sslmode=disable"} {
		if _, err := ParsePostgresDSN(dsn); err == nil { t.Fatalf("ParsePostgresDSN(%q) succeeded", dsn) }
	}
}
