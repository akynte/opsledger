package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
	"github.com/akynte/opsledger/internal/storage"
)

// DayWindow returns the UTC window of a date (YYYY-MM-DD).
func DayWindow(date string) (Window, error) {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Window{}, fmt.Errorf("date %q: %w", date, err)
	}
	return Window{Start: d.UTC(), End: d.UTC().AddDate(0, 0, 1)}, nil
}

// OriginObserver describes the origin observations.
var OriginObserver = Observer{
	ID:   "origin-nginx",
	Kind: "self-reported",
	Description: "nginx on the production host, as recorded by opsledger on the same host. " +
		"These numbers are reported by the system they describe.",
}

// OriginMethod states how origin data is collected.
const OriginMethod = "Every request nginx handles is written as a JSON access-log record to a Unix datagram " +
	"socket (nginx's syslog transport). opsledger parses each record, classifies it and aggregates it per minute " +
	"(counts by outcome and status class, an exact latency-threshold count, and a base-2 exponential histogram of " +
	"$request_time). Once a minute, opsledger also reads nginx's own request counter (stub_status) so that records " +
	"lost in transit can be counted."

// OriginLimitations apply to every origin section.
var OriginLimitations = []string{
	"Requests answered by the CDN edge (cached static assets) never reach the origin and are not counted.",
	"Outages that stop requests before nginx (DNS, CDN, network, host down) are invisible here; the external probe covers them partially.",
	"$request_time has millisecond resolution and includes sending the response to the CDN, not the visitor's full page load.",
	"The data is reported by the production host itself. A signed report proves which workflow published it and that it has not changed since, not that the host measured truthfully.",
}

// BuildOrigin assembles the origin section for window w from the store.
// It returns nil if the ledger has no data at all for the window.
func BuildOrigin(ctx context.Context, st *storage.Store, w Window, recorder Generator) (*Origin, error) {
	from, to := w.Start.Unix(), w.End.Unix()
	rows, err := st.Rows(ctx, from/60, to/60)
	if err != nil {
		return nil, err
	}
	runs, err := st.Runs(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 && len(runs) == 0 {
		return nil, nil
	}
	o := &Origin{
		Observer:    OriginObserver,
		Method:      OriginMethod,
		Recorder:    recorder,
		Hours:       make([]Hour, 24),
		Runs:        []RunInterval{},
		Releases:    []ReleaseEvent{},
		Limitations: OriginLimitations,
	}
	hourCells := make([]map[sli.Key]*series.Cell, 24)
	for i := range o.Hours {
		o.Hours[i] = Hour{Hour: i, Cells: []KeyedCell{}}
		hourCells[i] = map[sli.Key]*series.Cell{}
	}
	for _, r := range rows {
		h := int((r.Minute*60 - from) / 3600)
		if h < 0 || h > 23 {
			continue
		}
		c := hourCells[h][r.Key]
		if c == nil {
			c = series.New()
			hourCells[h][r.Key] = c
		}
		c.Merge(r.Cell)
	}
	for i := range 24 {
		for k, c := range hourCells[i] {
			c.Latency.Normalize()
			o.Hours[i].Cells = append(o.Hours[i].Cells, KeyedCell{Class: string(k.Class), Observer: string(k.Observer), Agent: string(k.Agent), Cell: *c})
		}
		sortCells(o.Hours[i].Cells)
		hs := from + int64(i)*3600
		ing, err := st.IngestTotals(ctx, hs/60, (hs+3600)/60)
		if err != nil {
			return nil, err
		}
		o.Hours[i].Ingest = IngestCounts{ing.Received, ing.Accepted, ing.Malformed, ing.Dropped, ing.Rejected}
	}

	// Reconciliation intervals: consecutive samples of the same ledger run,
	// attributed to the hour in which the interval ends.
	samples, err := st.Samples(ctx, from-600, to)
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		if b.TS < from || b.TS >= to {
			continue
		}
		h := int((b.TS - from) / 3600)
		secs := b.TS - a.TS
		comparable := a.Run == b.Run && b.Requests >= a.Requests && b.Received >= a.Received && secs > 0 && secs <= MaxSampleGap
		rc := &o.Hours[h].Reconciliation
		if comparable {
			rc.NginxRequests += b.Requests - a.Requests
			rc.Received += b.Received - a.Received
			rc.ReconciledSeconds += secs
		} else {
			rc.UnreconciledSeconds += min(secs, 3600)
		}
	}

	for _, r := range runs {
		end := r.LastSeen
		ri := RunInterval{Start: time.Unix(r.Started, 0).UTC(), End: time.Unix(end, 0).UTC(), Clean: r.Stopped != nil}
		o.Runs = append(o.Runs, ri)
	}
	rel, err := st.Releases(ctx, from, to)
	if err != nil {
		return nil, err
	}
	for _, e := range rel {
		o.Releases = append(o.Releases, ReleaseEvent{At: time.Unix(e.TS, 0).UTC(), ReleaseID: e.ReleaseID})
	}
	return o, nil
}

