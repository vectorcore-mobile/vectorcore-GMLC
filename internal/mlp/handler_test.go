package mlp

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/service"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

// newTestHandler sets up a real sqlite store in t.TempDir() with one
// bootstrapped client, using a fast poll interval so sync-wait tests don't
// take real seconds.
func newTestHandler(t *testing.T, defaultWait, maxWait time.Duration) (*Handler, *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "db.sqlite"), CheckpointPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(ctx) })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertClient(ctx, storage.Client{ID: "mlp-client", CredentialHash: auth.HashToken("mlp-token"), Enabled: true, Services: []domain.ServiceType{domain.ServiceImmediate}, TargetPrefixes: []string{"3"}}); err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, auth.New(st))
	h := &Handler{svc: svc, syncWaitDefault: defaultWait, syncWaitMax: maxWait, pollInterval: 5 * time.Millisecond, now: time.Now}
	return h, st
}

// completeNextQueued mirrors what internal/orchestrator would eventually do
// for a real positioning result, without needing the whole SLg/Diameter
// stack: claim the next queued request, drive it through the same
// Queued->Resolving->Locating->Completed state machine, and attach a
// result. Started as a goroutine before the blocking HTTP call that
// created the request.
func completeNextQueued(t *testing.T, st *sqlite.Store, result domain.Result) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, ok, err := st.ClaimNextQueued(ctx, time.Now())
		if err != nil {
			t.Errorf("claim next queued: %v", err)
			return
		}
		if !ok {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if _, err := st.TransitionRequest(ctx, req.ID, domain.StateLocating, ""); err != nil {
			t.Errorf("transition to locating: %v", err)
			return
		}
		if err := st.CompleteRequest(ctx, req.ID, result); err != nil {
			t.Errorf("complete request: %v", err)
		}
		return
	}
	t.Error("completeNextQueued: no queued request appeared within deadline")
}

func svcInitXML(body string) string {
	return `<svc_init ver="3.5.0"><hdr ver="3.5.0"><client><id>mlp-client</id><pwd>mlp-token</pwd></client></hdr>` + body + `</svc_init>`
}

func postXML(t *testing.T, h http.Handler, body string) (*http.Response, svcResultXML) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	resp := w.Result()
	var out svcResultXML
	_ = xml.Unmarshal(w.Body.Bytes(), &out)
	return resp, out
}

// svcResultXML is a test-local decode target for the svc_result/slia
// response shape (tests that expect the GEM fallback instead decode
// directly into gem — see TestHandlerUnrecognizedServiceReturnsGEM).
type svcResultXML struct {
	XMLName xml.Name
	Slia    slia `xml:"slia"`
}

func TestHandlerSubmitSuccessProducesCircularArea(t *testing.T) {
	h, st := newTestHandler(t, 2*time.Second, 5*time.Second)
	lat, lon, unc := 32.622474, -86.295311, 13.58
	go completeNextQueued(t, st, domain.Result{Latitude: &lat, Longitude: &lon, UncertaintyMeters: &unc, Shape: "ellipsoid_point_uncertainty_circle", CreatedAt: time.Date(2026, 8, 9, 20, 28, 43, 0, time.UTC)})

	body := svcInitXML(`<slir ver="3.5.0" res_type="SYNC"><msids><msid type="IMSI">311435000070572</msid></msids><eqop><hor_acc>50</hor_acc></eqop><geo_info><CoordinateReferenceSystem><Identifier><code>4326</code><codeSpace>EPSG</codeSpace><edition>6.1</edition></Identifier></CoordinateReferenceSystem></geo_info><loc_type type="CURRENT" /><prio type="HIGH" /></slir>`)
	resp, out := postXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.XMLName.Local != "svc_result" {
		t.Fatalf("root element = %q, want svc_result", out.XMLName.Local)
	}
	if len(out.Slia.Pos) != 1 {
		t.Fatalf("pos count = %d, want 1: %#v", len(out.Slia.Pos), out.Slia)
	}
	p := out.Slia.Pos[0]
	if p.Msid.Value != "311435000070572" {
		t.Fatalf("echoed msid = %q", p.Msid.Value)
	}
	if p.Pd == nil || p.Pd.Shape.CircularArea == nil {
		t.Fatalf("expected a CircularArea pd, got %#v", p)
	}
	ca := p.Pd.Shape.CircularArea
	if ca.SrsName != epsg4326SrsName {
		t.Fatalf("srsName = %q", ca.SrsName)
	}
	if ca.Radius != "14" { // 13.58 rounded
		t.Fatalf("radius = %q, want ~14", ca.Radius)
	}
	gotLat, err := decodeDMSH(ca.Coord.X)
	if err != nil {
		t.Fatal(err)
	}
	gotLon, err := decodeDMSH(ca.Coord.Y)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gotLat - lat; diff > 1e-5 || diff < -1e-5 {
		t.Errorf("latitude = %v, want %v", gotLat, lat)
	}
	if diff := gotLon - lon; diff > 1e-5 || diff < -1e-5 {
		t.Errorf("longitude = %v, want %v", gotLon, lon)
	}
}

