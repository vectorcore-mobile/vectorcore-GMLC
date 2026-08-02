package sqlite

import (
	"context"
	"fmt"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(context.Background(), Config{Path: filepath.Join(t.TempDir(), "gmlc.db"), Synchronous: "FULL", CheckpointPages: 10})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}
func TestMigrateAndRequestLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"001"}}); e != nil {
		t.Fatal(e)
	}
	r, created, e := s.CreateRequest(ctx, domain.Request{ID: "r", ClientID: "c", IdempotencyKey: "k", Service: domain.ServiceImmediate, TargetKind: "imsi", TargetValue: "00101", State: domain.StateQueued})
	if e != nil || !created {
		t.Fatalf("create: %v %v", created, e)
	}
	_, created, e = s.CreateRequest(ctx, domain.Request{ID: "new", ClientID: "c", IdempotencyKey: "k", Service: domain.ServiceImmediate, TargetKind: "imsi", TargetValue: "00101", State: domain.StateQueued})
	if e != nil || created {
		t.Fatal("idempotency failed")
	}
	if _, e = s.TransitionRequest(ctx, r.ID, domain.StateCompleted, ""); e != storage.ErrConflict {
		t.Fatalf("expected conflict: %v", e)
	}
	if _, e = s.TransitionRequest(ctx, r.ID, domain.StateCancelled, "x"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.TransitionRequest(ctx, r.ID, domain.StateCompleted, ""); e != storage.ErrConflict {
		t.Fatal("terminal request revived")
	}
}
func TestRecovery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true})
	_, _, _ = s.CreateRequest(ctx, domain.Request{ID: "r", ClientID: "c", IdempotencyKey: "k", State: domain.StateResolving})
	if e := s.Recover(ctx, time.Now()); e != nil {
		t.Fatal(e)
	}
	r, e := s.GetRequest(ctx, "r")
	if e != nil || r.State != domain.StateQueued {
		t.Fatalf("%v %v", r.State, e)
	}
}
func TestClaimCompleteAndRetry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true})
	_, _, _ = s.CreateRequest(ctx, domain.Request{ID: "r", ClientID: "c", IdempotencyKey: "k", State: domain.StateQueued})
	r, ok, e := s.ClaimNextQueued(ctx, time.Now())
	if e != nil || !ok || r.State != domain.StateResolving {
		t.Fatalf("%+v %v %v", r, ok, e)
	}
	if e = s.SaveServingNodeAndLocate(ctx, "r", domain.ServingNode{Type: "mme", MMEHost: "mme", MMERealm: "realm", Source: "test", ResolvedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	lat, lon := 1.0, 2.0
	if e = s.CompleteRequest(ctx, "r", domain.Result{RequestID: "r", RawGAD: []byte{0, 0, 0, 0, 0, 0, 0}, Shape: "ellipsoid_point", Latitude: &lat, Longitude: &lon, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	v, e := s.GetResult(ctx, "r")
	if e != nil || v.Latitude == nil || *v.Latitude != 1 {
		t.Fatalf("%+v %v", v, e)
	}
}
func TestCompleteRequestEllipseFieldsRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true})
	_, _, _ = s.CreateRequest(ctx, domain.Request{ID: "r", ClientID: "c", IdempotencyKey: "k", State: domain.StateQueued})
	if _, _, e := s.ClaimNextQueued(ctx, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := s.SaveServingNodeAndLocate(ctx, "r", domain.ServingNode{Type: "mme", MMEHost: "mme", MMERealm: "realm", Source: "test", ResolvedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	lat, lon, semiMajor, semiMinor, orientation := 1.0, 2.0, 10.0, 5.0, 90.0
	confidence := uint32(67)
	if e := s.CompleteRequest(ctx, "r", domain.Result{RequestID: "r", RawGAD: []byte{3, 0, 0, 0, 0, 0, 0, 1, 2, 90, 50}, Shape: "ellipsoid_point_uncertainty_ellipse", Latitude: &lat, Longitude: &lon, SemiMajorMeters: &semiMajor, SemiMinorMeters: &semiMinor, OrientationDegrees: &orientation, ConfidencePercent: &confidence, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	v, e := s.GetResult(ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	if v.SemiMajorMeters == nil || *v.SemiMajorMeters != 10 || v.SemiMinorMeters == nil || *v.SemiMinorMeters != 5 || v.OrientationDegrees == nil || *v.OrientationDegrees != 90 || v.ConfidencePercent == nil || *v.ConfidencePercent != 67 {
		t.Fatalf("ellipse fields did not round-trip: %+v", v)
	}
}
func TestCompleteRequestPLAMetadataRoundTrips(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true})
	_, _, _ = s.CreateRequest(ctx, domain.Request{ID: "r", ClientID: "c", IdempotencyKey: "k", State: domain.StateQueued})
	if _, _, e := s.ClaimNextQueued(ctx, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := s.SaveServingNodeAndLocate(ctx, "r", domain.ServingNode{Type: "mme", MMEHost: "mme", MMERealm: "realm", Source: "test", ResolvedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	age, accuracy := uint32(5), uint32(1)
	if e := s.CompleteRequest(ctx, "r", domain.Result{RequestID: "r", ECGI: []byte{1, 2, 3, 4, 5, 6, 7}, AgeOfLocationEstimate: &age, AccuracyFulfilment: &accuracy, RawVelocityEstimate: []byte{9, 9}, EUTRANPositioningData: []byte{8, 8}, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	v, e := s.GetResult(ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	if v.AgeOfLocationEstimate == nil || *v.AgeOfLocationEstimate != 5 || v.AccuracyFulfilment == nil || *v.AccuracyFulfilment != 1 {
		t.Fatalf("age/accuracy did not round-trip: %+v", v)
	}
	if string(v.RawVelocityEstimate) != "\x09\x09" || string(v.EUTRANPositioningData) != "\x08\x08" {
		t.Fatalf("raw velocity/eutran data did not round-trip: %+v", v)
	}
}
func TestClientLCSClientTypeRoundTrips(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if e := s.UpsertClient(ctx, storage.Client{ID: "emergency", CredentialHash: []byte("h"), Enabled: true, LCSClientType: domain.ClientTypeEmergencyServices}); e != nil {
		t.Fatal(e)
	}
	c, e := s.GetClientCredential(ctx, "emergency")
	if e != nil || c.LCSClientType != domain.ClientTypeEmergencyServices {
		t.Fatalf("%+v %v", c, e)
	}
	if e := s.UpsertClient(ctx, storage.Client{ID: "emergency", CredentialHash: []byte("h"), Enabled: true, LCSClientType: domain.ClientTypePLMNOperatorServices}); e != nil {
		t.Fatal(e)
	}
	c, e = s.GetClientCredential(ctx, "emergency")
	if e != nil || c.LCSClientType != domain.ClientTypePLMNOperatorServices {
		t.Fatalf("upsert did not update lcs_client_type: %+v %v", c, e)
	}
}
func TestPurgePurgesAuditEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true})
	_, _, _ = s.CreateRequest(ctx, domain.Request{ID: "r", ClientID: "c", IdempotencyKey: "k", State: domain.StateQueued})
	if e := s.RecordAudit(ctx, storage.AuditEvent{RequestID: "r", ClientID: "c", Type: "request_accepted", Detail: "x", At: time.Now().Add(-48 * time.Hour)}); e != nil {
		t.Fatal(e)
	}
	if e := s.RecordAudit(ctx, storage.AuditEvent{RequestID: "r", ClientID: "c", Type: "request_recent", Detail: "x", At: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if e := s.Purge(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	var count int
	if e := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_events").Scan(&count); e != nil {
		t.Fatal(e)
	}
	if count != 1 {
		t.Fatalf("expected only the recent audit event to survive purge, got %d", count)
	}
}
func TestConcurrentRequestCreationAndForeignKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.CreateRequest(ctx, domain.Request{ID: "bad", ClientID: "missing", IdempotencyKey: "k", State: domain.StateQueued}); err == nil {
		t.Fatal("foreign key not enforced")
	}
	if err := s.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.CreateRequest(ctx, domain.Request{ID: fmt.Sprintf("r%d", i), ClientID: "c", IdempotencyKey: fmt.Sprintf("k%d", i), State: domain.StateQueued})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
