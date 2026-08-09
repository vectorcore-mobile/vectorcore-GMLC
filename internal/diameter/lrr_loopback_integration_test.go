package diameter_test

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/fiorix/go-diameter/v4/diam/sm"
	vcdiam "github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/lrr"
	"github.com/vectorcore/gmlc/internal/slg"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

// lrrRelayFixture mirrors relay_peer_integration_test.go's newRelay pattern,
// but for the opposite direction: the relay here is an MME/SGSN stand-in
// that *sends* an LRR over the connection the GMLC dialed out on, and
// captures the LRA that comes back — matching this GMLC's DRA-only
// deployment model (there is no separate inbound listener; an inbound LRR
// arrives on the same association).
type lrrRelayFixture struct {
	ln    net.Listener
	lra   chan *diam.Message
	mu    sync.Mutex
	conns []diam.Conn
}

func newLRRRelay(t *testing.T) *lrrRelayFixture {
	t.Helper()
	f := &lrrRelayFixture{lra: make(chan *diam.Message, 2)}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f.ln = ln
	m := sm.New(&sm.Settings{OriginHost: "relay.example", OriginRealm: "example", VendorID: 10415, ProductName: "relay", HostIPAddresses: []datatype.Address{datatype.Address(net.ParseIP("127.0.0.1"))}, OnCER: func(c diam.Conn, _ *diam.Message) {
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
	}})
	m.HandleIdx(diam.CommandIndex{AppID: slg.ApplicationID, Code: slg.CommandLocationReport, Request: false}, diam.HandlerFunc(func(_ diam.Conn, x *diam.Message) {
		f.lra <- x
	}))
	go func() { _ = diam.Serve(ln, m) }()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}
func (f *lrrRelayFixture) latestConn() diam.Conn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[len(f.conns)-1]
}

func TestLRRLoopbackDecodeCorrelatePersistAndAnswer(t *testing.T) {
	f := newLRRRelay(t)
	ctx := context.Background()
	st, e := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "gmlc.db"), Synchronous: "FULL", CheckpointPages: 10})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })
	if e = st.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = st.UpsertClient(ctx, storage.Client{ID: "c", CredentialHash: []byte("h"), Enabled: true}); e != nil {
		t.Fatal(e)
	}
	// Seed a pending correlation target, as if an earlier PLR had requested
	// delayed reporting and stored this reference number — see
	// storage.DeferredLocationSubscription's doc comment for why nothing in
	// this codebase does that yet, but the correlation path must still work
	// once something does.
	if _, e = st.CreateDeferredLocationSubscription(ctx, "c", "imsi", "001010123456789", 9); e != nil {
		t.Fatal(e)
	}

	codec, e := slg.New(slg.Config{OriginHost: "gmlc.example", OriginRealm: "example"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	handler := lrr.New(codec, st)

	r := vcdiam.BuildRegistry(vcdiam.RegistryConfig{OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, ReconnectMin: 100 * time.Millisecond, ReconnectMax: 100 * time.Millisecond, WatchdogInterval: time.Second, WatchdogTimeout: time.Second, Peers: []vcdiam.PeerConfig{{Address: f.ln.Addr().String(), Transport: "tcp"}}, RequestHandlers: []vcdiam.RequestHandler{{AppID: slg.ApplicationID, Code: slg.CommandLocationReport, Handler: handler}}})
	r.Start()
	t.Cleanup(func() { _ = r.Close(ctx) })
	wait(t, r.OverallReady)

	conn := f.latestConn()
	m := diam.NewRequest(slg.CommandLocationReport, slg.ApplicationID, dict.Default)
	m.NewAVP(avp.SessionID, avp.Mbit, 0, datatype.UTF8String("relay.example;lrr;loopback"))
	m.NewAVP(avp.AuthSessionState, avp.Mbit, 0, datatype.Enumerated(1))
	m.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity("relay.example"))
	m.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
	m.NewAVP(avp.DestinationHost, avp.Mbit, 0, datatype.DiameterIdentity("gmlc.example"))
	m.NewAVP(avp.DestinationRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
	m.NewAVP(slg.AVPLocationEvent, avp.Mbit|avp.Vbit, slg.VendorID, datatype.Enumerated(slg.LocationEventDeferredMTLRResponse))
	m.NewAVP(avp.UserName, avp.Mbit, 0, datatype.UTF8String("001010123456789"))
	m.NewAVP(slg.AVPLCSReferenceNumber, avp.Vbit|avp.Mbit, slg.VendorID, datatype.OctetString([]byte{9}))
	m.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString([]byte{0, 0x40, 0, 0, 0x40, 0, 0}))
	if _, e = m.WriteTo(conn); e != nil {
		t.Fatal(e)
	}

	select {
	case a := <-f.lra:
		if a.Header.CommandFlags&diam.RequestFlag != 0 {
			t.Fatal("expected an answer, not a request")
		}
		rc, err := a.FindAVP(avp.ResultCode, 0)
		if err != nil || rc.Data.(datatype.Unsigned32) != diam.Success {
			t.Fatalf("bad LRA result code: %v %v", rc, err)
		}
		sid, err := a.FindAVP(avp.SessionID, 0)
		if err != nil || sid.Data.(datatype.UTF8String) != "relay.example;lrr;loopback" {
			t.Fatalf("Session-Id not echoed: %v %v", sid, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LRA absent")
	}

	// process() runs synchronously before the LRA is built/written, so by
	// the time the LRA arrived above, correlation must already have
	// transitioned the subscription out of pending.
	if _, e = st.FindPendingDeferredLocationSubscription(ctx, 9); e != storage.ErrNotFound {
		t.Fatalf("expected the subscription to no longer be pending after correlation, got %v", e)
	}
}
