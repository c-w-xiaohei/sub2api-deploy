package main

import "testing"

func TestManagedUpstashDatabaseName(t *testing.T) {
	if got := ManagedUpstashDatabaseName("tenant-a", ""); got != "tenant-a-redis" {
		t.Fatalf("default managed name = %q", got)
	}
	if got := ManagedUpstashDatabaseName("tenant-a", " shared-cache "); got != "shared-cache" {
		t.Fatalf("explicit managed name = %q", got)
	}
}

func TestBuildRedisConnection(t *testing.T) {
	input := validDeploymentInput()
	input.RedisMode = "upstash"
	input.RedisPassword = ""
	input.UpstashHost = "upstash.example.com"
	input.UpstashPort = intPointer(6380)
	input.UpstashUsername = "default"
	input.UpstashPassword = "upstash-secret"
	config, err := ValidateDeploymentConfig(input)
	if err != nil {
		t.Fatalf("ValidateDeploymentConfig() error = %v", err)
	}
	got := BuildRedisConnection(config)
	want := RedisConnection{Host: "upstash.example.com", Port: 6380, Username: "default", Password: "upstash-secret", DB: 0, EnableTLS: true}
	if got != want {
		t.Fatalf("BuildRedisConnection() = %+v, want %+v", got, want)
	}
}
