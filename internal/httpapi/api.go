package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/service"
	"github.com/vectorcore/gmlc/internal/storage"
)

type API struct {
	svc   *service.Service
	ready func() bool
}

func New(s *service.Service, ready func() bool) http.Handler {
	a := &API{svc: s, ready: ready}
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", a.health)
	m.HandleFunc("GET /readyz", a.readyz)
	m.HandleFunc("POST /v1/location-requests", a.submit)
	m.HandleFunc("GET /v1/location-requests/{id}", a.get)
	m.HandleFunc("DELETE /v1/location-requests/{id}", a.cancel)
	return m
}
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	if !a.ready() {
		problem(w, http.StatusServiceUnavailable, "not_ready", "service is not ready")
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "ready"})
}

type targetJSON struct {
	IMSI   string `json:"imsi"`
	MSISDN string `json:"msisdn"`
}

func (t targetJSON) empty() bool { return t.IMSI == "" && t.MSISDN == "" }
func (t targetJSON) domain() domain.Target {
	return domain.Target{IMSI: t.IMSI, MSISDN: t.MSISDN}
}

type submitJSON struct {
	Target targetJSON `json:"target"`
	// Targets is the batch-submit path: mutually exclusive with Target (a
	// request must set exactly one). Each target becomes its own
	// independently-pollable request — see service.SubmitBatch.
	Targets        []targetJSON       `json:"targets,omitempty"`
	ServiceType    domain.ServiceType `json:"service_type"`
	IdempotencyKey string             `json:"idempotency_key"`
	// LocationType selects TS 29.172 SLg-Location-Type. Omitted defaults to
	// "current", matching the request pipeline's own zero-value default.
	LocationType *string `json:"location_type,omitempty"`
	// Priority is TS 29.172 LCS-Priority (7.4.5); omitted sends no AVP.
	Priority *uint32 `json:"priority,omitempty"`
	// QoS is TS 29.172 LCS-QoS (7.4.6); omitted sends no AVP. Every child
	// field is independently optional, matching the wire format's own ABNF.
	QoS *qosJSON `json:"qos,omitempty"`
	// CallbackURL/CallbackSecret register async-completion delivery
	// (API-ASYNC): once this request reaches a terminal state, its status
	// (the same shape GET returns) is POSTed to CallbackURL, HMAC-signed
	// with CallbackSecret over the raw body (X-GMLC-Signature: sha256=
	// <hex>). Omitting both means no callback, matching today's poll-only
	// behavior; setting only one is rejected. For a batch submission
	// ("targets"), both apply to every request created by the call, sharing
	// one callback destination.
	CallbackURL    *string `json:"callback_url,omitempty"`
	CallbackSecret *string `json:"callback_secret,omitempty"`
}

type qosJSON struct {
	Class                    *string  `json:"class,omitempty"`
	HorizontalAccuracyMeters *float64 `json:"horizontal_accuracy_meters,omitempty"`
	VerticalAccuracyMeters   *float64 `json:"vertical_accuracy_meters,omitempty"`
	VerticalRequested        bool     `json:"vertical_requested,omitempty"`
	ResponseTime             *string  `json:"response_time,omitempty"`
}

var qosClassValues = map[string]uint32{
	"assured":     domain.QoSClassAssured,
	"best_effort": domain.QoSClassBestEffort,
}
var responseTimeValues = map[string]uint32{
	"low_delay":      domain.ResponseTimeLowDelay,
	"delay_tolerant": domain.ResponseTimeDelayTolerant,
}
var qosClassNames = map[uint32]string{
	domain.QoSClassAssured:    "assured",
	domain.QoSClassBestEffort: "best_effort",
}
var responseTimeNames = map[uint32]string{
	domain.ResponseTimeLowDelay:      "low_delay",
	domain.ResponseTimeDelayTolerant: "delay_tolerant",
}

