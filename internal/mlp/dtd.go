// Package mlp implements the OMA Mobile Location Protocol (MLP) v3.5 Le
// interface adapter, sitting beside internal/httpapi's REST/JSON adapter on
// the same protocol-neutral internal/service.Service core. This file holds
// the wire (XML) types. It is deliberately not a transcription of the full
// spec DTD — only the Standard Location Immediate Service (slir/slia) and
// General Error Message (gem) elements this phase implements are modeled.
// See docs/mlp-le-interface-plan.md for the phased scope.
package mlp

import (
	"encoding/xml"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const mlpVersion = "3.5.0"

// epsg4326SrsName matches the literal srsName value used in the spec's own
// slia worked example (OMA-TS-MLP-V3_5-20181211-C, page 39) — the informal
// abbreviated form the spec itself uses for GMLC responses, not the fully
// qualified http://www.opengis.net/gml/srs/epsg.xml#4326 form the spec's
// own note (page 29) says GML V2.1.1 would otherwise require.
const epsg4326SrsName = "www.epsg.org#4326"

// svcInit is the Service Initiation DTD (§5.6.2.1): header and body
// encapsulated in one document so a request can carry both authentication
// and the service payload in a single HTTP POST. Only slir is modeled;
// any other recognized-but-unimplemented body element (eme_lir, tlrr, hlir,
// ...) or anything that fails to decode falls through to the General Error
// Message path exactly like a genuinely unrecognized request would.
type svcInit struct {
	XMLName xml.Name `xml:"svc_init"`
	Ver     string   `xml:"ver,attr"`
	Hdr     hdr      `xml:"hdr"`
	Slir    *slir    `xml:"slir"`
	EmeLir  *emeLir  `xml:"eme_lir"`
	Hlir    *hlir    `xml:"hlir"`
}

// svcResult is the Service Result DTD (§5.6.2.2) counterpart used for every
// response this phase sends.
type svcResult struct {
	XMLName xml.Name `xml:"svc_result"`
	Ver     string   `xml:"ver,attr"`
	// Hdr is nil (omitted) on every response this listener itself sends
	// back to a client-initiated slir/eme_lir/hlir (hdr is optional per
	// the DTD, and there's nothing to identify since the client already
	// authenticated on the way in) — but is set by mlp.Pusher's own
	// outbound slrep/emerep pushes, where GMLC plays the "requesting"
	// role instead (see §5.6.2.3 Figures 10/11) and needs to present its
	// own client identity.
	Hdr    *hdr    `xml:"hdr,omitempty"`
	Slia   *slia   `xml:"slia"`
	EmeLia *emeLia `xml:"eme_lia"`
	Hlia   *hlia   `xml:"hlia"`
	// Slrep/Emerep are set only when this svcResult is an outbound push
	// (see mlp.Pusher); Slra is set only when decoding the LCS Client's
	// answer to a pushed slrep.
	Slrep  *slrep  `xml:"slrep"`
	Slra   *slra   `xml:"slra"`
	Emerep *emerep `xml:"emerep"`
}

// hdr is the MLP_HDR context/header (§5.2.3.1.1), reduced to what this
// phase reads: the client credentials. sessionid/subclient/requestor/
// supported_shapes/serving_node_privacy_action are all valid per the DTD
// but unused here.
type hdr struct {
	Ver    string  `xml:"ver,attr"`
	Client *client `xml:"client"`
}
type client struct {
	ID  string `xml:"id"`
	Pwd string `xml:"pwd"`
}

// msid is the MLP_ID identity element (§5.3.72). type defaults to MSISDN
// per the DTD when the attribute is absent; this phase accepts only MSISDN
// and IMSI (the only identity kinds GMLC itself has — see msidToTarget).
type msid struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}
type msids struct {
	Msid []msid `xml:"msid"`
}

// slir is the Standard Location Immediate Request DTD (§5.2.3.2.1). Only
// the msids-list form of the identity choice is supported (not the bare
// repeated-msid-siblings form, and not msid_range) — see
// docs/mlp-le-interface-plan.md's Phase A scope.
type slir struct {
	Ver     string   `xml:"ver,attr"`
	ResType string   `xml:"res_type,attr"`
	Msids   *msids   `xml:"msids"`
	EQoP    *eqop    `xml:"eqop"`
	GeoInfo *geoInfo `xml:"geo_info"`
	LocType *locType `xml:"loc_type"`
	Prio    *prio    `xml:"prio"`
}