func TestHandlerRejectsUnsupportedMsidType(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	body := svcInitXML(`<slir ver="3.5.0"><msids><msid type="IPV4">93.10.0.250</msid></msids></slir>`)
	resp, out := postXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (whole-request-level slia failure)", resp.StatusCode)
	}
	if out.Slia.Result == nil || out.Slia.Result.ResID != resultProtocolElementAttributeValueNotSupported.ID {
		t.Fatalf("result = %#v, want resid %s", out.Slia.Result, resultProtocolElementAttributeValueNotSupported.ID)
	}
}

func TestHandlerRejectsUnsupportedLocType(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	body := svcInitXML(`<slir ver="3.5.0"><msids><msid type="IMSI">311435000070572</msid></msids><loc_type type="LAST" /></slir>`)
	resp, out := postXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Slia.Result == nil || out.Slia.Result.ResID != resultProtocolElementAttributeValueNotSupported.ID {
		t.Fatalf("result = %#v, want resid %s", out.Slia.Result, resultProtocolElementAttributeValueNotSupported.ID)
	}
}

func TestHandlerMissingMsidsIsFormatError(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	body := svcInitXML(`<slir ver="3.5.0"></slir>`)
	resp, out := postXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Slia.Result == nil || out.Slia.Result.ResID != resultFormatError.ID {
		t.Fatalf("result = %#v, want resid %s", out.Slia.Result, resultFormatError.ID)
	}
}

func TestHandlerSyncWaitTimeoutProducesPoserr(t *testing.T) {
	h, _ := newTestHandler(t, 20*time.Millisecond, 20*time.Millisecond)
	// Deliberately never complete the request: nothing calls
	// completeNextQueued, so it stays Queued/Resolving until the deadline.
	body := svcInitXML(`<slir ver="3.5.0"><msids><msid type="IMSI">311435000070573</msid></msids></slir>`)
	resp, out := postXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(out.Slia.Pos) != 1 || out.Slia.Pos[0].PosErr == nil {
		t.Fatalf("expected a per-target poserr: %#v", out.Slia)
	}
	if out.Slia.Pos[0].PosErr.Result.ResID != resultTimeout.ID {
		t.Fatalf("poserr resid = %q, want %s (TIMEOUT)", out.Slia.Pos[0].PosErr.Result.ResID, resultTimeout.ID)
	}
}

func TestHandlerUnauthenticatedIsRejected(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	body := `<svc_init ver="3.5.0"><hdr ver="3.5.0"><client><id>mlp-client</id><pwd>wrong-token</pwd></client></hdr><slir ver="3.5.0"><msids><msid type="IMSI">311435000070572</msid></msids></slir></svc_init>`
	resp, out := postXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Slia.Result == nil || out.Slia.Result.ResID != resultUnauthorizedApplication.ID {
		t.Fatalf("result = %#v, want resid %s (UNAUTHORIZED APPLICATION)", out.Slia.Result, resultUnauthorizedApplication.ID)
	}
}

