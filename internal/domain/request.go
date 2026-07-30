package domain

import (
	"fmt"
	"strings"
	"time"
)

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
		return fmt.Errorf("at least one target identity is required")
	}
	for kind, v := range map[string]string{"IMSI": t.IMSI, "MSISDN": t.MSISDN} {
		if v != "" {
			if len(v) > 15 {
				return fmt.Errorf("%s exceeds 15 digits", kind)
			}
			for _, r := range v {
				if r < '0' || r > '9' {
					return fmt.Errorf("%s must contain only decimal digits", kind)
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
	ECGI                []byte
	Source              string
}
