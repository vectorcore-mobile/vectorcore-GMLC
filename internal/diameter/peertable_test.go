package diameter

import (
	"context"
	"testing"
)

func TestPeerTablePrefersExactDirectThenRelay(t *testing.T) {
	m1 := New(Config{}, func(context.Context) (RoundTripper, error) { return fake{}, nil })
	m2 := New(Config{}, func(context.Context) (RoundTripper, error) { return fake{}, nil })
	m1.set(StateReady, fake{})
	m2.set(StateReady, fake{})
	p := NewPeerTable()
	p.Upsert(m2, Capability{Ready: true, Relay: true, OriginHost: "relay", Applications: map[uint32]bool{RelayApplicationID: true}})
	p.Upsert(m1, Capability{Ready: true, OriginHost: "mme.example", OriginRealm: "example", Applications: map[uint32]bool{SLgApplicationID: true}})
	got, e := p.Select(RouteRequest{ApplicationID: SLgApplicationID, DestinationHost: "MME.EXAMPLE.", DestinationRealm: "example"})
	if e != nil || got != m1 {
		t.Fatal("exact direct not selected")
	}
	got, e = p.Select(RouteRequest{ApplicationID: SLhApplicationID, DestinationRealm: "other"})
	if e != nil || got != m2 {
		t.Fatal("relay fallback failed")
	}
}
