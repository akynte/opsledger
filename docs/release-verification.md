# Verifying a release

Every release of aliakbari.dev is described by a manifest (`release.json`, schema
[release-manifest-v1](https://aliakbari.dev/schemas/release-manifest-v1.json)) that CI writes after
all tests have passed and signs with Sigstore, keyless, as `.github/workflows/ci.yml` on `main`. The
signature is recorded in Sigstore's public transparency log. The production host verifies it before
deploying (docs/adr/0015); you can verify it too.

## 1. What does the host say it runs?

```bash
curl -s https://aliakbari.dev/api/v1/release | jq '.release | {commit, deployed_at, images, verification}'
```

This is the host's own statement (`"reported_by": "host"`).

## 2. Was that commit's release signed by CI?

The manifest and its bundle ship in the image `ghcr.io/akynte/aliakbari-release:<commit>` (a private
package while the repository is private; the owner can share the two files):

```bash
cid=$(docker create ghcr.io/akynte/aliakbari-release:$COMMIT /none)
docker cp $cid:/release.json . && docker cp $cid:/release.json.sigstore.json . && docker rm $cid

cosign verify-blob --bundle release.json.sigstore.json \
  --certificate-identity https://github.com/akynte/portfolio/.github/workflows/ci.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-sha "$COMMIT" release.json
```

Or with the same verifier the deploy uses, which also requires a GitHub-hosted runner and an allowed
trigger and compares image digests:

```bash
cd services/ledger
go run ./cmd/ledger verify-release -manifest ../../release.json -commit "$COMMIT" \
  -triggers push,workflow_dispatch \
  $(curl -s https://aliakbari.dev/api/v1/release | jq -r '.release.images | to_entries[] | "-image \(.key)=\(.value)"')
```

## 3. What does this prove, and what not?

- Proven: the workflow `ci.yml` on `main` of `akynte/portfolio`, running on a GitHub-hosted runner at
  commit `$COMMIT`, after the test jobs in the same run succeeded, published exactly these image
  digests. Anyone who changes a digest, the commit or any byte of the manifest breaks the signature.
- Not proven: that the host is running these images. `/api/v1/release` is the host's statement. The
  external probe checks behaviour (pages, API, snapshot consistency), not image digests.

## Operator notes

- `deploy.sh` reads the verifier image from `/srv/aliakbari/verifier.ref`. Bootstrap (once, by hand,
  after reviewing the image): `deploy.sh trust-verifier ghcr.io/akynte/aliakbari-ledger@sha256:<digest>`.
  After each verified deploy the pin advances to that release's ledger image.
- To deploy without verification (emergencies only): `DEPLOY_VERIFY=off DEPLOY_ALLOW_UNSIGNED=1
  deploy.sh deploy <sha>`. The deployment record and `/api/v1/release` then say "unverified".
- SBOMs (SPDX) are attested to each image with cosign: `cosign verify-attestation --type spdxjson
  --certificate-identity ... ghcr.io/akynte/aliakbari-web@sha256:<digest>`. When the repository is
  public, GitHub build-provenance attestations are added (`gh attestation verify oci://... -R
  akynte/portfolio`).
