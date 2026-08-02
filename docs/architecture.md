# VectorCore GMLC architecture

The GMLC is a single Go binary with adapters around a protocol-neutral service.
REST/JSON is the first Le adapter.  Future OMA MLP is another adapter; neither
JSON nor Diameter AVPs are domain objects.

`REST -> authentication/authorization/privacy -> request service -> storage`

Phase 2 adds a reusable Diameter lifecycle boundary and a TS 29.173 SLh client.
REST requests intentionally remain `queued`; SLh is independently usable and
may persist stable MME routing data, but it never yields a location result.

Each CER advertises SLh Application-Id 16777291 and SLg Application-Id
16777255. RIR/RIA uses command 8388622,
Session-Id, Auth-Session-State=NO_STATE_MAINTAINED, Origin/Destination AVPs,
User-Name, MSISDN (3GPP TBCD), Result-Code/Experimental-Result, and Serving-
Node MME-Name/MME-Realm. Destination-Host is sent only for an exact known
destination. Diameter is mandatory and peers independently use TCP or
clear-text SCTP with Diameter PPID 46.

Phase 2B uses `github.com/fiorix/go-diameter/v4` v4.3.0. Its state machine
performs TCP CER/CEA, inbound DWR/DWA base handling, and watchdog generation.
The GMLC adapter registers only configured application answer command indexes;
it correlates application requests by Hop-by-Hop ID inside one live connection,
removes entries on context completion, and drains them on connection loss or
shutdown. A new connection starts with a new empty correlation map. Readiness
is always true in Diameter-disabled development mode; with Diameter enabled it
is true only after the manager reaches negotiated `ready` state.

Phase 3A adds an outbound-only SLg client boundary: Application-Id `16777255`,
PLR/PLA command `8388620`, and 3GPP vendor `10415`. It supports only immediate
Current and Current-or-Last-Known requests. A successful PLA retains bounded
raw Location-Estimate, ECGI, and result metadata without decoding TS 23.032.
Deferred/no-immediate-result PLA is represented as such and never completes a
REST request. LRR/LRA is not registered or advertised.

Named peers have no configured application or network-function role. The CEA
publishes each peer's Origin-Host, Origin-Realm, and applications. Application
ID `0xffffffff` means relay-capable and provides fallback for either protocol.
Selection prefers a direct application peer by exact Destination-Host, then
Destination-Realm, then a relay. Peer order is only a deterministic tie-break.
Connection loss immediately removes that generation's capabilities, drains its
requests, and starts bounded reconnect. Readiness requires routes for both SLh
and SLg; a relay satisfies both. Subscriber or returned Diameter identities
never become network dial addresses. Product-Name is fixed to VectorCore-GMLC
and Vendor-Id/Supported-Vendor-Id are fixed to 10415.

| SLh outcome | Internal error |
| --- | --- |
| 5001 | unknown subscriber |
| 4201 | no serving MME |
| 5490 | unauthorized request |
| Base failure | base Diameter failure with code |
| Other experimental result | experimental failure with code |
| Bad correlation/grouping | malformed or contradictory response |

| Concern | Authority | Phase |
| --- | --- | --- |
| LCS architecture, policy and procedures | TS 23.271 Rel-16 | 1 onward |
| SLh RIR/RIA | TS 29.173 Rel-16 | 2 |
| SLg PLR/PLA and LRR/LRA | TS 29.172 Rel-16 | 3/4 |
| GAD and velocity | TS 23.032 Rel-16 | 3 |
| E-UTRAN positioning data | TS 29.171 Rel-16 | 3 |
| Diameter base | RFC 6733 | 2 onward |
| Le XML | OMA MLP | 5 |

SQLite is local-disk only: WAL mode, foreign keys, busy timeout, explicit
synchronous mode, embedded ordered migrations, and checkpoint-on-close. The
storage interface is intentionally SQL-neutral; PostgreSQL is deferred.

Planned schema boundaries not yet implemented are deferred subscriptions,
delivery outbox/attempts, and encrypted callback secrets.

Phase 4A decodes TS 23.032 V16.1.0 GAD Ellipsoid Point (0x0), Ellipsoid Point
with Uncertainty Circle (0x1), Ellipsoid Point with Uncertainty Ellipse
(0x3), and Polygon (0x5) — the shapes covering the large majority of
Cell-ID/E-CID/OTDOA positioning; altitude variants, arc, and high-accuracy
shapes remain unsupported and fail explicitly. The shape type is the low
nibble of octet 1 (`docs/specs/ts_23032_rel16.txt` Table 2a); Polygon's high
nibble instead carries its point count (3-15), and its own bit values are
sparse (e.g. Polygon = 0x5, not 4) — distinct from the sequential bit
numbering used by the unrelated Supported-GAD-Shapes AVP bitmask. Polygon
has no single center point, so it never populates `GeographicPosition`; the
raw Location-Estimate bytes are retained for diagnostics rather than
fabricating a misleading coordinate. Ellipse populates the center point plus
semi-major/semi-minor/orientation/confidence. Raw Location-Estimate bytes
always remain available regardless of shape. GAD is compact binary, not
ASN.1. Live multi-peer routing, relay fallback, mixed TCP/SCTP deployments,
and prolonged watchdog/reconnect operation remain lab-validation work rather
than development blockers.

