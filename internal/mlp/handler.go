package mlp

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/service"
	"github.com/vectorcore/gmlc/internal/storage"
)

// maxBodyBytes bounds the request body — MLP bodies are small XML
// documents, not bulk payloads.
const maxBodyBytes = 1 << 20

// Handler implements the MLP Le adapter: decode slir → validate → submit
// via internal/service.Service → block (SYNC-only, this phase) until every
// target reaches a terminal state or a bounded deadline passes → encode
// slia. See docs/mlp-le-interface-plan.md's "Decisions made with the user".
type Handler struct {
	svc *service.Service
	// syncWaitDefault/syncWaitMax bound how long a request blocks waiting
	// for every target to reach a terminal state. A client's own
	// eqop/resp_timer can shorten this (never lengthen it past
	// syncWaitMax) — see waitDeadline.
	syncWaitDefault, syncWaitMax time.Duration
	pollInterval                 time.Duration
	now                          func() time.Time
}

// New returns a Handler. defaultWait/maxWait come from Config.MLP; both
// must be positive (enforced by config.Validate).
func New(svc *service.Service, defaultWait, maxWait time.Duration) *Handler {
	return &Handler{svc: svc, syncWaitDefault: defaultWait, syncWaitMax: maxWait, pollInterval: 250 * time.Millisecond, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeGEM(w, resultServiceNotSupported, "only HTTP POST is supported")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		h.writeGEM(w, resultServiceNotSupported, "unable to read request body")
		return
	}
	var env svcInit
	if err := xml.Unmarshal(body, &env); err != nil {
		h.writeGEM(w, resultServiceNotSupported, "request is not a recognized or supported MLP service")
		return
	}
	switch {
	case env.Slir != nil:
		h.handleSLIR(w, r.Context(), env.Hdr, *env.Slir)
	case env.EmeLir != nil:
		h.handleEMELIR(w, r.Context(), env.Hdr, *env.EmeLir)
	case env.Hlir != nil:
		h.handleHLIR(w, r.Context(), env.Hdr, *env.Hlir)
	default:
		// Per §5.6.1: a request the location server can't identify as a
		// defined MLP service gets a General Error Message, not an slia —
		// this covers both malformed XML and any recognized-but-not
		// (yet)-implemented service (tlrr, hlir, ...).
		h.writeGEM(w, resultServiceNotSupported, "request is not a recognized or supported MLP service")
	}
}

func (h *Handler) handleSLIR(w http.ResponseWriter, ctx context.Context, header hdr, req slir) {
	clientID, token := credentialsFromHdr(header)
	priority := parsePrio(req.Prio)
	positions, rc, detail := h.resolveImmediate(ctx, clientID, token, req.Msids, req.LocType, req.GeoInfo, priority, req.EQoP)
	if rc != nil {
		h.writeTopLevelResult(w, *rc, detail)
		return
	}
	h.writeXML(w, http.StatusOK, svcResult{Ver: mlpVersion, Slia: &slia{Ver: mlpVersion, Pos: positions}})
}

// handleEMELIR is the Emergency Location Immediate Service counterpart of
// handleSLIR — the exact same submit/await/build pipeline (resolveImmediate),
// just with no prio field to parse and eme_lia-shaped output instead of
// slia-shaped. Emergency handling itself (LCS-Client-Type=EMERGENCY_SERVICES)
// is entirely a property of which GMLC client authenticated
// (internal/orchestrator resolves it purely from the client's own
// configuration, never from request content — see orchestrator.go's own
// doc comment) — eme_lir carries no signal that overrides or supplements
// that, matching the "client-config-only" decision recorded in
// docs/mlp-le-interface-plan.md's Phase B.
func (h *Handler) handleEMELIR(w http.ResponseWriter, ctx context.Context, header hdr, req emeLir) {
	clientID, token := credentialsFromHdr(header)
	positions, rc, detail := h.resolveImmediate(ctx, clientID, token, req.Msids, req.LocType, req.GeoInfo, nil, req.EQoP)
	if rc != nil {
		h.writeTopLevelResultEme(w, *rc, detail)
		return
	}
	h.writeXML(w, http.StatusOK, svcResult{Ver: mlpVersion, EmeLia: &emeLia{Ver: mlpVersion, EmePos: positions}})
}

