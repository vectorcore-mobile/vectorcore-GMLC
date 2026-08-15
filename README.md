# vectorcore-GMLC
Gateway Mobile Location Centre

## Description

VectorCore GMLC is a single Go binary implementing a 3GPP Gateway Mobile Location Centre for
LTE/EPC networks. It resolves subscriber location on behalf of LCS clients by locating the
subscriber's serving MME over Diameter SLh (TS 29.173), requesting a position over Diameter SLg
(TS 29.172), and decoding the returned TS 23.032 GAD shape. The GMLC is built as a protocol-neutral
request/orchestration/storage core with thin adapters around it: an OMA MLP Le interface
(`slir`/`slia`, in progress) as the standards-based alternative, and a
Diameter boundary shared by SLh, SLg, and inbound LRR handling. Requests are durably queued in
SQLite and driven to completion by a background worker, so results survive a restart.

## Features

- **OMA MLP Le adapter** — Standard Location Immediate Service (`slir`/`slia`) over a second,
  independently configurable HTTP listener.
- **Diameter SLh client** — resolves a subscriber's serving MME via TS 29.173 RIR/RIA.
- **Diameter SLg client** — requests immediate Current / Current-or-Last-Known position via TS
  29.172 PLR/PLA, including QoS (accuracy, response time), priority, and LCS-Service-Type.
- **Inbound LRR/LRA handling** — decodes and answers TS 29.172 Location-Report-Request over the
  same DRA-facing connection, with deferred-request correlation and MLP unsolicited-report
  (`slrep`/`emerep`) pushing.
- **TS 23.032 GAD decoding** — Ellipsoid Point, Point with Uncertainty Circle, Point with
  Uncertainty Ellipse, and Polygon shapes, plus velocity and accuracy metadata.
- **Multi-peer Diameter transport** — TCP and clear-text SCTP peers, capability-negotiated
  routing, relay fallback, watchdogs, and bounded reconnect.
- **Durable request queue** — SQLite-backed (WAL mode) queue with atomic claim/attempt tracking,
  crash-safe restart recovery, and configurable retention/purge.
- **Outbound delivery worker** — signed (HMAC-SHA256) webhook callbacks with encrypted-at-rest
  secrets and exponential-backoff retry, shared by async completions and LRR reports.
- **Per-client authorization** — bearer-token auth, timing-safe credential checks, and
  per-client target-prefix/service-type scoping, with operator-only LCS-Client-Type and
  LCS-Privacy-Check controls.
- **Operational tooling** — file-based logging with an optional debug console sink,
  `/healthz`/`/readyz` endpoints, and native Go fuzz targets for GAD, SLh, and SLg decoding
  (`make fuzz-smoke`).

## Building

Requires Go 1.26+ (see `go.mod`).

```bash
make clean   # remove bin/
make         # build bin/gmlc (default target)
```

`make` compiles `./cmd/gmlc` into `bin/gmlc`, with the build version injected via `-ldflags`
(`VERSION` in the `Makefile`). Other targets: `make test` (`go test ./...`), `make vet`
(`go vet ./...`), and `make fuzz-smoke` (short fuzz pass over GAD/SLh/SLg decoding).

Run it with a config file (see `config/gmlc.yaml.example`):

```bash
bin/gmlc -c config/gmlc.yaml       # normal run
bin/gmlc -c config/gmlc.yaml -d    # also log debug output to the console
bin/gmlc -v                        # print the build version and exit
```
