package sigverify

import (
	"errors"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

var policy = Policy{
	Repository: "akynte/portfolio", WorkflowPath: ".github/workflows/ci.yml", Ref: "refs/heads/main",
	Commit: strings.Repeat("a", 40), RequireGitHubHosted: true, Triggers: []string{"push", "workflow_dispatch"},
}

func goodSummary() certificate.Summary {
	return certificate.Summary{
		SubjectAlternativeName: policy.SAN(),
		Extensions: certificate.Extensions{
			Issuer:                 GitHubIssuer,
			SourceRepositoryURI:    "https://github.com/akynte/portfolio",
			SourceRepositoryRef:    "refs/heads/main",
			SourceRepositoryDigest: strings.Repeat("a", 40),
			RunnerEnvironment:      "github-hosted",
			BuildTrigger:           "push",
			RunInvocationURI:       "https://github.com/akynte/portfolio/actions/runs/1/attempts/1",
		},
	}
}

func TestClaimsAccepted(t *testing.T) {
	r, err := CheckClaims(goodSummary(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != policy.Commit || r.RunnerEnvironment != "github-hosted" {
		t.Fatalf("%+v", r)
	}
}

func TestClaimsRejected(t *testing.T) {
	cases := map[string]func(*certificate.Summary){
		"other workflow": func(s *certificate.Summary) {
			s.SubjectAlternativeName = "https://github.com/akynte/portfolio/.github/workflows/evil.yml@refs/heads/main"
		},
		"other branch": func(s *certificate.Summary) {
			s.SubjectAlternativeName = "https://github.com/akynte/portfolio/.github/workflows/ci.yml@refs/heads/feature"
		},
		"other repository": func(s *certificate.Summary) {
			s.Extensions.SourceRepositoryURI = "https://github.com/someone/portfolio"
		},
		"fork": func(s *certificate.Summary) {
			s.Extensions.SourceRepositoryURI = "https://github.com/akynte/portfolio-fork"
		},
		"other ref":         func(s *certificate.Summary) { s.Extensions.SourceRepositoryRef = "refs/pull/1/merge" },
		"other commit":      func(s *certificate.Summary) { s.Extensions.SourceRepositoryDigest = strings.Repeat("b", 40) },
		"no commit":         func(s *certificate.Summary) { s.Extensions.SourceRepositoryDigest = "" },
		"self-hosted":       func(s *certificate.Summary) { s.Extensions.RunnerEnvironment = "self-hosted" },
		"no runner env":     func(s *certificate.Summary) { s.Extensions.RunnerEnvironment = "" },
		"pull request":      func(s *certificate.Summary) { s.Extensions.BuildTrigger = "pull_request_target" },
		"other OIDC issuer": func(s *certificate.Summary) { s.Extensions.Issuer = "https://accounts.google.com" },
	}
	for name, mut := range cases {
		s := goodSummary()
		mut(&s)
		if _, err := CheckClaims(s, policy); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: got %v, want a policy rejection", name, err)
		}
	}
}

func TestPolicyValidation(t *testing.T) {
	for _, p := range []Policy{
		{Repository: "akynte", WorkflowPath: ".github/workflows/ci.yml", Ref: "refs/heads/main"},
		{Repository: "a/b", WorkflowPath: "ci.yml", Ref: "refs/heads/main"},
		{Repository: "a/b", WorkflowPath: ".github/workflows/../x.yml", Ref: "refs/heads/main"},
		{Repository: "a/b", WorkflowPath: ".github/workflows/ci.yml", Ref: "main"},
		{Repository: "a/b", WorkflowPath: ".github/workflows/ci.yml", Ref: "refs/heads/main", Commit: "abc"},
	} {
		if _, err := CheckClaims(goodSummary(), p); err == nil {
			t.Errorf("invalid policy accepted: %+v", p)
		}
	}
}

// End to end against a virtual Sigstore (CA, transparency log, TSA): the
// cryptographic layer must accept a correct signature and reject a wrong
// artifact, a wrong signer, a wrong issuer and an untrusted CA; and the
// claims layer must run on the verified certificate.
func TestCryptographicVerification(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte(`{"commit":"aaaa"}`)
	p := policy
	p.Commit, p.RequireGitHubHosted, p.Triggers = "", false, nil

	good, err := vs.Sign(p.SAN(), GitHubIssuer, artifact)
	if err != nil {
		t.Fatal(err)
	}
	// The virtual CA cannot add GitHub's certificate extensions, so a correct signature
	// passes the cryptographic layer and is then rejected by the claims layer. That
	// rejection proves the claims are read from the verified certificate.
	_, err = Verify(vs, good, artifact, p, Options{})
	if !errors.Is(err, ErrPolicy) || !strings.Contains(err.Error(), "source repository") {
		t.Fatalf("expected the claims check to run after a valid signature, got %v", err)
	}

	cryptoFailures := map[string]func() error{
		"modified artifact": func() error {
			_, err := Verify(vs, good, []byte(`{"commit":"bbbb"}`), p, Options{})
			return err
		},
		"other signer": func() error {
			e, _ := vs.Sign("https://github.com/akynte/portfolio/.github/workflows/evil.yml@refs/heads/main", GitHubIssuer, artifact)
			_, err := Verify(vs, e, artifact, p, Options{})
			return err
		},
		"other issuer": func() error {
			e, _ := vs.Sign(p.SAN(), "https://issuer.example", artifact)
			_, err := Verify(vs, e, artifact, p, Options{})
			return err
		},
		"untrusted CA": func() error {
			other, _ := ca.NewVirtualSigstore()
			e, _ := other.Sign(p.SAN(), GitHubIssuer, artifact)
			_, err := Verify(vs, e, artifact, p, Options{})
			return err
		},
	}
	for name, f := range cryptoFailures {
		err := f()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if errors.Is(err, ErrPolicy) {
			t.Errorf("%s: must fail cryptographic verification, not only the claims check: %v", name, err)
		}
	}
}