// handleHLIR is the Historic Location Immediate Service handler
// (§5.2.3.8). Unlike handleSLIR/handleEMELIR, this never submits a new
// positioning attempt — there is no service.Submit call — it's a pure read
// against location_history, already populated by internal/orchestrator on
// every completed request. See service.Service.QueryHistory.
func (h *Handler) handleHLIR(w http.ResponseWriter, ctx context.Context, header hdr, req hlir) {
	clientID, token := credentialsFromHdr(header)
	if rc := validateGeoInfo(req.GeoInfo); rc != nil {
		h.writeTopLevelResultHlia(w, *rc, "unsupported geo_info coordinate reference system")
		return
	}
	target, rc := msidToTarget(req.Msid)
	if rc != nil {
		h.writeTopLevelResultHlia(w, *rc, fmt.Sprintf("unsupported msid type %q", req.Msid.Type))
		return
	}
	start, stop, minInterval, limit, rc := parseHistoryWindow(req, h.now())
	if rc != nil {
		h.writeTopLevelResultHlia(w, *rc, "invalid history window")
		return
	}
	points, err := h.svc.QueryHistory(ctx, clientID, token, target, start, stop, minInterval, limit)
	if err != nil {
		h.writeTopLevelResultHlia(w, errorToResultCode(err), "")
		return
	}
	if len(points) == 0 {
		h.writeTopLevelResultHlia(w, resultPositionMethodFailure, "no location history recorded for target in the requested time window")
		return
	}
	positions := make([]pos, len(points))
	for i, p := range points {
		positions[i] = buildHistoryPos(req.Msid, p)
	}
	h.writeXML(w, http.StatusOK, svcResult{Ver: mlpVersion, Hlia: &hlia{Ver: mlpVersion, Pos: positions}})
}

// parseHistoryWindow validates and resolves hlir's start_time/stop_time/
// interval/no_of_reports into the (start, stop, minInterval, limit)
// QueryHistory needs. Rules follow §5.2.3.8.1: start_time is required;
// stop_time defaults to, and is clamped to, now if later than now
// (an open-ended "up to the present" query); a stop_time before start_time
// is rejected.
func parseHistoryWindow(req hlir, now time.Time) (start, stop time.Time, minInterval time.Duration, limit int, rc *resultCode) {
	if strings.TrimSpace(req.StartTime) == "" {
		c := resultFormatError
		return time.Time{}, time.Time{}, 0, 0, &c
	}
	start, err := parseMLPTime(req.StartTime)
	if err != nil {
		c := resultInvalidProtocolElementValue
		return time.Time{}, time.Time{}, 0, 0, &c
	}
	stop = now
	if req.StopTime != nil {
		stop, err = parseMLPTime(*req.StopTime)
		if err != nil {
			c := resultInvalidProtocolElementValue
			return time.Time{}, time.Time{}, 0, 0, &c
		}
		if stop.After(now) {
			stop = now
		}
	}
	if stop.Before(start) {
		c := resultInvalidProtocolElementValue
		return time.Time{}, time.Time{}, 0, 0, &c
	}
	if req.Interval != nil {
		secs, err := strconv.Atoi(strings.TrimSpace(*req.Interval))
		if err != nil || secs <= 0 {
			c := resultInvalidProtocolElementValue
			return time.Time{}, time.Time{}, 0, 0, &c
		}
		minInterval = time.Duration(secs) * time.Second
	}
	if req.NoOfReports != nil {
		n, err := strconv.Atoi(strings.TrimSpace(*req.NoOfReports))
		if err != nil || n <= 0 {
			c := resultInvalidProtocolElementValue
			return time.Time{}, time.Time{}, 0, 0, &c
		}
		limit = n
	}
	return start, stop, minInterval, limit, nil
}

