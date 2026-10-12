package report

import (
	"fmt"
	"slices"
	"time"

	"github.com/akynte/opsledger/internal/histogram"
	"github.com/akynte/opsledger/internal/sli"
	"github.com/akynte/opsledger/internal/stats"
)

// Summary is the rolling window over daily reports. It is derived only
// from the daily reports it lists (by SHA-256), so it can be recomputed.
type Summary struct {
	Schema        string         `json:"schema"`
	ReportVersion int            `json:"report_version"`
	SLIVersion    int            `json:"sli_version"`
	Service       string         `json:"service"`
	Generator     Generator      `json:"generator"`
	Window        SummaryWindow  `json:"window"`
	Days          []DayRef       `json:"days"`
	MissingDays   []string       `json:"missing_days"`
	Targets       Targets        `json:"targets"`
	Origin        *SummaryOrigin `json:"origin"`
	Probe         *SummaryProbe  `json:"probe"`
	Limitations   []string       `json:"limitations"`
}

// SummaryWindow is a range of whole UTC days, both ends inclusive.
type SummaryWindow struct {
	FirstDate string `json:"first_date"`
	LastDate  string `json:"last_date"`
	Days      int    `json:"days"`
}

// DayRef identifies one daily report included in a summary.
type DayRef struct {
	Date   string `json:"date"`
	SHA256 string `json:"sha256"`
	Origin bool   `json:"origin"`
	Probe  bool   `json:"probe"`
}

// Targets are the objectives the budgets are computed against.
type Targets struct {
	Availability float64 `json:"availability"`
	Latency      float64 `json:"latency"`
	Probe        float64 `json:"probe"`
	Completeness float64 `json:"completeness"`
}

// CurrentTargets are the objectives of sli.Version.
var CurrentTargets = Targets{
	Availability: sli.AvailabilityTarget,
	Latency:      sli.LatencyTarget,
	Probe:        sli.ProbeTarget,
	Completeness: sli.CoverageTarget,
}

// SummaryOrigin aggregates the origin sections of the window.
type SummaryOrigin struct {
	DaysWithData       int                         `json:"days_with_data"`
	Availability       stats.Proportion            `json:"availability"`
	AvailabilityBudget stats.Budget                `json:"availability_budget"`
	Latency            stats.Proportion            `json:"latency"`
	LatencyBudget      stats.Budget                `json:"latency_budget"`
	LatencyByClass     map[string]stats.Proportion `json:"latency_by_class"`
	Percentiles        map[string]Percentiles      `json:"percentiles"`
	Excluded           Excluded                    `json:"excluded"`
	Ingest             IngestCounts                `json:"ingest"`
	Reconciliation     Reconciliation              `json:"reconciliation"`
	Completeness       stats.Proportion            `json:"completeness"`
	Coverage           Coverage                    `json:"coverage"`
	Releases           []string                    `json:"releases"`
}

// SummaryProbe aggregates the probe sections of the window.
type SummaryProbe struct {
	DaysWithData   int              `json:"days_with_data"`
	ExpectedSlots  int              `json:"expected_slots"`
	ObservedSlots  int              `json:"observed_slots"`
	Runs           stats.Proportion `json:"runs"`
	Checks         stats.Proportion `json:"checks"`
	Budget         stats.Budget     `json:"budget"`
	ZeroFailureMax *float64         `json:"zero_failure_upper_bound"`
	Failures       int              `json:"failures"`
	HomeLatency    *Percentiles     `json:"home_latency"`
}

// SummaryLimitations apply to every summary.
var SummaryLimitations = []string{
	"Days without a report count as uncovered time; they are listed in missing_days.",
	"Budgets are computed over observed events only. With few events, one failure can exhaust a budget many times over.",
	"Probe success is a ratio of sampled runs, not continuous uptime.",
}

// DayInput is one daily report with the SHA-256 of its published bytes.
type DayInput struct {
	Report *Report
	SHA256 string
}

// BuildSummary derives the summary of the windowDays days ending at
// lastDate (inclusive) from the given daily reports. Reports outside the
// window are ignored. probeInterval is used to count expected probe slots
// on days without any report.
func BuildSummary(days []DayInput, lastDate string, windowDays, probeInterval int, service string, gen Generator) (*Summary, error) {
	last, err := time.Parse("2006-01-02", lastDate)
	if err != nil {
		return nil, fmt.Errorf("last date: %w", err)
	}
	if windowDays < 1 || windowDays > 366 {
		return nil, fmt.Errorf("window of %d days out of range", windowDays)
	}
	first := last.AddDate(0, 0, -(windowDays - 1))
	s := &Summary{
		Schema: SummarySchema, ReportVersion: ReportVersion, SLIVersion: sli.Version,
		Service: service, Generator: gen,
		Window:      SummaryWindow{FirstDate: first.Format("2006-01-02"), LastDate: lastDate, Days: windowDays},
		Days:        []DayRef{},
		MissingDays: []string{},
		Targets:     CurrentTargets,
		Limitations: SummaryLimitations,
	}
	byDate := map[string]DayInput{}
	for _, d := range days {
		if d.Report.SLIVersion != sli.Version {
			return nil, fmt.Errorf("%s: sli_version %d cannot be combined with %d", d.Report.Date, d.Report.SLIVersion, sli.Version)
		}
		if _, dup := byDate[d.Report.Date]; dup {
			return nil, fmt.Errorf("%s: two reports for the same day", d.Report.Date)
		}
		byDate[d.Report.Date] = d
	}
	var inWindow []DayInput
	for t := first; !t.After(last); t = t.AddDate(0, 0, 1) {
		date := t.Format("2006-01-02")
		d, ok := byDate[date]
		if !ok {
			s.MissingDays = append(s.MissingDays, date)
			continue
		}
		inWindow = append(inWindow, d)
		s.Days = append(s.Days, DayRef{Date: date, SHA256: d.SHA256, Origin: d.Report.Origin != nil, Probe: d.Report.Probe != nil})
	}
	deriveSummary(s, inWindow, probeInterval)
	return s, nil
}

