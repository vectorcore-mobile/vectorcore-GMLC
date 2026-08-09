package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
)

var (
	ErrIdempotencyRequired    = errors.New("idempotency key is required")
	ErrUnsupportedService     = errors.New("unsupported service type")
	ErrInvalidLocationType    = errors.New("unsupported location type")
	ErrCallbackRequiresSecret = errors.New("callback_url requires callback_secret")
	ErrDeliveryNotConfigured  = errors.New("callback delivery is not configured on this GMLC")
	ErrInvalidCallbackURL     = errors.New("invalid callback_url")
)

type Service struct {
	store         storage.Store
	auth          *auth.Authorizer
	onQueued      func()
	onCancel      func(string)
	encryptSecret func([]byte) ([]byte, error)
}

func (s *Service) SetQueuedHook(fn func())       { s.onQueued = fn }
func (s *Service) SetCancelHook(fn func(string)) { s.onCancel = fn }

// SetSecretEncryptor wires the callback-secret-at-rest encryption function
// (internal/delivery.EncryptSecret, closed over the operator's configured
// key) — mirrors SetQueuedHook/SetCancelHook's own optional-wiring pattern.
// A Service with no encryptor configured rejects any callback_url/
// callback_secret with ErrDeliveryNotConfigured, since it has no way to
// protect the secret at rest.
func (s *Service) SetSecretEncryptor(fn func([]byte) ([]byte, error)) { s.encryptSecret = fn }

func New(store storage.Store, authorizer *auth.Authorizer) *Service {
	return &Service{store: store, auth: authorizer}
}

type SubmitInput struct {
	ClientID, Token, IdempotencyKey string
	Target                          domain.Target
	Service                         domain.ServiceType
	// LocationType is TS 29.172 SLg-Location-Type; zero value is
	// domain.LocationCurrent, so callers that don't set it get today's
	// existing behavior unchanged.
	LocationType uint32
	Priority     *uint32
	QoS          *domain.QoSRequest
	// CallbackURL/CallbackSecret register async-completion delivery (API-
	// ASYNC): once this request reaches a terminal state, its status is
	// POSTed to CallbackURL, HMAC-signed with CallbackSecret. Both empty
	// means no callback (the caller must poll GET instead, as today). Both
	// must be set together — see Service.validateCallback.
	CallbackURL, CallbackSecret string
}

