---
title: Private visitor analytics with self-hosted Umami on first-party paths
status: accepted
date: "2026-10-12"
tags: [privacy, analytics, security]
---

## Context
ADR-0009 kept the site free of analytics; `deploy.sh refs` counted `?ref=` codes from the access
log. For a job search that is too little: which sources (LinkedIn profile and posts, GitHub,
applications, search) bring visitors, which projects they read, how they move between pages, and
whether they reach the CV or contact details. Server logs miss what never reaches the origin (pages
served from the CDN edge or the browser cache), client-side navigation (Next.js fetches
payloads, not pages) and clicks on external links. Building an analytics product would duplicate
a mature one.

## Options
- **Server logs only (GoAccess, or opsledger).** No script; misses navigation, clicks and cached
  pages, and would put visitor data into operational evidence.
- **GoatCounter.** Small and written in Go; no journeys, funnels or UTM reports.
- **Plausible Community Edition.** Needs ClickHouse; "at least 2 GB of RAM is recommended".
- **Self-hosted Umami 3.4 (PostgreSQL), with its tracker and collection endpoint on this site's
  own origin and everything else private.**

## Decision
Umami, used as an unmodified component, behind the site's own nginx and firewall.
- **Public surface:** `/x/s.js` (tracker) and `POST /x/api/send` (collection) only; every other
  `/x/` path is 404, and POST is refused on every other path. When Umami is down or not enabled,
  both answer 204 and the page is unaffected. Same origin, so the CSP stays `'self'`.
- **Forgery and data minimisation:** Umami accepts client-supplied `ip`, `userAgent` and
  `timestamp` fields, has no rate limit on collection, and checks bots by user agent only. nginx
  rate-limits collection (5 events/s per address, burst 50; throttled events get a silent 204); ModSecurity refuses events that set
  `ip`, `userAgent`, `timestamp`, `browser`, `os`, `device` or a distinct `id`, or name another
  website or host; nginx sends Umami only the truncated address (IPv4 /24, IPv6 /48);
  `SKIP_LOCATION_HEADERS` makes Umami ignore client-suppliable location headers and use its bundled
  GeoLite2 database. Analytics remain approximate and forgeable in bulk by anyone willing to send
  fake traffic; they are never used as public evidence.
- **Browser:** no cookies. Global Privacy Control, Do Not Track and an opt-out on /privacy (one
  localStorage key) stop all events. `/site-analytics.js` turns clicks on recruiting-relevant
  links into named events (résumé, email, LinkedIn, GitHub profile or repository, project,
  demo, outbound). A click is not a completed action; résumé file requests are counted from the
  server's own log, privately.
- **Private access:** the UI listens on the host's loopback only (nginx :8091 →
  `127.0.0.1:8091`), reached through an SSH tunnel, behind Umami's login. The default admin
  password is replaced by `infra/analytics/setup.sh bootstrap`. No share URLs.
- **Isolation and retention:** Umami and its PostgreSQL run on internal networks with no internet
  route, read-only root filesystems and memory limits, only when the owner has created the secrets
  file (Compose profile `analytics`). A retention job deletes events, sessions and their data older
  than 365 days; Umami itself keeps everything.

## Consequences
- ADR-0009 is superseded. The site still sets no cookies and loads no third-party script; it now
  runs a first-party script, and the footer and /privacy say so.
- Analytics data is private and approximate: ad blockers, GPC and DNT remove visitors from it;
  sessions come from a hash of the truncated address, user agent and a monthly salt, so one person
  on two networks counts twice and several people behind one /24 can merge. It cannot identify
  people or employers, and is not used to try.
- Cookieless does not mean consent-free in every jurisdiction (EDPB Guidelines 2/2023 on Art. 5(3)
  ePrivacy). The design follows the conditions French guidance sets for audience measurement
  without consent (information, objection, purpose limited to audience measurement, no
  cross-referencing, truncated address); whether that suffices for a given visitor's jurisdiction is
  a legal question this ADR does not settle.
- Resources, measured on the local stack (not the production host): Umami 241 MiB resident at idle,
  297 MiB at peak while ingesting about 90 events/s (its 0.5-CPU limit was the bottleneck);
  PostgreSQL about 35 MiB; the Umami image is 1.47 GB on disk. Limits: 512 MiB and 0.5 CPU for
  Umami, 256 MiB for PostgreSQL. Production use must be measured after deployment.
- Third-party images are not patched here. On 2026-10-12 Trivy reported, for Umami 3.4.0 (the latest
  release), fixable HIGH/CRITICAL issues in Next.js (`next/og` ImageResponse, Image Optimization),
  OpenSSL (QUIC), shell-quote, sharp, undici, deepmerge-ts and mysql2; and for postgres:17-alpine, Go
  standard-library issues in `gosu`. None is on a path the internet can reach: nginx forwards only
  the tracker script and the collection endpoint, through the firewall; QUIC, image optimisation,
  MySQL and WebSockets are not used; `gosu` never runs because the database runs as its own
  non-root user. The owner-only UI exposes every Umami route, but only through SSH. CI reports these
  images on every run (report only), and the pins are updated when upstream releases fixes.
