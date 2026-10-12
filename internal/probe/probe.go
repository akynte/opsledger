// Package probe checks the public site from outside, the way a visitor's
// browser would reach it: HTTPS through DNS and the CDN, with certificate
// verification. It records each check's result and duration.
//
// The probe sends an X-Opsledger-Probe header with its run id so that the
// origin can tell probe requests apart from organic traffic. The probe's
// own verdict never depends on that header: it judges responses only by
// what it receives.
package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/akynte/opsledger/internal/report"
)

// Config describes one probe run.
type Config struct {
	Target            string        // https://aliakbari.dev
	IntervalMinutes   int           // schedule interval; the run's slot is its start time floored to it
	RunID             string        // GitHub run id (or a local id)
	Attempt           int           // GitHub run attempt
	Revision          string        // commit of the probe's own source
	RunnerEnvironment string        // github-hosted, self-hosted or local
	Timeout           time.Duration // per check (default 15s)
	MinCertDays       int           // certificate must be valid at least this long (default 14)
	// ResolveTo, if set ("127.0.0.1:18443"), connects there instead of resolving the
	// target's host name: for testing against a local stack only.
	ResolveTo string
	// InsecureTLS disables certificate verification: for a local stack with a
	// self-signed certificate only. Production runs never set it.
	InsecureTLS bool
	Now         func() time.Time
}

// UserAgent identifies the probe in logs.
const UserAgent = "opsledger-probe/1 (+https://aliakbari.dev/operations)"

const maxBody = 8 << 20

var snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type response struct {
	status int
	body   []byte
	header http.Header
	tls    *tls.ConnectionState
	dur    time.Duration
	err    error
}

// Run performs every check and returns the run record. It never returns an
// error: failures are recorded in the checks.
func Run(ctx context.Context, cfg Config) report.ProbeRun {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MinCertDays <= 0 {
		cfg.MinCertDays = 14
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	start := cfg.Now().UTC()
	run := report.ProbeRun{
		Schema:            report.ProbeSchema,
		RunID:             cfg.RunID,
		Attempt:           max(cfg.Attempt, 1),
		StartedAt:         start,
		Revision:          cfg.Revision,
		RunnerEnvironment: cfg.RunnerEnvironment,
		Target:            strings.TrimSuffix(cfg.Target, "/"),
	}
	if cfg.IntervalMinutes > 0 {
		run.Slot = start.Truncate(time.Duration(cfg.IntervalMinutes) * time.Minute)
	} else {
		run.Slot = start.Truncate(time.Minute)
	}

	client := newClient(cfg)
	get := func(path string) response {
		cctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, run.Target+path, nil)
		if err != nil {
			return response{err: err}
		}
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
		req.Header.Set("X-Opsledger-Probe", cfg.RunID)
		t0 := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return response{err: err, dur: time.Since(t0)}
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		return response{status: resp.StatusCode, body: body, header: resp.Header, tls: resp.TLS, dur: time.Since(t0), err: err}
	}
	add := func(name, path string, r response, problem string) {
		c := report.Check{Name: name, URL: run.Target + path, Status: r.status, DurationUS: uint64(r.dur.Microseconds()), Bytes: int64(len(r.body))}
		switch {
		case r.err != nil:
			c.Error = trim(r.err.Error())
		case problem != "":
			c.Error = trim(problem)
		default:
			c.OK = true
		}
		run.Checks = append(run.Checks, c)
	}

	home := get("/")
	add("home", "/", home, expectHTML(home))
	add("tls", "/", home, certProblem(home, cfg.Now(), cfg.MinCertDays))

	snap := get("/api/v1/snapshot")
	id, problem := snapshotID(snap)
	add("api-snapshot", "/api/v1/snapshot", snap, problem)
	consistency := problem
	switch {
	case consistency != "":
	case home.err != nil || home.status != http.StatusOK:
		consistency = "home page unavailable"
	case !bytes.Contains(home.body, []byte(id[:12])):
		consistency = "the home page and the API serve different snapshots"
	}
	add("snapshot-consistency", "/", response{status: home.status, dur: 0}, consistency)

	for _, p := range []struct{ name, path string }{{"review", "/review"}, {"proof", "/proof"}} {
		r := get(p.path)
		add(p.name, p.path, r, expectHTML(r))
	}
	pdf := get("/resume.pdf")
	add("resume-pdf", "/resume.pdf", pdf, expectPDF(pdf))
	api := get("/api/v1/")
	add("api-index", "/api/v1/", api, expectJSON(api))

	run.FinishedAt = cfg.Now().UTC()
	run.OK = len(run.Checks) > 0
	for _, c := range run.Checks {
		run.OK = run.OK && c.OK
	}
	return run
}

func newClient(cfg Config) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if cfg.ResolveTo != "" {
				addr = cfg.ResolveTo
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	if cfg.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // local test stacks only, see Config
	}
	return &http.Client{
		Transport: tr,
		// Redirects are failures: every probed URL must answer directly.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func trim(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		return s[:297] + "..."
	}
	return s
}

func expectStatus(r response) string {
	if r.err != nil {
		return ""
	}
	if r.status != http.StatusOK {
		return fmt.Sprintf("status %d", r.status)
	}
	return ""
}

func expectHTML(r response) string {
	if p := expectStatus(r); p != "" || r.err != nil {
		return p
	}
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
		return "not HTML: " + r.header.Get("Content-Type")
	}
	if !bytes.Contains(r.body, []byte("<h1")) {
		return "page has no <h1>: rendered content missing"
	}
	return ""
}

func expectPDF(r response) string {
	if p := expectStatus(r); p != "" || r.err != nil {
		return p
	}
	if !bytes.HasPrefix(r.body, []byte("%PDF-")) {
		return "body is not a PDF"
	}
	return ""
}

func expectJSON(r response) string {
	if p := expectStatus(r); p != "" || r.err != nil {
		return p
	}
	if !json.Valid(r.body) {
		return "body is not JSON"
	}
	return ""
}

func snapshotID(r response) (string, string) {
	if p := expectJSON(r); p != "" || r.err != nil {
		return "", p
	}
	var v struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(r.body, &v); err != nil || !snapshotIDPattern.MatchString(v.SnapshotID) {
		return "", "no valid snapshot_id"
	}
	return v.SnapshotID, ""
}

func certProblem(r response, now time.Time, minDays int) string {
	if r.err != nil {
		return ""
	}
	if r.tls == nil || len(r.tls.PeerCertificates) == 0 {
		return "no TLS certificate"
	}
	left := r.tls.PeerCertificates[0].NotAfter.Sub(now)
	if left < time.Duration(minDays)*24*time.Hour {
		return fmt.Sprintf("certificate expires in %d days (minimum %d)", int(left.Hours()/24), minDays)
	}
	return ""
}
