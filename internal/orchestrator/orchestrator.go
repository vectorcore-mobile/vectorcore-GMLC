// Package orchestrator owns durable, single-worker request dispatch.
package orchestrator

import (
	"context"
	"errors"
	"github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
	"log/slog"
	"sync"
	"time"
)

type Resolver interface {
	ResolveServingNode(context.Context, domain.Target) (domain.ServingNode, error)
}
type Provider interface {
	ProvideLocation(context.Context, domain.ServingNode, domain.LocationRequest) (domain.PositioningResult, error)
}
type Worker struct {
	store    storage.Store
	resolver Resolver
	provider Provider
	wake     chan struct{}
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	active   map[string]context.CancelFunc
}

const maxAttempts = 3

func New(s storage.Store, r Resolver, p Provider) *Worker {
	return &Worker{store: s, resolver: r, provider: p, wake: make(chan struct{}, 1), done: make(chan struct{}), active: map[string]context.CancelFunc{}}
}
func (w *Worker) Start(ctx context.Context) {
	c, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	slog.Info("location request worker starting")
	go w.loop(c)
}
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
		slog.Debug("location request worker notified")
	default:
	}
}
func (w *Worker) Cancel(id string) {
	w.mu.Lock()
	c := w.active[id]
	w.mu.Unlock()
	if c != nil {
		slog.Debug("active location request cancellation signalled", "request_id", id)
		c()
	}
}
func (w *Worker) Close(ctx context.Context) error {
	if w.cancel != nil {
		slog.Info("location request worker stopping")
		w.cancel()
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *Worker) loop(ctx context.Context) {
	defer func() {
		slog.Info("location request worker stopped")
		close(w.done)
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if !w.one(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			case <-tick.C:
			}
		}
	}
}
func (w *Worker) one(ctx context.Context) bool {
	r, ok, e := w.store.ClaimNextQueued(ctx, time.Now())
	if e != nil || !ok {
		if e != nil && ctx.Err() == nil {
			slog.Warn("location request claim failed")
		}
		return false
	}
	slog.Debug("location request claimed", "request_id", r.ID, "attempt", r.AttemptCount)
	c, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.active[r.ID] = cancel
	w.mu.Unlock()
	defer func() { cancel(); w.mu.Lock(); delete(w.active, r.ID); w.mu.Unlock() }()
	t := domain.Target{}
	if r.TargetKind == "imsi" {
		t.IMSI = r.TargetValue
	} else {
		t.MSISDN = r.TargetValue
	}
	n, e := w.resolver.ResolveServingNode(c, t)
	if e != nil {
		if c.Err() != nil {
			slog.Debug("location request resolution interrupted", "request_id", r.ID)
			_ = w.store.Requeue(context.Background(), r.ID, domain.StateResolving, time.Now(), "shutdown_interrupted")
			return true
		}
		w.handleError(r.ID, r.AttemptCount, domain.StateResolving, e)
		return true
	}
	if e = w.store.SaveServingNodeAndLocate(c, r.ID, n); e != nil {
		slog.Debug("location request serving-node transition not applied", "request_id", r.ID)
		return true
	}
	slog.Debug("location request serving node resolved", "request_id", r.ID)
	out, e := w.provider.ProvideLocation(c, n, domain.LocationRequest{Target: t, LocationType: 0, ClientType: 1, ClientName: r.ClientID})
	if e != nil {
		if c.Err() != nil {
			slog.Debug("location request positioning interrupted", "request_id", r.ID)
			_ = w.store.Requeue(context.Background(), r.ID, domain.StateLocating, time.Now(), "shutdown_interrupted")
			return true
		}
		w.handleError(r.ID, r.AttemptCount, domain.StateLocating, e)
		return true
	}
	var lat, lon *float64
	if out.Position != nil {
		lat = &out.Position.Latitude
		lon = &out.Position.Longitude
	}
	if e = w.store.CompleteRequest(c, r.ID, domain.Result{RequestID: r.ID, RawGAD: out.RawLocationEstimate, Shape: func() string {
		if out.Position != nil {
			return out.Position.Shape
		}
		return ""
	}(), Latitude: lat, Longitude: lon, ECGI: out.ECGI, CreatedAt: time.Now(), Source: n.Source}); e != nil {
		slog.Warn("location request completion not applied", "request_id", r.ID)
		return true
	}
	slog.Info("location request completed", "request_id", r.ID)
	return true
}
func (w *Worker) handleError(id string, attempts int, from domain.State, e error) {
	if errors.Is(e, diameter.ErrUnavailable) || errors.Is(e, diameter.ErrConnectionLost) || errors.Is(e, context.DeadlineExceeded) {
		if attempts >= maxAttempts {
			slog.Warn("location request retry budget exhausted", "request_id", id, "attempt", attempts, "failure_code", "temporarily_unavailable")
			_ = w.store.FailRequest(context.Background(), id, from, "temporarily_unavailable", "location service temporarily unavailable")
			return
		}
		delay := time.Second * time.Duration(1<<(attempts-1))
		slog.Warn("location request transient failure; retry scheduled", "request_id", id, "attempt", attempts, "delay", delay, "failure_code", "temporarily_unavailable")
		_ = w.store.Requeue(context.Background(), id, from, time.Now().Add(delay), "temporarily_unavailable")
		return
	}
	slog.Warn("location request failed", "request_id", id, "failure_code", "network_failure")
	_ = w.store.FailRequest(context.Background(), id, from, "network_failure", "location request failed")
}
