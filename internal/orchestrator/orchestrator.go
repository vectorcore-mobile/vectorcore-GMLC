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
	// LCS-Client-Type is an operator-configured, per-client trust attribute
	// (config.Client.LCSClientType), never caller-supplied: EMERGENCY_SERVICES
	// and LAWFUL_INTERCEPT_SERVICES carry regulatory weight downstream, so
	// only a trusted deployment config may grant them. Falls back to
	// VALUE_ADDED_SERVICES, the prior hardcoded behavior, if the client
	// record can't be read here (it was already required to exist to reach
	// this point).
	clientType := domain.ClientTypeValueAddedServices
	if client, ce := w.store.GetClientCredential(c, r.ClientID); ce == nil {
		clientType = client.LCSClientType
	} else {
		slog.Warn("client lookup failed; defaulting LCS-Client-Type to VALUE_ADDED_SERVICES", "request_id", r.ID, "client_id", r.ClientID)
	}
	out, e := w.provider.ProvideLocation(c, n, domain.LocationRequest{Target: t, LocationType: 0, ClientType: clientType, ClientName: r.ClientID})
	if e != nil {
		if c.Err() != nil {
			slog.Debug("location request positioning interrupted", "request_id", r.ID)
			_ = w.store.Requeue(context.Background(), r.ID, domain.StateLocating, time.Now(), "shutdown_interrupted")
			return true
		}
		w.handleError(r.ID, r.AttemptCount, domain.StateLocating, e)
		return true
	}
	// A successful PLA carrying neither a location estimate nor additional
	// information (deferred/no-immediate-result) must never complete the
	// REST request — there is no positioning result to hand back.
	if out.Kind == "no_immediate_result" || out.Kind == "deferred" {
		slog.Warn("location request produced no immediate result", "request_id", r.ID, "kind", out.Kind)
		_ = w.store.FailRequest(context.Background(), r.ID, domain.StateLocating, "no_immediate_result", "PLA returned no immediate positioning result")
		return true
	}
	var lat, lon, uncertainty, semiMajor, semiMinor, orientation *float64
	var confidence *uint32
	shape := ""
	if out.Position != nil {
		lat = &out.Position.Latitude
		lon = &out.Position.Longitude
		uncertainty = out.Position.UncertaintyMeters
		semiMajor = out.Position.SemiMajorMeters
		semiMinor = out.Position.SemiMinorMeters
		orientation = out.Position.OrientationDegrees
		confidence = out.Position.ConfidencePercent
		shape = out.Position.Shape
	}
	// AgeOfLocationEstimate/AccuracyFulfilment/RawVelocityEstimate/
	// EUTRANPositioningData are independent of Position (they can accompany
	// an ECGI-only PLA too), so they're read from out directly, not out.Position.
	result := domain.Result{RequestID: r.ID, RawGAD: out.RawLocationEstimate, Shape: shape, Latitude: lat, Longitude: lon, UncertaintyMeters: uncertainty, SemiMajorMeters: semiMajor, SemiMinorMeters: semiMinor, OrientationDegrees: orientation, ConfidencePercent: confidence, AgeOfLocationEstimate: out.AgeOfLocationEstimate, AccuracyFulfilment: out.AccuracyFulfilment, RawVelocityEstimate: out.RawVelocityEstimate, EUTRANPositioningData: out.EUTRANPositioningData, ECGI: out.ECGI, CreatedAt: time.Now(), Source: n.Source}
	if e = w.store.CompleteRequest(c, r.ID, result); e != nil {
		slog.Warn("location request completion not applied", "request_id", r.ID)
		return true
	}
	slog.Info("location request completed", "request_id", r.ID)
	return true
}
func (w *Worker) handleError(id string, attempts int, from domain.State, e error) {
	if errors.Is(e, diameter.ErrUnavailable) || errors.Is(e, diameter.ErrConnectionLost) || errors.Is(e, context.DeadlineExceeded) {
		if attempts >= maxAttempts {
			slog.Warn("location request retry budget exhausted", "request_id", id, "attempt", attempts, "failure_code", "temporarily_unavailable", "error", e)
			_ = w.store.FailRequest(context.Background(), id, from, "temporarily_unavailable", "location service temporarily unavailable")
			return
		}
		delay := time.Second * time.Duration(1<<(attempts-1))
		slog.Warn("location request transient failure; retry scheduled", "request_id", id, "attempt", attempts, "delay", delay, "failure_code", "temporarily_unavailable", "error", e)
		_ = w.store.Requeue(context.Background(), id, from, time.Now().Add(delay), "temporarily_unavailable")
		return
	}
	slog.Warn("location request failed", "request_id", id, "failure_code", "network_failure", "error", e)
	_ = w.store.FailRequest(context.Background(), id, from, "network_failure", "location request failed")
}
