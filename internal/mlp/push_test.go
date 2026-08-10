package mlp

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vectorcore/gmlc/internal/domain"
)

func TestPusherPushEmergencyReportSendsWellFormedEmerep(t *testing.T) {
	var got svcResult
	var gotHdr *hdr
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(body, &got); err != nil {
			t.Errorf("server: decode push body: %v", err)
		}
		gotHdr = got.Hdr
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewPusher("", "", srv.URL, "gmlc-emergency-pusher", time.Second)
	lat, lon, unc := 32.622474, -86.295311, 25.0
	at := time.Date(2026, 8, 10, 19, 7, 53, 0, time.UTC)
	p.PushEmergencyReport(context.Background(), domain.Target{IMSI: "311435000070572"}, domain.LocationEventEmergencyCallOrigination, &domain.GeographicPosition{Latitude: lat, Longitude: lon, UncertaintyMeters: &unc}, at)

	if got.Emerep == nil {
		t.Fatalf("expected an emerep body, got %#v", got)
	}
	if gotHdr == nil || gotHdr.Client == nil || gotHdr.Client.ID != "gmlc-emergency-pusher" {
		t.Fatalf("expected hdr/client identity, got %#v", gotHdr)
	}
	if got.Emerep.EmeEvent.EmeTrigger != "EME_ORG" {
		t.Fatalf("eme_trigger = %q, want EME_ORG", got.Emerep.EmeEvent.EmeTrigger)
	}
	if len(got.Emerep.EmeEvent.EmePos) != 1 {
		t.Fatalf("eme_pos count = %d, want 1", len(got.Emerep.EmeEvent.EmePos))
	}
	pos := got.Emerep.EmeEvent.EmePos[0]
	if pos.Msid.Type != "IMSI" || pos.Msid.Value != "311435000070572" {
		t.Fatalf("eme_pos msid = %#v", pos.Msid)
	}
	if pos.Pd == nil || pos.Pd.Shape.CircularArea == nil {
		t.Fatalf("expected a CircularArea pd: %#v", pos)
	}
	if pos.Pd.Shape.CircularArea.Radius != "25" {
		t.Fatalf("radius = %q, want 25", pos.Pd.Shape.CircularArea.Radius)
	}
	gotLat, err := decodeDMSH(pos.Pd.Shape.CircularArea.Coord.X)
	if err != nil || gotLat < 32 || gotLat > 33 {
		t.Fatalf("latitude decode: %v %v", gotLat, err)
	}
}

func TestPusherPushEmergencyReportSkipsWithoutTargetIdentity(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewPusher("", "", srv.URL, "gmlc-emergency-pusher", time.Second)
	// A domain.Target with neither IMSI nor MSISDN — the LRR's own Target
	// is best-effort and may be empty; nothing valid can be pushed.
	p.PushEmergencyReport(context.Background(), domain.Target{}, domain.LocationEventEmergencyCallOrigination, nil, time.Now())

	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expected no HTTP call for a target-less report, got %d", calls)
	}
}

func TestPusherPushEmergencyReportUnmappedLocationEventIsNoOp(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewPusher("", "", srv.URL, "gmlc-emergency-pusher", time.Second)
	// MO_LR is not one of the three emergency trigger events — a caller
	// bug PushEmergencyReport must reject rather than sending malformed.
	p.PushEmergencyReport(context.Background(), domain.Target{IMSI: "311435000070572"}, domain.LocationEventMOLR, nil, time.Now())

	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expected no HTTP call for an unmapped location_event, got %d", calls)
	}
}

func TestPusherPushStandardReportSendsWellFormedSlrepAndReadsSlra(t *testing.T) {
	var got svcResult
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(body, &got); err != nil {
			t.Errorf("server: decode push body: %v", err)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, xml.Header)
		_ = xml.NewEncoder(w).Encode(svcResult{Ver: mlpVersion, Slra: &slra{Ver: mlpVersion, Result: result{ResID: "0", Value: "OK"}}})
	}))
	defer srv.Close()

	p := NewPusher(srv.URL, "gmlc-standard-pusher", "", "", time.Second)
	lat, lon := 32.622474, -86.295311
	at := time.Date(2026, 8, 10, 19, 7, 53, 0, time.UTC)
	// PushStandardReport doesn't return anything observable beyond logs;
	// this test's real assertion is on the request the server received.
	p.PushStandardReport(context.Background(), domain.Target{MSISDN: "12025550172"}, &domain.GeographicPosition{Latitude: lat, Longitude: lon}, at)

	if got.Slrep == nil {
		t.Fatalf("expected an slrep body, got %#v", got)
	}
	if len(got.Slrep.Pos) != 1 || got.Slrep.Pos[0].Msid.Type != "MSISDN" || got.Slrep.Pos[0].Msid.Value != "12025550172" {
		t.Fatalf("slrep pos = %#v", got.Slrep.Pos)
	}
	if got.Slrep.Pos[0].Pd == nil || got.Slrep.Pos[0].Pd.Shape.CircularArea == nil {
		t.Fatalf("expected a CircularArea pd: %#v", got.Slrep.Pos[0])
	}
}

func TestPusherPushStandardReportToleratesNonOKSlra(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, xml.Header)
		_ = xml.NewEncoder(w).Encode(svcResult{Ver: mlpVersion, Slra: &slra{Ver: mlpVersion, Result: result{ResID: "4", Value: "UNKNOWN SUBSCRIBER"}, AddInfo: "the subscriber is not registered in the LCS Client"}})
	}))
	defer srv.Close()

	p := NewPusher(srv.URL, "gmlc-standard-pusher", "", "", time.Second)
	// This must not panic or block despite the negative slra — a rejected
	// push is only logged, per Pusher's own fire-and-forget doc comment.
	p.PushStandardReport(context.Background(), domain.Target{IMSI: "311435000070572"}, nil, time.Now())
}

func TestNilPusherIsANoOp(t *testing.T) {
	var p *Pusher
	// Every exported method must tolerate a nil receiver (the
	// SetQueuedHook/SetCancelHook-style "unconfigured means disabled"
	// convention) — this must not panic.
	p.PushEmergencyReport(context.Background(), domain.Target{IMSI: "311435000070572"}, domain.LocationEventEmergencyCallOrigination, nil, time.Now())
	p.PushStandardReport(context.Background(), domain.Target{IMSI: "311435000070572"}, nil, time.Now())
}
