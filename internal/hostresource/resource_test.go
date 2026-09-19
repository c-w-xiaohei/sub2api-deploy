package hostresource

import "testing"

func TestHostTokenIsStable(t *testing.T) {
	if HostToken != "sub2api-host:index:Host" {
		t.Fatalf("HostToken = %q", HostToken)
	}
}

func TestPaymentGatewayPlacementTokenIsStable(t *testing.T) {
	if PaymentGatewayPlacementToken != "sub2api-host:index:PaymentGatewayPlacement" {
		t.Fatalf("PaymentGatewayPlacementToken = %q", PaymentGatewayPlacementToken)
	}
}
