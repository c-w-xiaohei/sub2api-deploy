package hostprotocol

import "testing"

func TestDecodeResponseRejectsNonStrictJSON(t *testing.T) {
	for _, body := range []string{`{"version":1,"error":{"category":"protocol","code":"malformed-frame"},"unknown":true}`, `{"version":1,"version":1,"error":{"category":"protocol","code":"malformed-frame"}}`, `{"version":1,"error":{"category":"protocol","code":"malformed-frame"}} {}`} {
		if _, err := DecodeResponse(appendFrame([]byte(body))); err == nil {
			t.Fatal("non-strict JSON accepted")
		}
	}
}
