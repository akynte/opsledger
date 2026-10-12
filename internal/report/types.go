// Package report defines the published daily operational report
// (ops-rollup-v1), the rolling summary (ops-summary-v1), how their derived
// statistics are computed from their raw content, and how a reviewer
// recomputes and checks them.
//
// Every report has two layers:
//
//   - raw: hourly counters and latency histograms, ingestion and
//     reconciliation counts, ledger run intervals, release events and probe
//     runs. These are the observations.
//   - derived: totals, SLI ratios with confidence intervals, percentiles,
//     coverage and gaps. These are computed from the raw layer by Derive.
//
// The generator and the verifier call the same Derive, so recomputation
// checks that every published number follows from the published
// observations. Whether the observations themselves are true is a separate
// question that no recomputation can answer; see docs/operations/slo.md.
package report

import (
	"time"

	"github.com/akynte/opsledger/internal/histogram"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/stats"
)

// Schema URLs and versions.
const (
	RollupSchema  = "https://aliakbari.dev/schemas/ops-rollup-v1.json"
	SummarySchema = "https://aliakbari.dev/schemas/ops-summary-v1.json"
	ProbeSchema   = "https://aliakbari.dev/schemas/probe-run-v1.json"
	ReportVersion = 1
)

// Report is one UTC day.
type Report struct {
	Schema        string    `json:"schema"`
	ReportVersion int       `json:"report_version"`
	SLIVersion    int       `json:"sli_version"`
	Date          string    `json:"date"` // YYYY-MM-DD (UTC)
	Window        Window    `json:"window"`
	Service       string    `json:"service"`
	Generator     Generator `json:"generator"`
	Origin        *Origin   `json:"origin"` // nil: no origin data for the day (Status says why)
	Probe         *Probe    `json:"probe"`  // nil: no probe data for the day
	Status        Status    `json:"status"`
	Limitations   []string  `json:"limitations"`
}

// Window is a half-open UTC interval [Start, End).
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Seconds is the window length.
func (w Window) Seconds() int64 { return int64(w.End.Sub(w.Start) / time.Second) }

// Generator identifies the software that produced a document.
type Generator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Status says which observers contributed and why one is missing.
type Status struct {
	Origin string `json:"origin"` // "complete", "partial" or "missing: <reason>"
	Probe  string `json:"probe"`
}

// Observer describes who made the observations and how far to trust them.
type Observer struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // "self-reported" (the production host) or "external"
	Description string `json:"description"`
}

// ---------------------------------------------------------------------------
// Origin (nginx → opsledger on the production host)

// Origin is the server-side view of the day.
type Origin struct {
	Observer    Observer       `json:"observer"`
	Method      string         `json:"method"`
	Recorder    Generator      `json:"recorder"` // the opsledger build that recorded the data
	Hours       []Hour         `json:"hours"`
	Runs        []RunInterval  `json:"runs"`
	Releases    []ReleaseEvent `json:"releases"`
	Derived     OriginDerived  `json:"derived"`
	Limitations []string       `json:"limitations"`
}

// Hour holds the raw counters of one hour of the day (0–23).
type Hour struct {
	Hour           int            `json:"hour"`
	Cells          []KeyedCell    `json:"cells"`
	Ingest         IngestCounts   `json:"ingest"`
	Reconciliation Reconciliation `json:"reconciliation"`
}

// KeyedCell is a series.Cell with its classification.
type KeyedCell struct {
	Class    string `json:"class"`
	Observer string `json:"observer"`
	Agent    string `json:"agent"`
	series.Cell
}

// IngestCounts are the ledger's own counters: what reached it and what it
// could not use.
type IngestCounts struct {
	Received  uint64 `json:"received"`  // datagrams read from the socket
	Accepted  uint64 `json:"accepted"`  // parsed and queued
	Malformed uint64 `json:"malformed"` // unparseable
	Dropped   uint64 `json:"dropped"`   // queue full, or lost while storage failed
	Rejected  uint64 `json:"rejected"`  // implausible timestamps
}

// Add accumulates o.
func (c *IngestCounts) Add(o IngestCounts) {
	c.Received += o.Received
	c.Accepted += o.Accepted
	c.Malformed += o.Malformed
	c.Dropped += o.Dropped
	c.Rejected += o.Rejected
}

// Reconciliation compares nginx's own request counter (stub_status)
// with the records the ledger received over the same intervals.
type Reconciliation struct {
	NginxRequests       uint64 `json:"nginx_requests"`       // requests nginx counted
	Received            uint64 `json:"received"`             // records the ledger received meanwhile
	ReconciledSeconds   int64  `json:"reconciled_seconds"`   // time covered by comparable samples
	UnreconciledSeconds int64  `json:"unreconciled_seconds"` // restarts or sample gaps: no comparison possible
}

// Add accumulates o.
func (r *Reconciliation) Add(o Reconciliation) {
	r.NginxRequests += o.NginxRequests
	r.Received += o.Received
	r.ReconciledSeconds += o.ReconciledSeconds
	r.UnreconciledSeconds += o.UnreconciledSeconds
}

