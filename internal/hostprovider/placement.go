package hostprovider

import (
	"context"
	"fmt"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

type paymentGatewayPlacement struct{}

func (paymentGatewayPlacement) check(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
	inputs := req.Inputs
	var failures []p.CheckFailure
	inputs.All(func(name string, _ property.Value) bool {
		if name != "id" && name != "server" {
			failures = append(failures, p.CheckFailure{Property: name, Reason: "is not allowed"})
		}
		return true
	})
	for _, name := range []string{"id", "server"} {
		value, ok := inputs.GetOk(name)
		if !ok || value.IsNull() {
			failures = append(failures, p.CheckFailure{Property: name, Reason: "is required"})
			continue
		}
		value = unwrap(value)
		if !value.IsString() || value.AsString() == "" {
			failures = append(failures, p.CheckFailure{Property: name, Reason: "must be a nonempty string"})
		}
	}
	return p.CheckResponse{Inputs: inputs, Failures: failures}, nil
}

func (paymentGatewayPlacement) diff(_ context.Context, req p.DiffRequest) (p.DiffResponse, error) {
	if err := rejectPaymentGatewayMoves(req.OldInputs, req.Inputs); err != nil {
		return p.DiffResponse{}, err
	}
	changed := !req.OldInputs.Equals(req.Inputs)
	details := map[string]p.PropertyDiff{}
	if changed {
		details["server"] = p.PropertyDiff{Kind: p.Update, InputDiff: true}
	}
	return p.DiffResponse{HasChanges: changed, DetailedDiff: details}, nil
}

func (paymentGatewayPlacement) create(_ context.Context, req p.CreateRequest) (p.CreateResponse, error) {
	if req.DryRun {
		return p.CreateResponse{Properties: req.Properties}, nil
	}
	if string(req.Urn) == "" {
		return p.CreateResponse{}, fmt.Errorf("payment gateway placement requires a resource name")
	}
	id := string(req.Urn.Name())
	return p.CreateResponse{ID: id, Properties: req.Properties}, nil
}

func (paymentGatewayPlacement) read(_ context.Context, req p.ReadRequest) (p.ReadResponse, error) {
	return p.ReadResponse{ID: req.ID, Inputs: req.Inputs, Properties: req.Properties}, nil
}

func (paymentGatewayPlacement) update(_ context.Context, req p.UpdateRequest) (p.UpdateResponse, error) {
	if err := rejectPaymentGatewayMoves(req.OldInputs, req.Inputs); err != nil {
		return p.UpdateResponse{}, err
	}
	return p.UpdateResponse{Properties: req.Inputs}, nil
}

func (paymentGatewayPlacement) delete(context.Context, p.DeleteRequest) error { return nil }

func rejectPaymentGatewayMoves(oldInputs, inputs property.Map) error {
	oldID, oldServer, err := paymentGatewayPlacementInputs(oldInputs)
	if err != nil {
		return fmt.Errorf("invalid prior payment gateway placements")
	}
	nextID, nextServer, err := paymentGatewayPlacementInputs(inputs)
	if err != nil {
		return fmt.Errorf("invalid payment gateway placements")
	}
	if oldID != nextID {
		return fmt.Errorf("payment gateway placement identity cannot change")
	}
	if nextServer != oldServer {
		return paymentGatewayMoveError(oldID, oldServer, nextServer)
	}
	return nil
}

func paymentGatewayPlacementInputs(inputs property.Map) (string, string, error) {
	id, validID := paymentGatewayPlacementInput(inputs, "id")
	server, validServer := paymentGatewayPlacementInput(inputs, "server")
	if !validID || !validServer {
		return "", "", fmt.Errorf("invalid placement")
	}
	return id, server, nil
}

func paymentGatewayPlacementInput(inputs property.Map, name string) (string, bool) {
	value, ok := inputs.GetOk(name)
	if !ok || value.IsNull() || value.IsComputed() {
		return "", false
	}
	value = unwrap(value)
	if !value.IsString() || value.AsString() == "" {
		return "", false
	}
	return value.AsString(), true
}
