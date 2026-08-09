package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

func testService(t *testing.T) *Service {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(t.TempDir(), "gmlc.db"), CheckpointPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertClient(context.Background(), storage.Client{ID: "c", CredentialHash: auth.HashToken("t"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	svc := New(s, auth.New(s))
	svc.SetSecretEncryptor(func(b []byte) ([]byte, error) { return append([]byte("enc:"), b...), nil })
	return svc
}

func TestSubmitCallbackValidation(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	base := SubmitInput{ClientID: "c", Token: "t", IdempotencyKey: "k1", Target: domain.Target{IMSI: "001010123456789"}, Service: domain.ServiceImmediate}

	in := base
	in.CallbackURL = "https://example.com/hook"
	if _, _, err := svc.Submit(ctx, in); !errors.Is(err, ErrCallbackRequiresSecret) {
		t.Fatalf("expected ErrCallbackRequiresSecret, got %v", err)
	}

	in = base
	in.IdempotencyKey = "k1b"
	in.CallbackURL, in.CallbackSecret = "not-a-url", "s"
	if _, _, err := svc.Submit(ctx, in); !errors.Is(err, ErrInvalidCallbackURL) {
		t.Fatalf("expected ErrInvalidCallbackURL, got %v", err)
	}

	in = base
	in.IdempotencyKey = "k1c"
	in.CallbackURL, in.CallbackSecret = "https://example.com/hook", "shh"
	r, created, err := svc.Submit(ctx, in)
	if err != nil || !created || r.SubscriptionID == nil {
		t.Fatalf("valid callback should succeed: %+v %v %v", r, created, err)
	}
}

// TestSubmitIdempotentReplayReusesSubscription guards against a subtle
// resource leak: an idempotent replay must never create a second
// subscription for a request that already has one — see Submit's own
// "created &&" gate on registerCallback.
func TestSubmitIdempotentReplayReusesSubscription(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	in := SubmitInput{ClientID: "c", Token: "t", IdempotencyKey: "k2", Target: domain.Target{IMSI: "001010123456789"}, Service: domain.ServiceImmediate, CallbackURL: "https://example.com/hook", CallbackSecret: "shh"}

	r1, created1, err := svc.Submit(ctx, in)
	if err != nil || !created1 || r1.SubscriptionID == nil {
		t.Fatalf("first submit: %+v %v %v", r1, created1, err)
	}
	r2, created2, err := svc.Submit(ctx, in)
	if err != nil || created2 {
		t.Fatalf("replay submit: %+v %v %v", r2, created2, err)
	}
	if r2.SubscriptionID == nil || *r2.SubscriptionID != *r1.SubscriptionID {
		t.Fatalf("replay should reuse the original subscription: r1=%v r2=%v", r1.SubscriptionID, r2.SubscriptionID)
	}
}

// TestSubmitBatchSharesOneSubscription guards the documented batch design:
// one callback destination for the whole batch, not N identical ones.
func TestSubmitBatchSharesOneSubscription(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	in := BatchSubmitInput{ClientID: "c", Token: "t", IdempotencyKey: "k3", Targets: []domain.Target{{IMSI: "001010123456789"}, {IMSI: "001010123456780"}}, Service: domain.ServiceImmediate, CallbackURL: "https://example.com/hook", CallbackSecret: "shh"}
	reqs, created, err := svc.SubmitBatch(ctx, in)
	if err != nil || len(reqs) != 2 || !created[0] || !created[1] {
		t.Fatalf("batch submit: %+v %v %v", reqs, created, err)
	}
	if reqs[0].SubscriptionID == nil || reqs[1].SubscriptionID == nil || *reqs[0].SubscriptionID != *reqs[1].SubscriptionID {
		t.Fatalf("expected both batch requests to share one subscription: %v %v", reqs[0].SubscriptionID, reqs[1].SubscriptionID)
	}
}

func TestSubmitCallbackWithoutEncryptorConfigured(t *testing.T) {
	s, err := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(t.TempDir(), "gmlc.db"), CheckpointPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertClient(context.Background(), storage.Client{ID: "c", CredentialHash: auth.HashToken("t"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}}); err != nil {
		t.Fatal(err)
	}
	svc := New(s, auth.New(s)) // no SetSecretEncryptor
	in := SubmitInput{ClientID: "c", Token: "t", IdempotencyKey: "k4", Target: domain.Target{IMSI: "001010123456789"}, Service: domain.ServiceImmediate, CallbackURL: "https://example.com/hook", CallbackSecret: "shh"}
	if _, _, err = svc.Submit(context.Background(), in); !errors.Is(err, ErrDeliveryNotConfigured) {
		t.Fatalf("expected ErrDeliveryNotConfigured, got %v", err)
	}
}
