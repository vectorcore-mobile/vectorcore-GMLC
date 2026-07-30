//go:build linux && !386

package diameter

import (
	"bytes"
	"context"
	"errors"
	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/ishidawataru/sctp"
	"net"
	"testing"
	"time"
)

type sctpEvent struct {
	c    *sctp.SCTPConn
	m    *diam.Message
	ppid uint32
}

func writeSCTP(c *sctp.SCTPConn, m *diam.Message) error {
	var b bytes.Buffer
	_, e := m.WriteTo(&b)
	if e != nil {
		return e
	}
	_, e = c.SCTPWrite(b.Bytes(), &sctp.SndRcvInfo{PPID: diam.DiameterPPID})
	return e
}
func TestSCTPIntegrationLifecycle(t *testing.T) {
	ensureFixtureApps()
	ln, e := sctp.ListenSCTP("sctp", &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, Port: 0})
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	events := make(chan sctpEvent, 32)
	accepts := make(chan *sctp.SCTPConn, 4)
	go func() {
		for {
			c, e := ln.AcceptSCTP()
			if e != nil {
				return
			}
			accepts <- c
			go func(c *sctp.SCTPConn) {
				defer c.Close()
				_ = c.SubscribeEvents(sctp.SCTP_EVENT_DATA_IO)
				buf := make([]byte, 65535)
				for {
					n, info, e := c.SCTPRead(buf)
					if e != nil {
						return
					}
					m, e := diam.ReadMessage(bytes.NewReader(buf[:n]), dict.Default)
					if e != nil {
						return
					}
					if info == nil {
						continue
					}
					events <- sctpEvent{c, m, info.PPID}
				}
			}(c)
		}
	}()
	r := BuildRegistry(RegistryConfig{OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, ReconnectMin: time.Millisecond, ReconnectMax: 10 * time.Millisecond, WatchdogInterval: 100 * time.Millisecond, WatchdogTimeout: time.Second, Peers: []PeerConfig{{Name: "sctp", Address: ln.Addr().String(), Transport: "sctp"}}})
	r.Start()
	defer r.Close(context.Background())
	first := <-accepts
	cer := <-events
	if cer.m.Header.CommandCode != diam.CapabilitiesExchange || cer.ppid != 46 {
		t.Fatalf("CER ppid=%d command=%d", cer.ppid, cer.m.Header.CommandCode)
	}
	cea := cer.m.Answer(diam.Success)
	cea.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity("sctp.peer.example"))
	cea.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
	cea.NewAVP(avp.AuthApplicationID, avp.Mbit, 0, datatype.Unsigned32(RelayApplicationID))
	if e := writeSCTP(first, cea); e != nil {
		t.Fatal(e)
	}
	waitReady(t, r.OverallReady)
	g1 := r.managers[0].Generation()
	var dwr sctpEvent
	deadline := time.After(time.Second)
	for {
		select {
		case x := <-events:
			if x.m.Header.CommandCode == diam.DeviceWatchdog {
				dwr = x
				goto gotDWR
			}
		case <-deadline:
			t.Fatal("no SCTP DWR")
		}
	}
gotDWR:
	if dwr.ppid != 46 {
		t.Fatalf("DWR ppid=%d", dwr.ppid)
	}
	if e := writeSCTP(first, dwr.m.Answer(diam.Success)); e != nil {
		t.Fatal(e)
	}
	rt, e := r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLhApplicationID})
	if e != nil {
		t.Fatal(e)
	}
	out := make(chan error, 1)
	go func() {
		_, e := rt.RoundTrip(context.Background(), diam.NewRequest(8388622, SLhApplicationID, dict.Default))
		out <- e
	}()
	var rir sctpEvent
	for {
		x := <-events
		if x.m.Header.CommandCode == 8388622 {
			rir = x
			break
		}
	}
	if rir.ppid != 46 {
		t.Fatalf("RIR ppid=%d", rir.ppid)
	}
	first.Close()
	if e := <-out; !errors.Is(e, ErrConnectionLost) {
		t.Fatalf("loss=%v", e)
	}
	downDeadline := time.Now().Add(time.Second)
	for r.managers[0].Ready() && time.Now().Before(downDeadline) {
		time.Sleep(time.Millisecond)
	}
	if r.managers[0].Ready() {
		t.Fatal("manager stayed ready")
	}
	second := <-accepts
	cer = <-events
	if cer.m.Header.CommandCode != diam.CapabilitiesExchange || cer.ppid != 46 {
		t.Fatal("second CER missing")
	}
	cea = cer.m.Answer(diam.Success)
	cea.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity("sctp.peer.example"))
	cea.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
	cea.NewAVP(avp.AuthApplicationID, avp.Mbit, 0, datatype.Unsigned32(RelayApplicationID))
	if e := writeSCTP(second, cea); e != nil {
		t.Fatal(e)
	}
	waitReady(t, r.OverallReady)
	if r.managers[0].Generation() <= g1 {
		t.Fatal("generation not advanced")
	}
	rt, _ = r.RoundTripperFor(context.Background(), RouteRequest{ApplicationID: SLhApplicationID})
	go func() {
		_, e := rt.RoundTrip(context.Background(), diam.NewRequest(8388622, SLhApplicationID, dict.Default))
		out <- e
	}()
	for {
		x := <-events
		if x.m.Header.CommandCode == 8388622 {
			if e := writeSCTP(second, x.m.Answer(diam.Success)); e != nil {
				t.Fatal(e)
			}
			break
		}
	}
	if e := <-out; e != nil {
		t.Fatal(e)
	}
	r.table.Remove(r.managers[0], g1)
	if !r.OverallReady() {
		t.Fatal("stale withdrawal removed new generation")
	}
	if e := r.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := r.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
}