// emeLirXML mirrors svcInitXML but wraps an eme_lir body instead of slir.
func emeLirXML(body string) string {
	return `<svc_init ver="3.5.0"><hdr ver="3.5.0"><client><id>mlp-client</id><pwd>mlp-token</pwd></client></hdr>` + body + `</svc_init>`
}

// emeLiaXML is a parallel decode target to svcResultXML, for the eme_lia
// response shape.
type emeLiaXML struct {
	XMLName xml.Name
	EmeLia  emeLia `xml:"eme_lia"`
}

func TestHandlerEmeLirSuccessProducesCircularAreaDetail(t *testing.T) {
	h, st := newTestHandler(t, 2*time.Second, 5*time.Second)
	lat, lon, unc := 32.622474, -86.295311, 13.58
	go completeNextQueued(t, st, domain.Result{Latitude: &lat, Longitude: &lon, UncertaintyMeters: &unc, Shape: "ellipsoid_point_uncertainty_circle", CreatedAt: time.Date(2026, 8, 9, 20, 28, 43, 0, time.UTC)})

	body := emeLirXML(`<eme_lir ver="3.5.0" res_type="SYNC"><msids><msid type="IMSI">311435000070572</msid></msids></eme_lir>`)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out emeLiaXML
	if err := xml.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	if len(out.EmeLia.EmePos) != 1 {
		t.Fatalf("eme_pos count = %d, want 1: %s", len(out.EmeLia.EmePos), w.Body.String())
	}
	p := out.EmeLia.EmePos[0]
	if p.Msid.Value != "311435000070572" || p.Pd == nil || p.Pd.Shape.CircularArea == nil {
		t.Fatalf("eme_pos = %#v", p)
	}
	gotLat, err := decodeDMSH(p.Pd.Shape.CircularArea.Coord.X)
	if err != nil || gotLat < 32 || gotLat > 33 {
		t.Fatalf("latitude decode: %v %v", gotLat, err)
	}
}

func TestHandlerEmeLirRejectsEMEMSIDType(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	body := emeLirXML(`<eme_lir ver="3.5.0"><msids><msid type="EME_MSID">520002-51-431172-6-06</msid></msids></eme_lir>`)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out emeLiaXML
	if err := xml.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	if out.EmeLia.Result == nil || out.EmeLia.Result.ResID != resultProtocolElementAttributeValueNotSupported.ID {
		t.Fatalf("result = %#v, want resid %s", out.EmeLia.Result, resultProtocolElementAttributeValueNotSupported.ID)
	}
}

func TestHandlerEmeLirUsesConfiguredClientTypeNotRequestContent(t *testing.T) {
	// Regression test for the Phase B design decision recorded in
	// docs/mlp-le-interface-plan.md: eme_lir carries no field that can
	// influence LCS-Client-Type — it's resolved purely from which GMLC
	// client authenticated (internal/orchestrator), exactly like slir. A
	// client configured as value_added_services sending eme_lir must still
	// work (it's not rejected merely for using the emergency message type),
	// proving eme_lir doesn't require or check any emergency-specific
	// client configuration that doesn't exist in this codebase.
	h, st := newTestHandler(t, 2*time.Second, 5*time.Second)
	lat, lon := 1.0, 2.0
	go completeNextQueued(t, st, domain.Result{Latitude: &lat, Longitude: &lon, Shape: "ellipsoid_point_uncertainty_circle", CreatedAt: time.Now()})
	body := emeLirXML(`<eme_lir ver="3.5.0"><msids><msid type="IMSI">311435000070572</msid></msids></eme_lir>`)
	resp, _ := postXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// hlirXML mirrors svcInitXML/emeLirXML but wraps an hlir body.
func hlirXML(body string) string {
	return `<svc_init ver="3.5.0"><hdr ver="3.5.0"><client><id>mlp-client</id><pwd>mlp-token</pwd></client></hdr>` + body + `</svc_init>`
}

// hliaXML is a parallel decode target to svcResultXML/emeLiaXML, for the
// hlia response shape.
type hliaXML struct {
	XMLName xml.Name
	Hlia    hlia `xml:"hlia"`
}

func postHlirXML(t *testing.T, h http.Handler, body string) (*http.Response, hliaXML) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	resp := w.Result()
	var out hliaXML
	if err := xml.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return resp, out
}

