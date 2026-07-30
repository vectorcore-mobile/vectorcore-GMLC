package diameter

import (
	"fmt"
	"strings"
	"sync"
)

type RouteRequest struct {
	ApplicationID                     uint32
	DestinationHost, DestinationRealm string
}
type Capability struct {
	Address, Transport, OriginHost, OriginRealm string
	Generation                                  uint64
	Ready, Relay                                bool
	Applications                                map[uint32]bool
}
type PeerTable struct {
	mu    sync.RWMutex
	peers []tablePeer
}
type tablePeer struct {
	manager    *Manager
	capability Capability
}

func NewPeerTable() *PeerTable { return &PeerTable{} }
func norm(v string) string     { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), ".")) }
func (p *PeerTable) Upsert(m *Manager, c Capability) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.peers {
		if p.peers[i].manager == m {
			if p.peers[i].capability.Generation > c.Generation {
				return
			}
			p.peers[i].capability = c
			return
		}
	}
	p.peers = append(p.peers, tablePeer{m, c})
}
func (p *PeerTable) Remove(m *Manager, generation uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.peers {
		if p.peers[i].manager == m {
			if p.peers[i].capability.Generation != generation {
				return
			}
			p.peers[i].capability.Ready = false
			p.peers[i].capability.Applications = nil
		}
	}
}
func (p *PeerTable) Select(r RouteRequest) (Requester, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var exactHost, realmDirect, anyDirect, relay []tablePeer
	for _, x := range p.peers {
		c := x.capability
		if !c.Ready || !x.manager.Ready() {
			continue
		}
		if c.Relay {
			relay = append(relay, x)
			continue
		}
		if !c.Applications[r.ApplicationID] {
			continue
		}
		anyDirect = append(anyDirect, x)
		if r.DestinationHost != "" && norm(c.OriginHost) == norm(r.DestinationHost) {
			exactHost = append(exactHost, x)
		}
		if r.DestinationRealm != "" && norm(c.OriginRealm) == norm(r.DestinationRealm) {
			realmDirect = append(realmDirect, x)
		}
	}
	if len(exactHost) > 0 {
		return exactHost[0].manager, nil
	}
	if len(realmDirect) > 0 {
		return realmDirect[0].manager, nil
	}
	if r.DestinationHost == "" && r.DestinationRealm == "" && len(anyDirect) > 0 {
		return anyDirect[0].manager, nil
	}
	if len(relay) > 0 {
		return relay[0].manager, nil
	}
	return nil, fmt.Errorf("diameter route unavailable for application %d", r.ApplicationID)
}
func (p *PeerTable) Capability(m *Manager) (Capability, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, x := range p.peers {
		if x.manager == m {
			return x.capability, x.capability.Ready
		}
	}
	return Capability{}, false
}
func (p *PeerTable) ReadyFor(app uint32) bool {
	_, e := p.Select(RouteRequest{ApplicationID: app})
	return e == nil
}
