package diameter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/fiorix/go-diameter/v4/diam/avp"
	"github.com/fiorix/go-diameter/v4/diam/datatype"
	"github.com/fiorix/go-diameter/v4/diam/dict"
	"github.com/fiorix/go-diameter/v4/diam/sm"
	"github.com/fiorix/go-diameter/v4/diam/sm/smpeer"
	"github.com/ishidawataru/sctp"
)

type Application struct {
	ID       uint32
	Commands []uint32
}
type TransportConfig struct {
	Name, Address, Transport, OriginHost, OriginRealm string
	HostIP                                            net.IP
	ConnectTimeout, WatchdogInterval, WatchdogTimeout time.Duration
	Applications                                      []Application
	ExpectedOriginHost, ExpectedOriginRealm           string
}

type TCPConfig = TransportConfig

func (c TransportConfig) Validate() error {
	if c.Address == "" || (c.Transport != "tcp" && c.Transport != "sctp") || c.OriginHost == "" || c.OriginRealm == "" || c.HostIP == nil {
		return fmt.Errorf("diameter transport: incomplete configuration")
	}
	if c.ConnectTimeout <= 0 || c.WatchdogInterval <= 0 || c.WatchdogTimeout <= 0 {
		return fmt.Errorf("diameter tcp: invalid timeout")
	}
	return nil
}
func TransportDialer(c TransportConfig) Dialer {
	return func(ctx context.Context) (RoundTripper, error) { return dialTransport(ctx, c) }
}
func TCPDialer(c TCPConfig) Dialer { c.Transport = "tcp"; return TransportDialer(c) }

type tcpRT struct {
	conn       diam.Conn
	pending    map[uint32]chan result
	mu         sync.Mutex
	closed     chan struct{}
	once       sync.Once
	seq        atomic.Uint32
	capability Capability
	lossErr    error
}
type result struct {
	msg *diam.Message
	err error
}

