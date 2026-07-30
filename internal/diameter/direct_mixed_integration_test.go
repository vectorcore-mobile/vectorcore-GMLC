package diameter

import (
	"context"
	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"net"
	"testing"
	"time"
)

func startFixtures(t *testing.T, peers ...*peerFixture) *Registry {
	c := RegistryConfig{OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, ReconnectMin: time.Millisecond, ReconnectMax: 10 * time.Millisecond, WatchdogInterval: time.Second, WatchdogTimeout: time.Second}
	for i, p := range peers {
		c.Peers = append(c.Peers, PeerConfig{Name: string(rune('a' + i)), Address: p.ln.Addr().String(), Transport: "tcp"})
	}
	r := BuildRegistry(c)
	r.Start()
	for _, p := range peers {
		close((<-p.handshakes).permit)
	}
	waitReady(t, r.OverallReady)
	return r
}
func TestDirectPeersIntegration(t *testing.T) {
	h := newDirectFixture(t, "hss.example", "example", SLhApplicationID)
	m1 := newDirectFixture(t, "mme1.example", "example", SLgApplicationID)
	m2 := newDirectFixture(t, "mme2.example", "example", SLgApplicationID)
	r := startFixtures(t, h, m1, m2)
	defer r.Close(context.Background())
	rt, e := r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLhApplicationID, DestinationRealm: "example"})
	if e != nil {
		t.Fatal(e)
	}
	go rt.RoundTrip(context.Background(), diam.NewRequest(8388622, SLhApplicationID, dict.Default))
	if (<-h.requests).msg.Header.ApplicationID != SLhApplicationID {
		t.Fatal("RIR not HSS")
	}
	select {
	case <-m1.requests:
		t.Fatal("MME1 got RIR")
	case <-m2.requests:
		t.Fatal("MME2 got RIR")
	default:
	}
	rt, e = r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLgApplicationID, DestinationHost: "MME2.EXAMPLE.", DestinationRealm: "example"})
	if e != nil {
		t.Fatal(e)
	}
	go rt.RoundTrip(context.Background(), diam.NewRequest(8388620, SLgApplicationID, dict.Default))
	if (<-m2.requests).msg.Header.ApplicationID != SLgApplicationID {
		t.Fatal("PLR not MME2")
	}
	select {
	case <-m1.requests:
		t.Fatal("MME1 got PLR")
	case <-h.requests:
		t.Fatal("HSS got PLR")
	default:
	}
}
func TestMixedPeersIntegration(t *testing.T) {
	relay := newPeerFixture(t)
	h := newDirectFixture(t, "hss.example", "example", SLhApplicationID)
	m := newDirectFixture(t, "mme.example", "example", SLgApplicationID)
	r := startFixtures(t, relay, h, m)
	defer r.Close(context.Background())
	rt, _ := r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLhApplicationID, DestinationRealm: "example"})
	go rt.RoundTrip(context.Background(), diam.NewRequest(8388622, SLhApplicationID, dict.Default))
	<-h.requests
	select {
	case <-relay.requests:
		t.Fatal("relay got direct RIR")
	default:
	}
	rt, _ = r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLgApplicationID, DestinationHost: "mme.example", DestinationRealm: "example"})
	go rt.RoundTrip(context.Background(), diam.NewRequest(8388620, SLgApplicationID, dict.Default))
	req := <-m.requests
	req.fc.conn.Close()
	next := <-m.handshakes
	if r.OverallReady() == false {
		t.Fatal("relay did not preserve readiness")
	}
	close(next.permit)
	waitReady(t, r.OverallReady)
}
