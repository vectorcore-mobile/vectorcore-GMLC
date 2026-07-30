package diameter_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/fiorix/go-diameter/v4/diam/sm"
	vcdiam "github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/slg"
	"github.com/vectorcore/gmlc/internal/slh"
)

var loadApps sync.Once

func loadTestApps(t *testing.T) {
	t.Helper()
	loadApps.Do(func() {
		if _, err := dict.Default.App(16777291, "auth"); err == nil {
			return
		}
		_ = dict.Default.Load(strings.NewReader(`<?xml version="1.0"?><diameter><application id="16777291" type="auth" name="SLh"><vendor id="10415" name="3GPP"/><command code="8388622" short="LR" name="LCS-Routing-Info"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command><avp name="Serving-Node" code="2401" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Grouped"><rule avp="AVP" required="false"/></data></avp><avp name="MME-Name" code="2402" vendor-id="10415" must="M,V" may-encrypt="N"><data type="DiameterIdentity"/></avp><avp name="MME-Realm" code="2408" vendor-id="10415" must="V" may-encrypt="N"><data type="DiameterIdentity"/></avp></application><application id="16777255" type="auth" name="SLg"><vendor id="10415" name="3GPP"/><command code="8388620" short="PL" name="Provide-Location"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command><avp name="Location-Estimate" code="1242" vendor-id="0" must="M" may-encrypt="N"><data type="OctetString"/></avp></application></diameter>`))
	})
}

type relayFixture struct {
	ln       net.Listener
	accepted atomic.Int32
	rir      chan *diam.Message
	plr      chan *diam.Message
	mu       sync.Mutex
	conns    []diam.Conn
}

func newRelay(t *testing.T) *relayFixture {
	t.Helper()
	loadTestApps(t)
	f := &relayFixture{rir: make(chan *diam.Message, 2), plr: make(chan *diam.Message, 2)}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f.ln = ln
	m := sm.New(&sm.Settings{OriginHost: "relay.example", OriginRealm: "example", VendorID: 10415, ProductName: "relay", HostIPAddresses: []datatype.Address{datatype.Address(net.ParseIP("127.0.0.1"))}, OnCER: func(c diam.Conn, msg *diam.Message) {
		f.accepted.Add(1)
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		if len(msg.AVP) == 0 {
			t.Error("empty CER")
		}
	}})
	m.HandleIdx(diam.CommandIndex{AppID: 16777291, Code: 8388622, Request: true}, diam.HandlerFunc(func(c diam.Conn, x *diam.Message) {
		f.rir <- x
		a := x.Answer(0)
		a.NewAVP(avp.ResultCode, avp.Mbit, 0, datatype.Unsigned32(diam.Success))
		a.NewAVP(2401, avp.Mbit|avp.Vbit, 10415, &diam.GroupedAVP{AVP: []*diam.AVP{diam.NewAVP(2402, avp.Mbit|avp.Vbit, 10415, datatype.DiameterIdentity("relay.example")), diam.NewAVP(2408, avp.Vbit, 10415, datatype.DiameterIdentity("example"))}})
		_, _ = a.WriteTo(c)
	}))
	m.HandleIdx(diam.CommandIndex{AppID: 16777255, Code: 8388620, Request: true}, diam.HandlerFunc(func(c diam.Conn, x *diam.Message) {
		f.plr <- x
		a := x.Answer(diam.Success)
		a.NewAVP(avp.LocationEstimate, avp.Mbit, 0, datatype.OctetString([]byte{0, 0, 0, 0, 0, 0, 0}))
		_, _ = a.WriteTo(c)
	}))
	go func() { _ = diam.Serve(ln, m) }()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}
func (f *relayFixture) closeLatest() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conns) > 0 {
		f.conns[len(f.conns)-1].Close()
	}
}
func wait(t *testing.T, fn func() bool) {
	t.Helper()
	d := time.Now().Add(2 * time.Second)
	for !fn() {
		if time.Now().After(d) {
			t.Fatal("timed out waiting for state")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestRelayPeerRIRThenPLRLoopback(t *testing.T) {
	f := newRelay(t)
	r := vcdiam.BuildRegistry(vcdiam.RegistryConfig{OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, ReconnectMin: 100 * time.Millisecond, ReconnectMax: 100 * time.Millisecond, WatchdogInterval: time.Second, WatchdogTimeout: time.Second, Peers: []vcdiam.PeerConfig{{Address: f.ln.Addr().String(), Transport: "tcp"}}})
	r.Start()
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	wait(t, r.OverallReady)
	if f.accepted.Load() != 1 {
		t.Fatalf("connections=%d", f.accepted.Load())
	}
	rt, e := r.RoundTripperFor(context.Background(), vcdiam.RouteRequest{ApplicationID: vcdiam.SLhApplicationID, DestinationRealm: "example"})
	if e != nil {
		t.Fatal(e)
	}
	res, e := slh.New(slh.Config{OriginHost: "gmlc.example", OriginRealm: "example", DestinationRealm: "example"}, rt)
	if e != nil {
		t.Fatal(e)
	}
	node, e := res.ResolveServingNode(context.Background(), domain.Target{IMSI: "001010123456789"})
	if e != nil || node.MMEHost != "relay.example" {
		t.Fatalf("node=%+v err=%v", node, e)
	}
	select {
	case m := <-f.rir:
		if m.Header.ApplicationID != 16777291 {
			t.Fatal("not RIR")
		}
	case <-time.After(time.Second):
		t.Fatal("RIA request absent")
	}
	p, e := slg.NewWithRegistry(slg.Config{OriginHost: "gmlc.example", OriginRealm: "example"}, r)
	if e != nil {
		t.Fatal(e)
	}
	out, e := p.ProvideLocation(context.Background(), node, domain.LocationRequest{Target: domain.Target{IMSI: "001010123456789"}, LocationType: slg.LocationCurrent, ClientType: 1, ClientName: "test"})
	if e != nil || string(out.RawLocationEstimate) != "\x00\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("out=%+v err=%v", out, e)
	}
	select {
	case m := <-f.plr:
		dh, _ := m.FindAVP(avp.DestinationHost, 0)
		if string(dh.Data.(datatype.DiameterIdentity)) != "relay.example" {
			t.Fatal("wrong MME route")
		}
	case <-time.After(time.Second):
		t.Fatal("PLR absent")
	}
	if f.accepted.Load() != 1 {
		t.Fatal("new TCP connection")
	}
}
