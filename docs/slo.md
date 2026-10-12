# Service level indicators and objectives (sli version 1)

Implemented in `services/ledger/internal/sli`; decisions in ADR-0016 and ADR-0017. A change to any
definition below requires a new sli version, so that reports produced under different rules are
never combined (`BuildSummary` refuses to).

## Observers

| Observer | Kind | Sees | Cannot see |
|---|---|---|---|
| origin (`origin-nginx`) | self-reported | every request nginx handles, with its status and `$request_time` | anything that fails before nginx (DNS, CDN, network, host down); responses served from the CDN edge or browser cache |
| probe (`github-hosted-probe`) | external | the visitor's path through DNS, CDN and TLS, every 5 minutes (from github.com/akynte/opsledger) | anything between runs; other networks than GitHub's; runs GitHub delays or skips (listed as missing slots) |

## Request classes (origin)

| Class | Requests |
|---|---|
| `page` | HTML documents, including the résumé PDF (counted, never singled out publicly) |
| `rsc` | Next.js client navigation and prefetch payloads (`RSC: 1`) |
| `api` | `/api/*` (evidence API, ops and release endpoints) |
| `static`, `media` | `/_next/static/*` and `/site-analytics.js`, `/demos/*` — served from disk by nginx |
| `analytics` | `/x/*` (first-party analytics script and collection) |
| `wellknown`, `other` | `/.well-known/*`; anything else on the public server (non-GET/HEAD) |
| `internal`, `plainhttp`, `unknownhost` | firewall tier, health and status endpoints; port 80; other host names |

Every request also carries an **observer** (`probe` if it had a well-formed `X-Opsledger-Probe`
header, else `organic`) and an **agent** (`bot` if nginx's user-agent map matched or the user agent
was empty, else `human`).

## Indicators

| SLI | Eligible | Good | Bad | Excluded (reported separately) |
|---|---|---|---|---|
| Origin availability | organic, human, `page`/`rsc`/`api` | status < 500 (except below) | 5xx | 403 (firewall), 429 (rate limit), 499 (client went away) |
| Origin latency | availability-eligible and not 5xx | `$request_time` ≤ 300 ms (`page`, `rsc`) or ≤ 100 ms (`api`) | slower | — |
| Probe | every probe run | all eight checks succeed | any check fails | missing slots (neither good nor bad) |
| Completeness | requests counted by nginx (`stub_status`) over reconciled intervals | a record reached the ledger | lost in transit | intervals spanning a restart of nginx or the ledger (reported as unreconciled seconds) |
| Coverage | the window | seconds a ledger run was alive (heartbeats; gaps < 60 s are jitter) | gaps ≥ 60 s | — |

Why these choices:
- **Bots and probes** are not users; probes have their own SLI.
- **403/429** are deliberate policy; counting them as failures would punish the firewall for working.
  They are visible in the reports, so a misconfigured rule shows up as a spike in `excluded`.
- **499** means the client gave up. If the server was slow, the latency SLI shows it.
- **Static files and images** are served from local disk and mostly from the CDN; they are reported
  but not part of the SLI.
- **Latency is decided at ingestion** with the exact threshold, not estimated from histograms.
- **`$request_time`** runs until nginx has sent the last byte to the CDN; it is not a page-load time.

## Objectives (28 days)

| SLI | Target |
|---|---|
| Origin availability | 99.5% |
| Origin latency | 99% |
| Probe runs | 99.5% |
| Completeness | 99% |

Error budget: `allowed = (1 − target) × eligible`; `consumed = bad / allowed`. With few eligible
events a single failure can consume the budget many times over; budgets are always shown with their
event counts.

## Statistics

- **Ratios** carry n and a 95% Wilson score interval (observations treated as independent trials).
- **Zero failures:** the exact one-sided 95% upper bound on the failure rate, 1 − 0.05^(1/n), is
  published. 8,064 probe runs (28 days at 5 minutes) bound it below about 0.04%; 1,344 runs (at 30
  minutes) would only bound it below about 0.22%. A probe success ratio is not an uptime percentage.
- **Percentiles** are nearest-rank, located in a histogram bucket and published as (lower, upper] in
  microseconds, never averaged, and flagged `reliable` only with at least ten observations above the
  quantile (p50: n ≥ 20, p95: n ≥ 200, p99: n ≥ 1,000).
- **Rounding:** ratios to 9 decimals; the verifier allows one unit in the last place.