A successful PLA that resolves to a `no_immediate_result`/`deferred` outcome
(no Location-Estimate, no ECGI) never completes a REST request; the
orchestrator fails it explicitly with `no_immediate_result` instead. SLh
routes to `diameter.hss_realm`/`diameter.hss_host` (defaulting to the GMLC's
own realm for same-operator deployments) rather than assuming the HSS shares
the GMLC's realm. `retention.purge_interval` (default 1h) enforces the
retention window on a recurring basis, not only at startup, and purge also
clears `audit_events` past `retention.request`.

Phase 4B.1 adds durable queue primitives: SQLite atomically claims due queued
requests, records attempts/next-attempt metadata, moves a resolving request to
locating together with its serving node, and commits a positioning result with
the completed state in one transaction. Restart recovery returns interrupted
resolving/locating work to queued. The single worker and HTTP result activation
are deferred to Phase 4B.2. The binary uses `-c`, `-d`, and `-v`; Make injects
the build version with linker flags.

At startup the console prints `Starting VectorCore GMLC` once. Operational
logs are file-only by default under `logging.file` at `logging.level`; `-d`
adds an independently filtered debug console sink. The `-v` path prints only
the injected version and does not initialize configuration or logging.

Phase 4C adds native Go fuzz targets for GAD, SLh TBCD encoding, SLg PLA, and
REST request JSON. Run `make fuzz-smoke` for a bounded pass (`FUZZ_TIME=30s`
overrides it); longer focused runs use the corresponding `go test -fuzz` target.
Intentional regression seeds belong beside their package fuzz tests. Fuzzing
supplements unit tests and lab interoperability testing.

Phase 4D verifies the SLg wire format against 3GPP TS 29.172 V16.1.0
(Release 16; `docs/specs/ts_29172_rel16.txt`, extracted from the 3GPP archive
`29172-g10.docx`) rather than assumption. LCS-EPS-Client-Name (2501) and
LCS-Requestor-Name (2502) are `[LCS-Name-String]`/`[LCS-Requestor-Id-String]`
+ `[LCS-Format-Indicator]` only — no LCS-Data-Coding-Scheme child, correcting
an earlier implementation that wrongly borrowed the TS 32.299 Rf/Ro
LCS-Client-Name shape. PLR also carries LCS-Priority (2503), Velocity-
Requested (2508), LCS-Service-Type-ID (2520), and Supported-GAD-Shapes
(2510) — a bitmask declaring the TS 23.032 shapes this GMLC can decode
(bits 0-3: point, circle, ellipse, polygon — kept in sync with
`internal/gad`'s supported shapes), so the network isn't invited to return
shapes it can't parse. PLA decoding now also captures Accuracy-Fulfilment-Indicator (2513),
Age-Of-Location-Estimate (2514), Velocity-Estimate (2515), and
EUTRAN-Positioning-Data (2516). `docs/specs/` holds the Release 16 source
documents (`.doc`/`.docx`) plus extracted text for the three governing specs
(TS 29.172, TS 29.173, TS 23.032) as the reference for any future AVP work —
check codes there before adding new wire fields rather than assuming.

LCS-Client-Type (TS 29.172 7.4, reused from TS 32.299) is an
operator-configured, per-client attribute (`clients[].lcs_client_type` in
config, defaulting to `value_added_services`) resolved at dispatch time via
the client's stored record — never caller-supplied over the REST API, since
`emergency_services`/`lawful_intercept_services` carry regulatory weight
(e.g. privacy-check bypass) at the MME/HSS.

`auth.Authenticate` is timing-safe against client-ID enumeration.
`Store.GetClientCredential` is a single fixed-cost query (independent of
whether the ID matches, and of how many services/prefixes that client has)
used for the credential check; the constant-time compare always runs
against it, using a fixed dummy hash when the client doesn't exist or is
disabled, so a failure never short-circuits before the compare. The
variable-cost `Store.GetClientAuthzData` lookup (services/prefixes) only
runs after a token has already been verified — timing there leaks nothing
an attacker didn't already need a valid credential to reach.

`location_results.raw_gad` was `NOT NULL` since the original schema, but an
`additional_information` (ECGI-only) PLA outcome has no Location-Estimate to
store — `CompleteRequest` always sent `NULL` for it, silently violating the
constraint and permanently stranding those requests in `locating` (never
retried, never failed). Migration `008_raw_gad_nullable.sql` recreates the
table with `raw_gad` nullable. `Age-Of-Location-Estimate` (2514) and
`Accuracy-Fulfilment-Indicator` (2513) are independent of Position — an
ECGI-only or Polygon completion can still carry them — so they're read from
the PLA result directly rather than gated behind a decoded position;
`Velocity-Estimate` (2515) and `EUTRAN-Positioning-Data` (2516) are kept as
undecoded bytes, like `raw_gad`, for diagnostics rather than structured API
exposure. The REST `result` object itself was gated on
Latitude/Longitude being present, which silently dropped ECGI-only and
Polygon completions from the API response even though the request
genuinely completed; it's now gated on request state alone, with each field
inside it independently optional.
