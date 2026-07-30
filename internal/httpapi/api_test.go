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
