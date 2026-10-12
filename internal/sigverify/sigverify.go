// Package sigverify verifies Sigstore bundles produced by keyless signing
// in GitHub Actions, and applies a policy to the verified signer.
//
// Verification happens in two layers:
//
//  1. sigstore-go checks the bundle: the signature over the artifact, the
//     certificate chain up to the trusted Fulcio CA, the transparency-log
//     entry and its timestamps, and that the certificate's subject (the
//     signing workflow) and OIDC issuer are the expected ones.
//  2. Policy checks the GitHub-specific claims in the verified certificate:
//     source repository, commit, ref, runner environment and trigger.
//
// Layer 2 only ever reads a certificate that layer 1 has verified. A valid
// signature proves which workflow, at which commit, signed which bytes. It
// does not prove that what those bytes say is true.
package sigverify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// GitHubIssuer is the OIDC issuer of GitHub Actions tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// Policy is what the signer must be.
type Policy struct {
	Repository   string // owner/name
	WorkflowPath string // .github/workflows/ci.yml
	Ref          string // refs/heads/main
	// Commit, if set, must equal the certificate's source repository digest.
	Commit string
	// RequireGitHubHosted rejects signatures made on self-hosted runners.
	RequireGitHubHosted bool
	// Triggers, if set, lists the allowed workflow triggers (push, schedule, ...).
	Triggers []string
}

// SAN is the certificate subject that keyless signing in this workflow produces.
func (p Policy) SAN() string {
	return fmt.Sprintf("https://github.com/%s/%s@%s", p.Repository, p.WorkflowPath, p.Ref)
}

func (p Policy) validate() error {
	switch {
	case !validRepo(p.Repository):
		return fmt.Errorf("policy: repository %q is not owner/name", p.Repository)
	case !strings.HasPrefix(p.WorkflowPath, ".github/workflows/") || strings.Contains(p.WorkflowPath, ".."):
		return fmt.Errorf("policy: workflow path %q", p.WorkflowPath)
	case !strings.HasPrefix(p.Ref, "refs/"):
		return fmt.Errorf("policy: ref %q", p.Ref)
	case p.Commit != "" && !isHex(p.Commit, 40):
		return fmt.Errorf("policy: commit %q is not a full SHA", p.Commit)
	}
	return nil
}

// Result describes a successful verification.
type Result struct {
	ArtifactSHA256    string    `json:"artifact_sha256"`
	SignerWorkflow    string    `json:"signer_workflow"`
	Issuer            string    `json:"issuer"`
	Repository        string    `json:"repository"`
	Commit            string    `json:"commit"`
	Ref               string    `json:"ref"`
	Trigger           string    `json:"trigger"`
	RunnerEnvironment string    `json:"runner_environment"`
	RunURL            string    `json:"run_url"`
	SignedAt          time.Time `json:"signed_at"`
}

// Options tune the cryptographic checks.
type Options struct {
	// RequireSCT requires a signed certificate timestamp from a trusted CT log
	// (public-good Fulcio certificates have one). Tests with a virtual CA disable it.
	RequireSCT bool
}

// ErrPolicy wraps policy failures, as opposed to cryptographic ones.
var ErrPolicy = errors.New("signer does not match policy")

// Verify checks entity as a signature over artifact by a signer matching p.
func Verify(trusted root.TrustedMaterial, entity verify.SignedEntity, artifact []byte, p Policy, opts Options) (*Result, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	vopts := []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	if opts.RequireSCT {
		vopts = append(vopts, verify.WithSignedCertificateTimestamps(1))
	}
	v, err := verify.NewVerifier(trusted, vopts...)
	if err != nil {
		return nil, fmt.Errorf("verifier: %w", err)
	}
	id, err := verify.NewShortCertificateIdentity(GitHubIssuer, "", p.SAN(), "")
	if err != nil {
		return nil, err
	}
	res, err := v.Verify(entity, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(artifact)), verify.WithCertificateIdentity(id)))
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return nil, errors.New("signature: no verified certificate (key-based signatures are not accepted)")
	}
	r, err := CheckClaims(*res.Signature.Certificate, p)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(artifact)
	r.ArtifactSHA256 = hex.EncodeToString(sum[:])
	for _, ts := range res.VerifiedTimestamps {
		if r.SignedAt.IsZero() || ts.Timestamp.Before(r.SignedAt) {
			r.SignedAt = ts.Timestamp.UTC()
		}
	}
	return r, nil
}