// slia is the Standard Location Immediate Answer DTD (§5.2.3.2.2):
// (pos+ | req_id | (result, add_info?)) — either per-target results, an
// async ack (not used in this SYNC-only phase), or a whole-request-level
// failure.
type slia struct {
	Ver     string  `xml:"ver,attr"`
	Pos     []pos   `xml:"pos,omitempty"`
	Result  *result `xml:"result,omitempty"`
	AddInfo string  `xml:"add_info,omitempty"`
}

// emeLir is the Emergency Location Immediate Request DTD (§5.2.3.3.1),
// reduced to the fields Phase B reads. Structurally identical to slir for
// every field this phase cares about, with two differences the DTD itself
// makes: no prio element (emergency requests have no priority choice to
// make), and msid type EME_MSID (ANSI/E911, IS-41D) is grammatically valid
// but not supported — msidToTarget rejects it the same way it rejects any
// non-MSISDN/IMSI type for slir. esrd/esrk (Emergency Services Routing
// Data/Key — North American only) are deliberately not modeled: nothing in
// GMLC generates or consumes them.
type emeLir struct {
	Ver     string   `xml:"ver,attr"`
	ResType string   `xml:"res_type,attr"`
	Msids   *msids   `xml:"msids"`
	EQoP    *eqop    `xml:"eqop"`
	GeoInfo *geoInfo `xml:"geo_info"`
	LocType *locType `xml:"loc_type"`
}

// emeLia is the Emergency Location Immediate Answer DTD (§5.2.3.3.2):
// (eme_pos+ | req_id | (result, add_info?)) — the same three-way choice
// slia has. eme_pos's own content model, reduced to what this phase
// produces (msid, (pd | poserr)), is identical to pos's, so the pos type
// is reused directly for the eme_pos wire element rather than duplicated
// — encoding/xml takes the wire element name from the containing field's
// own tag, not from the type, so this renders correctly as <eme_pos>.
// esrd/esrk/servingcell/target_serving_node/location_id (all optional per
// the DTD) are never populated, matching emeLir's own scope.
type emeLia struct {
	Ver     string  `xml:"ver,attr"`
	EmePos  []pos   `xml:"eme_pos,omitempty"`
	Result  *result `xml:"result,omitempty"`
	AddInfo string  `xml:"add_info,omitempty"`
}

// hlir is the Historic Location Immediate Request DTD (§5.2.3.8.1), reduced
// to what this phase supports: a single msid (the spec's own hlir, unlike
// slir/eme_lir, takes exactly one target, not a msids list), a
// [start_time, stop_time] window, and optional interval/no_of_reports to
// thin/cap the returned points. qop (an accuracy-based filter) and
// pushaddr (ASYNC delivery) are DTD-valid but not modeled: qop has no
// equivalent in location_history's stored data (no per-point accuracy
// classification is retained), and ASYNC is out of this SYNC-only phase's
// scope, same as slir/eme_lir. prio is likewise omitted — a historic query
// has no positioning method to prioritize.
type hlir struct {
	Ver         string   `xml:"ver,attr"`
	Msid        msid     `xml:"msid"`
	StartTime   string   `xml:"start_time"`
	StopTime    *string  `xml:"stop_time"`
	Interval    *string  `xml:"interval"`
	NoOfReports *string  `xml:"no_of_reports"`
	GeoInfo     *geoInfo `xml:"geo_info"`
}

// hlia is the Historic Location Immediate Answer DTD (§5.2.3.8.2):
// (pos+ | req_id | (result, add_info?)) — the same three-way choice slia
// has. Reuses pos directly: multiple pos entries for one hlir answer one
// target's history (multiple points in time), not multiple targets.
type hlia struct {
	Ver     string  `xml:"ver,attr"`
	Pos     []pos   `xml:"pos,omitempty"`
	Result  *result `xml:"result,omitempty"`
	AddInfo string  `xml:"add_info,omitempty"`
}

// slrep is the Standard Location Report DTD (§5.2.3.4.1): (pos+). Unlike
// slia, this is GMLC-initiated — an unsolicited push triggered by an
// inbound MO-LR Location-Report-Request, not a response to a client
// request — but its content model is identical, so pos is reused exactly
// the same way. See mlp.Pusher.
type slrep struct {
	Ver string `xml:"ver,attr"`
	Pos []pos  `xml:"pos"`
}