// buildHistoryPos renders one recorded fix as a pos, reusing the same
// CircularArea shape buildPos already produces for a live positioning
// result — a HistoryPoint carries the same point+uncertainty subset of
// domain.Result on which buildPos's success branch already operates.
func buildHistoryPos(m msid, p storage.HistoryPoint) pos {
	var radius float64
	if p.UncertaintyMeters != nil {
		radius = *p.UncertaintyMeters
	}
	pp := pos{Msid: m}
	if p.Latitude == nil || p.Longitude == nil {
		pp.PosErr = &posErr{Result: result{ResID: resultPositionMethodFailure.ID, Value: resultPositionMethodFailure.Slogan}, Time: timeElem{Value: formatMLPTime(p.RecordedAt)}}
		return pp
	}
	pp.Pd = &pd{
		Time: timeElem{UtcOff: "+0000", Value: formatMLPTime(p.RecordedAt)},
		Shape: shape{CircularArea: &circularArea{
			SrsName: epsg4326SrsName,
			Coord:   coord{X: encodeDMSH(*p.Latitude, "N", "S"), Y: encodeDMSH(*p.Longitude, "E", "W")},
			Radius:  fmt.Sprintf("%.0f", radius),
		}},
	}
	return pp
}

// resolveImmediate is the shared Standard/Emergency Location Immediate
// pipeline: validate, submit (single or batch), await every target's
// terminal state, and build one pos per target. A non-nil resultCode
// return means a whole-request-level failure — nothing target-specific
// was even attempted; otherwise the returned []pos has exactly one entry
// per target, each with either Pd or PosErr set (see buildPos).
func (h *Handler) resolveImmediate(ctx context.Context, clientID, token string, msidsEl *msids, lt *locType, gi *geoInfo, priority *uint32, e *eqop) ([]pos, *resultCode, string) {
	targets, rc, detail := extractTargetsFrom(msidsEl)
	if rc != nil {
		return nil, rc, detail
	}
	locationType, rc := parseLocType(lt)
	if rc != nil {
		return nil, rc, "unsupported loc_type"
	}
	if rc := validateGeoInfo(gi); rc != nil {
		return nil, rc, "unsupported geo_info coordinate reference system"
	}
	qos, respTimer, rc := parseEQoP(e)
	if rc != nil {
		return nil, rc, "invalid eqop"
	}

	idKey := uuid.NewString()
	var reqs []domain.Request
	if len(targets) == 1 {
		created, _, err := h.svc.Submit(ctx, service.SubmitInput{ClientID: clientID, Token: token, IdempotencyKey: idKey, Target: targets[0], Service: domain.ServiceImmediate, LocationType: locationType, Priority: priority, QoS: qos})
		if err != nil {
			rc := errorToResultCode(err)
			return nil, &rc, ""
		}
		reqs = []domain.Request{created}
	} else {
		created, _, err := h.svc.SubmitBatch(ctx, service.BatchSubmitInput{ClientID: clientID, Token: token, IdempotencyKey: idKey, Targets: targets, Service: domain.ServiceImmediate, LocationType: locationType, Priority: priority, QoS: qos})
		if err != nil {
			rc := errorToResultCode(err)
			return nil, &rc, ""
		}
		reqs = created
	}

	deadline := h.waitDeadline(respTimer)
	msidList := msidsEl.Msid
	positions := make([]pos, len(reqs))
	for i, created := range reqs {
		finalReq, result, timedOut := h.awaitTerminal(ctx, clientID, token, created.ID, deadline)
		positions[i] = buildPos(msidList[i], finalReq, result, timedOut, h.now())
	}
	return positions, nil, ""
}

// waitDeadline resolves how long to block: the client's own eqop/resp_timer
// if given, else the configured default — either way capped at the
// configured maximum, never lengthened past it.
func (h *Handler) waitDeadline(respTimer time.Duration) time.Time {
	wait := h.syncWaitDefault
	if respTimer > 0 {
		wait = respTimer
	}
	if wait > h.syncWaitMax {
		wait = h.syncWaitMax
	}
	return h.now().Add(wait)
}

// awaitTerminal polls the same read path (service.Service.GetResult) used
// to look up a request's status, until the request reaches a terminal
// state or deadline passes. This is the "bounded poll loop"
// decision recorded in docs/mlp-le-interface-plan.md — reusing an existing
// read rather than adding new orchestrator completion-notification
// plumbing.
func (h *Handler) awaitTerminal(ctx context.Context, clientID, token, id string, deadline time.Time) (domain.Request, domain.Result, bool) {
	for {
		req, result, err := h.svc.GetResult(ctx, clientID, token, id)
		if err == nil && req.State.Terminal() {
			return req, result, false
		}
		if !h.now().Before(deadline) {
			return req, domain.Result{}, true
		}
		select {
		case <-ctx.Done():
			return req, domain.Result{}, true
		case <-time.After(h.pollInterval):
		}
	}
}

