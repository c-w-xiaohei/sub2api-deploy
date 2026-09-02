package hostprovider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

func TestImportStepTokenBuildsReadOnlyStateFromVerifiedObservation(t *testing.T) {
	inputs := lifecycleInputs("edge")
	payload := frozenImportPayload(t, inputs)
	r := &recordingLifecycleTransport{}
	h := configuredLifecycleHost(t, lifecycleDependencies{transport: r, artifact: fatalArtifact(t), approve: fatalApproval(t)})
	revision := revision(t, h, inputs)
	r.outcomes = []lifecycleOutcome{response(inspected(observationFor(payload.Target, revision)))}

	got, err := h.read(t.Context(), p.ReadRequest{
		ID:  frozenImportToken(t, h.key, payload),
		Urn: lifecycleURN(payload.Resource),
	})
	if err != nil {
		t.Fatal("token import Read returned an error")
	}
	if got.ID != stableID(payload.Resource) {
		t.Fatal("token import did not return the canonical stable ID")
	}
	assertInputsPreserved(t, got.Inputs, inputs)
	if !valueAt(t, got.Inputs, "secrets").Secret() {
		t.Fatal("token import Inputs lost the secret class")
	}
	assertCheckpoint(t, got.Properties, inputs, observationFor(payload.Target, revision), revision)
	if !onlyInspect(r) || hasWrite(r) {
		t.Fatal("token import did not perform exactly one read-only inspect")
	}
	assertInspect(t, r.calls[0], inputs, revision)
}

type frozenTokenInputs struct {
	Resource hostcontract.ResourceIdentity `json:"resource"`
	Server   hostcontract.ServerTarget     `json:"server"`
	Target   hostcontract.Target           `json:"target"`
	Secrets  hostcontract.Secrets          `json:"secrets"`
}

func frozenImportPayload(t *testing.T, inputs property.Map) frozenTokenInputs {
	t.Helper()
	var payload frozenTokenInputs
	if decode(valueAt(t, inputs, "resource"), &payload.Resource) != nil ||
		decode(valueAt(t, inputs, "server"), &payload.Server) != nil ||
		decode(valueAt(t, inputs, "target"), &payload.Target) != nil ||
		decode(valueAt(t, inputs, "secrets"), &payload.Secrets) != nil {
		t.Fatal("fixture inputs did not decode")
	}
	return payload
}

func frozenImportToken(t *testing.T, key hostcontract.RevisionKey, inputs frozenTokenInputs) string {
	t.Helper()
	plain, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal("token payload did not marshal")
	}
	mac := func(domain, value []byte) []byte {
		h := hmac.New(sha256.New, key)
		_, _ = h.Write(domain)
		_, _ = h.Write(value)
		return h.Sum(nil)
	}
	pid := mac([]byte("sub2api-host/import-token/id/v1\x00"), plain)
	block, err := aes.NewCipher(mac([]byte("sub2api-host/import-token/key/v1\x00"), pid))
	if err != nil {
		t.Fatal("token cipher initialization failed")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal("token GCM initialization failed")
	}
	ciphertext := gcm.Seal(nil, mac([]byte("sub2api-host/import-token/nonce/v1\x00"), pid)[:12], plain, []byte("sub2api-host/import-token/v1"))
	return "hit1:" + base64.RawURLEncoding.EncodeToString(append(pid, ciphertext...))
}
