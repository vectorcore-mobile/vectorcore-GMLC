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
	// LCSPrivacyCheck is the TS 29.172 LCS-Privacy-Check this client's
	// requests are tagged with (domain.Privacy* constants). Operator-
	// configured only; see config.Client.LCSPrivacyCheck.
	LCSPrivacyCheck uint32
}
type AuditEvent struct {
	RequestID, ClientID, Type, Detail string
	At                                time.Time
}

// Subscription registers where and how to deliver a payload for a client —
// shared by both deferred-report delivery (LRR) and REST-async completion
// delivery, which otherwise reduce to the same "something happened later,
// tell an external party" problem. CallbackSecret is opaque ciphertext at
// this layer; internal/delivery owns encrypting/decrypting it, storage just
// persists whatever bytes it's given.
type Subscription struct {
	ID, ClientID, CallbackURL string
	CallbackSecret            []byte
	CreatedAt                 time.Time
}

type DeliveryState string

const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryInFlight  DeliveryState = "in_flight"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryFailed    DeliveryState = "failed"
)

// Delivery is one outbox entry: an opaque payload queued for HTTP delivery
// to its subscription's callback URL. Payload is deliberately opaque bytes,
// not a domain type — storage and the delivery worker don't need to know
// whether it's a location-request completion or an LRR report, only that it
// needs to be POSTed and signed.
type Delivery struct {
	ID, SubscriptionID   string
	Payload              []byte
	State                DeliveryState
	AttemptCount         int
	NextAttemptAt        *time.Time
	LastResponseCode     *int
	CreatedAt, UpdatedAt time.Time
}

type DeferredLocationSubscriptionState string

const (
	DeferredSubscriptionPending  DeferredLocationSubscriptionState = "pending"
	DeferredSubscriptionReported DeferredLocationSubscriptionState = "reported"
	DeferredSubscriptionExpired  DeferredLocationSubscriptionState = "expired"
)

// DeferredLocationSubscription is a correlation target for a future inbound
// LRR, keyed by the GMLC-assigned TS 29.172 LCS-Reference-Number (7.4.37,
// OctetString of length 1) sent in the originating PLR. Nothing in this
// codebase creates one yet — LocationType only supports immediate Current/
// Current-or-Last-Known, and BuildPLR never requests
// Delayed-Location-Reporting-Support (PLR-Flags) — so this exists so LRR
// correlation is spec-complete (TS 29.172 7.3.3's LCS-Reference-Number
// field) rather than silently unsupported, ready for whenever deferred/
// periodic/delayed-reporting requests are added.
type DeferredLocationSubscription struct {
	LCSReferenceNumber      byte
	ClientID                string
	TargetKind, TargetValue string
	State                   DeferredLocationSubscriptionState
	CreatedAt, UpdatedAt    time.Time
}

// LocationReport is a decoded, persisted inbound TS 29.172 Location-Report-
// Request (LRR). LCSReferenceNumber is nil for every report this GMLC can
// currently trigger (see DeferredLocationSubscription's doc comment) — that
// is the normal, expected case, not an error condition; it is only non-nil
// once a matching DeferredLocationSubscription existed at correlation time.
type LocationReport struct {
	ID                                         string
	LCSReferenceNumber                         *byte
	LocationEvent                              uint32
	TargetKind, TargetValue                    string
	RawGAD, ECGI                               []byte
	AccuracyFulfilment, AgeOfLocationEstimate  *uint32
	RawVelocityEstimate, EUTRANPositioningData []byte
	ReceivedAt                                 time.Time
}

