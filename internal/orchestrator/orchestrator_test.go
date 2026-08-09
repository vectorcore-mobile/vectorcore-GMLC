package orchestrator

import (
	"context"
	"github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
	"path/filepath"
	"testing"
	"time"
)

type fakeResolver struct {
	err   error
	block bool
}

func (f fakeResolver) ResolveServingNode(c context.Context, t domain.Target) (domain.ServingNode, error) {
	if f.block {
		<-c.Done()
		return domain.ServingNode{}, c.Err()
	}
	return domain.ServingNode{Type: "mme", MMEHost: "mme", MMERealm: "example", Source: "test", ResolvedAt: time.Now()}, f.err
}

type fakeProvider struct {
	err   error
	block bool
}

func (f fakeProvider) ProvideLocation(c context.Context, n domain.ServingNode, r domain.LocationRequest) (domain.PositioningResult, error) {
	if f.block {
		<-c.Done()
		return domain.PositioningResult{}, c.Err()
	}
	if f.err != nil {
		return domain.PositioningResult{}, f.err
	}
	lat, lon := 1.0, 2.0
	return domain.PositioningResult{RawLocationEstimate: []byte{0, 0, 0, 0, 0, 0, 0}, ECGI: []byte{1}, Position: &domain.GeographicPosition{Shape: "ellipsoid_point", Latitude: lat, Longitude: lon}}, nil
}

type noResultProvider struct{}

func (noResultProvider) ProvideLocation(c context.Context, n domain.ServingNode, r domain.LocationRequest) (domain.PositioningResult, error) {
	return domain.PositioningResult{Kind: "no_immediate_result"}, nil
}

// ecgiOnlyProvider mimics a real "additional_information" PLA outcome: ECGI
// but no decoded Location-Estimate, so RawLocationEstimate/Position are both
// zero-value. This previously stranded requests forever: CompleteRequest's
// INSERT violated location_results.raw_gad's NOT NULL constraint, silently
// failing and leaving the request stuck in "locating" with no retry path.
type ecgiOnlyProvider struct{}

func (ecgiOnlyProvider) ProvideLocation(c context.Context, n domain.ServingNode, r domain.LocationRequest) (domain.PositioningResult, error) {
	return domain.PositioningResult{Kind: "additional_information", ECGI: []byte{1, 2, 3, 4, 5, 6, 7}}, nil
}

type capturingProvider struct{ got chan domain.LocationRequest }

