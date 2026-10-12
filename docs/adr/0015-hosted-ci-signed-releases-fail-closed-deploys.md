---
title: Builds and tests on GitHub-hosted runners; releases signed with Sigstore; deploys verify before they change anything
status: accepted
date: "2026-10-12"
tags: [ci-cd, security, supply-chain]
---

## Context
Since ADR-0010 every workflow ran on the self-hosted runner on the production host. Any code a
pull request brings (build scripts, test code, dependencies) therefore executed as `deployer`,
whose Docker access is equivalent to root on the machine that serves the site. Pull requests were
limited to this repository, so only people with write access could trigger it, but a compromised
dependency or a mistaken change would run on production all the same. Separately, nothing tied a
running image to the commit and the CI run that tested it: deploy.sh trusted whatever the commit
tag in GHCR pointed to.

GitHub's artifact attestations would answer the second problem, but they need a public repository
or GitHub Enterprise Cloud ("To use artifact attestations in private or internal repositories, you
must be on a GitHub Enterprise Cloud plan", actions/attest README); this repository is private.
Sigstore keyless signing with the GitHub Actions OIDC token works for any repository.

## Options
- **Keep everything on the self-hosted runner.** Cheapest in hosted minutes; pull-request code keeps running on production.
- **Hosted runners for pull requests only, self-hosted for pushes to main.** Fewer minutes, but the images that are deployed would be built on the production host itself, and signing there would make the signature say "self-hosted".
- **Every build and test on GitHub-hosted runners; a separate main-only job signs the tested digests; the production runner only deploys, and deploy.sh verifies the signature with a pinned verifier before it changes anything.**

## Decision
The third option.
- `ci.yml`: all build and test jobs run on `ubuntu-24.04` with read-only tokens. `stack` builds the
  four images once, tests them end to end (including the deploy script's negative tests) and, for
  pushes to main only, pushes exactly those images.
- `release` (main only, after every test passed) is the only job that can mint an OIDC token. It
  checks the tags still point at the tested digests, writes `release.json` (release-manifest-v1:
  repository, commit, ref, workflow, run, the four image digests), signs it with `cosign sign-blob`
  (keyless, public-good Sigstore, recorded in the public transparency log), self-tests the
  signature with the same verifier deploy.sh uses — including refusals for a wrong commit,
  workflow, repository, ref, trigger, digest and a tampered manifest — and publishes it as the
  scratch image `aliakbari-release:<sha>`. SBOMs (SPDX, syft) are attested to each image with
  cosign. When the repository is public, GitHub build-provenance attestations are added as well.
- `deploy.yml`: the gate runs on a hosted runner and verifies the signed release before the
  production runner is involved. On the host, `deploy.sh` verifies it again with a verifier image
  pinned by digest in `/srv/aliakbari/verifier.ref` (outside any release): Sigstore signature and
  transparency-log entry, signer `ci.yml@refs/heads/main` of this repository on a GitHub-hosted
  runner, trigger `push` or `workflow_dispatch`, the certificate's source commit equal to the
  manifest's and to the commit being deployed, and every image digest the commit tags resolve to
  equal to the signed one. Any failure refuses the release before anything changes. After a
  verified deploy the pin advances to that release's own ledger image (verified by its
  predecessor). The first pin is set by hand (`deploy.sh trust-verifier`). Verification can only
  be switched off by an operator setting `DEPLOY_ALLOW_UNSIGNED=1`, and the deployment record then
  says "unverified".
- CodeQL and the evidence sync also moved to hosted runners. Only the deploy step uses the
  self-hosted runner.

## Consequences
- Pull-request code never runs on the production host. The production runner executes only the
  deploy job of a commit that is on main, passed CI on a push, and whose release CI signed.
- Anyone can check the release: `cosign verify-blob` on `release.json` with its bundle, and
  compare the digests with `/api/v1/release`. While the repository is private, the manifest image
  is a private package; the signature itself is in the public transparency log.
- The signature proves which workflow, at which commit, vouched for which digests. It does not
  prove the host runs them: `/api/v1/release` is the host's own statement and says so.
- Hosted minutes cost money while the repository is private (GitHub Free includes a monthly
  allowance; a full CI run uses several runner-minutes per job). Public repositories run hosted
  jobs at no charge.
- The deploy job still runs on a runner attached to this repository. When the repository becomes
  public, move it to a private deployment repository that only holds the deploy workflow and is
  triggered by `repository_dispatch` (templates/deploy-repo/), so the public repository has no
  self-hosted runner at all. That move, publishing the repository, and setting the first verifier
  pin are owner actions.
- Negative tests: `infra/dev/test-deploy.sh` (no pinned verifier, missing manifest, bad signature,
  re-pointed tag, verification switched off without consent; each must be refused for the expected
  reason), `services/ledger/internal/sigverify` and `internal/release` (wrong workflow, branch,
  repository, ref, commit, runner, trigger, issuer, untrusted CA, modified artifact, swapped or
  tagged digests), and the real-Sigstore refusals in the `release` job.