// slra is the Standard Location Report Answer DTD (§5.2.3.4.2): the LCS
// Client's response to a pushed slrep, read back by mlp.Pusher. Modeled as
// (result, add_info?) exactly per the DTD and both of the spec's own
// worked examples (§5.2.3.4.2).
type slra struct {
	Ver     string `xml:"ver,attr"`
	Result  result `xml:"result"`
	AddInfo string `xml:"add_info,omitempty"`
}

// emeEvent is MLP_EME_EVENT (§5.3.34): the trigger that initiated
// positioning for an emergency call, plus one or more eme_pos entries —
// pos reused exactly as emeLia does.
type emeEvent struct {
	// EmeTrigger is one of EME_ORG (origination), EME_REL (release), or
	// EME_HO (handover) — see emeTriggerFor.
	EmeTrigger string `xml:"eme_trigger,attr"`
	EmePos     []pos  `xml:"eme_pos"`
}

// emerep is the Emergency Location Report DTD (§5.2.3.5.1): (eme_event,
// locationserver_address? %extension.param;). locationserver_address (an
// address the LCS Client would use for a subsequent eme_lir) is not
// modeled — this GMLC has no use for advertising its own address back to
// itself. Unlike slrep, emerep has no answer message at all (§5.6.2.3
// Figure 10: an empty HTTP response is expected).
type emerep struct {
	Ver      string   `xml:"ver,attr"`
	EmeEvent emeEvent `xml:"eme_event"`
}

// eqop is the Extended Quality of Position DTD (§5.3, MLP_QOP), reduced to
// the fields this phase reads: horizontal accuracy, response-time class,
// and the response timer that also bounds this handler's synchronous wait.
type eqop struct {
	RespReq   *respReq `xml:"resp_req"`
	RespTimer *string  `xml:"resp_timer"`
	HorAcc    *horAcc  `xml:"hor_acc"`
}
type respReq struct {
	Type string `xml:"type,attr"`
}
type horAcc struct {
	Value string `xml:",chardata"`
}

// geoInfo carries only the coordinate reference system the client wants;
// this phase validates it (GMLC only ever produces WGS84/EPSG:4326) and
// otherwise ignores it — see validateGeoInfo.
type geoInfo struct {
	CRS coordinateReferenceSystem `xml:"CoordinateReferenceSystem"`
}
type coordinateReferenceSystem struct {
	Identifier identifier `xml:"Identifier"`
}
type identifier struct {
	Code string `xml:"code"`
}

// locType is MLP_FUNC's loc_type (§5.3.60). Only CURRENT (the default) and
// CURRENT_OR_LAST are supported — GMLC's own domain.LocationType has no
// other values.
type locType struct {
	Type string `xml:"type,attr"`
}

// prio is MLP_FUNC's prio (§5.3.95): NORMAL (default) or HIGH.
type prio struct {
	Type string `xml:"type,attr"`
}

// pos is one target's result within an slia (§5.2.2.3, MLP_LOC). Exactly
// one of Pd/PosErr is set.
type pos struct {
	Msid   msid    `xml:"msid"`
	Pd     *pd     `xml:"pd,omitempty"`
	PosErr *posErr `xml:"poserr,omitempty"`
}

// pd is a successful position determination (§5.3.91-ish, MLP_LOC). Only
// time+shape are modeled — every other pd child (MapData, alt, speed,
// direction, ...) is optional per the DTD and unused by this phase.
type pd struct {
	Time  timeElem `xml:"time"`
	Shape shape    `xml:"shape"`
}
type timeElem struct {
	UtcOff string `xml:"utc_off,attr"`
	Value  string `xml:",chardata"`
}

// shape (§5.2.2.5, MLP_SHAPE) is a CHOICE of geometry types; only
// CircularArea (Ellipsoid point with uncertainty circle) is modeled — the
// shape GMLC's own domain.Result/internal/gad already produce. Ellipse
// support is deferred (see the plan).
type shape struct {
	CircularArea *circularArea `xml:"CircularArea,omitempty"`
}
type circularArea struct {
	SrsName string `xml:"srsName,attr,omitempty"`
	Coord   coord  `xml:"coord"`
	Radius  string `xml:"radius"`
}

