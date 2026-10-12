# opsledger — Verifiable Operational Evidence

A Go service and a set of commands that measure a running website from its reverse proxy's request
stream and from outside, publish daily reports that anyone can recompute from their observations,
and verify signed releases before they are deployed. It runs on https://aliakbari.dev; see
[/operations](https://aliakbari.dev/operations) and the case study
[/work/opsledger](https://aliakbari.dev/work/opsledger).

Decisions: [ADR-0015](docs/adr/0015-hosted-ci-signed-releases-fail-closed-deploys.md) (signed
releases, fail-closed deploys), [ADR-0016](docs/adr/0016-opsledger-operational-evidence.md)
(the service), [ADR-0017](docs/adr/0017-slos-and-recomputable-signed-reports.md) (service
levels and reports), [ADR-0018](docs/adr/0018-first-party-analytics-umami.md) (private
analytics, kept separate). SLI definitions: [docs/operations/slo.md](docs/slo.md).

## What it does

```
nginx ──(one JSON record per request, Unix datagram socket)──► reader ─► bounded queue ─► aggregator ─► SQLite (WAL)
  │                                                                                                    │
  └── stub_status (nginx's own request counter) ◄── reconciliation sampler, once a minute              ├─► /api/v1/ops/* (read-only)
deploy.sh ── release.json (deployment record) ──► release watcher ─────────────────────────────────────┘   /api/v1/release

GitHub-hosted runners:  probe (every 30 min) ──► probe-run-v1 artifacts
                        ops-publish (daily)  ──► assemble ─► recompute ─► Sigstore signature ─► reports/ (public)
                        ci.yml release job   ──► release-manifest-v1 ─► Sigstore signature ─► aliakbari-release:<sha>
```

- **Ingestion** (`internal/ingest`): strict parsing of nginx's syslog datagrams; every field
  required, unknown fields rejected, ranges checked; malformed records and queue drops are counted,
  never silently discarded. The reader never blocks.
- **Aggregation** (`internal/aggregate`, `internal/series`): one goroutine owns all state; per minute
  and per (class, observer, agent) it keeps outcome and status counts, an exact latency-threshold
  count and a latency histogram. Late records merge into stored minutes.
- **Histograms** (`internal/histogram`): base-2 exponential buckets (OpenTelemetry layout) with
  bucket indices computed by exact integer arithmetic; merge is commutative and associative; nearest-
  rank quantiles are bounded to a bucket interval. Percentiles are never averaged.
- **Storage** (`internal/storage`): SQLite in WAL mode via a pure-Go driver; migrations, size cap,
  retention, recovery by replacement.
- **Reconciliation** (`internal/reconcile`): nginx's request counter against records received.
- **SLIs and statistics** (`internal/sli`, `internal/stats`): definitions, Wilson intervals, the exact
  zero-failure bound, error budgets.
- **Reports** (`internal/report`): `ops-rollup-v1` days and `ops-summary-v1` summaries — raw hourly
  observations plus every derived value — and `Recompute`, which rebuilds and compares them.
- **Probe** (`internal/probe`): the external checks.
- **Releases** (`internal/release`, `internal/sigverify`): release manifest and deployment record;
  Sigstore bundle verification with sigstore-go plus a policy on the signer's GitHub claims
  (repository, workflow, ref, commit, runner environment, trigger).

## Commands

```
ledger serve                     the collector and its API (configured by environment, below)
ledger probe -target URL         one external probe run → probe-run-v1 JSON
ledger day -db FILE -date D      one day's origin report from a database (offline)
ledger assemble -date D -origin FILE -probes DIR
                                 a daily report from the host's origin day and the probe runs
ledger summary -dir DIR -last D  the rolling summary (default 28 days)
ledger recompute DIR             verify every report under DIR; exit 1 on any discrepancy
ledger verify-bundle -artifact FILE [-bundle FILE] [-workflow PATH] [-commit SHA]
                                 verify a Sigstore bundle and its GitHub signer
ledger verify-release -manifest FILE -commit SHA -image svc=ref ... [-record-out FILE]
                                 verify a signed release manifest against the images being deployed
ledger healthcheck | version
```

Exit codes: 0 success, 1 failure or discrepancy, 2 usage or configuration error.

## Verify published reports yourself

The code, the external probe, the signed daily reports and the weekly evidence releases live in
the public repository [github.com/akynte/opsledger](https://github.com/akynte/opsledger), which is
mirrored from the site's repository. Reports are in `reports/YYYY/MM/YYYY-MM-DD.json`, the rolling
summary in `reports/summary.json`, each with its Sigstore bundle (`*.sigstore.json`).

```bash
git clone https://github.com/akynte/opsledger && cd opsledger
go run ./cmd/ledger recompute reports                     # every published value from its observations
go run ./cmd/ledger verify-bundle -artifact reports/summary.json \
  -repository akynte/opsledger -workflow .github/workflows/ops-publish.yml
curl -s https://aliakbari.dev/api/v1/ops/summary | jq '.origin.availability'   # the host's live view
```

## Automation (ADR-0019)

| Workflow (public repository) | When | What |
|---|---|---|
| `probe.yml` | every 5 minutes | external checks of https://aliakbari.dev; one record per run (artifact) |
| `ops-publish.yml` | daily, 00:20 UTC | assembles every missing day of the last week, rebuilds the summary, recomputes, signs, commits |
| `ci.yml` | on push, before releases | tests, fuzzing, vulnerabilities; recomputes every report and verifies every signature |
| `release.yml` | weekly, Mondays | runs CI at the commit, tags it, writes `evidence.lock.json`; MEASURED once the summary covers 14 days of origin and probe data |

The site's evidence sync pins each new release automatically and the site redeploys.

## API

Public, read-only, proxied by nginx ([openapi.json](openapi.json), a test keeps it in sync with the
handler): `/api/v1/ops/`, `/api/v1/ops/status`, `/api/v1/ops/summary`,
`/api/v1/ops/daily/{date}` (completed UTC days within retention only), `/api/v1/release`. GET and
HEAD only; query parameters and request bodies are rejected; responses carry ETags. Admin, never
proxied: `/healthz`, `/readyz` (fails if no write succeeded recently), `/metrics` (Prometheus text).

## Configuration (`ledger serve`)

| Variable | Default | |
|---|---|---|
| `LEDGER_SOCKET` | `/srv/ledger-socket/ingest.sock` | datagram socket nginx logs to (mode 0666 in a volume shared only with nginx) |
| `LEDGER_DB` | `/var/lib/ledger/ledger.db` | SQLite database |
| `LEDGER_HTTP_ADDR` / `LEDGER_ADMIN_ADDR` | `:8090` / `:9091` | public (via nginx) and admin listeners |
| `LEDGER_NGINX_STATUS_URL` | (off) | nginx `stub_status` for reconciliation |
| `LEDGER_RELEASE_FILE` | `/srv/release/release.json` | deployment record written by deploy.sh |
| `LEDGER_SERVICE` | `https://aliakbari.dev` | origin named in reports |
| `LEDGER_QUEUE` | `8192` | ingestion queue capacity |
| `LEDGER_RETENTION_DAYS` | `35` | minute data kept |
| `LEDGER_MAX_DB_MB` | `512` | database size cap |
| `LEDGER_FLUSH_EVERY` / `LEDGER_SAMPLE_EVERY` | `10s` / `1m` | write and reconciliation intervals |
| `SHUTDOWN_TIMEOUT`, `LOG_LEVEL`, `LOG_FORMAT` | `10s`, `info`, `json` | |

Invalid configuration exits with code 2 and lists every problem.

## Test

```bash
go test -race ./...                       # unit, property, integration, end-to-end serve test, Sigstore negative tests
go test -run '^$' -fuzz FuzzParseDatagram -fuzztime 30s ./internal/ingest
go test -run '^$' -fuzz FuzzRecordAndMerge -fuzztime 30s ./internal/histogram
go test -run '^$' -fuzz FuzzDecodeValidate -fuzztime 30s ./internal/histogram
go vet ./... && gofmt -l .
```

`testdata/fixturegen` generates the synthetic 15-day database from which
`packages/evidence/test/fixtures/ops-summary-15d.json` was built with the real pipeline.

## Limits

- The origin view is self-reported and blind to failures before nginx; the probe samples.
- Recomputation proves that statistics follow from observations, not that observations are true.
- Lab measurements (loss accounting, memory) are from one machine under synthetic load and are
  labelled as such wherever they appear.
