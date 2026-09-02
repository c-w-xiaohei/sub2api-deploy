package hostimport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

var tokenKey = hostcontract.RevisionKey([]byte("01234567890123456789012345678901"))

func TestTokenRoundTripDeterministicAndPayloadBound(t *testing.T) {
	inputs := tokenInputs()
	token, err := Encode(tokenKey, inputs)
	if err != nil { t.Fatal(err) }
	got, err := Decode(tokenKey, token)
	if err != nil || !deepEqual(got, inputs) { t.Fatalf("round trip failed: %#v %v", got, err) }
	retry, err := Encode(tokenKey, inputs)
	if err != nil || retry != token { t.Fatal("identical payload did not retain a stable token") }
	different := inputs; different.Server.SSHAlias = "other"
	other, err := Encode(tokenKey, different)
	if err != nil || other == token { t.Fatal("different payload did not receive a distinct token") }
}

func TestTokenRejectsAdversarialEnvelopeBeforeExposingPayload(t *testing.T) {
	token, err := Encode(tokenKey, tokenInputs())
	if err != nil { t.Fatal(err) }
	for _, value := range []string{
		"", "hit0:" + token[len(tokenPrefix):], "hit1:", "hit1:not-base64!", "hit1:AA==", "hit1:A", token + "=", token + "x", token[:len(token)-1],
		tokenPrefix + strings.Repeat("a", maxEncodedSize+1),
	} {
		assertInvalidToken(t, value)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, tokenPrefix))
	for _, index := range []int{0, pidSize, len(raw)-1} {
		changed := append([]byte(nil), raw...); changed[index] ^= 1
		assertInvalidToken(t, tokenPrefix+base64.RawURLEncoding.EncodeToString(changed))
	}
	for _, changed := range [][]byte{raw[:pidSize-1], append(raw, 0), raw[:len(raw)-1]} {
		assertInvalidToken(t, tokenPrefix+base64.RawURLEncoding.EncodeToString(changed))
	}
	if _, err := Decode(hostcontract.RevisionKey([]byte("abcdefghijklmnopqrstuvwxyz012345")), token); err == nil { t.Fatal("wrong revision key accepted") }
}

func TestTokenRejectsNonCanonicalAndInvalidPlaintext(t *testing.T) {
	for _, plain := range [][]byte{
		[]byte(`{"server":{"sshAlias":"edge"},"resource":{"environment":"canary","serverKey":"edge"},"target":{"releaseArtifact":"image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"secrets":{}}`),
		[]byte(`{"resource":{"environment":"canary","serverKey":"edge"},"resource":{"environment":"canary","serverKey":"edge"},"server":{"sshAlias":"edge"},"target":{"releaseArtifact":"image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"secrets":{}}`),
		[]byte(`{"resource":{"environment":"canary","serverKey":"edge"},"server":{"sshAlias":"edge"},"target":{"releaseArtifact":"image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"secrets":{},"extra":true}`),
		[]byte(`{"resource":{"environment":"canary","serverKey":"edge"},"server":{"sshAlias":"edge"},"target":{"releaseArtifact":"image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"secrets":{}} trailing`),
		[]byte(`{"resource":{"environment":"","serverKey":"edge"},"server":{"sshAlias":"edge"},"target":{"releaseArtifact":"image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"secrets":{}}`),
		[]byte(`{"resource":{"environment":"canary","serverKey":"edge"},"server":{"sshAlias":"bad alias"},"target":{"releaseArtifact":"image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"secrets":{}}`),
		[]byte(`{"resource":{"environment":"canary","serverKey":"edge"},"server":{"sshAlias":"edge"},"target":{},"secrets":{}}`),
		[]byte(`{"resource":{"environment":"canary","serverKey":"edge"},"server":{"sshAlias":"edge"},"target":{"releaseArtifact":"image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"secrets":{"apps":{"api":{"jwtSecret":"CANARY"}},"apps":{}}}`),
	} { assertInvalidToken(t, encryptPlaintext(t, plain)) }
}

