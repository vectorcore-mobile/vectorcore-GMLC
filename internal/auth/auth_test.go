package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
)

type fakeStore struct {
	storage.Store
	clients          map[string]storage.Client
	authzCalledFor   []string
	credentialCalled int
}

func (f *fakeStore) GetClientCredential(_ context.Context, id string) (storage.Client, error) {
	f.credentialCalled++
	c, ok := f.clients[id]
	if !ok {
		return storage.Client{}, storage.ErrNotFound
	}
	return c, nil
}
func (f *fakeStore) GetClientAuthzData(_ context.Context, id string) ([]domain.ServiceType, []string, error) {
	f.authzCalledFor = append(f.authzCalledFor, id)
	c := f.clients[id]
	return c.Services, c.TargetPrefixes, nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{clients: map[string]storage.Client{
		"good":     {ID: "good", CredentialHash: HashToken("secret"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}},
		"disabled": {ID: "disabled", CredentialHash: HashToken("secret"), Enabled: false},
	}}
}

func TestAuthenticateSuccess(t *testing.T) {
	fs := newFakeStore()
	a := New(fs)
	c, err := a.Authenticate(context.Background(), "good", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "good" || len(c.Services) != 1 || c.Services[0] != domain.ServiceImmediate || len(c.TargetPrefixes) != 1 {
		t.Fatalf("authz data not populated: %+v", c)
	}
	if len(fs.authzCalledFor) != 1 || fs.authzCalledFor[0] != "good" {
		t.Fatalf("expected GetClientAuthzData called once for good, got %v", fs.authzCalledFor)
	}
}
func TestAuthenticateFailureCasesNeverFetchAuthzData(t *testing.T) {
	for name, call := range map[string]func(*Authorizer) (storage.Client, error){
		"unknown client": func(a *Authorizer) (storage.Client, error) {
			return a.Authenticate(context.Background(), "nobody", "secret")
		},
		"wrong token": func(a *Authorizer) (storage.Client, error) {
			return a.Authenticate(context.Background(), "good", "wrong")
		},
		"disabled client": func(a *Authorizer) (storage.Client, error) {
			return a.Authenticate(context.Background(), "disabled", "secret")
		},
	} {
		t.Run(name, func(t *testing.T) {
			fs := newFakeStore()
			a := New(fs)
			_, err := call(a)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("expected ErrUnauthenticated, got %v", err)
			}
			// The variable-cost authorization lookup must never run before a
			// token has been verified — that's the whole point of the fix.
			if len(fs.authzCalledFor) != 0 {
				t.Fatalf("GetClientAuthzData must not be called on auth failure, got %v", fs.authzCalledFor)
			}
			if fs.credentialCalled != 1 {
				t.Fatalf("expected exactly one credential lookup, got %d", fs.credentialCalled)
			}
		})
	}
}
func TestAuthorize(t *testing.T) {
	a := New(newFakeStore())
	c := storage.Client{Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}}
	if err := a.Authorize(c, domain.ServiceImmediate, domain.Target{IMSI: "001010123456789"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(c, domain.ServiceImmediate, domain.Target{IMSI: "999010123456789"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for unmatched prefix, got %v", err)
	}
	if err := a.Authorize(c, domain.ServiceType("other"), domain.Target{IMSI: "001010123456789"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for unlisted service, got %v", err)
	}
}
