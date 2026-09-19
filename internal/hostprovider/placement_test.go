package hostprovider

import (
	"strings"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostresource"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

func TestPaymentGatewayPlacementDiffRejectsOnlyDirectMoves(t *testing.T) {
	placement := paymentGatewayPlacement{}
	for _, tc := range []struct {
		name                  string
		oldServer, nextServer string
		wantChange            bool
		wantError             string
	}{
		{name: "same", oldServer: "alpha", nextServer: "alpha"},
		{name: "move", oldServer: "alpha", nextServer: "bravo", wantError: "remove and apply first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := placement.diff(t.Context(), p.DiffRequest{OldInputs: placementInputs("primary", tc.oldServer), Inputs: placementInputs("primary", tc.nextServer)})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("Diff error = %v", err)
				}
				return
			}
			if err != nil || response.HasChanges != tc.wantChange {
				t.Fatalf("Diff = %#v, %v", response, err)
			}
		})
	}
}

func TestPaymentGatewayPlacementUpdateRechecksMove(t *testing.T) {
	placement := paymentGatewayPlacement{}
	old := placementInputs("primary", "alpha")
	next := placementInputs("primary", "bravo")
	if _, err := placement.update(t.Context(), p.UpdateRequest{OldInputs: old, Inputs: next}); err == nil || !strings.Contains(err.Error(), "remove and apply first") {
		t.Fatalf("Update error = %v", err)
	}
}

func TestPaymentGatewayPlacementCheckRequiresStringMap(t *testing.T) {
	placement := paymentGatewayPlacement{}
	for name, inputs := range map[string]property.Map{
		"missing":      property.NewMap(nil),
		"unknown":      property.NewMap(map[string]property.Value{"id": property.New("primary"), "server": property.New("alpha"), "extra": property.New("no")}),
		"empty id":     placementInputs("", "alpha"),
		"empty server": placementInputs("primary", ""),
	} {
		t.Run(name, func(t *testing.T) {
			response, err := placement.check(t.Context(), p.CheckRequest{Inputs: inputs})
			if err != nil || len(response.Failures) == 0 {
				t.Fatalf("Check = %#v, %v", response, err)
			}
		})
	}
	valid := placementInputs("primary", "alpha")
	response, err := placement.check(t.Context(), p.CheckRequest{Inputs: valid})
	if err != nil || len(response.Failures) != 0 || !response.Inputs.Equals(valid) {
		t.Fatalf("valid Check = %#v, %v", response, err)
	}
}

func TestProviderDispatchesPaymentGatewayPlacementWithoutHostTransport(t *testing.T) {
	provider := New("1.0.0")
	urn := resource.URN("urn:pulumi:test::runtime::" + hostresource.PaymentGatewayPlacementToken + "::payment-gateway-placement-primary")
	inputs := placementInputs("primary", "alpha")
	checked, err := provider.Check(t.Context(), p.CheckRequest{Urn: urn, Inputs: inputs})
	if err != nil || len(checked.Failures) != 0 {
		t.Fatalf("placement Check = %#v, %v", checked, err)
	}
	created, err := provider.Create(t.Context(), p.CreateRequest{Urn: urn, Properties: inputs})
	if err != nil || created.ID != "payment-gateway-placement-primary" || !created.Properties.Equals(inputs) {
		t.Fatalf("placement Create = %#v, %v", created, err)
	}
	read, err := provider.Read(t.Context(), p.ReadRequest{Urn: urn, ID: created.ID, Inputs: inputs, Properties: created.Properties})
	if err != nil || read.ID != created.ID || !read.Inputs.Equals(inputs) || !read.Properties.Equals(inputs) {
		t.Fatalf("placement Read = %#v, %v", read, err)
	}
	if err := provider.Delete(t.Context(), p.DeleteRequest{Urn: urn, ID: created.ID, Properties: inputs}); err != nil {
		t.Fatalf("placement Delete = %v", err)
	}
}

func placementInputs(id, server string) property.Map {
	return property.NewMap(map[string]property.Value{"id": property.New(id), "server": property.New(server)})
}
