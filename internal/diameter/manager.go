// Package diameter provides transport lifecycle and correlation ownership.
// Applications supply a RoundTripper; application packages never retain live
// connections or Diameter correlation values in persistent state.
package diameter

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
)

var ErrUnavailable = errors.New("diameter peer unavailable")
var ErrConnectionLost = errors.New("diameter peer connection lost")
var ErrShutdown = errors.New("diameter service shutting down")

type State string

const (
	StateDown       State = "down"
	StateConnecting State = "connecting"
	StateReady      State = "ready"
	StateStopping   State = "stopping"
)

type RoundTripper interface {
	RoundTrip(context.Context, *diam.Message) (*diam.Message, error)
	Close() error
}
type Requester interface {
	RoundTrip(context.Context, *diam.Message) (*diam.Message, error)
}
type closeWatcher interface{ Closed() <-chan struct{} }
type Dialer func(context.Context) (RoundTripper, error)
type Config struct{ ReconnectMin, ReconnectMax time.Duration }
type Manager struct {
	dial               Dialer
	cfg                Config
	mu                 sync.RWMutex
	state              State
	rt                 RoundTripper
	stop               chan struct{}
	done               chan struct{}
	once               sync.Once
	generation         uint64
	onState            func(*Manager, Capability)
	address, transport string
	name               string
}

func New(cfg Config, dial Dialer) *Manager {
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = time.Second
	}
	if cfg.ReconnectMax < cfg.ReconnectMin {
		cfg.ReconnectMax = 30 * time.Second
	}
	return &Manager{cfg: cfg, dial: dial, state: StateDown, stop: make(chan struct{}), done: make(chan struct{})}
}
func (m *Manager) Start()              { go m.loop() }
func (m *Manager) SetName(name string) { m.mu.Lock(); m.name = name; m.mu.Unlock() }

// SetCapabilityCallback is installed before Start. It receives only current
// connection generations; a down notification carries Ready=false.
func (m *Manager) SetCapabilityCallback(address, transport string, fn func(*Manager, Capability)) {
	m.mu.Lock()
	m.address, m.transport, m.onState = address, transport, fn
	m.mu.Unlock()
}
func (m *Manager) loop() {
	defer func() {
		slog.Info("diameter peer manager stopped", "peer", m.name, "address", m.address, "transport", m.transport)
		close(m.done)
	}()
	slog.Info("diameter peer manager starting", "peer", m.name, "address", m.address, "transport", m.transport)
	delay := m.cfg.ReconnectMin
	for {
		select {
		case <-m.stop:
			return
		default:
		}
		m.set(StateConnecting, nil)
		slog.Info("diameter connection attempt", "peer", m.name, "address", m.address, "transport", m.transport)
		rt, err := m.dial(context.Background())
		if err == nil {
			m.mu.Lock()
			m.generation++
			generation := m.generation
			m.mu.Unlock()
			m.set(StateReady, rt)
			slog.Info("diameter peer ready", "peer", m.name, "address", m.address, "transport", m.transport, "generation", generation)
			m.publish(rt, generation, true)
			delay = m.cfg.ReconnectMin
			var lost <-chan struct{}
			if w, ok := rt.(closeWatcher); ok {
				lost = w.Closed()
			}
			select {
			case <-m.stop:
				_ = rt.Close()
				slog.Info("diameter peer stopping", "peer", m.name)
				m.publish(rt, generation, false)
				return
			case <-lost:
				m.set(StateDown, nil)
				slog.Warn("diameter peer connection lost", "peer", m.name, "address", m.address, "transport", m.transport)
				m.publish(rt, generation, false)
			}
			continue
		}
		slog.Warn("diameter connection failed; reconnect scheduled", "peer", m.name, "address", m.address, "transport", m.transport, "error", err, "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-m.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < m.cfg.ReconnectMax {
			delay *= 2
			if delay > m.cfg.ReconnectMax {
				delay = m.cfg.ReconnectMax
			}
		}
	}
}
func (m *Manager) set(state State, rt RoundTripper) {
	m.mu.Lock()
	m.state = state
	m.rt = rt
	m.mu.Unlock()
}

type capabilitySource interface{ Capability() Capability }

func (m *Manager) publish(rt RoundTripper, generation uint64, ready bool) {
	m.mu.RLock()
	cb, address, transport := m.onState, m.address, m.transport
	m.mu.RUnlock()
	if cb == nil {
		return
	}
	c := Capability{Address: address, Transport: transport, Generation: generation, Ready: ready}
	if ready {
		if source, ok := rt.(capabilitySource); ok {
			c = source.Capability()
			c.Address = address
			c.Transport = transport
			c.Generation = generation
			c.Ready = true
		}
	}
	cb(m, c)
}
func (m *Manager) State() State       { m.mu.RLock(); defer m.mu.RUnlock(); return m.state }
func (m *Manager) Generation() uint64 { m.mu.RLock(); defer m.mu.RUnlock(); return m.generation }
func (m *Manager) Ready() bool        { return m.State() == StateReady }
func (m *Manager) RoundTrip(ctx context.Context, msg *diam.Message) (*diam.Message, error) {
	m.mu.RLock()
	rt := m.rt
	state := m.state
	m.mu.RUnlock()
	if state == StateStopping {
		return nil, ErrShutdown
	}
	if rt == nil || state != StateReady {
		return nil, ErrUnavailable
	}
	return rt.RoundTrip(ctx, msg)
}
func (m *Manager) Close(ctx context.Context) error {
	m.once.Do(func() { m.set(StateStopping, nil); close(m.stop) })
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
