package mlp

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/vectorcore/gmlc/internal/domain"
)

// Pusher sends unsolicited MLP reports — slrep (Standard Location Report,
// for MO-LR events) and emerep (Emergency Location Report, for emergency
// call origination/release/handover events) — to fixed, operator-
// configured LCS Client destinations. This is the "push" side of the Le
// interface, structurally different from slir/eme_lir/hlir: there is no
// originating MLP request and no per-subscriber routing, and (per
// §5.6.2.3 Figures 10/11) GMLC itself plays the "requesting" role on the
// wire, presenting its own hdr/client identity rather than reading one.
// Fire-and-forget: neither TS 23.271 nor MLP give the pushing side a
// retry contract for a failed or negative response (no correlation id, no
// subscription, no HMAC signature — see docs/mlp-le-interface-plan.md's
// explicit "not internal/delivery" decision), so a failure is only
// logged, never retried. A nil *Pusher is valid and a no-op, matching
// internal/service's SetQueuedHook/SetCancelHook nil-means-disabled
// convention — callers don't need to nil-check before calling.
type Pusher struct {
	client                    *http.Client
	standardURL, standardID   string
	emergencyURL, emergencyID string
}

// NewPusher builds a Pusher. Either URL may be empty to disable pushing
// that report type; a Pusher with both empty is valid but a permanent
// no-op.
func NewPusher(standardURL, standardClientID, emergencyURL, emergencyClientID string, timeout time.Duration) *Pusher {
	return &Pusher{client: &http.Client{Timeout: timeout}, standardURL: standardURL, standardID: standardClientID, emergencyURL: emergencyURL, emergencyID: emergencyClientID}
}

// targetToMsid renders a domain.Target back to wire msid form — the
// inverse of msidToTarget. Report pushing starts from an already-resolved
// domain.Target (an inbound LRR's own Target field), not a wire msid the
// way slir/eme_lir/hlir do, so this direction is needed here and nowhere
// else.
func targetToMsid(t domain.Target) msid {
	if t.IMSI != "" {
		return msid{Type: "IMSI", Value: t.IMSI}
	}
	return msid{Type: "MSISDN", Value: t.MSISDN}
}

// reportPos renders one inbound LRR's target and (optional) position as a
// pos/eme_pos entry. The spec permits an emergency report with no
// location estimate at all ("MAY include a location estimate... outside
// the scope of this document" — §5.2.3.5), so a nil position yields a pos
// with neither Pd nor PosErr set, which is valid per the DTD (pos/eme_pos
// requires only msid).
func reportPos(target domain.Target, position *domain.GeographicPosition, at time.Time) pos {
	p := pos{Msid: targetToMsid(target)}
	if position == nil {
		return p
	}
	var radius float64
	if position.UncertaintyMeters != nil {
		radius = *position.UncertaintyMeters
	}
	p.Pd = &pd{
		Time: timeElem{UtcOff: "+0000", Value: formatMLPTime(at)},
		Shape: shape{CircularArea: &circularArea{
			SrsName: epsg4326SrsName,
			Coord:   coord{X: encodeDMSH(position.Latitude, "N", "S"), Y: encodeDMSH(position.Longitude, "E", "W")},
			Radius:  fmt.Sprintf("%.0f", radius),
		}},
	}
	return p
}

// emeTriggerFor maps a TS 29.172 Location-Event to MLP's eme_trigger
// enum. Only the three emergency events are valid inputs — see
// PushEmergencyReport.
func emeTriggerFor(locationEvent uint32) (string, bool) {
	switch locationEvent {
	case domain.LocationEventEmergencyCallOrigination:
		return "EME_ORG", true
	case domain.LocationEventEmergencyCallRelease:
		return "EME_REL", true
	case domain.LocationEventEmergencyCallHandover:
		return "EME_HO", true
	default:
		return "", false
	}
}

// PushEmergencyReport sends an emerep for an emergency call event.
// locationEvent must be one of domain.LocationEventEmergencyCall
// {Origination,Release,Handover} — anything else is a caller bug, logged
// and dropped rather than sent malformed. A target with no identity
// (neither IMSI nor MSISDN — the LRR's own Target is best-effort, see
// domain.LocationReport's doc comment) can't be rendered as a valid msid,
// so it's skipped the same way.
func (p *Pusher) PushEmergencyReport(ctx context.Context, target domain.Target, locationEvent uint32, position *domain.GeographicPosition, at time.Time) {
	if p == nil || p.emergencyURL == "" {
		return
	}
	trigger, ok := emeTriggerFor(locationEvent)
	if !ok {
		slog.Error("emergency report push: unmapped location_event", "location_event", locationEvent)
		return
	}
	if target.IMSI == "" && target.MSISDN == "" {
		slog.Warn("emergency report push skipped: LRR carried no target identity", "eme_trigger", trigger)
		return
	}
	body := svcResult{Ver: mlpVersion, Hdr: &hdr{Ver: mlpVersion, Client: &client{ID: p.emergencyID}}, Emerep: &emerep{Ver: mlpVersion, EmeEvent: emeEvent{EmeTrigger: trigger, EmePos: []pos{reportPos(target, position, at)}}}}
	if _, err := p.post(ctx, p.emergencyURL, body); err != nil {
		slog.Warn("emergency report push failed", "eme_trigger", trigger, "url", p.emergencyURL, "error", err)
	}
}

// PushStandardReport sends an slrep for an MO-LR event, then reads back
// the LCS Client's slra and logs a non-OK result — see Pusher's own doc
// comment for why this is best-effort, not retried.
func (p *Pusher) PushStandardReport(ctx context.Context, target domain.Target, position *domain.GeographicPosition, at time.Time) {
	if p == nil || p.standardURL == "" {
		return
	}
	if target.IMSI == "" && target.MSISDN == "" {
		slog.Warn("standard report push skipped: LRR carried no target identity")
		return
	}
	body := svcResult{Ver: mlpVersion, Hdr: &hdr{Ver: mlpVersion, Client: &client{ID: p.standardID}}, Slrep: &slrep{Ver: mlpVersion, Pos: []pos{reportPos(target, position, at)}}}
	respBody, err := p.post(ctx, p.standardURL, body)
	if err != nil {
		slog.Warn("standard report push failed", "url", p.standardURL, "error", err)
		return
	}
	var ack svcResult
	if err := xml.Unmarshal(respBody, &ack); err != nil {
		slog.Warn("standard report push: LCS Client answer did not decode as svc_result/slra", "url", p.standardURL, "error", err)
		return
	}
	if ack.Slra == nil {
		slog.Warn("standard report push: LCS Client answer had no slra", "url", p.standardURL)
		return
	}
	if ack.Slra.Result.ResID != "0" {
		slog.Warn("standard report rejected by LCS Client", "resid", ack.Slra.Result.ResID, "slogan", ack.Slra.Result.Value)
	}
}

// post marshals v and POSTs it to url as application/xml, returning the
// response body for callers that need to inspect it (PushStandardReport's
// slra read-back).
func (p *Pusher) post(ctx context.Context, url string, v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return respBody, fmt.Errorf("mlp: push destination returned HTTP %d", resp.StatusCode)
	}
	return respBody, nil
}
