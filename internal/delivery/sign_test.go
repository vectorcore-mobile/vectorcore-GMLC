package delivery

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSignVerifiableIndependently(t *testing.T) {
	secret := []byte("webhook-secret")
	payload := []byte(`{"id":"abc","state":"completed"}`)
	got := Sign(secret, payload)
	if !strings.HasPrefix(got, "sha256=") {
		t.Fatalf("expected sha256= prefix, got %q", got)
	}
	// A receiving endpoint would recompute this exact way — the point of
	// the test is that Sign's output is independently reproducible with
	// stdlib primitives alone, not just "equal to itself".
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
func TestSignDiffersOnPayloadOrSecretChange(t *testing.T) {
	base := Sign([]byte("secret"), []byte("payload"))
	if Sign([]byte("secret"), []byte("different")) == base {
		t.Fatal("signature should change when payload changes")
	}
	if Sign([]byte("other-secret"), []byte("payload")) == base {
		t.Fatal("signature should change when secret changes")
	}
}
