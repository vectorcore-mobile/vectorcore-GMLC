package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidTarget marks Target.Validate failures as client-caused input
// errors, distinct from internal/backend failures, so HTTP handlers can
// safely surface the error text without risking leaking internal details.
var ErrInvalidTarget = errors.New("invalid target identity")

type ServiceType string

const ServiceImmediate ServiceType = "immediate"

type State string

const (
	StateQueued        State = "queued"
	StateResolving     State = "resolving"
	StateLocating      State = "locating"
	StateCompleted     State = "completed"
	StateFailed        State = "failed"
	StateCancelled     State = "cancelled"
	StateExpired       State = "expired"
	StateIndeterminate State = "indeterminate"
)

func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled || s == StateExpired || s == StateIndeterminate
}

func CanTransition(from, to State) bool {
	if from == to {
		return false
	}
	if from.Terminal() {
		return false
	}
	switch from {
	case StateQueued:
		return to == StateResolving || to == StateCancelled || to == StateExpired || to == StateFailed || to == StateIndeterminate
	case StateResolving:
		return to == StateLocating || to == StateQueued || to == StateCancelled || to == StateExpired || to == StateFailed || to == StateIndeterminate
	case StateLocating:
		return to == StateCompleted || to == StateQueued || to == StateCancelled || to == StateExpired || to == StateFailed || to == StateIndeterminate
	}
	return false
}

type Target struct {
	IMSI   string
	MSISDN string
}

func (t Target) Validate() error {
	t.IMSI, t.MSISDN = strings.TrimSpace(t.IMSI), strings.TrimSpace(t.MSISDN)
	if t.IMSI == "" && t.MSISDN == "" {
		return fmt.Errorf("at least one target identity is required: %w", ErrInvalidTarget)
	}
	for kind, v := range map[string]string{"IMSI": t.IMSI, "MSISDN": t.MSISDN} {
		if v != "" {
			if len(v) > 15 {
				return fmt.Errorf("%s exceeds 15 digits: %w", kind, ErrInvalidTarget)
			}
			for _, r := range v {
				if r < '0' || r > '9' {
					return fmt.Errorf("%s must contain only decimal digits: %w", kind, ErrInvalidTarget)
				}
			}
		}
	}
	return nil
}

func (t Target) Value() string {
	if t.IMSI != "" {
		return t.IMSI
	}
	return t.MSISDN
}
func (t Target) Kind() string {
	if t.IMSI != "" {
		return "imsi"
	}
	return "msisdn"
}

type Request struct {
	ID, ClientID, TargetKind, TargetValue, IdempotencyKey string
	Service                                               ServiceType
	State                                                 State
	FailureCode                                           string
	CreatedAt, UpdatedAt                                  time.Time
	AttemptCount                                          int
	NextAttemptAt                                         *time.Time
}

type Result struct {
	RequestID           string
	RawGAD              []byte
	Shape               string
	CreatedAt           time.Time
	Latitude, Longitude *float64
	UncertaintyMeters   *float64
	// SemiMajorMeters/SemiMinorMeters/OrientationDegrees/ConfidencePercent
	// are set only for Shape "ellipsoid_point_uncertainty_ellipse".
	SemiMajorMeters, SemiMinorMeters, OrientationDegrees *float64
	ConfidencePercent                                    *uint32
	// AgeOfLocationEstimate is TS 29.172 7.4.16, in minutes.
	// AccuracyFulfilment is the TS 29.172 7.4.15 raw enum (0 fulfilled, 1 not).
	// Both are independent of Shape/Position — they can accompany an
	// ECGI-only (additional_information) PLA too.
	AgeOfLocationEstimate, AccuracyFulfilment *uint32
	// RawVelocityEstimate/EUTRANPositioningData are undecoded TS 29.172
	// 7.4.17/7.4.18 payloads, retained for diagnostics like RawGAD — not
	// currently decoded for structured API exposure.
	RawVelocityEstimate, EUTRANPositioningData []byte
	ECGI                                       []byte
	Source                                     string
}
