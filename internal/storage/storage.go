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
	// LCSClientType is the TS 29.172 LCS-Client-Type this client's requests
	// are tagged with (domain.ClientType* constants). Operator-configured
	// only; see config.Client.LCSClientType.
	LCSClientType uint32
}
type AuditEvent struct {
	RequestID, ClientID, Type, Detail string
	At                                time.Time
}

type Store interface {
	Migrate(context.Context) error
	UpsertClient(context.Context, Client) error
	// GetClientCredential returns identity/credential/type only (one
	// fixed-cost query, independent of how many services/prefixes the
	// client has), so callers on the pre-authentication path — where an
	// attacker without a valid token can be probing — don't leak client
	// existence through query cost. Authorization data is fetched
	// separately via GetClientAuthzData, only once a token has already
	// been verified.
	GetClientCredential(context.Context, string) (Client, error)
	GetClientAuthzData(context.Context, string) (services []domain.ServiceType, targetPrefixes []string, err error)
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
