// Package release defines the two release documents:
//
//   - the manifest (release-manifest-v1), written and signed by CI on a
//     GitHub-hosted runner after the images passed every test. It binds a
//     commit to the exact image digests that were tested.
//   - the deployment record (release-v1), written by deploy.sh on the
//     production host after it verified the manifest's signature and pinned
//     the images by digest. opsledger serves it at /api/v1/release.
//
// The manifest is evidence anyone can verify (signature, transparency log).
// The deployment record is the host's own statement of what it runs: a
// reviewer can verify that the digests it names were built and signed by
// CI, but not, from outside, that the host really runs them.
package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Schema URLs.
const (
	ManifestSchema = "https://aliakbari.dev/schemas/release-manifest-v1.json"
	RecordSchema   = "https://aliakbari.dev/schemas/release-v1.json"
)

// Services are the images every release contains.
var Services = []string{"web", "api", "nginx", "ledger"}

var (
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageRE  = regexp.MustCompile(`^ghcr\.io/[a-z0-9-]+/aliakbari-([a-z]+)@sha256:[0-9a-f]{64}$`)
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Manifest is the signed statement CI makes about a release.
type Manifest struct {
	Schema     string            `json:"schema"`
	Repository string            `json:"repository"`
	Commit     string            `json:"commit"`
	Ref        string            `json:"ref"`
	Workflow   string            `json:"workflow"`
	RunID      string            `json:"run_id"`
	RunAttempt string            `json:"run_attempt"`
	CreatedAt  time.Time         `json:"created_at"`
	Images     map[string]string `json:"images"`
}

// ParseManifest decodes and validates a manifest strictly.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := decodeStrict(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Schema != ManifestSchema {
		return nil, fmt.Errorf("manifest: schema %q", m.Schema)
	}
	if !repoRE.MatchString(m.Repository) || !shaRE.MatchString(m.Commit) || !strings.HasPrefix(m.Ref, "refs/") ||
		!strings.HasPrefix(m.Workflow, ".github/workflows/") || m.RunID == "" || m.CreatedAt.IsZero() {
		return nil, errors.New("manifest: missing or malformed field")
	}
	if err := checkImages(m.Images); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

func checkImages(images map[string]string) error {
	if len(images) != len(Services) {
		return fmt.Errorf("expected images %v, got %d", Services, len(images))
	}
	for _, svc := range Services {
		ref, ok := images[svc]
		if !ok {
			return fmt.Errorf("image %q missing", svc)
		}
		m := imageRE.FindStringSubmatch(ref)
		if m == nil || m[1] != svc {
			return fmt.Errorf("image %q: %q is not ghcr.io/<owner>/aliakbari-%s@sha256:<digest>", svc, ref, svc)
		}
	}
	return nil
}

// Expectation is what the deployment is about to run.
type Expectation struct {
	Repository string
	Commit     string
	Images     map[string]string // service → registry@sha256:digest, as resolved by the deployer
}

// Check compares a (signature-verified) manifest with what is about to be
// deployed. Every image the deployer resolved must be exactly the digest
// CI signed: a re-tagged or re-pushed image is rejected.
func (m *Manifest) Check(e Expectation) error {
	if m.Repository != e.Repository {
		return fmt.Errorf("manifest is for %s, deploying %s", m.Repository, e.Repository)
	}
	if m.Commit != e.Commit {
		return fmt.Errorf("manifest is for commit %s, deploying %s", m.Commit, e.Commit)
	}
	keys := make([]string, 0, len(e.Images))
	for k := range e.Images {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := slices.Clone(Services)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		return fmt.Errorf("deploying images %v, the release has %v", keys, want)
	}
	for _, svc := range Services {
		if m.Images[svc] != e.Images[svc] {
			return fmt.Errorf("image %s: deploying %s, CI signed %s", svc, e.Images[svc], m.Images[svc])
		}
	}
	return nil
}

// Record is the deployment record written by deploy.sh.
type Record struct {
	Schema       string            `json:"schema"`
	ReleaseID    string            `json:"release_id"`
	Repository   string            `json:"repository"`
	Commit       string            `json:"commit"`
	DeployedAt   time.Time         `json:"deployed_at"`
	Images       map[string]string `json:"images"`
	Verification Verification      `json:"verification"`
}

// Verification records how the deployer checked the release.
type Verification struct {
	Status         string    `json:"status"` // "verified" or "unverified"
	Method         string    `json:"method"` // "sigstore-bundle" or "none"
	ManifestSHA256 string    `json:"manifest_sha256"`
	SignerWorkflow string    `json:"signer_workflow"`
	RunURL         string    `json:"run_url"`
	VerifiedAt     time.Time `json:"verified_at"`
	SignedAt       time.Time `json:"signed_at"`
}

// ParseRecord decodes and validates a deployment record strictly.
func ParseRecord(b []byte) (*Record, error) {
	var r Record
	if err := decodeStrict(b, &r); err != nil {
		return nil, fmt.Errorf("release record: %w", err)
	}
	if r.Schema != RecordSchema {
		return nil, fmt.Errorf("release record: schema %q", r.Schema)
	}
	if !shaRE.MatchString(r.ReleaseID) || r.ReleaseID != r.Commit || !repoRE.MatchString(r.Repository) || r.DeployedAt.IsZero() {
		return nil, errors.New("release record: missing or malformed field")
	}
	if err := checkImages(r.Images); err != nil {
		return nil, fmt.Errorf("release record: %w", err)
	}
	v := r.Verification
	switch v.Status {
	case "verified":
		if v.Method != "sigstore-bundle" || !digestRE.MatchString(v.ManifestSHA256) || v.SignerWorkflow == "" || v.VerifiedAt.IsZero() {
			return nil, errors.New("release record: a verified release must name its manifest digest, signer and time")
		}
	case "unverified":
		if v.Method != "none" {
			return nil, errors.New("release record: an unverified release has method none")
		}
	default:
		return nil, fmt.Errorf("release record: verification status %q", v.Status)
	}
	return &r, nil
}

func decodeStrict(b []byte, v any) error {
	if len(b) > 64<<10 {
		return errors.New("document too large")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}
