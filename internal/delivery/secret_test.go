package delivery

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}
func TestEncryptDecryptSecretRoundTrip(t *testing.T) {
	key := testKey(t)
	plaintext := []byte("super-secret-webhook-key")
	ct, err := EncryptSecret(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext must not equal plaintext")
	}
	got, err := DecryptSecret(key, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip mismatch: got %q, want %q", got, plaintext)
	}
}
func TestEncryptSecretNondeterministic(t *testing.T) {
	// Each call must use a fresh random nonce — two encryptions of the same
	// plaintext under the same key must not produce identical ciphertext,
	// or a repeated secret would be detectable/fingerprintable at rest.
	key := testKey(t)
	a, err := EncryptSecret(key, []byte("same-secret"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncryptSecret(key, []byte("same-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext")
	}
}
func TestDecryptSecretWrongKeyFails(t *testing.T) {
	ct, err := EncryptSecret(testKey(t), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecryptSecret(testKey(t), ct); err == nil {
		t.Fatal("expected decryption with the wrong key to fail")
	}
}
func TestDecryptSecretTamperedCiphertextFails(t *testing.T) {
	key := testKey(t)
	ct, err := EncryptSecret(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err = DecryptSecret(key, tampered); err == nil {
		t.Fatal("expected GCM authentication to reject tampered ciphertext")
	}
}
func TestInvalidKeySize(t *testing.T) {
	if _, err := EncryptSecret([]byte("too-short"), []byte("x")); err != ErrInvalidKey {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
	if _, err := DecryptSecret([]byte("too-short"), []byte("x")); err != ErrInvalidKey {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
}
