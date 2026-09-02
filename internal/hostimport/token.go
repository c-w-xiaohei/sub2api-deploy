// Package hostimport encodes the complete, closed Host import inputs.
package hostimport

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/sshcheck"
)

const (
	tokenPrefix = "hit1:"
	pidSize = 32
	maxPlaintextSize = 1 << 20
	maxEncodedSize = 2 << 20
)

var (
	aad = []byte("sub2api-host/import-token/v1")
	domainID = []byte("sub2api-host/import-token/id/v1\x00")
	domainKey = []byte("sub2api-host/import-token/key/v1\x00")
	domainNonce = []byte("sub2api-host/import-token/nonce/v1\x00")
	errInvalidToken = errors.New("invalid host import token")
)

// Inputs is deliberately closed: a token cannot carry provider-only state.
type Inputs struct {
	Resource hostcontract.ResourceIdentity `json:"resource"`
	Server hostcontract.ServerTarget `json:"server"`
	Target hostcontract.Target `json:"target"`
	Secrets hostcontract.Secrets `json:"secrets"`
}

// Encode deterministically authenticates canonical Host inputs. The per-payload
// key and nonce are derived from a keyed payload ID, so no random nonce is reused.
func Encode(key hostcontract.RevisionKey, inputs Inputs) (string, error) {
	plain, _, err := canonicalPayload(inputs)
	if err != nil || key.Validate() != nil { return "", errInvalidToken }
	pid := keyed(key, domainID, plain)
	block, err := aes.NewCipher(keyed(key, domainKey, pid))
	if err != nil { return "", errInvalidToken }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return "", errInvalidToken }
	nonce := keyed(key, domainNonce, pid)[:12]
	ciphertext := gcm.Seal(nil, nonce, plain, aad)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(append(pid, ciphertext...)), nil
}

func Decode(key hostcontract.RevisionKey, token string) (Inputs, error) {
	if key.Validate() != nil || len(token) <= len(tokenPrefix) || len(token) > len(tokenPrefix)+maxEncodedSize || !strings.HasPrefix(token, tokenPrefix) { return Inputs{}, errInvalidToken }
	encoded := token[len(tokenPrefix):]
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err == nil && base64.RawURLEncoding.EncodeToString(raw) != encoded { return Inputs{}, errInvalidToken }
	if err != nil || len(raw) < pidSize+16 || len(raw) > pidSize+maxPlaintextSize+16 { return Inputs{}, errInvalidToken }
	pid, ciphertext := raw[:pidSize], raw[pidSize:]
	block, err := aes.NewCipher(keyed(key, domainKey, pid))
	if err != nil { return Inputs{}, errInvalidToken }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return Inputs{}, errInvalidToken }
	plain, err := gcm.Open(nil, keyed(key, domainNonce, pid)[:12], ciphertext, aad)
	if err != nil || len(plain) > maxPlaintextSize { return Inputs{}, errInvalidToken }
	var inputs Inputs
	if !uniqueJSONKeys(plain) { return Inputs{}, errInvalidToken }
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&inputs) != nil || decoder.Decode(&struct{}{}) != io.EOF { return Inputs{}, errInvalidToken }
	canonical, normalized, err := canonicalPayload(inputs)
	if err != nil || !bytes.Equal(plain, canonical) || subtle.ConstantTimeCompare(pid, keyed(key, domainID, canonical)) != 1 { return Inputs{}, errInvalidToken }
	return normalized, nil
}

// encoding/json accepts duplicate object keys and keeps the last one. Tokens
// have a single canonical spelling, so reject them before decoding the payload.
func uniqueJSONKeys(value []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(value))
	var scan func() bool
	scan = func() bool {
		token, err := decoder.Token()
		if err != nil { return false }
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter { return true }
		switch delimiter {
		case '{':
			keys := map[string]struct{}{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok { return false }
				if _, exists := keys[name]; exists { return false }
				keys[name] = struct{}{}
				if !scan() { return false }
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !scan() { return false }
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !scan() { return false }
	_, err := decoder.Token()
	return err == io.EOF
}

func canonicalPayload(inputs Inputs) ([]byte, Inputs, error) {
	if inputs.Resource.Environment == "" || inputs.Resource.ServerKey == "" || inputs.Server.SSHAlias == "" || !utf8.ValidString(inputs.Resource.Environment) || !utf8.ValidString(inputs.Resource.ServerKey) || sshcheck.ValidateAlias(inputs.Server.SSHAlias) != nil { return nil, Inputs{}, errInvalidToken }
	target, secrets := hostcontract.NormalizeTargetSecrets(inputs.Target, inputs.Secrets)
	inputs.Target, inputs.Secrets = target, secrets
	if hostcontract.ValidateTarget(target, secrets) != nil { return nil, Inputs{}, errInvalidToken }
	plain, err := json.Marshal(inputs)
	if err != nil || len(plain) > maxPlaintextSize { return nil, Inputs{}, errInvalidToken }
	return plain, inputs, nil
}

func keyed(key hostcontract.RevisionKey, domain, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(domain)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