// RunInterval is a ledger process lifetime overlapping the day, as
// recorded by its heartbeats. End is the last heartbeat (or the stop).
type RunInterval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Clean bool      `json:"clean_stop"`
}

// ReleaseEvent is a release the host reported as deployed at At.
type ReleaseEvent struct {
	At        time.Time `json:"at"`
	ReleaseID string    `json:"release_id"`
}

// OriginDerived is computed from the raw origin data by Derive.
type OriginDerived struct {
	Totals         []KeyedCell                 `json:"totals"`
	Availability   stats.Proportion            `json:"availability"`
	Latency        stats.Proportion            `json:"latency"`
	LatencyByClass map[string]stats.Proportion `json:"latency_by_class"`
	Percentiles    map[string]Percentiles      `json:"percentiles"`
	Excluded       Excluded                    `json:"excluded"`
	Ingest         IngestCounts                `json:"ingest"`
	Reconciliation Reconciliation              `json:"reconciliation"`
	Completeness   stats.Proportion            `json:"completeness"`
	Coverage       Coverage                    `json:"coverage"`
	ActiveReleases []ActiveRelease             `json:"active_releases"`
}

// Excluded counts requests that are reported but not part of the
// availability SLI, by reason.
type Excluded struct {
	Blocked      uint64 `json:"blocked"`       // 403 from the firewall (eligible classes)
	RateLimited  uint64 `json:"rate_limited"`  // 429 (eligible classes)
	ClientClosed uint64 `json:"client_closed"` // 499 (eligible classes)
	Bot          uint64 `json:"bot"`           // automated user agents, any class
	Probe        uint64 `json:"probe"`         // external probe requests, any class
	NotEligible  uint64 `json:"not_eligible"`  // organic human requests in other classes (static, media, ...)
}

// Percentiles are nearest-rank latency quantiles of one class.
type Percentiles struct {
	N    uint64              `json:"n"`
	P50  QuantileBounds      `json:"p50"`
	P90  QuantileBounds      `json:"p90"`
	P95  QuantileBounds      `json:"p95"`
	P99  QuantileBounds      `json:"p99"`
	Hist histogram.Histogram `json:"histogram"`
}

// QuantileBounds locate a quantile: Lower < value <= Upper (microseconds).
type QuantileBounds struct {
	Defined  bool   `json:"defined"`
	Rank     uint64 `json:"rank"`
	Zero     bool   `json:"zero"`
	Index    int32  `json:"index"`
	LowerUS  uint64 `json:"lower_us"`
	UpperUS  uint64 `json:"upper_us"`
	Reliable bool   `json:"reliable"` // at least 10 observations above the quantile
}

// Coverage is the time the ledger was running during the window.
type Coverage struct {
	WindowSeconds  int64   `json:"window_seconds"`
	CoveredSeconds int64   `json:"covered_seconds"`
	Ratio          float64 `json:"ratio"`
	Gaps           []Gap   `json:"gaps"`
}

// Gap is an interval without a running ledger.
type Gap struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ActiveRelease is a release and the part of the window it was deployed.
type ActiveRelease struct {
	ReleaseID string    `json:"release_id"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
}

// ---------------------------------------------------------------------------
// Probe (GitHub-hosted runners → https://aliakbari.dev)

// Probe is the external view of the day.
type Probe struct {
	Observer        Observer     `json:"observer"`
	Method          string       `json:"method"`
	IntervalMinutes int          `json:"interval_minutes"`
	Runs            []ProbeRun   `json:"runs"`
	Derived         ProbeDerived `json:"derived"`
	Limitations     []string     `json:"limitations"`
}

// ProbeRun is one probe execution (one workflow run).
type ProbeRun struct {
	Schema            string    `json:"schema"`
	RunID             string    `json:"run_id"`
	Attempt           int       `json:"attempt"`
	Slot              time.Time `json:"slot"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	Revision          string    `json:"revision"`
	RunnerEnvironment string    `json:"runner_environment"`
	Target            string    `json:"target"`
	Checks            []Check   `json:"checks"`
	OK                bool      `json:"ok"`
}

// Check is one probe check.
type Check struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	OK         bool   `json:"ok"`
	Status     int    `json:"status"`
	DurationUS uint64 `json:"duration_us"`
	Bytes      int64  `json:"bytes"`
	Error      string `json:"error"`
}

// ProbeDerived is computed from the probe runs by Derive.
type ProbeDerived struct {
	ExpectedSlots  int              `json:"expected_slots"`
	ObservedSlots  int              `json:"observed_slots"`
	MissingSlots   []time.Time      `json:"missing_slots"`
	DuplicateRuns  int              `json:"duplicate_runs"`
	Runs           stats.Proportion `json:"runs"`
	Checks         stats.Proportion `json:"checks"`
	ZeroFailureMax *float64         `json:"zero_failure_upper_bound"`
	Failures       []ProbeFailure   `json:"failures"`
	HomeLatency    *Percentiles     `json:"home_latency"`
	MaxDelaySec    int64            `json:"max_start_delay_seconds"`
}

// ProbeFailure lists one failed check.
type ProbeFailure struct {
	Slot  time.Time `json:"slot"`
	Check string    `json:"check"`
	Error string    `json:"error"`
}