// fromDomain is the inverse of toDomain, for echoing a stored request's QoS
// back in a status response. Returns nil if q is nil, matching how an
// omitted "qos" object round-trips to no AVP and back to no object.
func qosFromDomain(q *domain.QoSRequest) map[string]any {
	if q == nil {
		return nil
	}
	out := map[string]any{}
	if q.Class != nil {
		out["class"] = qosClassNames[*q.Class]
	}
	if q.HorizontalAccuracyMeters != nil {
		out["horizontal_accuracy_meters"] = *q.HorizontalAccuracyMeters
	}
	if q.VerticalAccuracyMeters != nil {
		out["vertical_accuracy_meters"] = *q.VerticalAccuracyMeters
	}
	if q.VerticalRequested {
		out["vertical_requested"] = true
	}
	if q.ResponseTimeClass != nil {
		out["response_time"] = responseTimeNames[*q.ResponseTimeClass]
	}
	return out
}

// toDomain translates the REST-facing qosJSON into domain.QoSRequest,
// rejecting unknown enum strings. A nil q (no "qos" object in the request)
// returns a nil *domain.QoSRequest, meaning no LCS-QoS AVP is sent.
func (q *qosJSON) toDomain() (*domain.QoSRequest, error) {
	if q == nil {
		return nil, nil
	}
	out := &domain.QoSRequest{HorizontalAccuracyMeters: q.HorizontalAccuracyMeters, VerticalAccuracyMeters: q.VerticalAccuracyMeters, VerticalRequested: q.VerticalRequested}
	if q.Class != nil {
		v, ok := qosClassValues[*q.Class]
		if !ok {
			return nil, fmt.Errorf("invalid qos.class %q", *q.Class)
		}
		out.Class = &v
	}
	if q.ResponseTime != nil {
		v, ok := responseTimeValues[*q.ResponseTime]
		if !ok {
			return nil, fmt.Errorf("invalid qos.response_time %q", *q.ResponseTime)
		}
		out.ResponseTimeClass = &v
	}
	return out, nil
}

// locationTypeValues maps the REST-facing string enum to TS 29.172's wire
// values, kept decoupled from internal/slg so httpapi never needs to import
// the Diameter-facing packages (see docs/architecture.md's adapter layering).
var locationTypeValues = map[string]uint32{
	"current":               domain.LocationCurrent,
	"current_or_last_known": domain.LocationCurrentOrLastKnown,
}
var locationTypeNames = map[uint32]string{
	domain.LocationCurrent:            "current",
	domain.LocationCurrentOrLastKnown: "current_or_last_known",
}

func (a *API) credentials(r *http.Request) (string, string, bool) {
	id := r.Header.Get("X-LCS-Client-ID")
	v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return id, v, id != "" && v != ""
}
func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	id, t, ok := a.credentials(r)
	if !ok {
		slog.Warn("Le request rejected: authentication required")
		problem(w, 401, "unauthenticated", "authentication required")
		return
	}
	var in submitJSON
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		slog.Warn("Le request rejected: invalid JSON")
		problem(w, 400, "invalid_request", "invalid JSON request")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = in.IdempotencyKey
	}
	locationType := domain.LocationCurrent
	if in.LocationType != nil {
		v, ok := locationTypeValues[*in.LocationType]
		if !ok {
			slog.Warn("Le request rejected: invalid location_type")
			problem(w, 400, "invalid_request", "invalid location_type")
			return
		}
		locationType = v
	}
	qos, err := in.QoS.toDomain()
	if err != nil {
		slog.Warn("Le request rejected: invalid qos")
		problem(w, 400, "invalid_request", err.Error())
		return
	}
	hasSingle, hasMulti := !in.Target.empty(), len(in.Targets) > 0
	if hasSingle && hasMulti {
		slog.Warn("Le request rejected: both target and targets set")
		problem(w, 400, "invalid_request", "specify exactly one of target or targets")
		return
	}
	if hasMulti {
		a.submitBatch(w, r, id, t, in, key, locationType, qos)
		return
	}
	req, created, err := a.svc.Submit(r.Context(), service.SubmitInput{ClientID: id, Token: t, IdempotencyKey: key, Target: in.Target.domain(), Service: in.ServiceType, LocationType: locationType, Priority: in.Priority, QoS: qos, CallbackURL: strPtrValue(in.CallbackURL), CallbackSecret: strPtrValue(in.CallbackSecret)})
	if err != nil {
		slog.Warn("Le request rejected", "code", safeErrorCode(err))
		mapErr(w, err)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
		slog.Debug("Le location request reused", "request_id", req.ID, "state", req.State)
	} else {
		slog.Info("Le location request accepted", "request_id", req.ID, "state", req.State)
	}
	writeStatus(w, status, req)
}