// credentialsFromHdr extracts client id/pwd from the MLP header. A missing
// hdr/client simply yields empty strings, which service.Submit's own
// Authenticate call rejects with ErrUnauthenticated — no separate
// presence check needed here.
func credentialsFromHdr(h hdr) (clientID, token string) {
	if h.Client == nil {
		return "", ""
	}
	return strings.TrimSpace(h.Client.ID), strings.TrimSpace(h.Client.Pwd)
}

// extractTargetsFrom validates an slir/eme_lir msids list and converts it
// to domain.Targets. Any single unsupported msid type fails the whole
// request (a whole-request-level result, not a per-target poserr) — this
// mirrors internal/service.SubmitBatch's own fail-fast, all-or-nothing
// validation.
func extractTargetsFrom(m *msids) ([]domain.Target, *resultCode, string) {
	if m == nil || len(m.Msid) == 0 {
		rc := resultFormatError
		return nil, &rc, "missing msids"
	}
	targets := make([]domain.Target, len(m.Msid))
	for i, mm := range m.Msid {
		t, rc := msidToTarget(mm)
		if rc != nil {
			return nil, rc, fmt.Sprintf("unsupported msid type %q", mm.Type)
		}
		targets[i] = t
	}
	return targets, nil, ""
}

// msidToTarget accepts only MSISDN and IMSI — the only identity kinds
// domain.Target has. Every other MLP-defined msid type (IMEI, MIN, MDN,
// EME_MSID, ASID, ...) is grammatically valid per the DTD but not
// something this GMLC can resolve, so it's rejected with 113 (PROTOCOL
// ELEMENT ATTRIBUTE VALUE NOT SUPPORTED), not 110 (which is for a value
// invalid per the protocol grammar itself).
func msidToTarget(m msid) (domain.Target, *resultCode) {
	t := m.Type
	if t == "" {
		t = "MSISDN" // DTD default (§5.3.72)
	}
	value := strings.TrimSpace(m.Value)
	switch t {
	case "MSISDN":
		return domain.Target{MSISDN: value}, nil
	case "IMSI":
		return domain.Target{IMSI: value}, nil
	default:
		rc := resultProtocolElementAttributeValueNotSupported
		return domain.Target{}, &rc
	}
}

// parseLocType maps loc_type's type attribute to GMLC's domain.LocationType
// values. Absent or "CURRENT" (the DTD default) maps to LocationCurrent;
// every other DTD-valid value (LAST, LAST_OR_CURRENT, INITIAL,
// CURRENT_AND_INTERMEDIATE, LOCATION_URI) has no GMLC equivalent.
func parseLocType(lt *locType) (uint32, *resultCode) {
	if lt == nil || lt.Type == "" || lt.Type == "CURRENT" {
		return domain.LocationCurrent, nil
	}
	if lt.Type == "CURRENT_OR_LAST" {
		return domain.LocationCurrentOrLastKnown, nil
	}
	rc := resultProtocolElementAttributeValueNotSupported
	return 0, &rc
}

// parsePrio maps prio's type attribute to TS 29.172 LCS-Priority (7.4.5):
// 0 = highest priority, all other values (including absent, meaning
// NORMAL) omit the AVP entirely — matching service.SubmitInput's own
// Priority *uint32 semantics exactly.
func parsePrio(p *prio) *uint32 {
	if p != nil && p.Type == "HIGH" {
		v := uint32(0)
		return &v
	}
	return nil
}

// validateGeoInfo checks the client-requested coordinate reference system,
// if given. GMLC only ever produces WGS84/EPSG:4326 positions (see
// internal/gad), so any other CRS is rejected up front rather than
// silently returning coordinates in the wrong system.
func validateGeoInfo(g *geoInfo) *resultCode {
	if g == nil {
		return nil
	}
	if strings.TrimSpace(g.CRS.Identifier.Code) != "4326" {
		rc := resultProtocolElementAttributeValueNotSupported
		return &rc
	}
	return nil
}

