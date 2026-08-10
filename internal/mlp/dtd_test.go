package mlp

import (
	"encoding/xml"
	"math"
	"strings"
	"testing"
)

func TestEncodeDecodeDMSHRoundTrip(t *testing.T) {
	for _, tt := range []struct{ s, pos, neg string }{
		{"30 16 28.308N", "N", "S"},
		{"45 15 33.444E", "E", "W"},
		{"30 12 28.296N", "N", "S"},
		{"86 56 33.864E", "E", "W"},
		{"78 12 34.308N", "N", "S"},
		{"76 22 2.820E", "E", "W"},
		{"0 0 0.000N", "N", "S"},
	} {
		v, err := decodeDMSH(tt.s)
		if err != nil {
			t.Fatalf("decode %q: %v", tt.s, err)
		}
		got := encodeDMSH(v, tt.pos, tt.neg)
		if got != tt.s {
			t.Errorf("round trip %q -> %v -> %q, want %q", tt.s, v, got, tt.s)
		}
	}
}

func TestDecodeDMSHHemispheres(t *testing.T) {
	lat, err := decodeDMSH("30 16 28.308N")
	if err != nil || lat <= 0 {
		t.Fatalf("N should be positive: %v %v", lat, err)
	}
	lat, err = decodeDMSH("30 16 28.308S")
	if err != nil || lat >= 0 {
		t.Fatalf("S should be negative: %v %v", lat, err)
	}
	lon, err := decodeDMSH("86 29 31.883W")
	if err != nil || lon >= 0 {
		t.Fatalf("W should be negative: %v %v", lon, err)
	}
	if _, err := decodeDMSH("not a coordinate"); err == nil {
		t.Fatal("expected error for malformed DMSH")
	}
}

func TestEncodeDMSHRealFix(t *testing.T) {
	// The confirmed-live A-GNSS fix from this session (~32.622N -86.295W,
	// Alabama): verify encode/decode agree to within a few meters
	// (~0.0001 deg) after round-tripping through DMSH's 0.001s precision
	// (roughly 3cm at the equator, well under any reasonable tolerance).
	lat, lon := 32.622474, -86.295311
	x := encodeDMSH(lat, "N", "S")
	y := encodeDMSH(lon, "E", "W")
	gotLat, err := decodeDMSH(x)
	if err != nil {
		t.Fatal(err)
	}
	gotLon, err := decodeDMSH(y)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(gotLat-lat) > 1e-6 || math.Abs(gotLon-lon) > 1e-6 {
		t.Fatalf("precision loss: (%v,%v) -> (%q,%q) -> (%v,%v)", lat, lon, x, y, gotLat, gotLon)
	}
}

// specSliaExample is the OMA-TS-MLP-V3_5-20181211-C §5.2.3.2.2 worked
// example (pages 39-40), captured verbatim this session: a successful
// multi-target slia (three CircularArea fixes) plus one per-target
// poserr. It uses only bare <msid> (defaulting to MSISDN per the DTD),
// matching Phase A's supported scope exactly.
const specSliaExample = `<slia ver="3.5.0" >
	<pos>
	  <msid>461011334411</msid>
	  <pd>
		<time utc_off="+0200">20020623134453</time>
		<shape>
			<CircularArea srsName="www.epsg.org#4326">
				<coord>
				  <X>30 16 28.308N</X>
				  <Y>45 15 33.444E</Y>
				</coord>
			<radius>240</radius>
			</CircularArea>
		</shape>
	  </pd>
	</pos>
	<pos>
	  <msid>461018765710</msid>
	  <pd>
		<time utc_off="+0300">20020623134454</time>
		<shape>
			<CircularArea srsName="www.epsg.org#4326">
				<coord>
				  <X>30 12 28.296N</X>
				  <Y>86 56 33.864E</Y>
				</coord>
			<radius>570</radius>
			</CircularArea>
		</shape>
	  </pd>
	</pos>
	<pos>
	  <msid>461018765711</msid>
	  <pd>
		<time utc_off="+0300">20020623110205</time>
		<shape>
			<CircularArea srsName="www.epsg.org#4326">
				<coord>
				  <X>78 12 34.308N</X>
				  <Y>76 22 2.82E</Y>
				</coord>
			<radius>15</radius>
			</CircularArea>
		</shape>
	  </pd>
	</pos>
	<pos>
	  <msid>461018765712</msid>
	  <poserr>
		<result resid="10">QOP NOT ATTAINABLE</result>
		<time>20020623134454</time>
	  </poserr>
	</pos>
</slia>`

