package diameter

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
)

func rawDiameterPeer(t *testing.T, apps []uint32, check func(*diam.Message)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		cer, err := diam.ReadMessage(c, dict.Default)
		if err != nil {
			return
		}
		if check != nil {
			check(cer)
		}
		cea := cer.Answer(diam.Success)
		cea.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity("peer.example"))
		cea.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
		cea.NewAVP(avp.HostIPAddress, avp.Mbit, 0, datatype.Address(net.ParseIP("127.0.0.1")))
		cea.NewAVP(avp.VendorID, avp.Mbit, 0, datatype.Unsigned32(VendorID))
		cea.NewAVP(avp.ProductName, 0, 0, datatype.UTF8String("peer"))
		for _, id := range apps {
			cea.NewAVP(avp.AuthApplicationID, avp.Mbit, 0, datatype.Unsigned32(id))
			cea.AddAVP(vendorApp(VendorID, id))
		}
		_, _ = cea.WriteTo(c)
	}()
	return ln
}
func avps(m *diam.Message, code uint32) []*diam.AVP {
	var out []*diam.AVP
	for _, a := range m.AVP {
		if a.Code == code {
			out = append(out, a)
		}
	}
	return out
}
func TestProductionCERWireCapabilities(t *testing.T) {
	seen := make(chan *diam.Message, 1)
	ln := rawDiameterPeer(t, []uint32{RelayApplicationID}, func(m *diam.Message) { seen <- m })
	defer ln.Close()
	rt, err := dialTransport(context.Background(), TransportConfig{Address: ln.Addr().String(), Transport: "tcp", OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, WatchdogInterval: time.Second, WatchdogTimeout: time.Second, Applications: []Application{{ID: SLhApplicationID}, {ID: SLgApplicationID}}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	cer := <-seen
	for _, tt := range []struct {
		code uint32
		want string
	}{{avp.OriginHost, "gmlc.example"}, {avp.OriginRealm, "example"}, {avp.ProductName, ProductName}} {
		a, e := cer.FindAVP(tt.code, 0)
		got := ""
		if a != nil {
			switch v := a.Data.(type) {
			case datatype.DiameterIdentity:
				got = string(v)
			case datatype.UTF8String:
				got = string(v)
			}
		}
		if e != nil || got != tt.want {
			t.Fatalf("CER AVP %d = %v, %v", tt.code, a, e)
		}
	}
	if a, e := cer.FindAVP(avp.VendorID, 0); e != nil || uint32(a.Data.(datatype.Unsigned32)) != VendorID {
		t.Fatal("wrong Vendor-Id")
	}
	if a, e := cer.FindAVP(avp.InbandSecurityID, 0); e != nil || uint32(a.Data.(datatype.Unsigned32)) != 0 {
		t.Fatal("wrong Inband-Security-Id")
	}
	if len(avps(cer, avp.SupportedVendorID)) != 1 {
		t.Fatal("missing Supported-Vendor-Id")
	}
	if len(avps(cer, avp.HostIPAddress)) != 1 {
		t.Fatal("missing Host-IP-Address")
	}
	got := map[uint32]bool{}
	for _, a := range avps(cer, avp.VendorSpecificApplicationID) {
		g := a.Data.(*diam.GroupedAVP)
		for _, x := range g.AVP {
			if x.Code == avp.AuthApplicationID {
				got[uint32(x.Data.(datatype.Unsigned32))] = true
			}
		}
	}
	if !got[SLhApplicationID] || !got[SLgApplicationID] {
		t.Fatalf("CER apps=%v", got)
	}
}
func TestProductionCEAUsesVendorSpecificCapabilities(t *testing.T) {
	ln := rawDiameterPeer(t, []uint32{RelayApplicationID}, nil)
	defer ln.Close()
	rt, err := dialTransport(context.Background(), TransportConfig{Address: ln.Addr().String(), Transport: "tcp", OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, WatchdogInterval: time.Second, WatchdogTimeout: time.Second, Applications: []Application{{ID: SLhApplicationID}, {ID: SLgApplicationID}}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if !rt.Capability().Relay || !rt.Capability().Applications[RelayApplicationID] {
		t.Fatal("relay CEA not published")
	}
}
