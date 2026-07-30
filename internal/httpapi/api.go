package httpapi

import (
	"encoding/json"
	"errors"
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

type submitJSON struct {
	Target struct {
		IMSI   string `json:"imsi"`
		MSISDN string `json:"msisdn"`
	} `json:"target"`
	ServiceType    domain.ServiceType `json:"service_type"`
	IdempotencyKey string             `json:"idempotency_key"`
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
	req, created, err := a.svc.Submit(r.Context(), service.SubmitInput{ClientID: id, Token: t, IdempotencyKey: key, Target: domain.Target{IMSI: in.Target.IMSI, MSISDN: in.Target.MSISDN}, Service: in.ServiceType})
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
	out := map[string]any{"id": r.ID, "service_type": r.Service, "state": r.State, "failure_code": r.FailureCode, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt}
	if r.State == domain.StateCompleted && v.Latitude != nil && v.Longitude != nil {
		out["result"] = map[string]any{"shape": v.Shape, "latitude": *v.Latitude, "longitude": *v.Longitude, "created_at": v.CreatedAt, "ecgi": v.ECGI}
	}
	write(w, status, out)
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, code, detail string) {
	write(w, status, map[string]any{"error": map[string]string{"code": code, "detail": detail}})
}
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
	default:
		problem(w, 400, "invalid_request", err.Error())
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
	default:
		return "invalid_request"
	}
}
