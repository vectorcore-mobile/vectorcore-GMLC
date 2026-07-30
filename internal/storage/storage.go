package storage

import (
	"context"
	"errors"
	"time"

	"github.com/vectorcore/gmlc/internal/domain"
)

var (
	ErrNotFound = errors.New("storage: not found")
	ErrConflict = errors.New("storage: conflict")
)

type Client struct {
	ID             string
	CredentialHash []byte
	Services       []domain.ServiceType
	TargetPrefixes []string
	Enabled        bool
}
type AuditEvent struct {
	RequestID, ClientID, Type, Detail string
	At                                time.Time
}

type Store interface {
	Migrate(context.Context) error
	UpsertClient(context.Context, Client) error
	GetClient(context.Context, string) (Client, error)
	CreateRequest(context.Context, domain.Request) (domain.Request, bool, error)
	GetRequest(context.Context, string) (domain.Request, error)
	TransitionRequest(context.Context, string, domain.State, string) (domain.Request, error)
	ClaimNextQueued(context.Context, time.Time) (domain.Request, bool, error)
	SaveServingNodeAndLocate(context.Context, string, domain.ServingNode) error
	Requeue(context.Context, string, domain.State, time.Time, string) error
	FailRequest(context.Context, string, domain.State, string, string) error
	CompleteRequest(context.Context, string, domain.Result) error
	GetResult(context.Context, string) (domain.Result, error)
	SaveServingNode(context.Context, string, domain.ServingNode) error
	RecordAudit(context.Context, AuditEvent) error
	Recover(context.Context, time.Time) error
	Purge(context.Context, time.Time, time.Time) error
	Close(context.Context) error
}
