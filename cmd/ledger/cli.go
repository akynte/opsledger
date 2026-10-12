package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/akynte/opsledger/internal/probe"
	"github.com/akynte/opsledger/internal/release"
	"github.com/akynte/opsledger/internal/report"
	"github.com/akynte/opsledger/internal/sigverify"
	"github.com/akynte/opsledger/internal/storage"
)

func generator() report.Generator {
	return report.Generator{Name: "opsledger", Version: version, Commit: commitOrUnknown()}
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("ledger "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// writeOut writes b to path atomically, or to stdout when path is "" or "-".
func writeOut(path string, b []byte, stdout io.Writer) error {
	if path == "" || path == "-" {
		_, err := stdout.Write(b)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// probe ---------------------------------------------------------------------

func probeCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := newFlags("probe", stderr)
	target := fs.String("target", "https://aliakbari.dev", "site origin")
	interval := fs.Int("interval", 30, "schedule interval in minutes (the run's slot is its start floored to it)")
	out := fs.String("out", "", "output file (default stdout)")
	resolve := fs.String("resolve", "", "connect to this host:port instead of resolving the target (local stacks only)")
	insecure := fs.Bool("insecure", false, "skip certificate verification (local stacks only)")
	minCertDays := fs.Int("min-cert-days", 14, "fail the tls check if the certificate expires sooner")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return exitUsage
	}
	runID := getenv("GITHUB_RUN_ID")
	attempt := 1
	fmt.Sscanf(getenv("GITHUB_RUN_ATTEMPT"), "%d", &attempt)
	env := "local"
	switch getenv("RUNNER_ENVIRONMENT") {
	case "github-hosted":
		env = "github-hosted"
	case "self-hosted":
		env = "self-hosted"
	}
	if runID == "" {
		runID = fmt.Sprintf("local-%d", time.Now().Unix())
	}
	rev := getenv("GITHUB_SHA")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(rev) {
		rev = "unknown"
	}
	if (*resolve != "" || *insecure) && env == "github-hosted" {
		fmt.Fprintln(stderr, "ledger probe: --resolve and --insecure are for local stacks, not scheduled probes")
		return exitUsage
	}
	run := probe.Run(context.Background(), probe.Config{
		Target: *target, IntervalMinutes: *interval, RunID: runID, Attempt: attempt, Revision: rev,
		RunnerEnvironment: env, ResolveTo: *resolve, InsecureTLS: *insecure, MinCertDays: *minCertDays,
	})
	b, err := report.Marshal(run)
	if err == nil {
		err = report.ValidateDocument(report.ProbeSchema, b)
	}
	if err == nil {
		err = writeOut(*out, b, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ledger probe: %v\n", err)
		return exitFailure
	}
	for _, c := range run.Checks {
		state := "ok  "
		if !c.OK {
			state = "FAIL"
		}
		fmt.Fprintf(stderr, "%s %-21s %3d %7.1f ms %s\n", state, c.Name, c.Status, float64(c.DurationUS)/1000, c.Error)
	}
	if !run.OK {
		return exitFailure
	}
	return exitOK
}

// assemble ------------------------------------------------------------------

func assembleCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("assemble", stderr)
	date := fs.String("date", "", "UTC day (YYYY-MM-DD)")
	origin := fs.String("origin", "", "origin report of that day (as served by /api/v1/ops/daily/{date}); omit if unavailable")
	originMissing := fs.String("origin-missing", "the origin report could not be fetched", "reason recorded when -origin is omitted")
	probes := fs.String("probes", "", "directory of probe-run records (searched recursively)")
	interval := fs.Int("interval", 30, "probe schedule interval in minutes")
	service := fs.String("service", "https://aliakbari.dev", "service origin")
	out := fs.String("out", "", "output file (default stdout)")
	if fs.Parse(args) != nil || fs.NArg() > 0 || *date == "" {
		fmt.Fprintln(stderr, "ledger assemble: -date is required")
		return exitUsage
	}
	w, err := report.DayWindow(*date)
	if err != nil {
		fmt.Fprintf(stderr, "ledger assemble: %v\n", err)
		return exitUsage
	}
	var o *report.Origin
	originStatus := "missing: " + *originMissing
	if *origin != "" {
		b, err := os.ReadFile(*origin)
		if err != nil {
			fmt.Fprintf(stderr, "ledger assemble: %v\n", err)
			return exitFailure
		}
		// The host's report must itself recompute before it is republished.
		dir, err := os.MkdirTemp("", "assemble")
		if err != nil {
			fmt.Fprintf(stderr, "ledger assemble: %v\n", err)
			return exitFailure
		}
		defer os.RemoveAll(dir)
		os.WriteFile(filepath.Join(dir, *date+".json"), b, 0o644)
		res, err := report.Recompute(dir)
		if err != nil || !res.OK() {
			fmt.Fprintf(stderr, "ledger assemble: the origin report does not verify: %v %v\n", err, res.Discrepancies)
			return exitFailure
		}
		var hostReport report.Report
		json.Unmarshal(b, &hostReport)
		if hostReport.Date != *date {
			fmt.Fprintf(stderr, "ledger assemble: origin report is for %s\n", hostReport.Date)
			return exitFailure
		}
		o, originStatus = hostReport.Origin, hostReport.Status.Origin
	}
	var p *report.Probe
	probeStatus := "missing: no probe directory given"
	if *probes != "" {
		runs, err := report.LoadProbeRuns(*probes, w)
		if err != nil {
			fmt.Fprintf(stderr, "ledger assemble: %v\n", err)
			return exitFailure
		}
		p = report.BuildProbe(runs, *interval)
		probeStatus = "complete"
		if len(runs) == 0 {
			probeStatus = "partial: no probe runs found for this day"
		} else if expected := 1440 / *interval; len(runs) < expected {
			probeStatus = fmt.Sprintf("partial: %d of %d scheduled runs recorded", len(runs), expected)
		}
	}
	r, err := report.New(*date, *service, generator(), o, originStatus, p, probeStatus)
	if err != nil {
		fmt.Fprintf(stderr, "ledger assemble: %v\n", err)
		return exitFailure
	}
	b, err := report.Marshal(r)
	if err == nil {
		err = report.ValidateDocument(report.RollupSchema, b)
	}
	if err == nil {
		err = writeOut(*out, b, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ledger assemble: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// summary -------------------------------------------------------------------

func summaryCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("summary", stderr)
	dir := fs.String("dir", "", "directory of daily reports")
	last := fs.String("last", "", "last day of the window (YYYY-MM-DD)")
	days := fs.Int("days", 28, "window length in days")
	interval := fs.Int("interval", 30, "probe interval for days without probe data")
	service := fs.String("service", "https://aliakbari.dev", "service origin")
	out := fs.String("out", "", "output file (default stdout)")
	if fs.Parse(args) != nil || fs.NArg() > 0 || *dir == "" || *last == "" {
		fmt.Fprintln(stderr, "ledger summary: -dir and -last are required")
		return exitUsage
	}
	var inputs []report.DayInput
	err := filepath.WalkDir(*dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.json$`).MatchString(d.Name()) {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r report.Report
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		sum := sha256.Sum256(b)
		inputs = append(inputs, report.DayInput{Report: &r, SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "ledger summary: %v\n", err)
		return exitFailure
	}
	s, err := report.BuildSummary(inputs, *last, *days, *interval, *service, generator())
	if err != nil {
		fmt.Fprintf(stderr, "ledger summary: %v\n", err)
		return exitFailure
	}
	b, err := report.Marshal(s)
	if err == nil {
		err = report.ValidateDocument(report.SummarySchema, b)
	}
	if err == nil {
		err = writeOut(*out, b, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ledger summary: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// recompute -----------------------------------------------------------------

func recomputeCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("recompute", stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: ledger recompute [-json] DIR")
		return exitUsage
	}
	res, err := report.Recompute(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "ledger recompute: %v\n", err)
		return exitFailure
	}
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		for _, d := range res.Discrepancies {
			fmt.Fprintln(stdout, d.String())
		}
		verdict := "OK: every published value matches its recomputation"
		if !res.OK() {
			verdict = fmt.Sprintf("FAILED: %d discrepancies", len(res.Discrepancies))
		}
		fmt.Fprintf(stdout, "%d daily reports, %d summaries. %s\n", res.DailyReports, res.Summaries, verdict)
	}
	if res.DailyReports+res.Summaries == 0 {
		fmt.Fprintln(stderr, "ledger recompute: no reports found")
		return exitFailure
	}
	if !res.OK() {
		return exitFailure
	}
	return exitOK
}

// verify-bundle ---------------------------------------------------------------

type policyFlags struct {
	repository, workflow, ref, commit, trustedRoot, tufCache *string
	githubHosted                                             *bool
	triggers                                                 *string
}

func addPolicyFlags(fs *flag.FlagSet) policyFlags {
	return policyFlags{
		repository:   fs.String("repository", "akynte/portfolio", "repository that must have signed (owner/name)"),
		workflow:     fs.String("workflow", ".github/workflows/ci.yml", "workflow file that must have signed"),
		ref:          fs.String("ref", "refs/heads/main", "git ref the signing workflow must have run on"),
		commit:       fs.String("commit", "", "commit the signing workflow must have run at (optional)"),
		githubHosted: fs.Bool("github-hosted", true, "reject signatures made on self-hosted runners"),
		triggers:     fs.String("triggers", "", "comma-separated allowed triggers (optional), e.g. push,workflow_dispatch"),
		trustedRoot:  fs.String("trusted-root", "", "Sigstore trusted_root.json (default: fetch the public-good root over TUF)"),
		tufCache:     fs.String("tuf-cache", "", "directory to cache TUF metadata (default: memory only)"),
	}
}

func (p policyFlags) policy() sigverify.Policy {
	pol := sigverify.Policy{Repository: *p.repository, WorkflowPath: *p.workflow, Ref: *p.ref, Commit: *p.commit,
		RequireGitHubHosted: *p.githubHosted}
	if *p.triggers != "" {
		pol.Triggers = strings.Split(*p.triggers, ",")
	}
	return pol
}

func verifyBundleCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("verify-bundle", stderr)
	artifact := fs.String("artifact", "", "file that was signed")
	bundlePath := fs.String("bundle", "", "Sigstore bundle (default: ARTIFACT.sigstore.json)")
	pf := addPolicyFlags(fs)
	if fs.Parse(args) != nil || fs.NArg() > 0 || *artifact == "" {
		fmt.Fprintln(stderr, "ledger verify-bundle: -artifact is required")
		return exitUsage
	}
	if *bundlePath == "" {
		*bundlePath = *artifact + ".sigstore.json"
	}
	res, err := verifyFile(*artifact, *bundlePath, pf)
	return printVerification(res, err, stdout, stderr)
}

func verifyFile(artifact, bundlePath string, pf policyFlags) (*sigverify.Result, error) {
	b, err := sigverify.ReadFileLimited(artifact, 32<<20)
	if err != nil {
		return nil, err
	}
	bundle, err := sigverify.LoadBundle(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tm, err := sigverify.TrustedRoot(ctx, *pf.trustedRoot, *pf.tufCache)
	if err != nil {
		return nil, fmt.Errorf("trusted root: %w", err)
	}
	return sigverify.Verify(tm, bundle, b, pf.policy(), sigverify.Options{RequireSCT: true})
}

func printVerification(res *sigverify.Result, err error, stdout, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintf(stderr, "VERIFICATION FAILED: %v\n", err)
		return exitFailure
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Fprintln(stdout, string(b))
	return exitOK
}

// verify-release --------------------------------------------------------------

type imageFlags map[string]string

func (m imageFlags) String() string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (m imageFlags) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" || v == "" {
		return errors.New("want service=registry/image@sha256:digest")
	}
	if _, dup := m[k]; dup {
		return fmt.Errorf("image %s given twice", k)
	}
	m[k] = v
	return nil
}

func verifyReleaseCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("verify-release", stderr)
	manifestPath := fs.String("manifest", "", "release manifest (release.json from the release image)")
	bundlePath := fs.String("bundle", "", "its Sigstore bundle (default: MANIFEST.sigstore.json)")
	images := imageFlags{}
	fs.Var(images, "image", "service=image@sha256:digest about to be deployed (repeat for every service)")
	recordOut := fs.String("record-out", "", "on success, write the deployment record (release-v1) here")
	pf := addPolicyFlags(fs)
	if fs.Parse(args) != nil || fs.NArg() > 0 || *manifestPath == "" || *pf.commit == "" {
		fmt.Fprintln(stderr, "ledger verify-release: -manifest and -commit are required")
		return exitUsage
	}
	if *bundlePath == "" {
		*bundlePath = *manifestPath + ".sigstore.json"
	}
	res, err := verifyFile(*manifestPath, *bundlePath, pf)
	if err != nil {
		return printVerification(nil, err, stdout, stderr)
	}
	mb, _ := os.ReadFile(*manifestPath)
	m, err := release.ParseManifest(mb)
	if err != nil {
		return printVerification(nil, err, stdout, stderr)
	}
	// The signer's commit (from the certificate) and the manifest's commit must agree:
	// a workflow cannot vouch for a different revision than the one it ran.
	if m.Commit != res.Commit || m.Repository != *pf.repository {
		return printVerification(nil, fmt.Errorf("manifest names %s@%s but was signed at %s@%s", m.Repository, m.Commit, *pf.repository, res.Commit), stdout, stderr)
	}
	if err := m.Check(release.Expectation{Repository: *pf.repository, Commit: *pf.commit, Images: images}); err != nil {
		return printVerification(nil, err, stdout, stderr)
	}
	if *recordOut != "" {
		rec := release.Record{
			Schema: release.RecordSchema, ReleaseID: m.Commit, Repository: m.Repository, Commit: m.Commit,
			DeployedAt: time.Now().UTC().Truncate(time.Second), Images: m.Images,
			Verification: release.Verification{Status: "verified", Method: "sigstore-bundle", ManifestSHA256: res.ArtifactSHA256,
				SignerWorkflow: res.SignerWorkflow, RunURL: res.RunURL, VerifiedAt: time.Now().UTC().Truncate(time.Second), SignedAt: res.SignedAt},
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		if err := writeOut(*recordOut, append(b, '\n'), stdout); err != nil {
			return printVerification(nil, err, stdout, stderr)
		}
	}
	return printVerification(res, nil, stdout, stderr)
}

// day -------------------------------------------------------------------------

func dayCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("day", stderr)
	db := fs.String("db", "", "ledger database")
	date := fs.String("date", "", "UTC day (YYYY-MM-DD)")
	service := fs.String("service", "https://aliakbari.dev", "service origin")
	out := fs.String("out", "", "output file (default stdout)")
	if fs.Parse(args) != nil || fs.NArg() > 0 || *db == "" || *date == "" {
		fmt.Fprintln(stderr, "ledger day: -db and -date are required")
		return exitUsage
	}
	if _, err := os.Stat(*db); err != nil {
		fmt.Fprintf(stderr, "ledger day: %v\n", err)
		return exitFailure
	}
	ctx := context.Background()
	st, err := storage.Open(ctx, *db, storage.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "ledger day: %v\n", err)
		return exitFailure
	}
	defer st.Close()
	w, err := report.DayWindow(*date)
	if err != nil {
		fmt.Fprintf(stderr, "ledger day: %v\n", err)
		return exitUsage
	}
	o, err := report.BuildOrigin(ctx, st, w, generator())
	if err != nil {
		fmt.Fprintf(stderr, "ledger day: %v\n", err)
		return exitFailure
	}
	status := "complete"
	if o == nil {
		status = "missing: no data in this database"
	} else if o.Derived.Coverage.Ratio < 1 {
		status = "partial"
	}
	r, err := report.New(*date, *service, generator(), o, status, nil, "not collected by the host")
	if err == nil && r.Origin != nil && r.Origin.Derived.Coverage.Ratio < 1 {
		r.Status.Origin = "partial"
	}
	var b []byte
	if err == nil {
		b, err = report.Marshal(r)
	}
	if err == nil {
		err = writeOut(*out, b, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ledger day: %v\n", err)
		return exitFailure
	}
	return exitOK
}
