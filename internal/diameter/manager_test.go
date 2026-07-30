package diameter

import (
	"bytes"
	"context"
	"errors"
	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/fiorix/go-diameter/v4/diam/sm"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type fake struct{}

func (fake) RoundTrip(context.Context, *diam.Message) (*diam.Message, error) { return nil, nil }
func (fake) Close() error                                                    { return nil }
func TestReconnectReadinessAndShutdown(t *testing.T) {
	var n atomic.Int32
	m := New(Config{ReconnectMin: time.Millisecond, ReconnectMax: 2 * time.Millisecond}, func(context.Context) (RoundTripper, error) {
		if n.Add(1) < 2 {
			return nil, errors.New("down")
		}
		return fake{}, nil
	})
	m.Start()
	deadline := time.After(time.Second)
	for !m.Ready() {
		select {
		case <-deadline:
			t.Fatal("not ready")
		case <-time.After(time.Millisecond):
		}
	}
	if n.Load() < 2 {
		t.Fatal("no retry")
	}
	if e := m.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := m.RoundTrip(context.Background(), diam.NewRequest(1, 1, nil)); !errors.Is(e, ErrShutdown) {
		t.Fatal(e)
	}
}

func TestManagerLogsConnectionFailureAndReconnect(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)
	logs := &lockedBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	attempted := make(chan struct{}, 1)
	m := New(Config{ReconnectMin: time.Millisecond, ReconnectMax: time.Millisecond}, func(context.Context) (RoundTripper, error) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		return nil, errors.New("test dial failure")
	})
	m.SetName("peer-log")
	m.SetCapabilityCallback("127.0.0.1:3868", "tcp", nil)
	m.Start()
	defer m.Close(context.Background())

	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("manager did not attempt a connection")
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "diameter connection failed; reconnect scheduled") {
		if time.Now().After(deadline) {
			t.Fatalf("missing lifecycle failure log: %s", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
	got := logs.String()
	for _, want := range []string{"diameter peer manager starting", "peer-log", "127.0.0.1:3868", "transport=tcp", "diameter connection attempt", "reconnect scheduled"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lifecycle logs missing %q: %s", want, got)
		}
	}
}

func TestTCPAdapterLoopbackCERAndApplicationAnswer(t *testing.T) {
	if _, err := dict.Default.App(16777291, "auth"); err != nil {
		if err := dict.Default.Load(strings.NewReader(`<diameter><application id="16777291" type="auth" name="SLh"><vendor id="10415" name="3GPP"/><command code="8388622" short="LR" name="LCS-Routing-Info"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command><avp name="Serving-Node" code="2401" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Grouped"><rule avp="AVP" required="false"/></data></avp><avp name="MME-Name" code="2402" vendor-id="10415" must="M,V" may-encrypt="N"><data type="DiameterIdentity"/></avp><avp name="MME-Realm" code="2408" vendor-id="10415" must="V" may-encrypt="N"><data type="DiameterIdentity"/></avp></application><application id="16777255" type="auth" name="SLg"><vendor id="10415" name="3GPP"/><command code="8388620" short="PL" name="Provide-Location"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command><avp name="Location-Estimate" code="1242" vendor-id="0" must="M" may-encrypt="N"><data type="OctetString"/></avp></application></diameter>`)); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	machine := sm.New(&sm.Settings{OriginHost: "peer.example", OriginRealm: "example", VendorID: 10415, ProductName: "test", HostIPAddresses: []datatype.Address{datatype.Address(net.ParseIP("127.0.0.1"))}})
	machine.HandleIdx(diam.CommandIndex{AppID: 16777291, Code: 8388622, Request: true}, diam.HandlerFunc(func(c diam.Conn, m *diam.Message) {
		ans := m.Answer(diam.Success)
		ans.NewAVP(avp.OriginHost, avp.Mbit, 0, datatype.DiameterIdentity("peer.example"))
		ans.NewAVP(avp.OriginRealm, avp.Mbit, 0, datatype.DiameterIdentity("example"))
		_, _ = ans.WriteTo(c)
	}))
	go func() { _ = diam.Serve(ln, machine) }()
	rt, err := dialTCP(context.Background(), TCPConfig{Address: ln.Addr().String(), OriginHost: "gmlc.example", OriginRealm: "example", HostIP: net.ParseIP("127.0.0.1"), ConnectTimeout: time.Second, WatchdogInterval: time.Second, WatchdogTimeout: time.Second, Applications: []Application{{ID: 16777291, Commands: []uint32{8388622}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	m := diam.NewRequest(8388622, 16777291, nil)
	ans, err := rt.RoundTrip(context.Background(), m)
	if err != nil || ans == nil || ans.Header.HopByHopID != m.Header.HopByHopID {
		t.Fatalf("answer=%v err=%v", ans, err)
	}
}