func TestTokenNormalizesOrderAndNilEquivalenceAtBounds(t *testing.T) {
	first := tokenInputs()
	first.Target.Apps = []hostcontract.AppTarget{
		{ID: "bravo", Image: "bravo", Hostname: "bravo.example", ReadinessPath: "/ready"},
		{ID: "alpha", Image: "alpha", Hostname: "alpha.example", ReadinessPath: "/ready"},
	}
	second := tokenInputs()
	second.Target.Apps = []hostcontract.AppTarget{
		{ID: "alpha", Image: "alpha", Hostname: "alpha.example", ReadinessPath: "/ready"},
		{ID: "bravo", Image: "bravo", Hostname: "bravo.example", ReadinessPath: "/ready"},
	}
	orderedToken, err := Encode(tokenKey, first)
	if err != nil { t.Fatal(err) }
	reorderedToken, err := Encode(tokenKey, second)
	if err != nil || orderedToken != reorderedToken { t.Fatal("equivalent ordered payloads were not canonical") }

	first = tokenInputs()
	second = tokenInputs()
	second.Target.Apps = []hostcontract.AppTarget{}
	second.Target.DataServices = []hostcontract.LocalDataServiceTarget{}
	second.Target.Connectors = []hostcontract.TunnelConnectorTarget{}
	second.Secrets.Apps = map[string]hostcontract.AppSecrets{}
	firstToken, err := Encode(tokenKey, first); if err != nil { t.Fatal(err) }
	secondToken, err := Encode(tokenKey, second); if err != nil || firstToken != secondToken { t.Fatal("nil-equivalent payload was not canonical") }
	if _, err := Encode(hostcontract.RevisionKey([]byte("short")), first); err == nil { t.Fatal("short key accepted") }
	assertInvalidToken(t, tokenPrefix+strings.Repeat("a", maxEncodedSize+1))
	overlong := tokenInputs()
	overlong.Target.ReleaseArtifact = strings.Repeat("x", maxPlaintextSize)
	assertInvalidEncode(t, overlong)
	invalidIdentity := tokenInputs()
	invalidIdentity.Resource.Environment = ""
	assertInvalidEncode(t, invalidIdentity)
	invalidAlias := tokenInputs()
	invalidAlias.Server.SSHAlias = "bad alias"
	assertInvalidEncode(t, invalidAlias)
	invalidTarget := tokenInputs()
	invalidTarget.Target.ReleaseArtifact = ""
	assertInvalidEncode(t, invalidTarget)
}

func tokenInputs() Inputs { return Inputs{Resource: hostcontract.ResourceIdentity{Environment: "canary", ServerKey: "edge"}, Server: hostcontract.ServerTarget{SSHAlias: "edge"}, Target: hostcontract.Target{ReleaseArtifact: "image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Secrets: hostcontract.Secrets{}} }
func deepEqual(a, b Inputs) bool { a.Target, a.Secrets = hostcontract.NormalizeTargetSecrets(a.Target, a.Secrets); b.Target, b.Secrets = hostcontract.NormalizeTargetSecrets(b.Target, b.Secrets); return string(mustJSON(a)) == string(mustJSON(b)) }
func mustJSON(value any) []byte { valueJSON, _ := json.Marshal(value); return valueJSON }
func assertInvalidToken(t *testing.T, value string) { t.Helper(); if _, err := Decode(tokenKey, value); err == nil || strings.Contains(err.Error(), "CANARY") { t.Fatalf("invalid token accepted or exposed data: %v", err) } }
func assertInvalidEncode(t *testing.T, inputs Inputs) { t.Helper(); if _, err := Encode(tokenKey, inputs); err == nil || strings.Contains(err.Error(), "CANARY") { t.Fatalf("invalid token inputs accepted or exposed data: %v", err) } }
func encryptPlaintext(t *testing.T, plain []byte) string { t.Helper(); pid := testMAC(domainID, plain); block, err := aes.NewCipher(testMAC(domainKey, pid)); if err != nil { t.Fatal(err) }; gcm, err := cipher.NewGCM(block); if err != nil { t.Fatal(err) }; raw := append(pid, gcm.Seal(nil, testMAC(domainNonce, pid)[:12], plain, aad)...); return tokenPrefix + base64.RawURLEncoding.EncodeToString(raw) }
func testMAC(domain, payload []byte) []byte { mac := hmac.New(sha256.New, tokenKey); _, _ = mac.Write(domain); _, _ = mac.Write(payload); return mac.Sum(nil) }
