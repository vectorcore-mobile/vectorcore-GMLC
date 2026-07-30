package diameter

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
)

// peerFixture is a raw Diameter peer: it exposes handshake and request events
// while all client-side work remains the production connection stack.
type peerFixture struct {
	ln          net.Listener
	host, realm string
	apps        []uint32
	handshakes  chan *fixtureConn
	requests    chan *fixtureRequest
	mu          sync.Mutex
	closed      bool
}
type fixtureConn struct {
	conn   net.Conn
	permit chan struct{}
}
type fixtureRequest struct {
	fc  *fixtureConn
	msg *diam.Message
}

var fixtureApps sync.Once

func ensureFixtureApps() {
	fixtureApps.Do(func() {
		_ = dict.Default.Load(strings.NewReader(`<diameter><application id="16777291" type="auth" name="SLh"><vendor id="10415" name="3GPP"/><command code="8388622" short="LR" name="LCS-Routing-Info"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command><avp name="Serving-Node" code="2401" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Grouped"><rule avp="AVP" required="false"/></data></avp><avp name="MME-Name" code="2402" vendor-id="10415" must="M,V" may-encrypt="N"><data type="DiameterIdentity"/></avp><avp name="MME-Realm" code="2408" vendor-id="10415" must="V" may-encrypt="N"><data type="DiameterIdentity"/></avp></application><application id="16777255" type="auth" name="SLg"><vendor id="10415" name="3GPP"/><command code="8388620" short="PL" name="Provide-Location"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command><avp name="Location-Estimate" code="1242" vendor-id="0" must="M" may-encrypt="N"><data type="OctetString"/></avp></application></diameter>`))
	})
}

func newPeerFixture(t *testing.T) *peerFixture {
	t.Helper()
	ensureFixtureApps()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &peerFixture{ln: ln, host: "relay.example", realm: "example", apps: []uint32{RelayApplicationID}, handshakes: make(chan *fixtureConn, 8), requests: make(chan *fixtureRequest, 16)}
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			fc := &fixtureConn{conn: c, permit: make(chan struct{})}
			go f.serve(fc)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}
func newDirectFixture(t *testing.T, host, realm string, app uint32) *peerFixture {
	f := newPeerFixture(t)
	f.host, f.realm, f.apps = host, realm, []uint32{app}
	return f
}
func (f *peerFixture) serve(fc *fixtureConn) {
	defer fc.conn.Close()
	cer, e := diam.ReadMessage(fc.conn, dict.Default)
	if e != nil {
		return
	}
	f.handshakes <- fc
	<-fc.permit
	cea := cer.Answer(diam.Success)
	cea.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity(f.host))
	cea.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity(f.realm))
	for _, app := range f.apps {
		cea.NewAVP(avp.AuthApplicationID, avp.Mbit, 0, datatype.Unsigned32(app))
	}
	_, _ = cea.WriteTo(fc.conn)
	for {
		m, e := diam.ReadMessage(fc.conn, dict.Default)
		if e != nil {
			return
		}
		if m.Header.CommandFlags&diam.RequestFlag != 0 && m.Header.CommandCode != diam.DeviceWatchdog {
			f.requests <- &fixtureRequest{fc, m}
		}
	}
}
func fixtureRegistry(t *testing.T, f *peerFixture) *Registry {
	t.Helper()
	r := BuildRegistry(RegistryConfig{OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, ReconnectMin: time.Millisecond, ReconnectMax: 10 * time.Millisecond, WatchdogInterval: time.Second, WatchdogTimeout: time.Second, Peers: []PeerConfig{{Name: "relay", Address: f.ln.Addr().String(), Transport: "tcp"}}})
	r.Start()
	fc := <-f.handshakes
	close(fc.permit)
	waitReady(t, r.OverallReady)
	return r
}
func waitReady(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func answerRIR(fr *fixtureRequest) {
	a := fr.msg.Answer(diam.Success)
	a.NewAVP(2401, avp.Mbit|avp.Vbit, 10415, &diam.GroupedAVP{AVP: []*diam.AVP{diam.NewAVP(2402, avp.Mbit|avp.Vbit, 10415, datatype.DiameterIdentity("mme.example")), diam.NewAVP(2408, avp.Mbit|avp.Vbit, 10415, datatype.DiameterIdentity("example"))}})
	_, _ = a.WriteTo(fr.fc.conn)
}
func answerPLR(fr *fixtureRequest) {
	a := fr.msg.Answer(diam.Success)
	a.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString([]byte{1, 2, 3}))
	_, _ = a.WriteTo(fr.fc.conn)
}
func TestPeerClosePendingRIRDisconnectReconnect(t *testing.T) {
	f := newPeerFixture(t)
	r := fixtureRegistry(t, f)
	defer r.Close(context.Background())
	rt, e := r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLhApplicationID})
	if e != nil {
		t.Fatal(e)
	}
	out := make(chan error, 1)
	go func() {
		_, e := rt.RoundTrip(context.Background(), diam.NewRequest(8388622, SLhApplicationID, dict.Default))
		out <- e
	}()
	req := <-f.requests
	if req.msg.Header.ApplicationID != SLhApplicationID {
		t.Fatal("not RIR")
	}
	req.fc.conn.Close()
	if e := <-out; !errors.Is(e, ErrConnectionLost) {
		t.Fatalf("got %v", e)
	}
	next := <-f.handshakes
	if r.OverallReady() {
		t.Fatal("stale ready during reconnect")
	}
	close(next.permit)
	waitReady(t, r.OverallReady)
	if r.managers[0].Generation() < 2 {
		t.Fatal("generation not advanced")
	}
	rt, e = r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLhApplicationID})
	if e != nil {
		t.Fatal(e)
	}
	go func() {
		_, e := rt.RoundTrip(context.Background(), diam.NewRequest(8388622, SLhApplicationID, dict.Default))
		out <- e
	}()
	answerRIR(<-f.requests)
	if e := <-out; e != nil {
		t.Fatal(e)
	}
	if e := r.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestPeerClosePendingPLRDisconnectReconnect(t *testing.T) {
	f := newPeerFixture(t)
	r := fixtureRegistry(t, f)
	defer r.Close(context.Background())
	rt, e := r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLgApplicationID, DestinationHost: "mme.example", DestinationRealm: "example"})
	if e != nil {
		t.Fatal(e)
	}
	out := make(chan error, 1)
	go func() {
		_, e := rt.RoundTrip(context.Background(), diam.NewRequest(8388620, SLgApplicationID, dict.Default))
		out <- e
	}()
	req := <-f.requests
	if req.msg.Header.ApplicationID != SLgApplicationID {
		t.Fatal("not PLR")
	}
	req.fc.conn.Close()
	if e := <-out; !errors.Is(e, ErrConnectionLost) {
		t.Fatalf("got %v", e)
	}
	next := <-f.handshakes
	close(next.permit)
	waitReady(t, r.OverallReady)
	rt, e = r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLgApplicationID, DestinationHost: "mme.example", DestinationRealm: "example"})
	if e != nil {
		t.Fatal(e)
	}
	go func() {
		_, e := rt.RoundTrip(context.Background(), diam.NewRequest(8388620, SLgApplicationID, dict.Default))
		out <- e
	}()
	answerPLR(<-f.requests)
	if e := <-out; e != nil {
		t.Fatal(e)
	}
}
