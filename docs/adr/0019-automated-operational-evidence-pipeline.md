---
title: Automated operational evidence pipeline with a public opsledger repository
status: accepted
date: "2026-10-12"
tags: [automation, evidence, operations, security]
---

## Context
opsledger was deployed on 2026-10-12, but turning its observations into a MEASURED claim needed
an owner at every step: the reports were signed into a branch of this private repository, which a
reviewer cannot read; the claim had to be promoted by hand once 14 days had accumulated; the
evidence source had to be pinned through a reviewed pull request; and every change to `deploy.sh`
had to be copied to the host by hand. The probe ran every 30 minutes because hosted minutes cost
money in a private repository. The owner will not be available to do any of this.

## Decision
- **Public repository `akynte/opsledger`.** `opsledger-mirror.yml` publishes `services/ledger`
  (module path rewritten to `github.com/akynte/opsledger`), its ADRs and documents, and the
  workflows in `services/ledger/publish/` on every change, with a deploy key that can write only to
  that repository. The site's own repository stays private (ADR-0013); nothing in the mirror is
  secret, and the published reports contain the same aggregates `/api/v1/ops` already serves.
- **Probe and reports move there.** `probe.yml` runs every 5 minutes (free hosted minutes);
  `ops-publish.yml` signs reports as `akynte/opsledger/.github/workflows/ops-publish.yml@refs/heads/main`
  and assembles every day of the last week without a report, so a failed run repairs itself the
  next day (within the 8-day probe artifact and 35-day ledger retention). Its CI recomputes every
  report and verifies every signature on every push.
- **Weekly evidence release.** `release.yml` (Mondays) runs CI at the commit, tags it and writes
  `evidence.lock.json`. `opsledger-measurement` is TESTED · DEPLOYED until the signed summary at
  that commit covers ≥ 14 days of both origin and probe data; then the release cites the summary as
  `ops_reports` and the claim becomes MEASURED. The rule is code in the release script, not a
  person's judgement, and the site's audit (`E_OPS`) re-checks it independently.
- **Automatic pinning, limited to opsledger.** On schedule, `evidence-sync` advances the
  `opsledger` source to its latest release, re-verifies it against GitHub (tag, commit, CI run, test
  names, vendored files), runs the full audit and build, and pushes the result to `main` with a
  deploy key (so CI and the deploy run). Any failure leaves `main` untouched. Other sources still
  go through a pull request: their claims are written by hand.
- **The release manager updates itself.** The nginx image carries `deploy.sh`; after a verified
  deploy, the host's copy is replaced by the one inside the signed image (the previous copy is kept
  in `state/deploy.sh.previous`). No unsigned code reaches the host this way.

## Consequences
- After a merge, nothing needs an owner: code, probes, reports, promotion, pinning, deploy and the
  release manager all advance by themselves, and each step fails closed.
- A push to `main` without review is limited to one source's lock entry and vendored files that
  passed the same audit as a reviewed PR; CI still gates the deploy.
- Two deploy keys exist (write to akynte/opsledger; write to akynte/portfolio). They are stored
  only as Actions secrets and can be revoked in the repository settings.
- GitHub disables schedules after 60 days without activity; the daily report commit and the weekly
  pin commit keep both repositories active.
- Hosted minutes in the private repository drop (the probe no longer runs there); a weekly deploy is
  the main recurring cost.
