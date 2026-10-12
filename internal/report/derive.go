package report

import (
	"cmp"
	"slices"
	"sort"
	"time"

	"github.com/akynte/opsledger/internal/histogram"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
	"github.com/akynte/opsledger/internal/stats"
)

// LatencyClasses are the classes with a latency SLI, in publication order.
var LatencyClasses = []sli.Class{sli.Page, sli.RSC, sli.API}

// quantiles published for every latency class (permille).
var quantiles = []uint64{500, 900, 950, 990}

// Derive computes every derived field of r from its raw content,
// overwriting whatever was there.
func Derive(r *Report) {
	if r.Origin != nil {
		r.Origin.Derived = deriveOrigin(r.Origin, r.Window)
	}
	if r.Probe != nil {
		r.Probe.Derived = deriveProbe(r.Probe, r.Window)
	}
}

func keyOf(c KeyedCell) sli.Key {
	return sli.Key{Class: sli.Class(c.Class), Observer: sli.Observer(c.Observer), Agent: sli.Agent(c.Agent)}
}

func sortCells(cells []KeyedCell) {
	slices.SortFunc(cells, func(a, b KeyedCell) int {
		return cmp.Or(cmp.Compare(a.Class, b.Class), cmp.Compare(a.Observer, b.Observer), cmp.Compare(a.Agent, b.Agent))
	})
}

// mergeCells sums cells by key.
func mergeCells(groups ...[]KeyedCell) []KeyedCell {
	byKey := map[sli.Key]*series.Cell{}
	for _, g := range groups {
		for _, c := range g {
			k := keyOf(c)
			acc := byKey[k]
			if acc == nil {
				acc = series.New()
				byKey[k] = acc
			}
			cc := c.Cell
			acc.Merge(&cc)
		}
	}
	out := make([]KeyedCell, 0, len(byKey))
	for k, c := range byKey {
		c.Latency.Normalize()
		out = append(out, KeyedCell{Class: string(k.Class), Observer: string(k.Observer), Agent: string(k.Agent), Cell: *c})
	}
	sortCells(out)
	return out
}

func deriveOrigin(o *Origin, w Window) OriginDerived {
	var d OriginDerived
	groups := make([][]KeyedCell, 0, len(o.Hours))
	for _, h := range o.Hours {
		groups = append(groups, h.Cells)
		d.Ingest.Add(h.Ingest)
		d.Reconciliation.Add(h.Reconciliation)
	}
	d.Totals = mergeCells(groups...)
	d.Availability, d.Latency, d.LatencyByClass, d.Percentiles, d.Excluded = sliFromTotals(d.Totals)

	// Completeness: of the requests nginx counted over reconciled intervals,
	// how many reached the ledger. Timing at interval edges can make the
	// ledger's count slightly higher; it is capped at nginx's count.
	d.Completeness = stats.NewProportion(d.Reconciliation.NginxRequests, min(d.Reconciliation.Received, d.Reconciliation.NginxRequests))
	d.Coverage = coverage(o.Runs, w)
	d.ActiveReleases = activeReleases(o.Releases, w)
	return d
}

// sliFromTotals computes the SLIs from per-key totals. It is shared by the
// daily report and the rolling summary.
func sliFromTotals(totals []KeyedCell) (avail, lat stats.Proportion, latByClass map[string]stats.Proportion,
	pct map[string]Percentiles, ex Excluded) {
	var availN, availGood, latN, latGood uint64
	latClassN, latClassGood := map[sli.Class]uint64{}, map[sli.Class]uint64{}
	hists := map[sli.Class]*histogram.Histogram{}
	for _, c := range totals {
		k := keyOf(c)
		switch {
		case k.Observer == sli.Probe:
			ex.Probe += c.Total
			continue
		case k.Agent == sli.Bot:
			ex.Bot += c.Total
			continue
		case !sli.AvailabilityEligible(k):
			ex.NotEligible += c.Total
			continue
		}
		ex.Blocked += c.Blocked
		ex.RateLimited += c.RateLimited
		ex.ClientClosed += c.ClientClosed
		availN += c.OK + c.ServerError
		availGood += c.OK
		latN += c.LatEligible
		latGood += c.LatGood
		latClassN[k.Class] += c.LatEligible
		latClassGood[k.Class] += c.LatGood
		h := hists[k.Class]
		if h == nil {
			h = histogram.New(histogram.DefaultScale)
			hists[k.Class] = h
		}
		h.Merge(c.Latency)
	}
	avail = stats.NewProportion(availN, availGood)
	lat = stats.NewProportion(latN, latGood)
	latByClass = map[string]stats.Proportion{}
	pct = map[string]Percentiles{}
	for _, cl := range LatencyClasses {
		latByClass[string(cl)] = stats.NewProportion(latClassN[cl], latClassGood[cl])
		pct[string(cl)] = percentilesOf(hists[cl])
	}
	return
}