func deriveSummary(s *Summary, days []DayInput, probeInterval int) {
	windowSeconds := int64(s.Window.Days) * 86400
	var (
		totals                    [][]KeyedCell
		ingest                    IngestCounts
		recon                     Reconciliation
		covered                   int64
		gaps                      []Gap
		releases                  []string
		originDays, probeDays     int
		expected, observed        int
		runsN, runsGood           uint64
		checksN, checksGood       uint64
		failures                  int
		home                      = histogram.New(histogram.DefaultScale)
		probeDaysPresent          = map[string]bool{}
		probeIntervalOfPresentDay int
	)
	for _, d := range days {
		r := d.Report
		if o := r.Origin; o != nil {
			originDays++
			totals = append(totals, o.Derived.Totals)
			ingest.Add(o.Derived.Ingest)
			recon.Add(o.Derived.Reconciliation)
			covered += o.Derived.Coverage.CoveredSeconds
			gaps = append(gaps, o.Derived.Coverage.Gaps...)
			for _, a := range o.Derived.ActiveReleases {
				if len(releases) == 0 || releases[len(releases)-1] != a.ReleaseID {
					releases = append(releases, a.ReleaseID)
				}
			}
		} else {
			w, _ := DayWindow(r.Date)
			gaps = append(gaps, Gap{Start: w.Start, End: w.End})
		}
		if p := r.Probe; p != nil {
			probeDays++
			probeDaysPresent[r.Date] = true
			probeIntervalOfPresentDay = p.IntervalMinutes
			expected += p.Derived.ExpectedSlots
			observed += p.Derived.ObservedSlots
			runsN += p.Derived.Runs.N
			runsGood += p.Derived.Runs.Good
			checksN += p.Derived.Checks.N
			checksGood += p.Derived.Checks.Good
			failures += len(p.Derived.Failures)
			if p.Derived.HomeLatency != nil {
				h := p.Derived.HomeLatency.Hist
				home.Merge(&h)
			}
		}
	}
	if originDays > 0 {
		o := &SummaryOrigin{DaysWithData: originDays, Ingest: ingest, Reconciliation: recon, Releases: releases}
		if o.Releases == nil {
			o.Releases = []string{}
		}
		merged := mergeCells(totals...)
		o.Availability, o.Latency, o.LatencyByClass, o.Percentiles, o.Excluded = sliFromTotals(merged)
		o.AvailabilityBudget = stats.NewBudget(sli.AvailabilityTarget, o.Availability.N, o.Availability.Bad)
		o.LatencyBudget = stats.NewBudget(sli.LatencyTarget, o.Latency.N, o.Latency.Bad)
		o.Completeness = stats.NewProportion(recon.NginxRequests, min(recon.Received, recon.NginxRequests))
		// Days without a report are uncovered time.
		for _, m := range s.MissingDays {
			w, _ := DayWindow(m)
			gaps = append(gaps, Gap{Start: w.Start, End: w.End})
		}
		slices.SortFunc(gaps, func(a, b Gap) int { return a.Start.Compare(b.Start) })
		o.Coverage = Coverage{WindowSeconds: windowSeconds, CoveredSeconds: covered, Gaps: mergeGaps(gaps)}
		if windowSeconds > 0 {
			o.Coverage.Ratio = stats.Round(float64(covered) / float64(windowSeconds))
		}
		s.Origin = o
	}
	if probeDays > 0 {
		interval := probeInterval
		if interval <= 0 {
			interval = probeIntervalOfPresentDay
		}
		// Days without probe data still had scheduled slots.
		for t, _ := time.Parse("2006-01-02", s.Window.FirstDate); ; t = t.AddDate(0, 0, 1) {
			date := t.Format("2006-01-02")
			if !probeDaysPresent[date] && interval > 0 {
				expected += 1440 / interval
			}
			if date == s.Window.LastDate {
				break
			}
		}
		p := &SummaryProbe{DaysWithData: probeDays, ExpectedSlots: expected, ObservedSlots: observed, Failures: failures}
		p.Runs = stats.NewProportion(runsN, runsGood)
		p.Checks = stats.NewProportion(checksN, checksGood)
		p.Budget = stats.NewBudget(sli.ProbeTarget, runsN, runsN-runsGood)
		if runsN > 0 && runsGood == runsN {
			b := stats.ZeroFailureUpperBound(runsN)
			p.ZeroFailureMax = &b
		}
		if home.Count() > 0 {
			pc := percentilesOf(home)
			p.HomeLatency = &pc
		}
		s.Probe = p
	}
}

// mergeGaps joins touching or overlapping gaps.
func mergeGaps(gaps []Gap) []Gap {
	out := []Gap{}
	for _, g := range gaps {
		if n := len(out); n > 0 && !g.Start.After(out[n-1].End) {
			if g.End.After(out[n-1].End) {
				out[n-1].End = g.End
			}
			continue
		}
		out = append(out, g)
	}
	return out
}
