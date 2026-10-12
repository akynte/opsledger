# Security model and threat model: opsledger, releases, analytics

Scope: the additions of October 2026 (ADR-0015 to ADR-0018). The site's earlier controls — static
pages, hashed CSP with Trusted Types, ModSecurity + CRS, rate limits, non-root read-only containers,
internal networks — are described in ADR-0002, ADR-0007, ADR-0011 and ADR-0014 and still apply.

## Assets

| Asset | Why it matters |
|---|---|
| The production host | Serves the site; Docker access on it is root-equivalent |
| Release integrity | What runs must be what CI tested |
| Published operational evidence | Claims on the site rest on it |
| Visitor analytics data | Personal data in some jurisdictions; private by design |
| Secrets: analytics DB password and app secret, Umami admin password, GHCR token (per job) | Access to the above |

## Trust boundaries

1. **Internet → nginx (443).** Everything public enters here: TLS, method gate (POST only on
   `/x/api/send`), rate limits, then the firewall tier for anything that reaches an application.
2. **nginx → ledger** over a Unix datagram socket in a volume shared only by those two containers.
   The ledger trusts nothing in a record: strict parser, fixed field set, ranges checked. nginx maps
   every client-controlled value (method, RSC header, probe header, path length) before sending.
3. **nginx → Umami** on an internal network shared only by those two; **Umami → PostgreSQL** on
   another. Neither has an internet route.
4. **Owner → Umami UI** via `127.0.0.1:8091` on the host, reached only through SSH.
5. **GitHub Actions → production.** Build and test jobs run on GitHub-hosted runners. Only the
   deploy job runs on the self-hosted runner, and `deploy.sh` verifies a Sigstore-signed release
   manifest with a verifier pinned outside the release before changing anything.
6. **Reviewer → evidence.** Reports and releases are verified with public Sigstore infrastructure
   and the open-source verifier; nothing requires production credentials.

## Threats and mitigations

| Threat | Mitigation | Residual risk |
|---|---|---|
| Pull-request code runs on the production host | All build/test jobs on hosted runners; self-hosted runner only in `deploy.yml` (main, CI success, signed) | Workflow files on main are trusted; anyone with write access is trusted |
| A pushed or re-tagged image is deployed | deploy.sh pins digests from the signed manifest and refuses if tags resolve elsewhere (test: re-pointed tag) | — |
| Forged release manifest | Sigstore signature + transparency log; signer must be `ci.yml@refs/heads/main` of this repository on a GitHub-hosted runner, trigger push/dispatch, at the commit deployed (tests: wrong workflow, branch, repository, ref, commit, runner, trigger, issuer, untrusted CA, tampered manifest) | Compromise of the GitHub account or of Sigstore's public infrastructure |
| A malicious release ships a verifier that accepts anything | The verifier used is pinned in `/srv/aliakbari/verifier.ref`, outside the release; a new verifier is pinned only after its release was verified by the old one | The first pin is trust on first use, set by the owner |
| Verification switched off quietly | `DEPLOY_VERIFY=off` refused unless `DEPLOY_ALLOW_UNSIGNED=1` (operator, by hand); the record then says "unverified" and so does `/api/v1/release` | An operator can still choose to deploy unverified |
| A request hides itself from the measurements | nginx maps client values to fixed forms; malformed records are counted; nginx's own counter reconciles totals | A flood larger than the socket queue loses records — counted, not hidden |
| A forged `X-Opsledger-Probe` moves traffic out of the organic SLI | Only organic traffic forms the availability SLI; the probe SLI comes from the probe's own measurements, not from origin records | A visitor can exclude their own requests from the origin SLI, which only removes data |
| Host fabricates its own operational data | Signed reports bind the published bytes to the publishing workflow and time; the external probe is independent | The origin view is self-reported, and the reports say so |
| Report tampering after publication | Sigstore signature per file; `ledger recompute` detects inconsistent edits; summaries are bound to daily files by SHA-256 | — |
| Forged analytics events (fake IP, user agent, time, website, host, visitor id) | ModSecurity refuses events setting `ip`, `userAgent`, `timestamp`, `browser`, `os`, `device`, `id`, or another website or host; rate limit 5/s per address (burst 50) | Bulk fake traffic from many addresses remains possible; analytics are never used as evidence |
| Attacks through analytics fields (XSS, SQLi, RCE) | CRS inspects every field; for `url`, `referrer` and `title` only SQLi/RCE/RFI/protocol groups are excluded (measured false positives); XSS rules still apply; Umami uses parameterised queries and an escaping UI | — |
| Umami admin exposed | UI only on host loopback (SSH tunnel), Umami login, default password replaced by `setup.sh bootstrap`; no share URLs | A host compromise exposes analytics data |
| Known vulnerabilities in third-party images (Umami 3.4.0: Next.js, OpenSSL, sharp, …; postgres: Go stdlib in `gosu`) | Only `/x/s.js` and `POST /x/api/send` are reachable from the internet, through the firewall; the vulnerable features are not on those paths or not used (ADR-0018); internal networks without egress; read-only, non-root containers; CI reports the images on every run | The owner-only UI exposes all of Umami's routes; upstream fixes must be picked up by updating the pinned digests |
| Spoofed location headers | Umami runs with `SKIP_LOCATION_HEADERS`; nginx sends only the truncated address | — |
| Analytics or ledger failure takes the site down | nginx starts and serves without either; the tracker paths answer 204; deploys never wait on them | — |
| Secrets in the repository | Secrets live in `/srv/aliakbari/secrets/` (mode 0600) and in GitHub's per-job tokens; gitleaks scans the full history in CI | — |
| Public endpoints leak infrastructure detail | `/api/v1/ops/*` serves completed days only, aggregates only; `/api/v1/release` names image digests and the commit, which are already public in the transparency log; a test asserts it contains no paths, addresses or tokens | — |

## Privacy properties (by construction)

- The ledger never receives an address, user agent, query string or referrer.
- Umami receives only the truncated address (IPv4 /24, IPv6 /48) and stores none.
- No cookies; one localStorage key only if a visitor opts out.
- No attempt is made, and none is possible from this data, to identify people or employers.
