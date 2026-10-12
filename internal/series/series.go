// Package series holds the mergeable per-key counters that the aggregator
// builds per minute and that storage and reports combine over longer
// windows. Every field is a count or a histogram, so combining cells is
// addition: there is no average or percentile here that could be combined
// incorrectly.
package series

import (
	"github.com/akynte/opsledger/internal/histogram"
	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/sli"
)

// Cell aggregates the requests of one key over some window.
type Cell struct {
	Total uint64 `json:"total"`

	// Outcomes (they sum to Total).
	OK           uint64 `json:"ok"`
	Blocked      uint64 `json:"blocked"`
	RateLimited  uint64 `json:"rate_limited"`
	ClientClosed uint64 `json:"client_closed"`
	ServerError  uint64 `json:"server_error"`

	// Status classes (they sum to Total).
	S1xx uint64 `json:"s1xx"`
	S2xx uint64 `json:"s2xx"`
	S3xx uint64 `json:"s3xx"`
	S4xx uint64 `json:"s4xx"`
	S5xx uint64 `json:"s5xx"`

	// Latency SLI: requests eligible for it and those within the threshold,
	// decided exactly at ingestion (not estimated from the histogram).
	LatEligible uint64 `json:"lat_eligible"`
	LatGood     uint64 `json:"lat_good"`

	// Duration of every request in the cell, in microseconds.
	Latency *histogram.Histogram `json:"latency"`
}

// New returns an empty cell.
func New() *Cell { return &Cell{Latency: histogram.New(histogram.DefaultScale)} }

// Add records one request with the given key.
func (c *Cell) Add(k sli.Key, r ingest.Record) {
	c.Total++
	o := sli.OutcomeOf(r.Status)
	switch o {
	case sli.OK:
		c.OK++
	case sli.Blocked:
		c.Blocked++
	case sli.RateLimited:
		c.RateLimited++
	case sli.ClientClosed:
		c.ClientClosed++
	case sli.ServerError:
		c.ServerError++
	}
	switch {
	case r.Status < 200:
		c.S1xx++
	case r.Status < 300:
		c.S2xx++
	case r.Status < 400:
		c.S3xx++
	case r.Status < 500:
		c.S4xx++
	default:
		c.S5xx++
	}
	if sli.LatencyEligible(k, o) {
		c.LatEligible++
		if th, _ := sli.LatencyThreshold(k.Class); r.Duration <= uint64(th.Microseconds()) {
			c.LatGood++
		}
	}
	c.Latency.Record(r.Duration)
}

// Merge adds o into c.
func (c *Cell) Merge(o *Cell) {
	c.Total += o.Total
	c.OK += o.OK
	c.Blocked += o.Blocked
	c.RateLimited += o.RateLimited
	c.ClientClosed += o.ClientClosed
	c.ServerError += o.ServerError
	c.S1xx += o.S1xx
	c.S2xx += o.S2xx
	c.S3xx += o.S3xx
	c.S4xx += o.S4xx
	c.S5xx += o.S5xx
	c.LatEligible += o.LatEligible
	c.LatGood += o.LatGood
	if c.Latency == nil {
		c.Latency = histogram.New(histogram.DefaultScale)
	}
	c.Latency.Merge(o.Latency)
}

// Clone returns a deep copy.
func (c *Cell) Clone() *Cell {
	d := *c
	if c.Latency != nil {
		d.Latency = c.Latency.Clone()
	}
	return &d
}

// Consistent reports the first internal inconsistency, or "" if none.
func (c *Cell) Consistent() string {
	if c.OK+c.Blocked+c.RateLimited+c.ClientClosed+c.ServerError != c.Total {
		return "outcomes do not sum to total"
	}
	if c.S1xx+c.S2xx+c.S3xx+c.S4xx+c.S5xx != c.Total {
		return "status classes do not sum to total"
	}
	if c.S5xx != c.ServerError {
		return "5xx count differs from server errors"
	}
	if c.LatGood > c.LatEligible || c.LatEligible > c.OK {
		return "latency counts out of range"
	}
	if c.Latency == nil {
		return "missing latency histogram"
	}
	if err := c.Latency.Validate(); err != nil {
		return "latency histogram: " + err.Error()
	}
	if c.Latency.Count() != c.Total {
		return "latency histogram count differs from total"
	}
	return ""
}