func TestSliaDecodesSpecExampleBytes(t *testing.T) {
	var v slia
	if err := xml.Unmarshal([]byte(specSliaExample), &v); err != nil {
		t.Fatalf("decode spec example: %v", err)
	}
	if len(v.Pos) != 4 {
		t.Fatalf("expected 4 pos entries, got %d", len(v.Pos))
	}
	if v.Pos[0].Msid.Value != "461011334411" || v.Pos[0].Msid.Type != "" {
		t.Fatalf("pos[0] msid = %#v", v.Pos[0].Msid)
	}
	if v.Pos[0].Pd == nil || v.Pos[0].Pd.Shape.CircularArea == nil {
		t.Fatalf("pos[0] missing CircularArea: %#v", v.Pos[0])
	}
	ca := v.Pos[0].Pd.Shape.CircularArea
	if ca.SrsName != epsg4326SrsName || ca.Radius != "240" {
		t.Fatalf("pos[0] CircularArea = %#v", ca)
	}
	lat, err := decodeDMSH(ca.Coord.X)
	if err != nil || lat < 30 || lat > 31 {
		t.Fatalf("pos[0] latitude decode: %v %v", lat, err)
	}
	lon, err := decodeDMSH(ca.Coord.Y)
	if err != nil || lon < 45 || lon > 46 {
		t.Fatalf("pos[0] longitude decode: %v %v", lon, err)
	}
	if v.Pos[0].Pd.Time.UtcOff != "+0200" || v.Pos[0].Pd.Time.Value != "20020623134453" {
		t.Fatalf("pos[0] time = %#v", v.Pos[0].Pd.Time)
	}
	// The fourth target is the spec's own poserr example.
	last := v.Pos[3]
	if last.Msid.Value != "461018765712" || last.Pd != nil || last.PosErr == nil {
		t.Fatalf("pos[3] = %#v", last)
	}
	if last.PosErr.Result.ResID != "10" || last.PosErr.Result.Value != "QOP NOT ATTAINABLE" {
		t.Fatalf("pos[3] poserr result = %#v", last.PosErr.Result)
	}
	if last.PosErr.Time.Value != "20020623134454" {
		t.Fatalf("pos[3] poserr time = %#v", last.PosErr.Time)
	}

	// Round-trip stability: marshal what we decoded, then decode that
	// output again and confirm it's structurally identical — this is the
	// meaningful invariant for XML (byte-identical isn't, since whitespace/
	// attribute order aren't semantically significant).
	out, err := xml.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var v2 slia
	if err := xml.Unmarshal(out, &v2); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if len(v2.Pos) != 4 || v2.Pos[0].Pd.Shape.CircularArea.Coord.X != ca.Coord.X || v2.Pos[3].PosErr.Result.ResID != "10" {
		t.Fatalf("round trip mismatch: %#v", v2)
	}
}

// specEmeLiaExample is OMA-TS-MLP-V3_5-20181211-C §5.2.3.3.2's worked
// example (pages 43-44), captured this session. It uses msid type
// "EME_MSID" and carries an <esrk> — both deliberately out of Phase B's
// scope (see emeLir/emeLia's own doc comments: EME_MSID has no GMLC
// equivalent, esrk/esrd are North-American-only and never populated) — so
// this test only asserts what emeLia/pos *do* capture (msid value/type,
// time, CircularArea), and explicitly confirms esrk is silently dropped
// rather than causing a decode error, which is the expected, documented
// behavior for an out-of-scope optional element.
const specEmeLiaExample = `<eme_lia ver="3.5.0">
  <eme_pos>
    <msid type="EME_MSID">520002-51-431172-6-06</msid>
    <pd>
      <time utc_off="+0300">20020623134453</time>
      <shape>
        <CircularArea srsName="www.epsg.org#4326">
          <coord>
            <X>30 24 43.53N</X>
            <Y>45 28 09.534W</Y>
          </coord>
          <radius>15</radius>
        </CircularArea>
      </shape>
    </pd>
    <esrk>7839298236</esrk>
  </eme_pos>
</eme_lia>`

func TestEmeLiaDecodesSpecExampleShape(t *testing.T) {
	var v emeLia
	if err := xml.Unmarshal([]byte(specEmeLiaExample), &v); err != nil {
		t.Fatalf("decode spec example: %v", err)
	}
	if len(v.EmePos) != 1 {
		t.Fatalf("expected 1 eme_pos entry, got %d", len(v.EmePos))
	}
	p := v.EmePos[0]
	if p.Msid.Type != "EME_MSID" || p.Msid.Value != "520002-51-431172-6-06" {
		t.Fatalf("eme_pos msid = %#v", p.Msid)
	}
	if p.Pd == nil || p.Pd.Shape.CircularArea == nil {
		t.Fatalf("eme_pos missing CircularArea: %#v", p)
	}
	ca := p.Pd.Shape.CircularArea
	if ca.Radius != "15" || ca.SrsName != epsg4326SrsName {
		t.Fatalf("eme_pos CircularArea = %#v", ca)
	}
	lat, err := decodeDMSH(ca.Coord.X)
	if err != nil || lat < 30 || lat > 31 {
		t.Fatalf("eme_pos latitude decode: %v %v", lat, err)
	}
	// esrk is out of scope and has no field on pos — round-tripping through
	// our own type must not error, it just won't reproduce that element.
	out, err := xml.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if strings.Contains(string(out), "esrk") {
		t.Errorf("esrk unexpectedly survived round trip (expected to be dropped, out of Phase B scope): %s", out)
	}
}