func (p capturingProvider) ProvideLocation(c context.Context, n domain.ServingNode, r domain.LocationRequest) (domain.PositioningResult, error) {
	p.got <- r
	lat, lon := 1.0, 2.0
	return domain.PositioningResult{RawLocationEstimate: []byte{0, 0, 0, 0, 0, 0, 0}, Position: &domain.GeographicPosition{Shape: "ellipsoid_point", Latitude: lat, Longitude: lon}}, nil
}
func store(t *testing.T) *sqlite.Store {
	t.Helper()
	s, e := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(t.TempDir(), "x.db")})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertClient(context.Background(), storage.Client{ID: "c", CredentialHash: []byte("x"), Enabled: true}); e != nil {
		t.Fatal(e)
	}
	return s
}
func queued(t *testing.T, s storage.Store, id string) {
	t.Helper()
	_, _, e := s.CreateRequest(context.Background(), domain.Request{ID: id, ClientID: "c", IdempotencyKey: id, TargetKind: "imsi", TargetValue: "001010123456789", State: domain.StateQueued})
	if e != nil {
		t.Fatal(e)
	}
}
func waitState(t *testing.T, s storage.Store, id string, want domain.State) {
	t.Helper()
	d := time.Now().Add(time.Second)
	for {
		r, e := s.GetRequest(context.Background(), id)
		if e == nil && r.State == want {
			return
		}
		if time.Now().After(d) {
			t.Fatalf("state=%v err=%v", r.State, e)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestWorkerSuccessRetryAndCancellation(t *testing.T) {
	s := store(t)
	defer s.Close(context.Background())
	queued(t, s, "ok")
	w := New(s, fakeResolver{}, fakeProvider{})
	w.Start(context.Background())
	w.Notify()
	waitState(t, s, "ok", domain.StateCompleted)
	v, e := s.GetResult(context.Background(), "ok")
	if e != nil || v.Latitude == nil || *v.Latitude != 1 || len(v.RawGAD) == 0 {
		t.Fatalf("%+v %v", v, e)
	}
	_ = w.Close(context.Background())
	queued(t, s, "retry")
	w = New(s, fakeResolver{}, fakeProvider{err: diameter.ErrConnectionLost})
	w.Start(context.Background())
	w.Notify()
	d := time.Now().Add(time.Second)
	var r domain.Request
	for {
		r, _ = s.GetRequest(context.Background(), "retry")
		if r.State == domain.StateQueued && r.AttemptCount == 1 {
			break
		}
		if time.Now().After(d) {
			t.Fatalf("retry %+v", r)
		}
		time.Sleep(time.Millisecond)
	}
	if r.AttemptCount != 1 {
		t.Fatal(r.AttemptCount)
	}
	_ = w.Close(context.Background())
	queued(t, s, "no-result")
	w = New(s, fakeResolver{}, noResultProvider{})
	w.Start(context.Background())
	w.Notify()
	waitState(t, s, "no-result", domain.StateFailed)
	if r, e := s.GetRequest(context.Background(), "no-result"); e != nil || r.FailureCode != "no_immediate_result" {
		t.Fatalf("%+v %v", r, e)
	}
	_ = w.Close(context.Background())
	queued(t, s, "cancel")
	w = New(s, fakeResolver{block: true}, fakeProvider{})
	w.Start(context.Background())
	w.Notify()
	waitState(t, s, "cancel", domain.StateResolving)
	if _, e := s.TransitionRequest(context.Background(), "cancel", domain.StateCancelled, "cancelled"); e != nil {
		t.Fatal(e)
	}
	w.Cancel("cancel")
	waitState(t, s, "cancel", domain.StateCancelled)
	_ = w.Close(context.Background())
}
func TestWorkerUsesClientConfiguredLCSClientType(t *testing.T) {
	s := store(t)
	defer s.Close(context.Background())
	if e := s.UpsertClient(context.Background(), storage.Client{ID: "plmn-client", CredentialHash: []byte("x"), Enabled: true, LCSClientType: domain.ClientTypePLMNOperatorServices}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.CreateRequest(context.Background(), domain.Request{ID: "req", ClientID: "plmn-client", IdempotencyKey: "req", TargetKind: "imsi", TargetValue: "001010123456789", State: domain.StateQueued}); e != nil {
		t.Fatal(e)
	}
	cp := capturingProvider{got: make(chan domain.LocationRequest, 1)}
	w := New(s, fakeResolver{}, cp)
	w.Start(context.Background())
	w.Notify()
	select {
	case r := <-cp.got:
		if r.ClientType != domain.ClientTypePLMNOperatorServices {
			t.Fatalf("expected ClientType=%d (PLMN operator), got %d", domain.ClientTypePLMNOperatorServices, r.ClientType)
		}
	case <-time.After(time.Second):
		t.Fatal("provider not invoked")
	}
	waitState(t, s, "req", domain.StateCompleted)
	_ = w.Close(context.Background())
}

// TestWorkerUsesClientConfiguredPrivacyCheck asserts LCS-Privacy-Check is
// resolved from the client's own operator-configured record, exactly like
// LCS-Client-Type — never influenced by anything on the stored request
// itself (there is no PrivacyCheck field on domain.Request/SubmitInput at
// all; the REST API has no way to set it), since it governs the target
// subscriber's privacy protection, not the requesting client's own request.
func TestWorkerUsesClientConfiguredPrivacyCheck(t *testing.T) {
	s := store(t)
	defer s.Close(context.Background())
	if e := s.UpsertClient(context.Background(), storage.Client{ID: "notify-client", CredentialHash: []byte("x"), Enabled: true, LCSPrivacyCheck: domain.PrivacyAllowedWithNotification}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.CreateRequest(context.Background(), domain.Request{ID: "req-privacy", ClientID: "notify-client", IdempotencyKey: "req-privacy", TargetKind: "imsi", TargetValue: "001010123456789", State: domain.StateQueued}); e != nil {
		t.Fatal(e)
	}
	cp := capturingProvider{got: make(chan domain.LocationRequest, 1)}
	w := New(s, fakeResolver{}, cp)
	w.Start(context.Background())
	w.Notify()
	select {
	case r := <-cp.got:
		if r.PrivacyCheck == nil || *r.PrivacyCheck != domain.PrivacyAllowedWithNotification {
			t.Fatalf("expected PrivacyCheck=%d (allowed with notification), got %v", domain.PrivacyAllowedWithNotification, r.PrivacyCheck)
		}
	case <-time.After(time.Second):
		t.Fatal("provider not invoked")
	}
	waitState(t, s, "req-privacy", domain.StateCompleted)
	_ = w.Close(context.Background())
}
func TestWorkerCompletesECGIOnlyResult(t *testing.T) {
	s := store(t)
	defer s.Close(context.Background())
	queued(t, s, "ecgi-only")
	w := New(s, fakeResolver{}, ecgiOnlyProvider{})
	w.Start(context.Background())
	defer w.Close(context.Background())
	w.Notify()
	waitState(t, s, "ecgi-only", domain.StateCompleted)
	v, e := s.GetResult(context.Background(), "ecgi-only")
	if e != nil {
		t.Fatal(e)
	}
	if string(v.ECGI) != "\x01\x02\x03\x04\x05\x06\x07" {
		t.Fatalf("ECGI not stored: %+v", v)
	}
	if v.Latitude != nil || v.Longitude != nil {
		t.Fatalf("expected no position for ECGI-only result: %+v", v)
	}
}

// TestWorkerCompletionHookFiresOnSuccessAndFailure guards API-ASYNC's
// integration point: the hook must fire with a State that reflects the
// just-persisted transition (StateCompleted/StateFailed), not the worker's
// stale pre-transition in-memory copy — see notifyCompletion.
func TestWorkerCompletionHookFiresOnSuccessAndFailure(t *testing.T) {
	s := store(t)
	defer s.Close(context.Background())
	type event struct {
		r domain.Request
		v domain.Result
	}
	got := make(chan event, 4)

	queued(t, s, "hook-ok")
	w := New(s, fakeResolver{}, fakeProvider{})
	w.SetCompletionHook(func(r domain.Request, v domain.Result) { got <- event{r, v} })
	w.Start(context.Background())
	w.Notify()
	select {
	case e := <-got:
		if e.r.ID != "hook-ok" || e.r.State != domain.StateCompleted || e.v.Latitude == nil || *e.v.Latitude != 1 {
			t.Fatalf("unexpected completion event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("completion hook did not fire on success")
	}
	_ = w.Close(context.Background())

	queued(t, s, "hook-fail")
	w2 := New(s, fakeResolver{}, noResultProvider{})
	w2.SetCompletionHook(func(r domain.Request, v domain.Result) { got <- event{r, v} })
	w2.Start(context.Background())
	w2.Notify()
	select {
	case e := <-got:
		if e.r.ID != "hook-fail" || e.r.State != domain.StateFailed || e.r.FailureCode != "no_immediate_result" {
			t.Fatalf("unexpected failure completion event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("completion hook did not fire on failure")
	}
	_ = w2.Close(context.Background())
}