// MaxSampleGap is the longest interval between reconciliation samples that
// is still compared (samples are taken every minute).
const MaxSampleGap = 300

// ProbeObserver describes the external probe.
var ProbeObserver = Observer{
	ID:   "github-hosted-probe",
	Kind: "external",
	Description: "A scheduled GitHub Actions workflow on GitHub-hosted runners, outside the production host and " +
		"its network, requests public pages and the API over HTTPS through the CDN, like a visitor.",
}

// ProbeMethod states how probe data is collected.
const ProbeMethod = "Each scheduled run performs a fixed list of HTTPS checks (status, expected content, snapshot " +
	"consistency between pages and the API, certificate validity) and records the result, timings and the " +
	"probe's own source revision. A run succeeds only if every check succeeds. Results are uploaded as workflow " +
	"artifacts and collected once a day."

// ProbeLimitations apply to every probe section.
var ProbeLimitations = []string{
	"The probe samples the site once per scheduled run; an outage that starts and ends between runs is not observed.",
	"GitHub may delay or skip scheduled runs under load; missing slots are listed and are neither successes nor failures.",
	"Runs come from GitHub's network only: one vantage point, not a representative sample of visitors' networks.",
	"A success ratio of probe runs is not an uptime percentage; see the interval bounds and the zero-failure bound.",
}

// LoadProbeRuns reads every probe-run file under dir (recursively) whose
// slot falls inside w. Files that fail validation are an error: a probe
// result that cannot be read must not silently disappear.
func LoadProbeRuns(dir string, w Window) ([]ProbeRun, error) {
	var runs []ProbeRun
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := ValidateDocument(ProbeSchema, b); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		var r ProbeRun
		if err := decodeStrict(b, &r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !r.Slot.Before(w.Start) && r.Slot.Before(w.End) {
			runs = append(runs, r)
		}
		return nil
	})
	return runs, err
}

// BuildProbe assembles the probe section from runs.
func BuildProbe(runs []ProbeRun, intervalMinutes int) *Probe {
	if runs == nil {
		runs = []ProbeRun{}
	}
	return &Probe{
		Observer:        ProbeObserver,
		Method:          ProbeMethod,
		IntervalMinutes: intervalMinutes,
		Runs:            runs,
		Limitations:     ProbeLimitations,
	}
}

// ReportLimitations apply to every report.
var ReportLimitations = []string{
	"Recomputing this report proves that its statistics follow from its observations; it cannot prove the observations are true.",
	"Small samples produce wide intervals; read every ratio together with its n and its interval.",
}

// New assembles and derives a daily report.
func New(date, service string, gen Generator, origin *Origin, originStatus string, probe *Probe, probeStatus string) (*Report, error) {
	w, err := DayWindow(date)
	if err != nil {
		return nil, err
	}
	r := &Report{
		Schema: RollupSchema, ReportVersion: ReportVersion, SLIVersion: sli.Version,
		Date: date, Window: w, Service: service, Generator: gen,
		Origin: origin, Probe: probe,
		Status:      Status{Origin: originStatus, Probe: probeStatus},
		Limitations: ReportLimitations,
	}
	Derive(r)
	return r, nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// Marshal encodes a report or summary deterministically (sorted map keys,
// two-space indentation, trailing newline), so the same content always has
// the same bytes and therefore the same SHA-256 and signature.
func Marshal(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
