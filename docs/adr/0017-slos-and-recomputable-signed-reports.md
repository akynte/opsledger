---
title: Service levels from two observers, published as signed daily reports that anyone can recompute
status: accepted
date: "2026-10-12"
tags: [sre, observability, verification, evidence]
---

## Context
A number like "99.9% available" says nothing without who observed it, what counted, how many
observations it rests on, and whether it can be checked. The origin sees every request that
reaches it but nothing that fails before it; an external probe sees the visitor's path but only
when it runs. GitHub's scheduler "can be delayed during periods of high loads", and while the
repository is private each probe run costs hosted minutes. A portfolio site has little traffic, so
many ratios rest on small samples.

## Options
- **Publish dashboards of percentages.** Easy to read and impossible to check.
- **Publish raw request logs.** Checkable, but they are visitor data.
- **Publish per-hour aggregates (counts and mergeable histograms) together with every statistic
  derived from them, sign them, and ship a verifier that recomputes the statistics.**

## Decision
The third option.
- **Two observers, never mixed.** *Origin* (self-reported: nginx → opsledger on the host) and
  *probe* (external: a scheduled workflow on GitHub-hosted runners that requests the home page,
  /review, /proof, the résumé PDF, the API index and snapshot, and checks content, snapshot
  consistency between pages and the API, and certificate validity of at least 14 days).
- **SLIs (sli version 1).** Origin availability: organic, non-bot page, navigation and API
  requests on the public server; 5xx is bad; firewall 403s, 429s and client disconnects (499) are
  excluded and reported; static files and images are reported but not part of the SLI. Origin
  latency: such requests that did not fail, within 300 ms (pages, navigation) or 100 ms (API) of
  nginx's `$request_time`, decided exactly at ingestion. Probe: a run succeeds only if every check
  does. Completeness: records received over nginx's own request count. Objectives over 28 days:
  99.5% availability, 99% latency, 99.5% probe runs, 99% completeness.
- **Statistics.** Every ratio is published with its n and a 95% Wilson score interval; with zero
  probe failures the exact one-sided bound 1 − 0.05^(1/n) is published as well. Percentiles are
  nearest-rank from merged histograms, published as a bucket interval (lower, upper] with a
  `reliable` flag that requires at least ten observations above the quantile; percentiles are never
  averaged. Missing probe slots are listed and count neither as success nor failure.
- **Reports.** `ops-rollup-v1` per UTC day: 24 hourly cells (counts, exact latency counts,
  histograms), ingestion and reconciliation counts, ledger run intervals, release events and probe
  runs (raw), plus every derived value. `ops-summary-v1` over 28 days, derived only from the daily
  files it lists by SHA-256. `ledger recompute DIR` validates schemas, checks raw consistency,
  recomputes every derived value with the same code that produced it and exits nonzero on any
  difference; tests cover tampered counts, intervals, percentile bounds, missing slots, coverage,
  probe results, windows, unknown fields and replaced daily files.
- **Publication.** Since ADR-0019, `ops-publish.yml` runs daily in the public repository
  github.com/akynte/opsledger: it fetches each completed day of the last week that has no report
  yet from the host, collects the day's probe runs, assembles and recomputes, signs the files with
  Sigstore as `akynte/opsledger/.github/workflows/ops-publish.yml@refs/heads/main`, verifies its
  own signatures, and commits them to `reports/`. The probe runs there every 5 minutes (hosted
  minutes are free for public repositories).

## Consequences
- A reviewer can recompute every published number from the published observations, and verify
  which workflow published them and when. No recomputation can show that the observations are
  true; the signature and the external probe narrow, not close, that gap, and the reports say so.
- At 5-minute intervals, 28 days of zero failures bound the failure rate below about 0.04% (8,064
  runs, if GitHub runs every slot); at 30-minute intervals it would be about 0.22% (1,344 runs),
  which does not support "99.9%".
- MEASURED claims (ADR-0008) can cite a signed summary from a pinned source once it covers at
  least 14 days of both origin and probe data and the environment is `deployed` (never
  `production`, which stays reserved for employment). The audit enforces these rules; the weekly
  release of the public repository promotes the claim once they hold (ADR-0019).
