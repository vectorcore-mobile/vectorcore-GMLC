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

Phase 4A decodes TS 23.032 GAD Ellipsoid Point only (shape type 0). Raw
Location-Estimate bytes remain available for diagnostics; unsupported shapes
fail explicitly. GAD is compact binary, not ASN.1. Live multi-peer routing,
relay fallback, mixed TCP/SCTP deployments, and prolonged watchdog/reconnect
operation remain lab-validation work rather than development blockers.

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