// submitBatch handles the "targets" path of submit: N targets become N
// independent requests, returned as a JSON array under "requests". There is
// no batch-grouping id — each element is polled/cancelled via the existing
// single-request endpoints exactly like any other request id.
func (a *API) submitBatch(w http.ResponseWriter, r *http.Request, id, t string, in submitJSON, key string, locationType uint32, qos *domain.QoSRequest) {
	targets := make([]domain.Target, len(in.Targets))
	for i, tj := range in.Targets {
		targets[i] = tj.domain()
	}
	reqs, createdFlags, err := a.svc.SubmitBatch(r.Context(), service.BatchSubmitInput{ClientID: id, Token: t, IdempotencyKey: key, Targets: targets, Service: in.ServiceType, LocationType: locationType, Priority: in.Priority, QoS: qos, CallbackURL: strPtrValue(in.CallbackURL), CallbackSecret: strPtrValue(in.CallbackSecret)})
	if err != nil {
		slog.Warn("Le batch request rejected", "code", safeErrorCode(err))
		mapErr(w, err)
		return
	}
	out := make([]map[string]any, len(reqs))
	anyCreated := false
	for i, req := range reqs {
		out[i] = RequestJSON(req, domain.Result{})
		if createdFlags[i] {
			anyCreated = true
		}
	}
	status := http.StatusOK
	if anyCreated {
		status = http.StatusAccepted
	}
	slog.Info("Le batch location request accepted", "count", len(reqs))
	write(w, status, map[string]any{"requests": out})
}
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id, t, ok := a.credentials(r)
	if !ok {
		problem(w, 401, "unauthenticated", "authentication required")
		return
	}
	req, result, err := a.svc.GetResult(r.Context(), id, t, r.PathValue("id"))
	if err != nil {
		slog.Warn("Le request lookup rejected", "code", safeErrorCode(err))
		mapErr(w, err)
		return
	}
	slog.Debug("Le request status returned", "request_id", req.ID, "state", req.State)
	writeStatusResult(w, 200, req, result)
}
func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	id, t, ok := a.credentials(r)
	if !ok {
		problem(w, 401, "unauthenticated", "authentication required")
		return
	}
	req, err := a.svc.Cancel(r.Context(), id, t, r.PathValue("id"))
	if err != nil {
		slog.Warn("Le request cancellation rejected", "code", safeErrorCode(err))
		mapErr(w, err)
		return
	}
	slog.Info("Le location request cancelled", "request_id", req.ID, "state", req.State)
	writeStatus(w, 200, req)
}
func writeStatus(w http.ResponseWriter, status int, r domain.Request) {
	writeStatusResult(w, status, r, domain.Result{})
}
func writeStatusResult(w http.ResponseWriter, status int, r domain.Request, v domain.Result) {
	write(w, status, RequestJSON(r, v))
}

// strPtrValue dereferences an optional string field, treating an absent
// (nil) JSON field the same as an explicitly empty one — both mean "no
// callback" to service.Submit/SubmitBatch's own validateCallback.
func strPtrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RequestJSON builds the JSON representation of a single request/result
// pair — shared between the single-submit response (writeStatusResult),
// each element of a batch-submit response array, and the async-completion
// callback payload (see cmd/gmlc/main.go's orchestrator completion hook),
// all of which must render the exact same shape.
func RequestJSON(r domain.Request, v domain.Result) map[string]any {
	out := map[string]any{"id": r.ID, "service_type": r.Service, "state": r.State, "failure_code": r.FailureCode, "location_type": locationTypeNames[r.LocationType], "created_at": r.CreatedAt, "updated_at": r.UpdatedAt}
	if r.Priority != nil {
		out["priority"] = *r.Priority
	}
	if qos := qosFromDomain(r.QoS); qos != nil {
		out["qos"] = qos
	}
	// A completed request always has a result row, but not every completion
	// has a position — additional_information (ECGI-only) and Polygon
	// (no single center point) completions don't. Gating on Latitude/
	// Longitude here would silently drop those results from the response
	// even though the request genuinely completed.
	if r.State == domain.StateCompleted {
		result := map[string]any{"created_at": v.CreatedAt}
		if v.Shape != "" {
			result["shape"] = v.Shape
		}
		if v.Latitude != nil && v.Longitude != nil {
			result["latitude"] = *v.Latitude
			result["longitude"] = *v.Longitude
		}
		if len(v.ECGI) > 0 {
			result["ecgi"] = v.ECGI
		}
		if v.UncertaintyMeters != nil {
			result["uncertainty_meters"] = *v.UncertaintyMeters
		}
		if v.SemiMajorMeters != nil {
			result["semi_major_meters"] = *v.SemiMajorMeters
		}
		if v.SemiMinorMeters != nil {
			result["semi_minor_meters"] = *v.SemiMinorMeters
		}
		if v.OrientationDegrees != nil {
			result["orientation_degrees"] = *v.OrientationDegrees
		}
		if v.ConfidencePercent != nil {
			result["confidence_percent"] = *v.ConfidencePercent
		}
		if v.AgeOfLocationEstimate != nil {
			result["age_of_location_estimate_minutes"] = *v.AgeOfLocationEstimate
		}
		if v.AccuracyFulfilment != nil {
			result["accuracy_fulfilment"] = *v.AccuracyFulfilment
		}
		out["result"] = result
	}
	return out
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, code, detail string) {
	write(w, status, map[string]any{"error": map[string]string{"code": code, "detail": detail}})
}

