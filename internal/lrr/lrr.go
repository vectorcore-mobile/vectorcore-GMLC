// Package lrr handles inbound TS 29.172 Location-Report-Request (LRR)
// dispatch: decode, correlate against any pending deferred-location
// subscription, persist, and answer with an LRA — the event-driven mirror
// of internal/orchestrator's role for the outbound PLR/PLA flow. It is
// registered as a Diameter request handler (internal/diameter.RequestHandler)
// on the same GMLC-initiated connection an MME/SGSN's LRR arrives over —
// there is no separate inbound listener, matching this GMLC's DRA-only
// deployment model.
package lrr

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/fiorix/go-diameter/v4/diam"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/slg"
	"github.com/vectorcore/gmlc/internal/storage"
)

type Handler struct {
	codec *slg.Provider
	store storage.Store
}

// New builds an LRR handler. codec only needs OriginHost/OriginRealm — it is
// used purely for DecodeLRR/BuildLRA (wire marshaling), never for outbound
// RoundTrip, so it does not need a transport or registry.
func New(codec *slg.Provider, s storage.Store) *Handler {
	return &Handler{codec: codec, store: s}
}

// ServeDIAM implements diam.Handler, so *Handler can be registered directly
// as an internal/diameter.RequestHandler.Handler.
func (h *Handler) ServeDIAM(c diam.Conn, m *diam.Message) {
	ctx := context.Background()
	report, decErr := h.codec.DecodeLRR(m)
	resultCode := uint32(diam.Success)
	if decErr != nil {
		slog.Warn("LRR decode failed", "error", decErr)
		resultCode = diam.UnableToComply
	} else {
		h.process(ctx, report)
	}
	a, buildErr := h.codec.BuildLRA(m, resultCode)
	if buildErr != nil {
		// The request was malformed enough that even a well-formed answer
		// can't be built (e.g. missing Session-Id) — nothing useful to send
		// back; the MME will eventually time out the transaction.
		slog.Error("LRA build failed", "error", buildErr)
		return
	}
	if _, err := a.WriteTo(c); err != nil {
		slog.Warn("LRA write failed", "error", err)
	}
}

// process correlates and persists a successfully decoded report. Failures
// here are logged, not surfaced to the MME/SGSN — the LRA has already
// promised receipt (or will, via ServeDIAM's Success result code) once
// decoding succeeded; TS 29.172 gives GMLC no mechanism to retract that
// after the fact, so a storage failure here is a GMLC-local problem, not a
// protocol-level one.
func (h *Handler) process(ctx context.Context, r domain.LocationReport) {
	var correlated *byte
	if r.LCSReferenceNumber != nil {
		ref := *r.LCSReferenceNumber
		if _, err := h.store.FindPendingDeferredLocationSubscription(ctx, ref); err == nil {
			correlated = &ref
			if e := h.store.MarkDeferredLocationSubscriptionReported(ctx, ref); e != nil {
				slog.Warn("deferred location subscription transition failed", "lcs_reference_number", ref, "error", e)
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			slog.Warn("deferred location subscription lookup failed", "lcs_reference_number", ref, "error", err)
		}
	}
	rep := storage.LocationReport{
		LCSReferenceNumber:    correlated,
		LocationEvent:         r.LocationEvent,
		RawGAD:                r.RawLocationEstimate,
		ECGI:                  r.ECGI,
		AccuracyFulfilment:    r.AccuracyFulfilment,
		AgeOfLocationEstimate: r.AgeOfLocationEstimate,
		RawVelocityEstimate:   r.RawVelocityEstimate,
		EUTRANPositioningData: r.EUTRANPositioningData,
		ReceivedAt:            time.Now().UTC(),
	}
	if r.Target.IMSI != "" || r.Target.MSISDN != "" {
		rep.TargetKind, rep.TargetValue = r.Target.Kind(), r.Target.Value()
	}
	if _, err := h.store.CreateLocationReport(ctx, rep); err != nil {
		slog.Warn("location report persist failed", "location_event", r.LocationEvent, "error", err)
	}
}
