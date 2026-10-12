package series

import (
	"testing"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/sli"
)

var page = sli.Key{Class: sli.Page, Observer: sli.Organic, Agent: sli.Human}

func TestAddCountsOutcomesStatusesAndLatency(t *testing.T) {
	c := New()
	for _, r := range []ingest.Record{
		{Status: 200, Duration: 10_000}, {Status: 304, Duration: 2_000}, {Status: 404, Duration: 1_000},
		{Status: 403}, {Status: 429}, {Status: 499, Duration: 900_000}, {Status: 502, Duration: 5_000},
		{Status: 200, Duration: 400_000}, // slower than the 300 ms page objective
	} {
		c.Add(page, r)
	}
	if c.Total != 8 || c.OK != 4 || c.Blocked != 1 || c.RateLimited != 1 || c.ClientClosed != 1 || c.ServerError != 1 {
		t.Fatalf("outcomes %+v", c)
	}
	if c.S2xx != 2 || c.S3xx != 1 || c.S4xx != 4 || c.S5xx != 1 {
		t.Fatalf("status classes %+v", c)
	}
	// Latency-eligible: the OK outcomes only (200, 304, 404, 200); within 300 ms: three of them.
	if c.LatEligible != 4 || c.LatGood != 3 {
		t.Fatalf("latency %d/%d", c.LatGood, c.LatEligible)
	}
	if msg := c.Consistent(); msg != "" {
		t.Fatal(msg)
	}
}

func TestNoLatencySLIForStaticFilesOrBots(t *testing.T) {
	c := New()
	c.Add(sli.Key{Class: sli.Static, Observer: sli.Organic, Agent: sli.Human}, ingest.Record{Status: 200})
	c.Add(sli.Key{Class: sli.Page, Observer: sli.Organic, Agent: sli.Bot}, ingest.Record{Status: 200})
	if c.LatEligible != 0 || c.Latency.Count() != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestMergeAndConsistency(t *testing.T) {
	a, b := New(), New()
	a.Add(page, ingest.Record{Status: 200, Duration: 1000})
	b.Add(page, ingest.Record{Status: 500, Duration: 2000})
	m := a.Clone()
	m.Merge(b)
	if m.Total != 2 || m.ServerError != 1 || m.Latency.Count() != 2 || a.Total != 1 {
		t.Fatalf("merge %+v (a must be unchanged: %+v)", m, a)
	}
	broken := m.Clone()
	broken.S2xx++
	if broken.Consistent() == "" {
		t.Fatal("inconsistent status classes accepted")
	}
	broken = m.Clone()
	broken.LatGood = broken.LatEligible + 1
	if broken.Consistent() == "" {
		t.Fatal("latency good above eligible accepted")
	}
}
