package diameter

import (
	"context"
	"log/slog"
	"net"
	"time"
)

// PeerConfig contains connectivity/admission details only. Remote application
// and relay roles are learned from the CEA, never configured.
type PeerConfig struct{ Name, Address, Transport, ExpectedOriginHost, ExpectedOriginRealm string }
type RegistryConfig struct {
	OriginHost, OriginRealm                                                       string
	HostIP                                                                        net.IP
	ConnectTimeout, ReconnectMin, ReconnectMax, WatchdogInterval, WatchdogTimeout time.Duration
	Peers                                                                         []PeerConfig
}
type Registry struct {
	table    *PeerTable
	managers []*Manager
}

func BuildRegistry(c RegistryConfig) *Registry {
	r := &Registry{table: NewPeerTable()}
	for _, p := range c.Peers {
		p := p
		tc := TransportConfig{Name: p.Name, Address: p.Address, Transport: p.Transport, OriginHost: c.OriginHost, OriginRealm: c.OriginRealm, HostIP: c.HostIP, ConnectTimeout: c.ConnectTimeout, WatchdogInterval: c.WatchdogInterval, WatchdogTimeout: c.WatchdogTimeout, ExpectedOriginHost: p.ExpectedOriginHost, ExpectedOriginRealm: p.ExpectedOriginRealm, Applications: []Application{{ID: SLhApplicationID, Commands: []uint32{8388622}}, {ID: SLgApplicationID, Commands: []uint32{8388620}}}}
		m := New(Config{ReconnectMin: c.ReconnectMin, ReconnectMax: c.ReconnectMax}, TransportDialer(tc))
		m.SetName(p.Name)
		m.SetCapabilityCallback(p.Address, p.Transport, func(m *Manager, cap Capability) {
			if cap.Ready {
				slog.Debug("diameter CEA negotiated", "peer", p.Name, "origin_host", cap.OriginHost, "origin_realm", cap.OriginRealm, "relay", cap.Relay, "applications", cap.Applications, "generation", cap.Generation)
				r.table.Upsert(m, cap)
			} else {
				slog.Debug("diameter capability withdrawn", "peer", p.Name, "generation", cap.Generation)
				r.table.Remove(m, cap.Generation)
			}
		})
		r.managers = append(r.managers, m)
	}
	return r
}
func (r *Registry) Start() {
	if len(r.managers) == 0 {
		return
	}
	slog.Info("diameter registry starting", "peers", len(r.managers))
	for _, m := range r.managers {
		m.Start()
	}
}
func (r *Registry) RoundTripperFor(_ context.Context, request RouteRequest) (Requester, error) {
	return r.table.Select(request)
}
func (r *Registry) OverallReady() bool {
	return r.table.ReadyFor(SLhApplicationID) && r.table.ReadyFor(SLgApplicationID)
}
func (r *Registry) Close(ctx context.Context) error {
	slog.Info("diameter registry stopping", "peers", len(r.managers))
	for _, m := range r.managers {
		if err := m.Close(ctx); err != nil {
			return err
		}
	}
	slog.Info("diameter registry stopped")
	return nil
}
