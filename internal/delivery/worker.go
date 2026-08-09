package delivery

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/vectorcore/gmlc/internal/storage"
)

// Config controls delivery attempt pacing. Callers should apply defaults
// (see New) before relying on zero-value fields.
type Config struct {
	MaxAttempts     int
	RetryBackoffMin time.Duration
	RetryBackoffMax time.Duration
	RequestTimeout  time.Duration
	// EncryptionKey is the 32-byte AES-256 key used to decrypt callback
	// secrets read back from storage (see secret.go). Required; New panics
	// if it isn't exactly 32 bytes, since a worker that can't decrypt any
	// secret can't do its job at all — better to fail at startup than
	// silently fail every delivery.
	EncryptionKey []byte
}

// Worker polls for due deliveries and POSTs them to their subscription's
// callback URL, HMAC-signed. It runs on its own goroutine/ticker loop,
// entirely separate from internal/orchestrator.Worker — deliberately, not
// just for package separation. An HTTP callback endpoint is arbitrary,
// external, and can hang or be slow in ways a GMLC operator doesn't
// control; the Diameter-facing orchestrator worker already has a proven
// failure mode where one unbounded call wedged its entire single-worker
// loop (a missing SLg RequestTimeout, fixed separately) — sharing a loop
// between Diameter request processing and arbitrary outbound HTTP calls
// would reintroduce exactly that risk from a new direction.
type Worker struct {
	store  storage.Store
	cfg    Config
	client *http.Client
	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

func New(s storage.Store, cfg Config) *Worker {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.RetryBackoffMin <= 0 {
		cfg.RetryBackoffMin = time.Second
	}
	if cfg.RetryBackoffMax < cfg.RetryBackoffMin {
		cfg.RetryBackoffMax = 5 * time.Minute
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if len(cfg.EncryptionKey) != 32 {
		panic("delivery: New requires a 32-byte EncryptionKey")
	}
	return &Worker{store: s, cfg: cfg, client: &http.Client{Timeout: cfg.RequestTimeout}, wake: make(chan struct{}, 1), done: make(chan struct{})}
}
func (w *Worker) Start(ctx context.Context) {
	c, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	slog.Info("delivery worker starting")
	go w.loop(c)
}
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
		slog.Debug("delivery worker notified")
	default:
	}
}
func (w *Worker) Close(ctx context.Context) error {
	if w.cancel != nil {
		slog.Info("delivery worker stopping")
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
		slog.Info("delivery worker stopped")
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
	d, ok, e := w.store.ClaimNextDelivery(ctx, time.Now())
	if e != nil || !ok {
		if e != nil && ctx.Err() == nil {
			slog.Warn("delivery claim failed", "error", e)
		}
		return false
	}
	slog.Debug("delivery claimed", "delivery_id", d.ID, "attempt", d.AttemptCount)
	sub, e := w.store.GetSubscription(ctx, d.SubscriptionID)
	if e != nil {
		slog.Warn("delivery subscription lookup failed; failing delivery", "delivery_id", d.ID, "error", e)
		_ = w.store.FailDelivery(context.Background(), d.ID, 0)
		return true
	}
	secret, e := DecryptSecret(w.cfg.EncryptionKey, sub.CallbackSecret)
	if e != nil {
		slog.Error("delivery callback secret decryption failed; failing delivery", "delivery_id", d.ID, "error", e)
		_ = w.store.FailDelivery(context.Background(), d.ID, 0)
		return true
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, sub.CallbackURL, bytes.NewReader(d.Payload))
	if e != nil {
		slog.Warn("delivery request construction failed; failing delivery", "delivery_id", d.ID, "error", e)
		_ = w.store.FailDelivery(context.Background(), d.ID, 0)
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, Sign(secret, d.Payload))
	resp, e := w.client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			slog.Debug("delivery interrupted by shutdown; retry scheduled", "delivery_id", d.ID)
			_ = w.store.RequeueDelivery(context.Background(), d.ID, time.Now())
			return true
		}
		w.retryOrFail(d.ID, d.AttemptCount, 0, e)
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Info("delivery succeeded", "delivery_id", d.ID, "status", resp.StatusCode)
		_ = w.store.MarkDelivered(context.Background(), d.ID, resp.StatusCode)
		return true
	}
	w.retryOrFail(d.ID, d.AttemptCount, resp.StatusCode, fmt.Errorf("unexpected status %d", resp.StatusCode))
	return true
}

// retryOrFail mirrors orchestrator.Worker.handleError's retry-budget/backoff
// shape, adapted to Config's operator-tunable min/max rather than a fixed
// formula, since delivery targets (arbitrary external endpoints) don't have
// the same well-known latency characteristics a GMLC's own Diameter peers do.
func (w *Worker) retryOrFail(id string, attempts, responseCode int, cause error) {
	if attempts >= w.cfg.MaxAttempts {
		slog.Warn("delivery retry budget exhausted", "delivery_id", id, "attempt", attempts, "error", cause)
		_ = w.store.FailDelivery(context.Background(), id, responseCode)
		return
	}
	delay := backoff(attempts, w.cfg.RetryBackoffMin, w.cfg.RetryBackoffMax)
	slog.Warn("delivery attempt failed; retry scheduled", "delivery_id", id, "attempt", attempts, "delay", delay, "error", cause)
	_ = w.store.RequeueDelivery(context.Background(), id, time.Now().Add(delay))
}

// backoff is exponential from RetryBackoffMin, capped at RetryBackoffMax.
// The overflow guard matters because attempts is unbounded in principle
// (MaxAttempts is the caller's responsibility to enforce, not backoff's) —
// 1<<(attempts-1) would otherwise wrap negative for a large enough attempts
// and produce a nonsensical (even negative) delay.
func backoff(attempts int, min, max time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts - 1
	if shift > 32 {
		return max
	}
	d := min * time.Duration(1<<shift)
	if d <= 0 || d > max {
		return max
	}
	return d
}