func percentilesOf(h *histogram.Histogram) Percentiles {
	if h == nil {
		h = histogram.New(histogram.DefaultScale)
	}
	h = h.Clone()
	h.Normalize()
	p := Percentiles{N: h.Count(), Hist: *h}
	out := []*QuantileBounds{&p.P50, &p.P90, &p.P95, &p.P99}
	for i, pm := range quantiles {
		q, ok := h.QuantilePermille(pm)
		if !ok {
			continue
		}
		*out[i] = QuantileBounds{Defined: true, Rank: q.Rank, Zero: q.Zero, Index: q.Index,
			LowerUS: q.Lower, UpperUS: q.Upper, Reliable: stats.ReliableQuantile(p.N, pm)}
	}
	return p
}

// coverage is the union of run intervals clipped to the window, and the
// gaps between them. Gaps shorter than a minute are heartbeat jitter
// (heartbeats are written every few seconds) and are not reported.
func coverage(runs []RunInterval, w Window) Coverage {
	type iv struct{ a, b int64 }
	var ivs []iv
	for _, r := range runs {
		a, b := max(r.Start.Unix(), w.Start.Unix()), min(r.End.Unix(), w.End.Unix())
		if b > a {
			ivs = append(ivs, iv{a, b})
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a < ivs[j].a })
	var merged []iv
	for _, x := range ivs {
		if n := len(merged); n > 0 && x.a <= merged[n-1].b+60 {
			merged[n-1].b = max(merged[n-1].b, x.b)
			continue
		}
		merged = append(merged, x)
	}
	c := Coverage{WindowSeconds: w.Seconds(), Gaps: []Gap{}}
	cursor := w.Start.Unix()
	gap := func(a, b int64) {
		if b-a >= 60 {
			c.Gaps = append(c.Gaps, Gap{Start: time.Unix(a, 0).UTC(), End: time.Unix(b, 0).UTC()})
		} else if b > a {
			c.CoveredSeconds += b - a // heartbeat jitter, not an outage
		}
	}
	for _, m := range merged {
		gap(cursor, m.a)
		c.CoveredSeconds += m.b - m.a
		cursor = m.b
	}
	gap(cursor, w.End.Unix())
	c.CoveredSeconds = min(c.CoveredSeconds, c.WindowSeconds)
	if c.WindowSeconds > 0 {
		c.Ratio = stats.Round(float64(c.CoveredSeconds) / float64(c.WindowSeconds))
	}
	return c
}

// activeReleases turns release events into the intervals of the window
// during which each release was deployed.
func activeReleases(events []ReleaseEvent, w Window) []ActiveRelease {
	evs := slices.Clone(events)
	slices.SortStableFunc(evs, func(a, b ReleaseEvent) int { return a.At.Compare(b.At) })
	out := []ActiveRelease{}
	for i, e := range evs {
		from := e.At
		if from.Before(w.Start) {
			from = w.Start
		}
		to := w.End
		if i+1 < len(evs) && evs[i+1].At.Before(to) {
			to = evs[i+1].At
		}
		if !to.After(from) {
			continue
		}
		out = append(out, ActiveRelease{ReleaseID: e.ReleaseID, From: from, To: to})
	}
	return out
}

func deriveProbe(p *Probe, w Window) ProbeDerived {
	d := ProbeDerived{MissingSlots: []time.Time{}, Failures: []ProbeFailure{}}
	interval := time.Duration(p.IntervalMinutes) * time.Minute
	if interval > 0 {
		d.ExpectedSlots = int(w.End.Sub(w.Start) / interval)
	}
	seen := map[int64]bool{}
	var runsN, runsGood, checksN, checksGood uint64
	home := histogram.New(histogram.DefaultScale)
	runs := slices.Clone(p.Runs)
	slices.SortStableFunc(runs, func(a, b ProbeRun) int { return a.Slot.Compare(b.Slot) })
	for _, r := range runs {
		if seen[r.Slot.Unix()] {
			d.DuplicateRuns++
		}
		seen[r.Slot.Unix()] = true
		runsN++
		if r.OK {
			runsGood++
		}
		if delay := int64(r.StartedAt.Sub(r.Slot) / time.Second); delay > d.MaxDelaySec {
			d.MaxDelaySec = delay
		}
		for _, c := range r.Checks {
			checksN++
			if c.OK {
				checksGood++
			} else {
				d.Failures = append(d.Failures, ProbeFailure{Slot: r.Slot, Check: c.Name, Error: c.Error})
			}
			if c.Name == "home" && c.OK {
				home.Record(c.DurationUS)
			}
		}
	}
	d.ObservedSlots = len(seen)
	if interval > 0 {
		for t := w.Start; t.Before(w.End); t = t.Add(interval) {
			if !seen[t.Unix()] {
				d.MissingSlots = append(d.MissingSlots, t)
			}
		}
	}
	d.Runs = stats.NewProportion(runsN, runsGood)
	d.Checks = stats.NewProportion(checksN, checksGood)
	if runsN > 0 && runsGood == runsN {
		b := stats.ZeroFailureUpperBound(runsN)
		d.ZeroFailureMax = &b
	}
	if home.Count() > 0 {
		pc := percentilesOf(home)
		d.HomeLatency = &pc
	}
	return d
}