// coord (§5.3.21-ish) orders its axes (X, Y) as (Northing, Easting) — i.e.
// X is latitude, Y is longitude — matching EPSG:4326's own default axis
// order (see the spec's page 29/150 notes), confirmed against the spec's
// own worked example: <X>30 16 28.308N</X><Y>45 15 33.444E</Y>. Both are
// DMSH (degree-minute-second-hemisphere) strings, not decimal degrees —
// see encodeDMSH/decodeDMSH.
type coord struct {
	X string `xml:"X"`
	Y string `xml:"Y"`
}

// posErr (§5.2.2.4, MLP_RES) reports a per-target failure. Modeled as
// exactly (result, time), matching every worked poserr example in the spec
// (pages 40, 57) byte-for-byte — no add_info child is shown in any of them.
type posErr struct {
	Result result   `xml:"result"`
	Time   timeElem `xml:"time"`
}

// result (§5.3.108) carries the MLP §5.4 resid as an attribute and the
// human-readable slogan as element text.
type result struct {
	ResID string `xml:"resid,attr"`
	Value string `xml:",chardata"`
}

// gem is the General Error Message (§5.2.3.7) — a standalone document, not
// wrapped in svc_result, sent (per §5.6.1) as an HTTP 404 when the location
// server can't identify a defined MLP service in the request at all.
type gem struct {
	XMLName xml.Name `xml:"gem"`
	Ver     string   `xml:"ver,attr"`
	Result  result   `xml:"result"`
	AddInfo string   `xml:"add_info,omitempty"`
}

// dmshPattern matches MLP's DMSH coordinate format: "<deg> <min> <sec>H",
// e.g. "30 16 28.308N" — no separators besides the ASCII spaces between
// degrees/minutes/seconds, hemisphere letter directly appended to seconds.
var dmshPattern = regexp.MustCompile(`^(\d+) (\d+) (\d+(?:\.\d+)?)([NSEW])$`)

// encodeDMSH renders a decimal-degree coordinate as MLP's DMSH string.
// positiveHemi/negativeHemi select which hemisphere letters apply (N/S for
// latitude, E/W for longitude). Degrees/minutes/seconds are derived from a
// single rounded milli-arcsecond integer, not chained float arithmetic, so
// a value like 59.9999999 seconds can't round up into an invalid "60.000"
// display.
func encodeDMSH(decimalDegrees float64, positiveHemi, negativeHemi string) string {
	hemi := positiveHemi
	v := decimalDegrees
	if v < 0 {
		hemi = negativeHemi
		v = -v
	}
	totalMilliArcSec := int64(math.Round(v * 3600 * 1000))
	deg := totalMilliArcSec / (3600 * 1000)
	rem := totalMilliArcSec - deg*3600*1000
	min := rem / (60 * 1000)
	rem -= min * 60 * 1000
	sec := float64(rem) / 1000.0
	return fmt.Sprintf("%d %d %.3f%s", deg, min, sec, hemi)
}

// decodeDMSH parses MLP's DMSH coordinate format back to decimal degrees.
// Not needed by this phase's own request handling (slir carries no
// coordinates, only geo_info's CRS selection), but implemented alongside
// encodeDMSH so dtd_test.go can round-trip the spec's own worked examples
// exactly, not just structurally.
func decodeDMSH(s string) (float64, error) {
	m := dmshPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("mlp: invalid DMSH coordinate %q", s)
	}
	deg, _ := strconv.ParseFloat(m[1], 64)
	min, _ := strconv.ParseFloat(m[2], 64)
	sec, _ := strconv.ParseFloat(m[3], 64)
	v := deg + min/60 + sec/3600
	if m[4] == "S" || m[4] == "W" {
		v = -v
	}
	return v, nil
}

// formatMLPTime renders a timestamp in MLP's compact time format (§5.3.133),
// e.g. "20020623134453" — matching every worked example in the spec.
func formatMLPTime(t time.Time) string {
	return t.UTC().Format("20060102150405")
}

// parseMLPTime parses MLP's compact time format (§5.3.133) back to a
// time.Time, interpreting it as UTC — hlir's start_time/stop_time carry no
// timezone offset of their own (unlike pd's time, which pairs the same
// format with a separate utc_off attribute).
func parseMLPTime(s string) (time.Time, error) {
	return time.ParseInLocation("20060102150405", strings.TrimSpace(s), time.UTC)
}
