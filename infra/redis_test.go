package main

import "testing"

func TestManagedUpstashDatabaseName(t *testing.T) {
	if got := ManagedUpstashDatabaseName("tenant-a", ""); got != "tenant-a-redis" { t.Fatalf("default managed name = %q", got) }
	if got := ManagedUpstashDatabaseName("tenant-a", " shared-cache "); got != "shared-cache" { t.Fatalf("explicit managed name = %q", got) }
}