// prepare runs the validation and authentication common to Submit and
// SubmitBatch: service-type/location-type checks, idempotency-key presence,
// and authentication. It does not touch any target — callers validate and
// authorize their own target(s) afterward, since Submit has exactly one and
// SubmitBatch has many.
func (s *Service) prepare(ctx context.Context, clientID, token, idempotencyKey string, svc domain.ServiceType, locationType uint32) (storage.Client, domain.ServiceType, error) {
	if svc != "" && svc != domain.ServiceImmediate {
		return storage.Client{}, svc, fmt.Errorf("%q: %w", svc, ErrUnsupportedService)
	}
	svc = domain.ServiceImmediate
	if locationType != domain.LocationCurrent && locationType != domain.LocationCurrentOrLastKnown {
		return storage.Client{}, svc, fmt.Errorf("%d: %w", locationType, ErrInvalidLocationType)
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 255 {
		return storage.Client{}, svc, ErrIdempotencyRequired
	}
	c, err := s.auth.Authenticate(ctx, clientID, token)
	return c, svc, err
}

// validateCallback enforces the callback_url/callback_secret contract
// up front — before any request row is created — so a rejected callback
// never leaves a half-registered request behind. Both empty is valid (no
// callback); anything else requires both fields and a configured encryptor.
func (s *Service) validateCallback(callbackURL, callbackSecret string) error {
	if callbackURL == "" && callbackSecret == "" {
		return nil
	}
	if callbackURL == "" || callbackSecret == "" {
		return ErrCallbackRequiresSecret
	}
	u, err := url.Parse(callbackURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidCallbackURL
	}
	if s.encryptSecret == nil {
		return ErrDeliveryNotConfigured
	}
	return nil
}

// registerCallback encrypts callbackSecret and creates its
// storage.Subscription, returning the subscription id to attach to a
// request. Only called after validateCallback has already passed and the
// owning request is confirmed newly created (never on an idempotent
// replay) — see Submit/SubmitBatch.
func (s *Service) registerCallback(ctx context.Context, clientID, callbackURL, callbackSecret string) (string, error) {
	ciphertext, err := s.encryptSecret([]byte(callbackSecret))
	if err != nil {
		return "", err
	}
	sub, err := s.store.CreateSubscription(ctx, clientID, callbackURL, ciphertext)
	if err != nil {
		return "", err
	}
	return sub.ID, nil
}

func (s *Service) Submit(ctx context.Context, in SubmitInput) (domain.Request, bool, error) {
	if err := in.Target.Validate(); err != nil {
		return domain.Request{}, false, err
	}
	if err := s.validateCallback(in.CallbackURL, in.CallbackSecret); err != nil {
		return domain.Request{}, false, err
	}
	c, svc, err := s.prepare(ctx, in.ClientID, in.Token, in.IdempotencyKey, in.Service, in.LocationType)
	if err != nil {
		return domain.Request{}, false, err
	}
	in.Service = svc
	if err = s.auth.Authorize(c, in.Service, in.Target); err != nil {
		return domain.Request{}, false, err
	}
	r := domain.Request{ID: uuid.NewString(), ClientID: c.ID, IdempotencyKey: in.IdempotencyKey, Service: in.Service, TargetKind: in.Target.Kind(), TargetValue: in.Target.Value(), State: domain.StateQueued, LocationType: in.LocationType, Priority: in.Priority, QoS: in.QoS}
	r, created, err := s.store.CreateRequest(ctx, r)
	if err != nil {
		return r, false, err
	}
	// Callback registration only happens for a genuinely new request — never
	// on an idempotent replay, which would otherwise create an orphaned
	// subscription nothing ever delivers to (the existing request's
	// callback, if any, was already registered on its original submit).
	if created && in.CallbackURL != "" {
		subID, err := s.registerCallback(ctx, c.ID, in.CallbackURL, in.CallbackSecret)
		if err != nil {
			return r, created, err
		}
		if err = s.store.SetRequestSubscription(ctx, r.ID, subID); err != nil {
			return r, created, err
		}
		r.SubscriptionID = &subID
	}
	event := "request_idempotent"
	if created {
		event = "request_accepted"
	}
	_ = s.store.RecordAudit(ctx, storage.AuditEvent{RequestID: r.ID, ClientID: c.ID, Type: event, Detail: "immediate location request"})
	if created && s.onQueued != nil {
		s.onQueued()
	}
	return r, created, nil
}

// BatchSubmitInput mirrors SubmitInput but carries multiple targets, each
// becoming its own independent domain.Request — there is no batch-grouping
// record; the orchestrator already processes location_requests rows one at
// a time regardless of how they were submitted, so a batch needs no new
// storage concept, only N ordinary rows.
type BatchSubmitInput struct {
	ClientID, Token, IdempotencyKey string
	Targets                         []domain.Target
	Service                         domain.ServiceType
	LocationType                    uint32
	Priority                        *uint32
	QoS                             *domain.QoSRequest
	// CallbackURL/CallbackSecret register one shared subscription for the
	// whole batch — see SubmitInput's own doc comment. Each created request
	// still gets its own storage.Delivery at completion time; they just
	// share one callback destination rather than N identical ones.
	CallbackURL, CallbackSecret string
}

var ErrEmptyBatch = errors.New("at least one target is required")

// SubmitBatch validates every target before creating any request, so bad
// input (a malformed target anywhere in the batch) rejects the whole call
// with nothing created — matching Submit's own fail-fast behavior. Once
// validation passes, each target is created via its own CreateRequest call
// rather than one wrapping DB transaction: each is independently idempotent
// under its derived key ("<key>#<index>"), so a rare mid-batch storage
// failure just needs the identical batch call retried with the same
// Idempotency-Key — already-created targets resolve as idempotent replays,
// not duplicates, and the remaining ones get created. A wrapping transaction
// would only protect against that same rare failure mode, at the cost of
// holding one write transaction open across N inserts.
func (s *Service) SubmitBatch(ctx context.Context, in BatchSubmitInput) ([]domain.Request, []bool, error) {
	if len(in.Targets) == 0 {
		return nil, nil, ErrEmptyBatch
	}
	for _, t := range in.Targets {
		if err := t.Validate(); err != nil {
			return nil, nil, err
		}
	}
	if err := s.validateCallback(in.CallbackURL, in.CallbackSecret); err != nil {
		return nil, nil, err
	}
	c, svc, err := s.prepare(ctx, in.ClientID, in.Token, in.IdempotencyKey, in.Service, in.LocationType)
	if err != nil {
		return nil, nil, err
	}
	in.Service = svc
	for _, t := range in.Targets {
		if err = s.auth.Authorize(c, in.Service, t); err != nil {
			return nil, nil, err
		}
	}
	reqs := make([]domain.Request, 0, len(in.Targets))
	created := make([]bool, 0, len(in.Targets))
	var subID string
	for i, t := range in.Targets {
		key := fmt.Sprintf("%s#%d", in.IdempotencyKey, i)
		r := domain.Request{ID: uuid.NewString(), ClientID: c.ID, IdempotencyKey: key, Service: in.Service, TargetKind: t.Kind(), TargetValue: t.Value(), State: domain.StateQueued, LocationType: in.LocationType, Priority: in.Priority, QoS: in.QoS}
		r, wasCreated, err := s.store.CreateRequest(ctx, r)
		if err != nil {
			return reqs, created, err
		}
		if wasCreated && in.CallbackURL != "" {
			if subID == "" {
				subID, err = s.registerCallback(ctx, c.ID, in.CallbackURL, in.CallbackSecret)
				if err != nil {
					return reqs, created, err
				}
			}
			if err = s.store.SetRequestSubscription(ctx, r.ID, subID); err != nil {
				return reqs, created, err
			}
			r.SubscriptionID = &subID
		}
		event := "request_idempotent"
		if wasCreated {
			event = "request_accepted"
		}
		_ = s.store.RecordAudit(ctx, storage.AuditEvent{RequestID: r.ID, ClientID: c.ID, Type: event, Detail: "immediate location request (batch)"})
		if wasCreated && s.onQueued != nil {
			s.onQueued()
		}
		reqs = append(reqs, r)
		created = append(created, wasCreated)
	}
	return reqs, created, nil
}
func (s *Service) Get(ctx context.Context, clientID, token, id string) (domain.Request, error) {
	c, err := s.auth.Authenticate(ctx, clientID, token)
	if err != nil {
		return domain.Request{}, err
	}
	r, err := s.store.GetRequest(ctx, id)
	if err != nil {
		return r, err
	}
	if r.ClientID != c.ID {
		return domain.Request{}, storage.ErrNotFound
	}
	return r, nil
}
func (s *Service) GetResult(ctx context.Context, clientID, token, id string) (domain.Request, domain.Result, error) {
	r, e := s.Get(ctx, clientID, token, id)
	if e != nil {
		return r, domain.Result{}, e
	}
	if r.State != domain.StateCompleted {
		return r, domain.Result{}, nil
	}
	v, e := s.store.GetResult(ctx, id)
	return r, v, e
}
func (s *Service) Cancel(ctx context.Context, clientID, token, id string) (domain.Request, error) {
	r, err := s.Get(ctx, clientID, token, id)
	if err != nil {
		return r, err
	}
	r, err = s.store.TransitionRequest(ctx, r.ID, domain.StateCancelled, "cancelled_by_client")
	if err == nil {
		_ = s.store.RecordAudit(ctx, storage.AuditEvent{RequestID: r.ID, ClientID: clientID, Type: "request_cancelled", Detail: "cancelled by client"})
	}
	if err == nil && s.onCancel != nil {
		s.onCancel(r.ID)
	}
	return r, err
}
