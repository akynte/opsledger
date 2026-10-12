package release

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	commit = strings.Repeat("a", 40)
	images = map[string]string{
		"web":    "ghcr.io/akynte/aliakbari-web@sha256:" + strings.Repeat("1", 64),
		"api":    "ghcr.io/akynte/aliakbari-api@sha256:" + strings.Repeat("2", 64),
		"nginx":  "ghcr.io/akynte/aliakbari-nginx@sha256:" + strings.Repeat("3", 64),
		"ledger": "ghcr.io/akynte/aliakbari-ledger@sha256:" + strings.Repeat("4", 64),
	}
)

func manifest() Manifest {
	return Manifest{
		Schema: ManifestSchema, Repository: "akynte/portfolio", Commit: commit, Ref: "refs/heads/main",
		Workflow: ".github/workflows/ci.yml", RunID: "123", RunAttempt: "1",
		CreatedAt: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), Images: clone(images),
	}
}

func clone(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func encode(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestManifestRoundTripAndCheck(t *testing.T) {
	m, err := ParseManifest(encode(t, manifest()))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(Expectation{Repository: "akynte/portfolio", Commit: commit, Images: clone(images)}); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentMustMatchTheSignedManifest(t *testing.T) {
	m, _ := ParseManifest(encode(t, manifest()))
	cases := map[string]Expectation{
		"other repository": {Repository: "someone/portfolio", Commit: commit, Images: clone(images)},
		"other commit":     {Repository: "akynte/portfolio", Commit: strings.Repeat("b", 40), Images: clone(images)},
		"modified digest": func() Expectation {
			im := clone(images)
			im["web"] = "ghcr.io/akynte/aliakbari-web@sha256:" + strings.Repeat("9", 64)
			return Expectation{Repository: "akynte/portfolio", Commit: commit, Images: im}
		}(),
		"missing image": func() Expectation {
			im := clone(images)
			delete(im, "ledger")
			return Expectation{Repository: "akynte/portfolio", Commit: commit, Images: im}
		}(),
		"extra image": func() Expectation {
			im := clone(images)
			im["miner"] = "ghcr.io/akynte/aliakbari-miner@sha256:" + strings.Repeat("5", 64)
			return Expectation{Repository: "akynte/portfolio", Commit: commit, Images: im}
		}(),
		"tag instead of digest": func() Expectation {
			im := clone(images)
			im["api"] = "ghcr.io/akynte/aliakbari-api:" + commit
			return Expectation{Repository: "akynte/portfolio", Commit: commit, Images: im}
		}(),
	}
	for name, e := range cases {
		if err := m.Check(e); err == nil {
			t.Errorf("%s: deployment accepted", name)
		}
	}
}

func TestManifestValidation(t *testing.T) {
	mods := map[string]func(*Manifest){
		"schema":         func(m *Manifest) { m.Schema = "x" },
		"short commit":   func(m *Manifest) { m.Commit = "abc" },
		"ref":            func(m *Manifest) { m.Ref = "main" },
		"workflow":       func(m *Manifest) { m.Workflow = "ci.yml" },
		"tagged image":   func(m *Manifest) { m.Images["web"] = "ghcr.io/akynte/aliakbari-web:latest" },
		"swapped images": func(m *Manifest) { m.Images["web"], m.Images["api"] = m.Images["api"], m.Images["web"] },
		"other registry": func(m *Manifest) {
			m.Images["web"] = "docker.io/akynte/aliakbari-web@sha256:" + strings.Repeat("1", 64)
		},
		"missing service": func(m *Manifest) { delete(m.Images, "nginx") },
	}
	for name, mod := range mods {
		m := manifest()
		m.Images = clone(images)
		mod(&m)
		if _, err := ParseManifest(encode(t, m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	b := encode(t, manifest())
	if _, err := ParseManifest(append(b[:len(b)-1], []byte(`,"extra":1}`)...)); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestRecordValidation(t *testing.T) {
	good := Record{
		Schema: RecordSchema, ReleaseID: commit, Repository: "akynte/portfolio", Commit: commit,
		DeployedAt: time.Now().UTC(), Images: clone(images),
		Verification: Verification{Status: "verified", Method: "sigstore-bundle", ManifestSHA256: strings.Repeat("e", 64),
			SignerWorkflow: "https://github.com/akynte/portfolio/.github/workflows/ci.yml@refs/heads/main", VerifiedAt: time.Now().UTC()},
	}
	if _, err := ParseRecord(encode(t, good)); err != nil {
		t.Fatal(err)
	}
	unverified := good
	unverified.Verification = Verification{Status: "unverified", Method: "none"}
	if _, err := ParseRecord(encode(t, unverified)); err != nil {
		t.Fatalf("an honestly unverified record must parse: %v", err)
	}
	bad := good
	bad.Verification.ManifestSHA256 = ""
	if _, err := ParseRecord(encode(t, bad)); err == nil {
		t.Fatal("verified record without manifest digest accepted")
	}
	bad = good
	bad.ReleaseID = strings.Repeat("b", 40)
	if _, err := ParseRecord(encode(t, bad)); err == nil {
		t.Fatal("release id different from commit accepted")
	}
}