// CheckClaims applies the GitHub-specific part of the policy to a
// certificate summary that has already been cryptographically verified.
func CheckClaims(c certificate.Summary, p Policy) (*Result, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	e := c.Extensions
	fail := func(format string, args ...any) (*Result, error) {
		return nil, fmt.Errorf("%w: %s", ErrPolicy, fmt.Sprintf(format, args...))
	}
	if c.SubjectAlternativeName != p.SAN() {
		return fail("signed by %q, want %q", c.SubjectAlternativeName, p.SAN())
	}
	if e.Issuer != GitHubIssuer {
		return fail("issuer %q, want %q", e.Issuer, GitHubIssuer)
	}
	if want := "https://github.com/" + p.Repository; e.SourceRepositoryURI != want {
		return fail("source repository %q, want %q", e.SourceRepositoryURI, want)
	}
	if e.SourceRepositoryRef != p.Ref {
		return fail("source ref %q, want %q", e.SourceRepositoryRef, p.Ref)
	}
	if !isHex(e.SourceRepositoryDigest, 40) {
		return fail("certificate has no source commit")
	}
	if p.Commit != "" && e.SourceRepositoryDigest != p.Commit {
		return fail("signed at commit %s, want %s", e.SourceRepositoryDigest, p.Commit)
	}
	if p.RequireGitHubHosted && e.RunnerEnvironment != "github-hosted" {
		return fail("runner environment %q: only GitHub-hosted runners are trusted to sign", e.RunnerEnvironment)
	}
	if len(p.Triggers) > 0 && !slices.Contains(p.Triggers, e.BuildTrigger) {
		return fail("trigger %q not in %v", e.BuildTrigger, p.Triggers)
	}
	return &Result{
		SignerWorkflow:    c.SubjectAlternativeName,
		Issuer:            e.Issuer,
		Repository:        p.Repository,
		Commit:            e.SourceRepositoryDigest,
		Ref:               e.SourceRepositoryRef,
		Trigger:           e.BuildTrigger,
		RunnerEnvironment: e.RunnerEnvironment,
		RunURL:            e.RunInvocationURI,
	}, nil
}

// LoadBundle reads a Sigstore bundle (protobuf JSON, as written by
// `cosign sign-blob --bundle ... --new-bundle-format`).
func LoadBundle(path string) (*bundle.Bundle, error) {
	return bundle.LoadJSONFromPath(path)
}

// TrustedRoot loads trusted material: from a trusted_root.json file if
// path is set, otherwise from the Sigstore public-good TUF repository
// (network; cached in cacheDir, or in memory only if cacheDir is "").
func TrustedRoot(ctx context.Context, path, cacheDir string) (root.TrustedMaterial, error) {
	if path != "" {
		return root.NewTrustedRootFromPath(path)
	}
	opts := tuf.DefaultOptions().WithContext(ctx)
	if cacheDir == "" {
		opts = opts.WithDisableLocalCache()
	} else {
		opts = opts.WithCachePath(cacheDir)
	}
	return root.FetchTrustedRootWithOptions(opts)
}

// ReadFileLimited reads a file of at most limit bytes.
func ReadFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", path, limit)
	}
	return b, nil
}

func validRepo(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && owner != "" && name != "" && !strings.ContainsAny(s, " @:") && strings.Count(s, "/") == 1
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
