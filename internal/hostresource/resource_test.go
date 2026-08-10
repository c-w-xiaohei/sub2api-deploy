package hostresource

import "testing"

func TestHostTokenIsStable(t *testing.T) {
	if HostToken != "sub2api-host:index:Host" {
		t.Fatalf("HostToken = %q", HostToken)
	}
}
