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
