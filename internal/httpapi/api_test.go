package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/service"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

type lockedLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestRESTAuthenticationIdempotencyAndCancel(t *testing.T) {
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
	h := New(service.New(s, auth.New(s)), func() bool { return true })
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-LCS-Client-ID", "client")
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("Idempotency-Key", "same")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call(http.MethodPost, "/v1/location-requests", `{"target":{"imsi":"001010123456789"},"service_type":"immediate"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.ID == "" || out.State != string(domain.StateQueued) {
		t.Fatal("missing persistent queued request")
	}
	if w = call(http.MethodPost, "/v1/location-requests", `{"target":{"imsi":"001010123456789"},"service_type":"immediate"}`); w.Code != http.StatusOK {
		t.Fatalf("idempotency: %d", w.Code)
	}
	if w = call(http.MethodDelete, "/v1/location-requests/"+out.ID, ""); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	bad := httptest.NewRequest(http.MethodPost, "/v1/location-requests", strings.NewReader(`{}`))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, bad)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("expected auth failure, got %d", rw.Code)
	}
}

func TestSubmitLocationTypeAndPriority(t *testing.T) {
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
	h := New(service.New(s, auth.New(s)), func() bool { return true })
	call := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/location-requests", strings.NewReader(body))
		r.Header.Set("X-LCS-Client-ID", "client")
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := call("loctype-1", `{"target":{"imsi":"001010123456789"},"service_type":"immediate","location_type":"current_or_last_known","priority":5}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		LocationType string `json:"location_type"`
		Priority     *int   `json:"priority"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.LocationType != "current_or_last_known" {
		t.Fatalf("expected current_or_last_known, got %q", out.LocationType)
	}
	if out.Priority == nil || *out.Priority != 5 {
		t.Fatalf("expected priority 5, got %v", out.Priority)
	}

	w = call("loctype-2", `{"target":{"imsi":"001010123456789"},"service_type":"immediate"}`)
	var defaulted struct {
		LocationType string `json:"location_type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &defaulted); err != nil {
		t.Fatal(err)
	}
	if defaulted.LocationType != "current" {
		t.Fatalf("expected default location_type current, got %q", defaulted.LocationType)
	}

	if w = call("loctype-3", `{"target":{"imsi":"001010123456789"},"service_type":"immediate","location_type":"bogus"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid location_type, got %d %s", w.Code, w.Body.String())
	}
}

func TestSubmitBatch(t *testing.T) {
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
	h := New(service.New(s, auth.New(s)), func() bool { return true })
	call := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/location-requests", strings.NewReader(body))
		r.Header.Set("X-LCS-Client-ID", "client")
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := call("batch-1", `{"targets":[{"imsi":"001010123456789"},{"imsi":"001010123456790"},{"msisdn":"001234567"}],"service_type":"immediate"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch submit: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Requests []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Requests) != 3 {
		t.Fatalf("expected 3 requests, got %d: %s", len(out.Requests), w.Body.String())
	}
	ids := map[string]bool{}
	for _, r := range out.Requests {
		if r.ID == "" || r.State != string(domain.StateQueued) {
			t.Fatalf("malformed batch element: %+v", r)
		}
		ids[r.ID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 distinct ids, got %d", len(ids))
	}
	// Each element is independently pollable/cancellable via the existing
	// single-request endpoints — no batch-specific lookup path.
	for id := range ids {
		get := httptest.NewRequest(http.MethodGet, "/v1/location-requests/"+id, nil)
		get.Header.Set("X-LCS-Client-ID", "client")
		get.Header.Set("Authorization", "Bearer token")
		gw := httptest.NewRecorder()
		h.ServeHTTP(gw, get)
		if gw.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", id, gw.Code, gw.Body.String())
		}
	}

	// Retrying the identical batch call (same Idempotency-Key) must be
	// idempotent per target, not create duplicates, and report 200 since
	// nothing new was created.
	if w = call("batch-1", `{"targets":[{"imsi":"001010123456789"},{"imsi":"001010123456790"},{"msisdn":"001234567"}],"service_type":"immediate"}`); w.Code != http.StatusOK {
		t.Fatalf("idempotent batch replay: %d %s", w.Code, w.Body.String())
	}
	var replay struct {
		Requests []struct {
			ID string `json:"id"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	for _, r := range replay.Requests {
		if !ids[r.ID] {
			t.Fatalf("idempotent replay returned a new id %s not in original batch %v", r.ID, ids)
		}
	}

	// A malformed target anywhere in the batch rejects the whole call —
	// nothing from it should have been created.
	if w = call("batch-2", `{"targets":[{"imsi":"001010123456791"},{"imsi":"not-digits"}],"service_type":"immediate"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for partially invalid batch, got %d %s", w.Code, w.Body.String())
	}

	// target and targets together is rejected.
	if w = call("batch-3", `{"target":{"imsi":"001010123456789"},"targets":[{"imsi":"001010123456790"}],"service_type":"immediate"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for target+targets, got %d %s", w.Code, w.Body.String())
	}

	// An empty targets array falls through the same "no target supplied"
	// path an empty singular target already takes.
	if w = call("batch-4", `{"targets":[],"service_type":"immediate"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty targets, got %d %s", w.Code, w.Body.String())
	}
}
func TestSubmitQoS(t *testing.T) {
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
	h := New(service.New(s, auth.New(s)), func() bool { return true })
	call := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/location-requests", strings.NewReader(body))
		r.Header.Set("X-LCS-Client-ID", "client")
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := call("qos-1", `{"target":{"imsi":"001010123456789"},"service_type":"immediate","qos":{"class":"assured","horizontal_accuracy_meters":100,"vertical_requested":true,"response_time":"low_delay"}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		QoS map[string]any `json:"qos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.QoS == nil {
		t.Fatalf("expected qos object in response, got none: %s", w.Body.String())
	}
	if out.QoS["class"] != "assured" || out.QoS["response_time"] != "low_delay" || out.QoS["vertical_requested"] != true {
		t.Fatalf("unexpected qos round-trip: %+v", out.QoS)
	}
	if _, ok := out.QoS["horizontal_accuracy_meters"]; !ok {
		t.Fatalf("expected horizontal_accuracy_meters in qos: %+v", out.QoS)
	}

	if w = call("qos-2", `{"target":{"imsi":"001010123456789"},"service_type":"immediate","qos":{"class":"bogus"}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid qos.class, got %d %s", w.Code, w.Body.String())
	}

	w = call("qos-3", `{"target":{"imsi":"001010123456789"},"service_type":"immediate"}`)
	var noQoS struct {
		QoS map[string]any `json:"qos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &noQoS); err != nil {
		t.Fatal(err)
	}
	if noQoS.QoS != nil {
		t.Fatalf("expected no qos object when omitted, got %+v", noQoS.QoS)
	}
}

// TestWriteStatusResultShowsECGIOnlyCompletion is a regression test for a
// bug where the "result" object was gated on Latitude/Longitude being
// present, silently dropping ECGI-only (additional_information) and Polygon
// completions from the API response even though the request genuinely
// completed.
func TestWriteStatusResultShowsECGIOnlyCompletion(t *testing.T) {
	req := domain.Request{ID: "r1", State: domain.StateCompleted}
	res := domain.Result{ECGI: []byte{1, 2, 3, 4, 5, 6, 7}}
	w := httptest.NewRecorder()
	writeStatusResult(w, 200, req, res)
	var out struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Result == nil {
		t.Fatalf("expected a result object for a completed ECGI-only request, got none: %s", w.Body.String())
	}
	if _, ok := out.Result["ecgi"]; !ok {
		t.Fatalf("expected ecgi in result: %+v", out.Result)
	}
	if _, ok := out.Result["latitude"]; ok {
		t.Fatalf("did not expect latitude for an ECGI-only result: %+v", out.Result)
	}
}
func TestLeOperationalLogsAreRedacted(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)
	logs := &lockedLogBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "db.sqlite"), CheckpointPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertClient(ctx, storage.Client{ID: "client", CredentialHash: auth.HashToken("secret-token"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}}); err != nil {
		t.Fatal(err)
	}
	h := New(service.New(s, auth.New(s)), func() bool { return true })
	r := httptest.NewRequest(http.MethodPost, "/v1/location-requests", strings.NewReader(`{"target":{"imsi":"001010123456789"},"service_type":"immediate"}`))
	r.Header.Set("X-LCS-Client-ID", "client")
	r.Header.Set("Authorization", "Bearer secret-token")
	r.Header.Set("Idempotency-Key", "log-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c := httptest.NewRequest(http.MethodDelete, "/v1/location-requests/"+out.ID, nil)
	c.Header.Set("X-LCS-Client-ID", "client")
	c.Header.Set("Authorization", "Bearer secret-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, c)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	got := logs.String()
	for _, want := range []string{"Le location request accepted", "Le location request cancelled", out.ID} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in logs: %s", want, got)
		}
	}
	for _, secret := range []string{"001010123456789", "secret-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sensitive value leaked to logs: %s", got)
		}
	}
}
