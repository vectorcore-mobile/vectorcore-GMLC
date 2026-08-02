package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
)

var (
	ErrUnauthenticated = errors.New("authentication failed")
	ErrForbidden       = errors.New("not authorized")
)

type Authorizer struct{ store storage.Store }

func New(store storage.Store) *Authorizer { return &Authorizer{store: store} }
func HashToken(token string) []byte       { sum := sha256.Sum256([]byte(token)); return sum[:] }

// dummyCredentialHash is a fixed, 32-byte (matching HashToken's SHA-256
// output length) placeholder compared against when a client ID doesn't
// resolve to a real credential, so the constant-time compare below always
// runs — never skipped based on whether the client exists.
var dummyCredentialHash = HashToken("constant-time-comparison-placeholder")

// Authenticate is timing-safe against client-ID enumeration: it performs
// exactly one fixed-cost lookup (GetClientCredential) and one constant-time
// compare for every clientID, valid or not, before either can short-circuit.
// Authorization data (Services/TargetPrefixes), whose cost scales with the
// client's data, is only fetched after a token has already been verified —
// timing there leaks nothing an attacker didn't already have to know to get
// this far.
func (a *Authorizer) Authenticate(ctx context.Context, clientID, token string) (storage.Client, error) {
	c, err := a.store.GetClientCredential(ctx, clientID)
	hash := dummyCredentialHash
	valid := err == nil && c.Enabled
	if valid {
		hash = c.CredentialHash
	}
	match := subtle.ConstantTimeCompare(hash, HashToken(token)) == 1
	if !valid || !match {
		return storage.Client{}, ErrUnauthenticated
	}
	services, prefixes, err := a.store.GetClientAuthzData(ctx, clientID)
	if err != nil {
		return storage.Client{}, ErrUnauthenticated
	}
	c.Services, c.TargetPrefixes = services, prefixes
	return c, nil
}
func (a *Authorizer) Authorize(c storage.Client, service domain.ServiceType, target domain.Target) error {
	ok := false
	for _, s := range c.Services {
		if s == service {
			ok = true
		}
	}
	if !ok {
		return ErrForbidden
	}
	for _, p := range c.TargetPrefixes {
		if strings.HasPrefix(target.Value(), p) {
			return nil
		}
	}
	return ErrForbidden
}
