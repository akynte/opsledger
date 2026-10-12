---
title: opsledger measures the site from nginx's request stream, in a Go service with its own SQLite file
status: accepted
date: "2026-10-12"
tags: [observability, sre, backend, go]
---

## Context
The evidence model (ADR-0008) has a MEASURED level, but nothing on this site was measured: the
snapshot counted zero benchmarks and zero incidents, and the uptime workflow probed every 30
minutes and kept no record. A claim about how the site behaves needs observations of the running
system, kept long enough to compute rolling objectives, and published in a form a reviewer can
check. ADR-0004 rules out a database for data that never changes at runtime; observations are
exactly the data that does.

## Options
- **Prometheus (+ Grafana) with an nginx exporter.** Mature, but a time-series database and
  dashboards for a site of this size, percentiles computed by interpolation, and no way to publish
  data that a reviewer can recompute exactly.
- **Parse nginx's access log files.** No new transport, but log rotation, file positions after
  restarts and the Docker log driver all become the ledger's problem, and the visitor log holds
  addresses and query strings the ledger should never see.
- **nginx sends one minimal record per request to a small Go service over a Unix datagram socket;
  the service aggregates per minute into mergeable structures in SQLite.**

## Decision
The third option: `services/ledger` (opsledger).
- **Record.** nginx's `syslog:server=unix:` transport sends a JSON record per request — time,
  server block, method, a path capped at 192 bytes, status, `$request_time`, and three flags nginx
  computes itself (bot user agent, Next.js navigation, probe). No address, user agent, query string
  or referrer. Every client-controlled value is mapped to a fixed form in nginx first, so no
  request can make its own record unparseable and so vanish from the measurements. Requests that
  skip the visitor log (static files, health checks, the firewall tier) are still recorded.
- **Pipeline.** One goroutine reads the socket and never blocks: a bounded queue (8,192 records)
  feeds one aggregator goroutine that owns all state; when the queue is full, records are dropped
  and counted. The aggregator writes closed minutes every 10 seconds in one transaction; late
  records merge into stored minutes. Shutdown stops the HTTP servers, then the reader, then drains
  the queue and writes the partial minute.
- **Aggregates.** Per minute and per (class, observer, agent): outcome and status-class counts, an
  exact count of requests within the latency objective, and a base-2 exponential histogram of
  durations (OpenTelemetry bucketing, scale 3) whose bucket indices are computed with integer
  arithmetic, so merging and percentiles are deterministic on every machine.
- **Completeness.** Once a minute the ledger reads nginx's own request counter (`stub_status`, on
  a private port) together with its received-record counter. Over a day, the difference is the
  number of records lost in transit, measured independently of the ledger's own counters. Ledger
  run intervals (heartbeats) give coverage.
- **Storage.** SQLite in WAL mode (pure-Go driver, so the image stays CGO-free and distroless):
  schema migrations by `user_version`, a size cap (`max_page_count`, 512 MiB), hourly pruning
  (35 days), and `quick_check` at start; a corrupt or newer-version file is moved aside and a new
  one created, and the run is recorded with that note so reports show the gap. There is no backup:
  published daily reports are the durable record, so losing the file loses only unpublished days.
- **API.** Read-only and public through nginx: `/api/v1/ops/{,status,summary,daily/{date}}` and
  `/api/v1/release`. Only completed UTC days are served; the current day never is. Admin
  (`/healthz`, `/readyz`, `/metrics`) is on a port that is never proxied.
- **Isolation.** The site never depends on the ledger. nginx starts and serves without its socket
  (records are dropped, an alert is logged), deploy.sh starts the ledger first but never waits on
  it, and a failing ledger cannot roll back a deployment of the site.

## Consequences
- Lab measurement (Docker Desktop, 16 CPUs, Linux; not the production host): with the kernel's
  default of 10 queued datagrams per Unix socket, a burst of 200,000 requests at about 130,000
  requests/s lost 1,603 records, and the reconciliation reported exactly 1,603 missing — matching
  the 1,603 "send() to syslog failed" warnings nginx logged. With `net.unix.max_dgram_qlen` 512 (set
  per container), three runs of the same burst at 136,000–144,000 requests/s lost none. The ledger's
  resident memory stayed at 16–20 MiB. Production traffic is orders of magnitude lower; these
  numbers bound behaviour, they do not describe production.
- ADR-0004 is amended: the ledger keeps runtime state in its own SQLite file, with the migrations,
  limits and recovery above. The site's own content still has no database.
- ADR-0003 is amended: opsledger is a second Go service at `/api/v1/ops` and `/api/v1/release`;
  the evidence API itself is unchanged.
- The origin view is self-reported and blind to anything that stops requests before nginx; the
  external probe (ADR-0017) is the independent check.
