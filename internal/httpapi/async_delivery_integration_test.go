package httpapi_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/delivery"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/httpapi"
	"github.com/vectorcore/gmlc/internal/orchestrator"
	"github.com/vectorcore/gmlc/internal/service"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

// fakeResolver/fakeProvider mirror internal/orchestrator's own test doubles
// (unexported there, so redefined here) — this test exercises the full
// API-ASYNC path end to end: REST submit -> orchestrator completion ->
// completion hook -> delivery outbox -> delivery.Worker HTTP POST, exactly
// as cmd/gmlc/main.go wires them, just with a fake Diameter round trip so
// the test is fast and deterministic.
type fakeResolver struct{}

func (fakeResolver) ResolveServingNode(context.Context, domain.Target) (domain.ServingNode, error) {
	return domain.ServingNode{Type: "mme", MMEHost: "mme.example", MMERealm: "example"}, nil
}

type fakeProvider struct{}

func (fakeProvider) ProvideLocation(context.Context, domain.ServingNode, domain.LocationRequest) (domain.PositioningResult, error) {
	return domain.PositioningResult{Kind: "location_estimate", Position: &domain.GeographicPosition{Shape: "ellipsoid_point", Latitude: 45, Longitude: 90}}, nil
}

type deliveredCallback struct {
	body []byte
	sig  string
}

func TestAsyncCallbackDeliveredAfterCompletion(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "db.sqlite"), CheckpointPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertClient(ctx, storage.Client{ID: "client", CredentialHash: auth.HashToken("token"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}}); err != nil {
		t.Fatal(err)
	}

	got := make(chan deliveredCallback, 1)
	callbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- deliveredCallback{body: b, sig: r.Header.Get(delivery.SignatureHeader)}
		w.WriteHeader(http.StatusOK)
	}))
	defer callbackSrv.Close()

	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal(err)
	}

	svc := service.New(s, auth.New(s))
	svc.SetSecretEncryptor(func(secret []byte) ([]byte, error) { return delivery.EncryptSecret(key, secret) })

	worker := orchestrator.New(s, fakeResolver{}, fakeProvider{})
	worker.Start(ctx)
	defer func() { _ = worker.Close(ctx) }()
	svc.SetQueuedHook(worker.Notify)

	dw := delivery.New(s, delivery.Config{EncryptionKey: key, RequestTimeout: 2 * time.Second, RetryBackoffMin: 20 * time.Millisecond, RetryBackoffMax: 50 * time.Millisecond})
	dw.Start(ctx)
	defer func() { _ = dw.Close(ctx) }()

	// Mirrors cmd/gmlc/main.go's own SetCompletionHook wiring exactly.
	worker.SetCompletionHook(func(r domain.Request, v domain.Result) {
		if r.SubscriptionID == nil {
			return
		}
		payload, e := json.Marshal(httpapi.RequestJSON(r, v))
		if e != nil {
			return
		}
		if _, e = s.CreateDelivery(context.Background(), *r.SubscriptionID, payload); e == nil {
			dw.Notify()
		}
	})

	h := httpapi.New(svc, func() bool { return true })
	body := `{"target":{"imsi":"001010123456789"},"service_type":"immediate","callback_url":"` + callbackSrv.URL + `","callback_secret":"shh-its-a-secret"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/location-requests", strings.NewReader(body))
	req.Header.Set("X-LCS-Client-ID", "client")
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "async-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var submitted struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &submitted); e != nil || submitted.ID == "" {
		t.Fatalf("bad submit response: %v %s", e, w.Body.String())
	}

	var cb deliveredCallback
	select {
	case cb = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("callback was never delivered")
	}

	var payload map[string]any
	if e := json.Unmarshal(cb.body, &payload); e != nil {
		t.Fatalf("callback body not JSON: %v", e)
	}
	if payload["id"] != submitted.ID || payload["state"] != string(domain.StateCompleted) {
		t.Fatalf("unexpected callback payload: %s", cb.body)
	}
	result, ok := payload["result"].(map[string]any)
	if !ok || result["latitude"] != 45.0 || result["longitude"] != 90.0 {
		t.Fatalf("callback payload missing expected result: %s", cb.body)
	}

	mac := hmac.New(sha256.New, []byte("shh-its-a-secret"))
	mac.Write(cb.body)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if cb.sig != wantSig {
		t.Fatalf("signature = %q, want %q", cb.sig, wantSig)
	}
}

func TestSubmitCallbackRequiresSecretAndValidURL(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "db.sqlite"), CheckpointPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertClient(ctx, storage.Client{ID: "client", CredentialHash: auth.HashToken("token"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}}); err != nil {
		t.Fatal(err)
	}
	// No SetSecretEncryptor — this GMLC has delivery disabled.
	h := httpapi.New(service.New(s, auth.New(s)), func() bool { return true })
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/location-requests", strings.NewReader(body))
		req.Header.Set("X-LCS-Client-ID", "client")
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("Idempotency-Key", "x")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	// callback_url without callback_secret is rejected.
	if w := call(`{"target":{"imsi":"001010123456789"},"service_type":"immediate","callback_url":"https://example.com/hook"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for callback_url without secret, got %d %s", w.Code, w.Body.String())
	}
	// A malformed URL is rejected.
	if w := call(`{"target":{"imsi":"001010123456789"},"service_type":"immediate","callback_url":"not-a-url","callback_secret":"s"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed callback_url, got %d %s", w.Code, w.Body.String())
	}
	// A well-formed callback on a GMLC without delivery configured is
	// rejected with a distinct, non-generic reason.
	w := call(`{"target":{"imsi":"001010123456789"},"service_type":"immediate","callback_url":"https://example.com/hook","callback_secret":"s"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unconfigured delivery, got %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Error.Code != "delivery_not_configured" {
		t.Fatalf("expected delivery_not_configured, got %+v (%s)", out, w.Body.String())
	}
}
