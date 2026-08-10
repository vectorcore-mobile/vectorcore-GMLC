package lrr

import (
	"context"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/vectorcore/gmlc/internal/mlp"
	"github.com/vectorcore/gmlc/internal/slg"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

func testStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, e := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(t.TempDir(), "gmlc.db"), Synchronous: "FULL", CheckpointPages: 10})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertClient(context.Background(), storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// testConnPair returns a diam.Conn wrapping one end of an in-memory
// net.Pipe, and the raw net.Conn for the other end — enough to invoke
// ServeDIAM directly and read back whatever it writes, without a real
// listener/dial round trip (that's what the internal/diameter loopback
// integration test already covers end to end).
func testConnPair(t *testing.T) (diam.Conn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	c, e := diam.NewConn(server, "test", nil, dict.Default)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); client.Close() })
	return c, client
}

// pushedSvcResult decodes the wire XML mlp.Pusher sends, independent of
// package mlp's own (unexported) types — this is the black-box shape an
// LCS Client destination would actually see.
type pushedSvcResult struct {
	XMLName xml.Name `xml:"svc_result"`
	Hdr     struct {
		Client struct {
			ID string `xml:"id"`
		} `xml:"client"`
	} `xml:"hdr"`
	Emerep *struct {
		EmeEvent struct {
			EmeTrigger string `xml:"eme_trigger,attr"`
			EmePos     []struct {
				Msid struct {
					Type  string `xml:"type,attr"`
					Value string `xml:",chardata"`
				} `xml:"msid"`
			} `xml:"eme_pos"`
		} `xml:"eme_event"`
	} `xml:"emerep"`
	Slrep *struct {
		Pos []struct {
			Msid struct {
				Type  string `xml:"type,attr"`
				Value string `xml:",chardata"`
			} `xml:"msid"`
		} `xml:"pos"`
	} `xml:"slrep"`
}

