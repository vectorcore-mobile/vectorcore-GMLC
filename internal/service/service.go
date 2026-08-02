package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/storage"
)

var (
	ErrIdempotencyRequired = errors.New("idempotency key is required")
	ErrUnsupportedService  = errors.New("unsupported service type")
)

type Service struct {
	store    storage.Store
	auth     *auth.Authorizer
	onQueued func()
	onCancel func(string)
}

func (s *Service) SetQueuedHook(fn func())       { s.onQueued = fn }
func (s *Service) SetCancelHook(fn func(string)) { s.onCancel = fn }

func New(store storage.Store, authorizer *auth.Authorizer) *Service {
	return &Service{store: store, auth: authorizer}
}

type SubmitInput struct {
	ClientID, Token, IdempotencyKey string
	Target                          domain.Target
	Service                         domain.ServiceType
}

func (s *Service) Submit(ctx context.Context, in SubmitInput) (domain.Request, bool, error) {
	if err := in.Target.Validate(); err != nil {
		return domain.Request{}, false, err
	}
	if in.Service != "" && in.Service != domain.ServiceImmediate {
		return domain.Request{}, false, fmt.Errorf("%q: %w", in.Service, ErrUnsupportedService)
	}
	in.Service = domain.ServiceImmediate
	if strings.TrimSpace(in.IdempotencyKey) == "" || len(in.IdempotencyKey) > 255 {
		return domain.Request{}, false, ErrIdempotencyRequired
	}
	c, err := s.auth.Authenticate(ctx, in.ClientID, in.Token)
	if err != nil {
		return domain.Request{}, false, err
	}
	if err = s.auth.Authorize(c, in.Service, in.Target); err != nil {
		return domain.Request{}, false, err
	}
	r := domain.Request{ID: uuid.NewString(), ClientID: c.ID, IdempotencyKey: in.IdempotencyKey, Service: in.Service, TargetKind: in.Target.Kind(), TargetValue: in.Target.Value(), State: domain.StateQueued}
	r, created, err := s.store.CreateRequest(ctx, r)
	if err != nil {
		return r, false, err
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