// parseEQoP maps eqop to a domain.QoSRequest (nil if eqop is absent or
// carries none of the fields this phase reads) plus the client's own
// resp_timer, if given, as a time.Duration for waitDeadline.
func parseEQoP(e *eqop) (*domain.QoSRequest, time.Duration, *resultCode) {
	if e == nil {
		return nil, 0, nil
	}
	var q domain.QoSRequest
	var any bool
	if e.HorAcc != nil {
		v, err := strconv.ParseFloat(strings.TrimSpace(e.HorAcc.Value), 64)
		if err != nil {
			rc := resultInvalidProtocolElementValue
			return nil, 0, &rc
		}
		q.HorizontalAccuracyMeters = &v
		any = true
	}
	if e.RespReq != nil {
		switch e.RespReq.Type {
		case "", "DELAY_TOL":
			v := domain.ResponseTimeDelayTolerant
			q.ResponseTimeClass = &v
		case "LOW_DELAY", "NO_DELAY":
			v := domain.ResponseTimeLowDelay
			q.ResponseTimeClass = &v
		default:
			rc := resultProtocolElementAttributeValueNotSupported
			return nil, 0, &rc
		}
		any = true
	}
	var respTimer time.Duration
	if e.RespTimer != nil {
		secs, err := strconv.Atoi(strings.TrimSpace(*e.RespTimer))
		if err != nil || secs <= 0 {
			rc := resultInvalidProtocolElementValue
			return nil, 0, &rc
		}
		respTimer = time.Duration(secs) * time.Second
	}
	if !any {
		return nil, respTimer, nil
	}
	return &q, respTimer, nil
}

// buildPos renders one target's outcome. A completed request with a point
// (Latitude/Longitude both set — the shape GMLC's A-GNSS/ECID/OTDOA
// estimators all ultimately produce) becomes a CircularArea pd; anything
// else (failed/cancelled/expired/indeterminate, a completed request
// without a single-point geometry, or a timeout) becomes a poserr.
func buildPos(m msid, req domain.Request, posResult domain.Result, timedOut bool, now time.Time) pos {
	p := pos{Msid: m}
	if !timedOut && req.State == domain.StateCompleted && posResult.Latitude != nil && posResult.Longitude != nil {
		var radius float64
		if posResult.UncertaintyMeters != nil {
			radius = *posResult.UncertaintyMeters
		}
		p.Pd = &pd{
			Time: timeElem{UtcOff: "+0000", Value: formatMLPTime(posResult.CreatedAt)},
			Shape: shape{CircularArea: &circularArea{
				SrsName: epsg4326SrsName,
				Coord:   coord{X: encodeDMSH(*posResult.Latitude, "N", "S"), Y: encodeDMSH(*posResult.Longitude, "E", "W")},
				Radius:  fmt.Sprintf("%.0f", radius),
			}},
		}
		return p
	}
	rc := outcomeResultCode(req, timedOut)
	p.PosErr = &posErr{Result: result{ResID: rc.ID, Value: rc.Slogan}, Time: timeElem{Value: formatMLPTime(now)}}
	return p
}

func (h *Handler) writeTopLevelResult(w http.ResponseWriter, rc resultCode, detail string) {
	s := &slia{Ver: mlpVersion, Result: &result{ResID: rc.ID, Value: rc.Slogan}, AddInfo: detail}
	h.writeXML(w, http.StatusOK, svcResult{Ver: mlpVersion, Slia: s})
}

func (h *Handler) writeTopLevelResultEme(w http.ResponseWriter, rc resultCode, detail string) {
	e := &emeLia{Ver: mlpVersion, Result: &result{ResID: rc.ID, Value: rc.Slogan}, AddInfo: detail}
	h.writeXML(w, http.StatusOK, svcResult{Ver: mlpVersion, EmeLia: e})
}

func (h *Handler) writeTopLevelResultHlia(w http.ResponseWriter, rc resultCode, detail string) {
	hl := &hlia{Ver: mlpVersion, Result: &result{ResID: rc.ID, Value: rc.Slogan}, AddInfo: detail}
	h.writeXML(w, http.StatusOK, svcResult{Ver: mlpVersion, Hlia: hl})
}

func (h *Handler) writeGEM(w http.ResponseWriter, rc resultCode, detail string) {
	h.writeXML(w, http.StatusNotFound, gem{Ver: mlpVersion, Result: result{ResID: rc.ID, Value: rc.Slogan}, AddInfo: detail})
}

func (h *Handler) writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}
