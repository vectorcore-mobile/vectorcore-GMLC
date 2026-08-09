package delivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

func testStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(t.TempDir(), "gmlc.db"), Synchronous: "FULL", CheckpointPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertClient(context.Background(), storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}
func newSubscription(t *testing.T, s *sqlite.Store, key []byte, secret, url string) storage.Subscription {
	t.Helper()
	ct, err := EncryptSecret(key, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateSubscription(context.Background(), "c", url, ct)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	d := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(d) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func deliveryState(t *testing.T, s *sqlite.Store, id string) (storage.DeliveryState, int) {
	t.Helper()
	d, err := s.GetDelivery(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.State, d.AttemptCount
}

func TestWorkerDeliversAndSignsPayload(t *testing.T) {
	key := testKey(t)
	secret := "shh-its-a-secret"
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignatureHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := testStore(t)
	sub := newSubscription(t, s, key, secret, srv.URL)
	payload := []byte(`{"id":"req-1","state":"completed"}`)
	d, err := s.CreateDelivery(context.Background(), sub.ID, payload)
	if err != nil {
		t.Fatal(err)
	}

	w := New(s, Config{EncryptionKey: key, RequestTimeout: 2 * time.Second})
	w.Start(context.Background())
	defer func() { _ = w.Close(context.Background()) }()
	w.Notify()

	waitFor(t, 2*time.Second, func() bool {
		state, _ := deliveryState(t, s, d.ID)
		return state == storage.DeliveryDelivered
	})
	if string(gotBody) != string(payload) {
		t.Fatalf("delivered body = %q, want %q", gotBody, payload)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("signature = %q, want %q", gotSig, want)
	}
}

func TestWorkerRetriesThenSucceeds(t *testing.T) {
	key := testKey(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := testStore(t)
	sub := newSubscription(t, s, key, "secret", srv.URL)
	d, err := s.CreateDelivery(context.Background(), sub.ID, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	w := New(s, Config{EncryptionKey: key, MaxAttempts: 5, RetryBackoffMin: 20 * time.Millisecond, RetryBackoffMax: 50 * time.Millisecond, RequestTimeout: time.Second})
	w.Start(context.Background())
	defer func() { _ = w.Close(context.Background()) }()
	w.Notify()

	waitFor(t, 3*time.Second, func() bool {
		state, _ := deliveryState(t, s, d.ID)
		return state == storage.DeliveryDelivered
	})
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected exactly 3 HTTP attempts (2 failures + 1 success), got %d", got)
	}
	_, dbAttempts := deliveryState(t, s, d.ID)
	if dbAttempts != 3 {
		t.Fatalf("expected attempt_count=3, got %d", dbAttempts)
	}
}

func TestWorkerGivesUpAfterMaxAttempts(t *testing.T) {
	key := testKey(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := testStore(t)
	sub := newSubscription(t, s, key, "secret", srv.URL)
	d, err := s.CreateDelivery(context.Background(), sub.ID, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	w := New(s, Config{EncryptionKey: key, MaxAttempts: 3, RetryBackoffMin: 10 * time.Millisecond, RetryBackoffMax: 30 * time.Millisecond, RequestTimeout: time.Second})
	w.Start(context.Background())
	defer func() { _ = w.Close(context.Background()) }()
	w.Notify()

	waitFor(t, 3*time.Second, func() bool {
		state, _ := deliveryState(t, s, d.ID)
		return state == storage.DeliveryFailed
	})
	// Give any (incorrect) further retry a chance to happen before asserting
	// none did.
	time.Sleep(100 * time.Millisecond)
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected exactly MaxAttempts=3 HTTP attempts, got %d", got)
	}
}

// subscriptionLookupFailsStore wraps a real store but forces
// GetSubscription to fail, so the worker's own defensive
// subscription-lookup-failure path (Worker.one, on a GetSubscription error)
// can be exercised directly. The database's foreign key constraint already
// guarantees CreateDelivery can never reference a nonexistent subscription
// through the public API — proven by TestWorkerUnknownSubscriptionFailsDeliveryImmediately's
// FK rejection before this wrapper existed — so this simulates the only
// other realistic cause of a GetSubscription failure (a transient storage
// error), which the worker still needs to handle without hanging forever.
type subscriptionLookupFailsStore struct{ storage.Store }

func (subscriptionLookupFailsStore) GetSubscription(context.Context, string) (storage.Subscription, error) {
	return storage.Subscription{}, errors.New("simulated storage failure")
}

func TestWorkerSubscriptionLookupFailureFailsDeliveryImmediately(t *testing.T) {
	key := testKey(t)
	s := testStore(t)
	sub := newSubscription(t, s, key, "secret", "http://unused.example")
	d, err := s.CreateDelivery(context.Background(), sub.ID, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	w := New(subscriptionLookupFailsStore{s}, Config{EncryptionKey: key, RequestTimeout: time.Second})
	w.Start(context.Background())
	defer func() { _ = w.Close(context.Background()) }()
	w.Notify()

	waitFor(t, 2*time.Second, func() bool {
		state, _ := deliveryState(t, s, d.ID)
		return state == storage.DeliveryFailed
	})
}
