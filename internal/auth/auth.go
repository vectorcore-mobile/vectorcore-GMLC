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

func (a *Authorizer) Authenticate(ctx context.Context, clientID, token string) (storage.Client, error) {
	c, err := a.store.GetClient(ctx, clientID)
	if err != nil || !c.Enabled {
		return storage.Client{}, ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare(c.CredentialHash, HashToken(token)) != 1 {
		return storage.Client{}, ErrUnauthenticated
	}
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