// pushCaptureServer starts an httptest server that decodes every pushed
// svc_result onto ch and answers with a well-formed slra (accepted by any
// slrep it happens to receive; ignored for emerep, which has no answer
// message per §5.6.2.3 Figure 10).
func pushCaptureServer(t *testing.T, ch chan<- pushedSvcResult) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v pushedSvcResult
		if err := xml.Unmarshal(body, &v); err != nil {
			t.Errorf("decode pushed body: %v", err)
		}
		ch <- v
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, xml.Header)
		_, _ = io.WriteString(w, `<svc_result ver="3.5.0"><slra ver="3.5.0"><result resid="0">OK</result></slra></svc_result>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveLRRAndAwaitAnswer sends m through h.ServeDIAM over a fresh
// testConnPair and blocks until the LRA is read back, so the caller can
// then assert on whatever h did synchronously as part of handling it
// (process, including any push, completes before ServeDIAM writes the
// LRA).
func serveLRRAndAwaitAnswer(t *testing.T, h *Handler, m *diam.Message) {
	t.Helper()
	c, client := testConnPair(t)
	done := make(chan struct{})
	go func() { h.ServeDIAM(c, m); close(done) }()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, e := diam.ReadMessage(client, dict.Default); e != nil {
		t.Fatal(e)
	}
	<-done
}

func TestServeDIAMPushesStandardReportOnMOLREvent(t *testing.T) {
	s := testStore(t)
	codec, e := slg.New(slg.Config{OriginHost: "gmlc.example", OriginRealm: "example"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	pushed := make(chan pushedSvcResult, 1)
	srv := pushCaptureServer(t, pushed)

	h := New(codec, s)
	h.SetPusher(mlp.NewPusher(srv.URL, "gmlc-standard-pusher", "", "", 2*time.Second))

	m := diam.NewRequest(slg.CommandLocationReport, slg.ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("mme.example;lrr;molr"))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	m.NewAVP(slg.AVPLocationEvent, avp.Mbit|avp.Vbit, slg.VendorID, datatype.Enumerated(slg.LocationEventMOLR))
	m.NewAVP(avp.UserName, avp.Mbit, 0, datatype.UTF8String("311435000070572"))
	m.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString([]byte{0, 0x40, 0, 0, 0x40, 0, 0}))
	serveLRRAndAwaitAnswer(t, h, m)

	select {
	case v := <-pushed:
		if v.Slrep == nil {
			t.Fatalf("expected an slrep push, got %#v", v)
		}
		if v.Hdr.Client.ID != "gmlc-standard-pusher" {
			t.Fatalf("hdr/client id = %q, want gmlc-standard-pusher", v.Hdr.Client.ID)
		}
		if len(v.Slrep.Pos) != 1 || v.Slrep.Pos[0].Msid.Value != "311435000070572" {
			t.Fatalf("slrep pos = %#v", v.Slrep.Pos)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected an slrep push, none received")
	}
}

func TestServeDIAMPushesEmergencyReportOnEmergencyOriginationEvent(t *testing.T) {
	s := testStore(t)
	codec, e := slg.New(slg.Config{OriginHost: "gmlc.example", OriginRealm: "example"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	pushed := make(chan pushedSvcResult, 1)
	srv := pushCaptureServer(t, pushed)

	h := New(codec, s)
	h.SetPusher(mlp.NewPusher("", "", srv.URL, "gmlc-emergency-pusher", 2*time.Second))

	m := diam.NewRequest(slg.CommandLocationReport, slg.ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("mme.example;lrr;eme"))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	m.NewAVP(slg.AVPLocationEvent, avp.Mbit|avp.Vbit, slg.VendorID, datatype.Enumerated(slg.LocationEventEmergencyCallOrigination))
	m.NewAVP(avp.UserName, avp.Mbit, 0, datatype.UTF8String("311435000070572"))
	m.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString([]byte{0, 0x40, 0, 0, 0x40, 0, 0}))
	serveLRRAndAwaitAnswer(t, h, m)

	select {
	case v := <-pushed:
		if v.Emerep == nil {
			t.Fatalf("expected an emerep push, got %#v", v)
		}
		if v.Hdr.Client.ID != "gmlc-emergency-pusher" {
			t.Fatalf("hdr/client id = %q, want gmlc-emergency-pusher", v.Hdr.Client.ID)
		}
		if v.Emerep.EmeEvent.EmeTrigger != "EME_ORG" {
			t.Fatalf("eme_trigger = %q, want EME_ORG", v.Emerep.EmeEvent.EmeTrigger)
		}
		if len(v.Emerep.EmeEvent.EmePos) != 1 || v.Emerep.EmeEvent.EmePos[0].Msid.Value != "311435000070572" {
			t.Fatalf("eme_pos = %#v", v.Emerep.EmeEvent.EmePos)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected an emerep push, none received")
	}
}

func TestServeDIAMDoesNotPushForNonReportingLocationEvent(t *testing.T) {
	s := testStore(t)
	codec, e := slg.New(slg.Config{OriginHost: "gmlc.example", OriginRealm: "example"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	pushed := make(chan pushedSvcResult, 1)
	srv := pushCaptureServer(t, pushed)

	h := New(codec, s)
	h.SetPusher(mlp.NewPusher(srv.URL, "gmlc-standard-pusher", srv.URL, "gmlc-emergency-pusher", 2*time.Second))

	m := diam.NewRequest(slg.CommandLocationReport, slg.ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("mme.example;lrr;deferred"))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	// DeferredMTLRResponse has no MLP report counterpart — see push's own
	// doc comment.
	m.NewAVP(slg.AVPLocationEvent, avp.Mbit|avp.Vbit, slg.VendorID, datatype.Enumerated(slg.LocationEventDeferredMTLRResponse))
	m.NewAVP(avp.UserName, avp.Mbit, 0, datatype.UTF8String("311435000070572"))
	serveLRRAndAwaitAnswer(t, h, m)

	select {
	case v := <-pushed:
		t.Fatalf("expected no push for a non-reporting location_event, got %#v", v)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestServeDIAMDecodeFailureRespondsUnableToComply(t *testing.T) {
	s := testStore(t)
	codec, e := slg.New(slg.Config{OriginHost: "gmlc.example", OriginRealm: "example"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	h := New(codec, s)

	c, client := testConnPair(t)
	// Location-Event is mandatory (TS 29.172 7.3.3); deliberately omitted so
	// DecodeLRR fails and ServeDIAM must fall back to UnableToComply rather
	// than silently claiming success.
	m := diam.NewRequest(slg.CommandLocationReport, slg.ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("mme.example;lrr;bad"))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))

	done := make(chan struct{})
	go func() { h.ServeDIAM(c, m); close(done) }()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	ans, e := diam.ReadMessage(client, dict.Default)
	if e != nil {
		t.Fatal(e)
	}
	<-done
	if ans.Header.CommandFlags&diam.RequestFlag != 0 {
		t.Fatal("expected an answer, not a request")
	}
	rc, err := ans.FindAVP(avp.ResultCode, 0)
	if err != nil || rc.Data.(datatype.Unsigned32) != diam.UnableToComply {
		t.Fatalf("expected UnableToComply, got %v %v", rc, err)
	}
}