// HistoryPoint is one recorded location_history fix for a target — the
// subset of domain.Result an MLP hlia's <pos> needs (see internal/mlp's
// buildPos, which HistoryPoint mirrors): point + circular uncertainty
// only, no ellipse fields, matching what the existing MLP immediate
// services already render.
type HistoryPoint struct {
	Shape                                  string
	Latitude, Longitude, UncertaintyMeters *float64
	RecordedAt                             time.Time
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
	// SetRequestSubscription attaches a delivery subscription to an existing
	// request. Called once, only for a newly created request that requested
	// a callback — never on an idempotent replay, so a replay can't attach a
	// second subscription to an already-registered request.
	SetRequestSubscription(ctx context.Context, requestID, subscriptionID string) error
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
	// Purge deletes terminal-state location_requests/location_results/
	// audit_events past their retention cutoffs. It never touches
	// location_history, which has its own, unbounded-by-default retention —
	// see RecordHistory.
	Purge(context.Context, time.Time, time.Time) error

	// RecordHistory appends one location_history row for target from a
	// just-completed result. Called on every completed request regardless
	// of which adapter (REST or MLP) submitted it, so MLP's Historic
	// Location Immediate service (hlir/hlia) can serve fixes obtained
	// either way. Independent of and outliving location_requests/
	// location_results, which Purge deletes on its own schedule — instead,
	// each target's own history is self-pruning: only its most recent
	// points are kept (see the sqlite implementation's
	// maxHistoryPointsPerTarget).
	RecordHistory(ctx context.Context, targetKind, targetValue string, v domain.Result) error
	// QueryHistory returns location_history points for target recorded
	// within [start, stop] (inclusive), oldest first, thinned so
	// consecutive returned points are at least minInterval apart (zero
	// disables thinning), then capped to at most limit points (zero means
	// unlimited).
	QueryHistory(ctx context.Context, targetKind, targetValue string, start, stop time.Time, minInterval time.Duration, limit int) ([]HistoryPoint, error)

	// CreateSubscription generates and returns the subscription id; nothing
	// external needs to predict it ahead of creation.
	CreateSubscription(ctx context.Context, clientID, callbackURL string, callbackSecret []byte) (Subscription, error)
	GetSubscription(context.Context, string) (Subscription, error)
	GetDelivery(context.Context, string) (Delivery, error)
	// CreateDelivery enqueues one pending outbox entry for the given
	// subscription. The caller (LRR ingestion, REST completion hook)
	// already knows the subscription id; storage never looks one up on its
	// own, so this can't accidentally deliver to the wrong client's
	// callback.
	CreateDelivery(context.Context, string, []byte) (Delivery, error)
	// ClaimNextDelivery mirrors ClaimNextQueued's claim-with-lease shape:
	// atomically picks the oldest due pending delivery and marks it
	// in-flight, incrementing AttemptCount, so concurrent workers (or a
	// restart mid-delivery) can't double-send the same payload.
	ClaimNextDelivery(context.Context, time.Time) (Delivery, bool, error)
	// RequeueDelivery mirrors Requeue: transitions an in-flight delivery
	// back to DeliveryPending with a future next_attempt_at after a
	// transient failure, so it's claimable again but not immediately.
	RequeueDelivery(context.Context, string, time.Time) error
	// FailDelivery mirrors FailRequest: marks a delivery permanently failed
	// once its retry budget is exhausted.
	FailDelivery(context.Context, string, int) error
	// MarkDelivered mirrors CompleteRequest: records a successful delivery.
	MarkDelivered(context.Context, string, int) error

	// CreateDeferredLocationSubscription registers a pending LRR correlation
	// target under a GMLC-assigned LCS-Reference-Number.
	CreateDeferredLocationSubscription(ctx context.Context, clientID, targetKind, targetValue string, ref byte) (DeferredLocationSubscription, error)
	// FindPendingDeferredLocationSubscription looks up a still-pending
	// subscription by LCS-Reference-Number. ErrNotFound means the inbound
	// report is unsolicited — the normal case today, not an error condition
	// for the caller to surface.
	FindPendingDeferredLocationSubscription(ctx context.Context, ref byte) (DeferredLocationSubscription, error)
	// MarkDeferredLocationSubscriptionReported transitions a pending
	// subscription once its correlated LRR has arrived.
	MarkDeferredLocationSubscriptionReported(ctx context.Context, ref byte) error
	// CreateLocationReport persists a decoded inbound LRR, whether or not it
	// correlated to a pending subscription.
	CreateLocationReport(context.Context, LocationReport) (LocationReport, error)

	Close(context.Context) error
}