func dialTCP(ctx context.Context, c TCPConfig) (*tcpRT, error) {
	c.Transport = "tcp"
	return dialTransport(ctx, c)
}
func dialTransport(ctx context.Context, c TransportConfig) (*tcpRT, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rt := &tcpRT{pending: map[uint32]chan result{}, closed: make(chan struct{})}
	settings := &sm.Settings{OriginHost: datatype.DiameterIdentity(c.OriginHost), OriginRealm: datatype.DiameterIdentity(c.OriginRealm), VendorID: datatype.Unsigned32(VendorID), ProductName: datatype.UTF8String(ProductName), HostIPAddresses: []datatype.Address{datatype.Address(c.HostIP)}}
	machine := sm.New(settings)
	// {0,0,false} is not the library's wildcard sentinel — diam.ALL_CMD_INDEX
	// ({^0,^0,false}, see diam/server.go) is. An answer whose application
	// isn't in dict.Default fails ServeMux's FindCommand lookup and falls
	// through to ALL_CMD_INDEX before ever reaching the exact-match handlers
	// registered below; without this, that fallback finds nothing and the
	// answer is silently dropped instead of delivered.
	machine.HandleIdx(diam.ALL_CMD_INDEX, diam.HandlerFunc(func(_ diam.Conn, m *diam.Message) { rt.deliver(m) }))
	// Applications with dictionary entries (or that arrive before the
	// catch-all above, per exact-index-first ServeMux lookup order) are
	// still matched by exact command index here.
	apps := make([]*diam.AVP, 0, len(c.Applications))
	for _, app := range c.Applications {
		for _, code := range app.Commands {
			machine.HandleIdx(diam.CommandIndex{AppID: app.ID, Code: code, Request: false}, diam.HandlerFunc(func(_ diam.Conn, m *diam.Message) { rt.deliver(m) }))
		}
		apps = append(apps, vendorApp(VendorID, app.ID))
	}
	cli := &sm.Client{Dict: dict.Default, Handler: machine, MaxRetransmits: 0, EnableWatchdog: true, WatchdogInterval: c.WatchdogInterval, RetransmitInterval: c.WatchdogTimeout, SupportedVendorID: []*diam.AVP{diam.NewAVP(avp.SupportedVendorID, avp.Mbit, 0, datatype.Unsigned32(SupportedVendorID))}, VendorSpecificApplicationID: apps}
	type dialResult struct {
		c   diam.Conn
		err error
	}
	done := make(chan dialResult, 1)
	go func() {
		var x diam.Conn
		var e error
		if c.Transport == "sctp" {
			x, e = dialSCTP(cli, c)
		} else {
			x, e = cli.DialTimeout(c.Address, c.ConnectTimeout)
		}
		done <- dialResult{x, e}
	}()
	select {
	case x := <-done:
		if x.err != nil {
			return nil, x.err
		}
		slog.Debug("diameter transport connected; awaiting CER/CEA", "peer", c.Name, "address", c.Address, "transport", c.Transport)
		rt.conn = x.c
		meta, ok := smpeer.FromContext(x.c.Context())
		if !ok {
			x.c.Close()
			return nil, fmt.Errorf("diameter %s: missing negotiated CEA metadata", c.Transport)
		}
		if (c.ExpectedOriginHost != "" && norm(string(meta.OriginHost)) != norm(c.ExpectedOriginHost)) || (c.ExpectedOriginRealm != "" && norm(string(meta.OriginRealm)) != norm(c.ExpectedOriginRealm)) {
			x.c.Close()
			return nil, fmt.Errorf("diameter %s: negotiated peer identity does not match expected identity", c.Transport)
		}
		rt.capability = Capability{OriginHost: string(meta.OriginHost), OriginRealm: string(meta.OriginRealm), Applications: map[uint32]bool{}}
		for _, id := range meta.Applications {
			rt.capability.Applications[id] = true
		}
		rt.capability.Relay = rt.capability.Applications[RelayApplicationID]
		slog.Debug("diameter CER/CEA negotiation completed", "peer", c.Name, "address", c.Address, "transport", c.Transport)
		go rt.watch()
		return rt, nil
	case <-ctx.Done():
		// The dial goroutine may still be in flight and later succeed; if it
		// does, nobody else will ever observe or close that connection, so
		// drain it here to avoid leaking a live socket.
		go func() {
			if x := <-done; x.err == nil && x.c != nil {
				x.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
func dialSCTP(cli *sm.Client, c TransportConfig) (diam.Conn, error) {
	raddr, err := sctp.ResolveSCTPAddr("sctp", c.Address)
	if err != nil {
		return nil, fmt.Errorf("resolve SCTP peer %s: %w", c.Address, err)
	}
	// Bind to the route-selected local address rather than the static
	// configured host_ip_address. An unbound (or wrong-address-bound) SCTP
	// socket lets Linux advertise every local address (loopback, Docker
	// bridges, other NICs) as valid paths for the association; a peer that
	// keys admission on source IP can accept the initial handshake and then
	// reject once traffic actually uses one of those other addresses.
	// host_ip_address only needs to be correct for the CER AVP — the bind
	// itself must match what routing to c.Address would actually pick.
	local, err := routeLocalAddress(c.Address)
	if err != nil {
		return nil, fmt.Errorf("resolve SCTP local address for %s: %w", c.Address, err)
	}
	laddr := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: local}}}
	raw, err := (&sctp.SocketConfig{InitMsg: sctp.InitMsg{NumOstreams: diam.MaxOutboundSCTPStreams, MaxInstreams: diam.MaxInboundSCTPStreams}}).Dial("sctp", laddr, raddr)
	if err != nil {
		return nil, fmt.Errorf("dial SCTP peer %s: %w", c.Address, err)
	}
	return cli.NewConn(diam.NewSCTPConn(raw), c.Address)
}

// routeLocalAddress asks the kernel which local address it would actually
// select to reach remote (no packets are sent — UDP dial just resolves the
// route), rather than trusting a possibly-stale configured address.
func routeLocalAddress(remote string) (net.IP, error) {
	conn, err := net.Dial("udp", remote)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return nil, fmt.Errorf("no local IP selected for %s", remote)
	}
	return addr.IP, nil
}
func vendorApp(v, id uint32) *diam.AVP {
	return diam.NewAVP(avp.VendorSpecificApplicationID, avp.Mbit, 0, &diam.GroupedAVP{AVP: []*diam.AVP{diam.NewAVP(avp.VendorID, avp.Mbit, 0, datatype.Unsigned32(v)), diam.NewAVP(avp.AuthApplicationID, avp.Mbit, 0, datatype.Unsigned32(id))}})
}
func (r *tcpRT) RoundTrip(ctx context.Context, m *diam.Message) (*diam.Message, error) {
	if m == nil {
		return nil, errors.New("nil diameter message")
	}
	id := r.seq.Add(1)
	m.Header.HopByHopID = id
	ch := make(chan result, 1)
	r.mu.Lock()
	select {
	case <-r.closed:
		r.mu.Unlock()
		return nil, ErrUnavailable
	default:
		r.pending[id] = ch
	}
	r.mu.Unlock()
	if _, err := m.WriteTo(r.conn); err != nil {
		r.remove(id)
		return nil, err
	}
	select {
	case x := <-ch:
		return x.msg, x.err
	case <-ctx.Done():
		r.remove(id)
		return nil, ctx.Err()
	case <-r.closed:
		r.remove(id)
		return nil, r.closedError()
	}
}
func (r *tcpRT) deliver(m *diam.Message) {
	if m.Header.CommandFlags&diam.RequestFlag != 0 {
		return
	}
	r.mu.Lock()
	ch := r.pending[m.Header.HopByHopID]
	delete(r.pending, m.Header.HopByHopID)
	r.mu.Unlock()
	if ch != nil {
		ch <- result{msg: m}
	}
}
func (r *tcpRT) remove(id uint32) { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }
func (r *tcpRT) watch() {
	if n, ok := r.conn.(diam.CloseNotifier); ok {
		<-n.CloseNotify()
	}
	r.failAll(ErrConnectionLost)
}
func (r *tcpRT) failAll(err error) {
	r.once.Do(func() {
		r.lossErr = err
		close(r.closed)
		r.mu.Lock()
		defer r.mu.Unlock()
		for id, ch := range r.pending {
			delete(r.pending, id)
			ch <- result{err: err}
		}
	})
}
func (r *tcpRT) closedError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lossErr != nil {
		return r.lossErr
	}
	return ErrUnavailable
}
func (r *tcpRT) Closed() <-chan struct{} { return r.closed }
func (r *tcpRT) Capability() Capability {
	c := r.capability
	c.Applications = map[uint32]bool{}
	for k, v := range r.capability.Applications {
		c.Applications[k] = v
	}
	return c
}
func (r *tcpRT) Close() error {
	if r.conn != nil {
		r.conn.Close()
	}
	r.failAll(ErrShutdown)
	return nil
}
