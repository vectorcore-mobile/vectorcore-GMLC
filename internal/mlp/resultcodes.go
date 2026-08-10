package mlp

import (
	"errors"

	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/service"
	"github.com/vectorcore/gmlc/internal/storage"
)

// resultCode pairs an MLP §5.4 resid with its defined slogan text — every
// value below is taken verbatim from the spec's own result-code table, not
// invented.
type resultCode struct {
	ID     string
	Slogan string
}

var (
	resultSystemFailure                             = resultCode{"1", "SYSTEM FAILURE"}
	resultUnauthorizedApplication                   = resultCode{"3", "UNAUTHORIZED APPLICATION"}
	resultUnknownSubscriber                         = resultCode{"4", "UNKNOWN SUBSCRIBER"}
	resultPositionMethodFailure                     = resultCode{"6", "POSITION METHOD FAILURE"}
	resultTimeout                                   = resultCode{"7", "TIMEOUT"}
	resultFormatError                               = resultCode{"105", "FORMAT ERROR"}
	resultServiceNotSupported                       = resultCode{"108", "SERVICE NOT SUPPORTED"}
	resultInvalidProtocolElementValue               = resultCode{"110", "INVALID PROTOCOL ELEMENT VALUE"}
	resultProtocolElementAttributeValueNotSupported = resultCode{"113", "PROTOCOL ELEMENT ATTRIBUTE VALUE NOT SUPPORTED"}
)

// QOP NOT ATTAINABLE (201) and POSITIONING NOT ALLOWED (202) are defined by
// the spec but not produced here: domain.Request.FailureCode is a small set
// of generic strings ("no_immediate_result", "temporarily_unavailable",
// "network_failure") set by internal/orchestrator — it doesn't currently
// carry enough signal to distinguish a QoS-unattainable or privacy-denied
// PLA outcome from any other position-method failure. Giving FailureCode
// that granularity is a orchestrator/domain change, not an internal/mlp
// one — out of scope for this phase; POSITION METHOD FAILURE (6) is the
// honest fallback until it exists.

// errorToResultCode maps a service/storage/auth error to an MLP §5.4
// resid, mirroring internal/httpapi/api.go's mapErr for the equivalent
// REST-side cases rather than re-deriving the mapping from scratch. Only
// errors known to originate from client-supplied input get a specific
// code; anything else (unexpected backend failure) maps to SYSTEM FAILURE
// so internal error detail is never disclosed on the wire, matching
// mapErr's own default-500 reasoning.
func errorToResultCode(err error) resultCode {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return resultUnauthorizedApplication
	case errors.Is(err, auth.ErrForbidden):
		return resultUnauthorizedApplication
	case errors.Is(err, storage.ErrNotFound):
		return resultUnknownSubscriber
	case errors.Is(err, domain.ErrInvalidTarget):
		return resultInvalidProtocolElementValue
	case errors.Is(err, service.ErrUnsupportedService):
		return resultServiceNotSupported
	case errors.Is(err, service.ErrInvalidLocationType):
		return resultProtocolElementAttributeValueNotSupported
	case errors.Is(err, service.ErrIdempotencyRequired):
		return resultSystemFailure
	default:
		return resultSystemFailure
	}
}

// outcomeResultCode maps a terminal (or timed-out) domain.Request to the
// per-target poserr resid. GMLC's domain.Request doesn't currently expose
// enough structured failure detail (beyond the free-text FailureCode) to
// distinguish every MLP failure resid, so States that don't have a closer
// match fall back to POSITION METHOD FAILURE — the general "the location
// service failed to obtain the user's position" code.
func outcomeResultCode(req domain.Request, timedOut bool) resultCode {
	if timedOut {
		return resultTimeout
	}
	switch req.State {
	case domain.StateFailed, domain.StateCancelled, domain.StateExpired, domain.StateIndeterminate:
		return resultPositionMethodFailure
	default:
		return resultPositionMethodFailure
	}
}