// mapErr translates service/storage errors to HTTP responses. Only errors
// known to originate from client-supplied input are safe to echo back to the
// caller; anything else (unexpected storage/backend failures) must map to a
// generic 500 so internal error text is never disclosed to an API client.
func mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		problem(w, 401, "unauthenticated", "authentication failed")
	case errors.Is(err, auth.ErrForbidden):
		problem(w, 403, "forbidden", "client is not authorized for this request")
	case errors.Is(err, storage.ErrNotFound):
		problem(w, 404, "not_found", "request not found")
	case errors.Is(err, storage.ErrConflict):
		problem(w, 409, "invalid_state", "operation is not valid for request state")
	case errors.Is(err, service.ErrIdempotencyRequired):
		problem(w, 400, "idempotency_required", "Idempotency-Key is required")
	case errors.Is(err, domain.ErrInvalidTarget):
		problem(w, 400, "invalid_target", err.Error())
	case errors.Is(err, service.ErrUnsupportedService):
		problem(w, 400, "unsupported_service_type", err.Error())
	case errors.Is(err, service.ErrInvalidLocationType):
		problem(w, 400, "invalid_request", "invalid location_type")
	case errors.Is(err, service.ErrEmptyBatch):
		problem(w, 400, "invalid_request", err.Error())
	case errors.Is(err, service.ErrCallbackRequiresSecret):
		problem(w, 400, "invalid_request", err.Error())
	case errors.Is(err, service.ErrInvalidCallbackURL):
		problem(w, 400, "invalid_request", err.Error())
	case errors.Is(err, service.ErrDeliveryNotConfigured):
		problem(w, 400, "delivery_not_configured", err.Error())
	default:
		problem(w, 500, "internal_error", "the request could not be completed")
	}
}

// safeErrorCode is suitable for operational logs. It intentionally excludes
// raw protocol and subscriber-bearing error text.
func safeErrorCode(err error) string {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return "unauthenticated"
	case errors.Is(err, auth.ErrForbidden):
		return "forbidden"
	case errors.Is(err, storage.ErrNotFound):
		return "not_found"
	case errors.Is(err, storage.ErrConflict):
		return "invalid_state"
	case errors.Is(err, service.ErrIdempotencyRequired):
		return "idempotency_required"
	case errors.Is(err, domain.ErrInvalidTarget):
		return "invalid_target"
	case errors.Is(err, service.ErrUnsupportedService):
		return "unsupported_service_type"
	case errors.Is(err, service.ErrInvalidLocationType):
		return "invalid_request"
	case errors.Is(err, service.ErrEmptyBatch):
		return "invalid_request"
	case errors.Is(err, service.ErrCallbackRequiresSecret), errors.Is(err, service.ErrInvalidCallbackURL):
		return "invalid_request"
	case errors.Is(err, service.ErrDeliveryNotConfigured):
		return "delivery_not_configured"
	default:
		return "internal_error"
	}
}