func TestHandlerHlirSuccessReturnsHistoryPointsOldestFirst(t *testing.T) {
	h, st := newTestHandler(t, time.Second, time.Second)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	lat1, lon1 := 32.6, -86.2
	lat2, lon2 := 32.7, -86.3
	if err := st.RecordHistory(context.Background(), "imsi", "311435000070572", domain.Result{Latitude: &lat1, Longitude: &lon1, Shape: "ellipsoid_point_uncertainty_circle", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(context.Background(), "imsi", "311435000070572", domain.Result{Latitude: &lat2, Longitude: &lon2, Shape: "ellipsoid_point_uncertainty_circle", CreatedAt: base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	body := hlirXML(`<hlir ver="3.5.0"><msid type="IMSI">311435000070572</msid><start_time>` + formatMLPTime(base.Add(-time.Hour)) + `</start_time><stop_time>` + formatMLPTime(base.Add(2*time.Hour)) + `</stop_time></hlir>`)
	resp, out := postHlirXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(out.Hlia.Pos) != 2 {
		t.Fatalf("pos count = %d, want 2: %#v", len(out.Hlia.Pos), out.Hlia)
	}
	if out.Hlia.Pos[0].Pd == nil || out.Hlia.Pos[0].Pd.Shape.CircularArea == nil {
		t.Fatalf("pos[0] missing CircularArea: %#v", out.Hlia.Pos[0])
	}
	gotLat0, err := decodeDMSH(out.Hlia.Pos[0].Pd.Shape.CircularArea.Coord.X)
	if err != nil || gotLat0 < 32.5 || gotLat0 > 32.65 {
		t.Fatalf("pos[0] expected oldest fix (lat~32.6), got %v %v", gotLat0, err)
	}
	gotLat1, err := decodeDMSH(out.Hlia.Pos[1].Pd.Shape.CircularArea.Coord.X)
	if err != nil || gotLat1 < 32.65 || gotLat1 > 32.75 {
		t.Fatalf("pos[1] expected newest fix (lat~32.7), got %v %v", gotLat1, err)
	}
}

func TestHandlerHlirMissingStartTimeIsFormatError(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	body := hlirXML(`<hlir ver="3.5.0"><msid type="IMSI">311435000070572</msid></hlir>`)
	resp, out := postHlirXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Hlia.Result == nil || out.Hlia.Result.ResID != resultFormatError.ID {
		t.Fatalf("result = %#v, want resid %s", out.Hlia.Result, resultFormatError.ID)
	}
}

func TestHandlerHlirRejectsStopBeforeStart(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	body := hlirXML(`<hlir ver="3.5.0"><msid type="IMSI">311435000070572</msid><start_time>` + formatMLPTime(base) + `</start_time><stop_time>` + formatMLPTime(base.Add(-time.Hour)) + `</stop_time></hlir>`)
	resp, out := postHlirXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Hlia.Result == nil || out.Hlia.Result.ResID != resultInvalidProtocolElementValue.ID {
		t.Fatalf("result = %#v, want resid %s", out.Hlia.Result, resultInvalidProtocolElementValue.ID)
	}
}

func TestHandlerHlirNoDataReturnsPositionMethodFailure(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	body := hlirXML(`<hlir ver="3.5.0"><msid type="IMSI">311435000070572</msid><start_time>` + formatMLPTime(base) + `</start_time></hlir>`)
	resp, out := postHlirXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Hlia.Result == nil || out.Hlia.Result.ResID != resultPositionMethodFailure.ID {
		t.Fatalf("result = %#v, want resid %s (no recorded history)", out.Hlia.Result, resultPositionMethodFailure.ID)
	}
}

func TestHandlerHlirRejectsUnsupportedMsidType(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	body := hlirXML(`<hlir ver="3.5.0"><msid type="EME_MSID">520002-51-431172-6-06</msid><start_time>` + formatMLPTime(base) + `</start_time></hlir>`)
	resp, out := postHlirXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Hlia.Result == nil || out.Hlia.Result.ResID != resultProtocolElementAttributeValueNotSupported.ID {
		t.Fatalf("result = %#v, want resid %s", out.Hlia.Result, resultProtocolElementAttributeValueNotSupported.ID)
	}
}

func TestHandlerHlirIntervalThinsResults(t *testing.T) {
	h, st := newTestHandler(t, time.Second, time.Second)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	lats := []float64{32.1, 32.2, 32.3}
	for i, lat := range lats {
		lat := lat
		lon := -86.0
		if err := st.RecordHistory(context.Background(), "imsi", "311435000070572", domain.Result{Latitude: &lat, Longitude: &lon, Shape: "ellipsoid_point_uncertainty_circle", CreatedAt: base.Add(time.Duration(i) * 30 * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	body := hlirXML(`<hlir ver="3.5.0"><msid type="IMSI">311435000070572</msid><start_time>` + formatMLPTime(base.Add(-time.Minute)) + `</start_time><stop_time>` + formatMLPTime(base.Add(time.Hour)) + `</stop_time><interval>60</interval></hlir>`)
	resp, out := postHlirXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(out.Hlia.Pos) != 2 {
		t.Fatalf("expected interval=60 to thin 3 points 30s apart down to 2, got %d: %#v", len(out.Hlia.Pos), out.Hlia)
	}
}

func TestHandlerHlirNoOfReportsLimitsResults(t *testing.T) {
	h, st := newTestHandler(t, time.Second, time.Second)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		lat, lon := 32.1, -86.0
		if err := st.RecordHistory(context.Background(), "imsi", "311435000070572", domain.Result{Latitude: &lat, Longitude: &lon, Shape: "ellipsoid_point_uncertainty_circle", CreatedAt: base.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	body := hlirXML(`<hlir ver="3.5.0"><msid type="IMSI">311435000070572</msid><start_time>` + formatMLPTime(base.Add(-time.Minute)) + `</start_time><stop_time>` + formatMLPTime(base.Add(4*time.Hour)) + `</stop_time><no_of_reports>1</no_of_reports></hlir>`)
	resp, out := postHlirXML(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(out.Hlia.Pos) != 1 {
		t.Fatalf("expected no_of_reports=1 to cap to 1 point, got %d: %#v", len(out.Hlia.Pos), out.Hlia)
	}
}

func TestHandlerUnrecognizedServiceReturnsGEM(t *testing.T) {
	h, _ := newTestHandler(t, time.Second, time.Second)
	for _, body := range []string{
		`<svc_init ver="3.5.0"><hdr ver="3.5.0"><client><id>mlp-client</id><pwd>mlp-token</pwd></client></hdr><tlrr ver="3.5.0"></tlrr></svc_init>`,
		`not even xml`,
		`<slir ver="3.5.0"><msids><msid>1</msid></msids></slir>`, // bare, not svc_init-wrapped
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		resp := w.Result()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("body %q: status = %d, want 404", body, resp.StatusCode)
			continue
		}
		var g gem
		if err := xml.Unmarshal(w.Body.Bytes(), &g); err != nil {
			t.Errorf("body %q: gem decode: %v", body, err)
			continue
		}
		if g.Result.ResID != resultServiceNotSupported.ID {
			t.Errorf("body %q: gem resid = %q, want %s", body, g.Result.ResID, resultServiceNotSupported.ID)
		}
	}
}
