package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
	"github.com/akynte/opsledger/internal/storage"
)

const date = "2026-10-11"

var (
	day0 = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	gen  = Generator{Name: "opsledger", Version: "test", Commit: strings.Repeat("c", 40)}
	sha1 = strings.Repeat("a", 40)
	sha2 = strings.Repeat("b", 40)
)

// fixtureStore fills a store with a known day: requests in three hours,
// one 5xx, one slow page, a bot, a probe request, a WAF block, ledger runs
// with a gap, reconciliation samples and two releases.
func fixtureStore(t *testing.T) *storage.Store {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, filepath.Join(t.TempDir(), "l.db"), storage.Options{Now: func() time.Time { return day0 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	add := func(cells map[storage.MinuteKey]*series.Cell, at time.Time, r ingest.Record) {
		r.Time = at
		k := sli.Classify(r)
		mk := storage.MinuteKey{Minute: at.Unix() / 60, Key: k}
		if cells[mk] == nil {
			cells[mk] = series.New()
		}
		cells[mk].Add(k, r)
	}
	page := func(status int, us uint64) ingest.Record {
		return ingest.Record{Server: ingest.ServerPublic, Method: "GET", Path: "/", Status: status, Duration: us}
	}
	cells := map[storage.MinuteKey]*series.Cell{}
	for i := range 50 {
		add(cells, day0.Add(time.Hour+time.Duration(i)*time.Second), page(200, uint64(2000+i*100)))
	}
	add(cells, day0.Add(2*time.Hour), page(502, 15000))
	add(cells, day0.Add(2*time.Hour), page(200, 900_000)) // slow
	add(cells, day0.Add(2*time.Hour), page(403, 1000))    // firewall
	bot := page(200, 3000)
	bot.Bot = true
	add(cells, day0.Add(3*time.Hour), bot)
	probe := page(200, 4000)
	probe.Probe = true
	add(cells, day0.Add(3*time.Hour), probe)
	api := ingest.Record{Server: ingest.ServerPublic, Method: "GET", Path: "/api/v1/", Status: 200, Duration: 1000}
	add(cells, day0.Add(3*time.Hour), api)
	static := ingest.Record{Server: ingest.ServerPublic, Method: "GET", Path: "/_next/static/a.js", Status: 200}
	add(cells, day0.Add(3*time.Hour), static)

	run, _ := st.StartRun(ctx, "test", "")
	if err := st.Write(ctx, storage.Flush{Cells: cells, Ingest: []storage.IngestDelta{
		{Minute: day0.Add(time.Hour).Unix() / 60, Received: 52, Accepted: 50, Malformed: 1, Dropped: 1},
		{Minute: day0.Add(2*time.Hour).Unix() / 60, Received: 3, Accepted: 3},
	}, Run: run, Heartbeat: day0.Add(10 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	// A second run after a 2-hour gap, ending at the end of the day.
	st2, _ := storage.Open(ctx, st.Path(), storage.Options{Now: func() time.Time { return day0.Add(12 * time.Hour) }})
	run2, _ := st2.StartRun(ctx, "test", "")
	st2.Write(ctx, storage.Flush{Run: run2, Heartbeat: day0.Add(24 * time.Hour).Unix()})
	st2.Close()

	for i, s := range []storage.Sample{
		{TS: day0.Add(time.Hour).Unix(), Run: run, Requests: 1000, Received: 10},
		{TS: day0.Add(time.Hour + time.Minute).Unix(), Run: run, Requests: 1100, Received: 108},
		{TS: day0.Add(time.Hour + 2*time.Minute).Unix(), Run: run, Requests: 1200, Received: 208},
		{TS: day0.Add(time.Hour + 3*time.Minute).Unix(), Run: run, Requests: 5, Received: 210}, // nginx restarted
		{TS: day0.Add(time.Hour + 4*time.Minute).Unix(), Run: run, Requests: 55, Received: 260},
	} {
		if err := st.AddSample(ctx, s); err != nil {
			t.Fatal(i, err)
		}
	}
	st.AddRelease(ctx, storage.ReleaseEvent{TS: day0.Add(-time.Hour).Unix(), ReleaseID: sha1, Info: []byte(`{}`)})
	st.AddRelease(ctx, storage.ReleaseEvent{TS: day0.Add(6 * time.Hour).Unix(), ReleaseID: sha2, Info: []byte(`{}`)})
	return st
}

func probeRuns() []ProbeRun {
	var runs []ProbeRun
	for i := 0; i < 48; i += 2 { // every other 30-minute slot: half the slots are missing
		slot := day0.Add(time.Duration(i) * 30 * time.Minute)
		ok := i != 10
		check := Check{Name: "home", URL: "https://aliakbari.dev/", OK: ok, Status: 200, DurationUS: uint64(80_000 + i*1000), Bytes: 40000}
		if !ok {
			check.Status, check.Error = 502, "status 502"
		}
		runs = append(runs, ProbeRun{
			Schema: ProbeSchema, RunID: fmt.Sprint(1000 + i), Attempt: 1, Slot: slot,
			StartedAt: slot.Add(3 * time.Minute), FinishedAt: slot.Add(3*time.Minute + 2*time.Second),
			Revision: sha1, RunnerEnvironment: "github-hosted", Target: "https://aliakbari.dev",
			Checks: []Check{check, {Name: "api", URL: "https://aliakbari.dev/api/v1/", OK: true, Status: 200, DurationUS: 50_000}},
			OK:     ok,
		})
	}
	return runs
}

func buildReport(t *testing.T) *Report {
	t.Helper()
	st := fixtureStore(t)
	w, _ := DayWindow(date)
	o, err := BuildOrigin(context.Background(), st, w, gen)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(date, "https://aliakbari.dev", gen, o, "complete", BuildProbe(probeRuns(), 30), "complete")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writeJSON(t *testing.T, path string, v any) []byte {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDerivedValuesFromFixture(t *testing.T) {
	r := buildReport(t)
	d := r.Origin.Derived
	// 50 + 502 + slow page + api = 53 eligible; the 403, bot, probe and static are excluded.
	if d.Availability.N != 53 || d.Availability.Bad != 1 {
		t.Fatalf("availability %+v", d.Availability)
	}
	if d.Latency.N != 52 || d.Latency.Good != 51 {
		t.Fatalf("latency %+v (the 900 ms page misses the 300 ms threshold)", d.Latency)
	}
	if d.Excluded.Blocked != 1 || d.Excluded.Bot != 1 || d.Excluded.Probe != 1 || d.Excluded.NotEligible != 1 {
		t.Fatalf("excluded %+v", d.Excluded)
	}
	if d.Ingest.Received != 55 || d.Ingest.Malformed != 1 || d.Ingest.Dropped != 1 {
		t.Fatalf("ingest %+v", d.Ingest)
	}
	// Two comparable intervals (100 + 100 requests, 98 + 100 received); the restart and the
	// interval after it are not comparable... the one after the restart is (5 → 55).
	if d.Reconciliation.NginxRequests != 250 || d.Reconciliation.Received != 248 {
		t.Fatalf("reconciliation %+v", d.Reconciliation)
	}
	if d.Reconciliation.UnreconciledSeconds != 60 {
		t.Fatalf("the nginx restart interval must be unreconciled: %+v", d.Reconciliation)
	}
	// Runs: 00:00–10:00 and 12:00–24:00; gap 10:00–12:00.
	if len(d.Coverage.Gaps) != 1 || d.Coverage.CoveredSeconds != 22*3600 {
		t.Fatalf("coverage %+v", d.Coverage)
	}
	if len(d.ActiveReleases) != 2 || d.ActiveReleases[0].ReleaseID != sha1 || !d.ActiveReleases[0].From.Equal(day0) {
		t.Fatalf("releases %+v", d.ActiveReleases)
	}
	p := r.Probe.Derived
	if p.ExpectedSlots != 48 || p.ObservedSlots != 24 || len(p.MissingSlots) != 24 {
		t.Fatalf("slots %+v", p)
	}
	if p.Runs.N != 24 || p.Runs.Bad != 1 || p.ZeroFailureMax != nil || len(p.Failures) != 1 {
		t.Fatalf("probe runs %+v", p.Runs)
	}
	if p.MaxDelaySec != 180 {
		t.Fatalf("delay %d", p.MaxDelaySec)
	}
}

func TestPublishedReportRecomputesCleanly(t *testing.T) {
	dir := t.TempDir()
	r := buildReport(t)
	writeJSON(t, filepath.Join(dir, "2026", "10", date+".json"), r)
	res, err := Recompute(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || res.DailyReports != 1 {
		t.Fatalf("clean report did not recompute: %+v", res.Discrepancies)
	}
}

// mutate decodes the published JSON into a generic tree, applies f and writes it back.
func mutate(t *testing.T, path string, f func(m map[string]any)) {
	t.Helper()
	b, _ := os.ReadFile(path)
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(path, out, 0o644)
}

func at(m map[string]any, path ...any) map[string]any {
	var cur any = m
	for _, p := range path {
		switch k := p.(type) {
		case string:
			cur = cur.(map[string]any)[k]
		case int:
			cur = cur.([]any)[k]
		}
	}
	return cur.(map[string]any)
}

func TestTamperingIsDetected(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"derived availability ratio": func(m map[string]any) {
			at(m, "origin", "derived", "availability")["ratio"] = json.Number("1")
		},
		"derived interval": func(m map[string]any) {
			at(m, "origin", "derived", "availability")["wilson95_lower"] = json.Number("0.99")
		},
		"raw count, derived unchanged": func(m map[string]any) {
			c := at(m, "origin", "hours", 1, "cells", 0)
			c["total"], c["ok"], c["s2xx"] = json.Number("51"), json.Number("51"), json.Number("51")
		},
		"server error hidden": func(m map[string]any) {
			c := at(m, "origin", "hours", 2, "cells", 0)
			c["server_error"], c["s5xx"] = json.Number("0"), json.Number("0")
		},
		"probe failure flipped": func(m map[string]any) {
			at(m, "probe", "runs", 5)["ok"] = true
		},
		"percentile bound": func(m map[string]any) {
			at(m, "origin", "derived", "percentiles", "page", "p95")["upper_us"] = json.Number("1")
		},
		"missing slot removed": func(m map[string]any) {
			d := at(m, "probe", "derived")
			d["missing_slots"] = d["missing_slots"].([]any)[1:]
		},
		"coverage": func(m map[string]any) {
			at(m, "origin", "derived", "coverage")["covered_seconds"] = json.Number("86400")
		},
		"unknown field": func(m map[string]any) { m["note"] = "trust me" },
		"wrong window": func(m map[string]any) {
			at(m, "window")["start"] = "2026-10-10T00:00:00Z"
		},
		"histogram count": func(m map[string]any) {
			at(m, "origin", "hours", 1, "cells", 0, "latency")["zero_count"] = json.Number("3")
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, date+".json")
			writeJSON(t, path, buildReport(t))
			mutate(t, path, f)
			res, err := Recompute(dir)
			if err != nil {
				t.Fatal(err)
			}
			if res.OK() {
				t.Fatal("tampered report recomputed cleanly")
			}
		})
	}
}

func TestMisnamedAndUnexpectedFiles(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "2026-10-12.json"), buildReport(t)) // date mismatch
	os.WriteFile(filepath.Join(dir, "notes.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "x.sigstore.json"), []byte(`not checked here`), 0o644)
	res, _ := Recompute(dir)
	var named, unexpected bool
	for _, d := range res.Discrepancies {
		named = named || d.Path == "date"
		unexpected = unexpected || strings.Contains(d.Problem, "unexpected file")
	}
	if !named || !unexpected {
		t.Fatalf("discrepancies %+v", res.Discrepancies)
	}
}

func TestSummaryRecomputesAndIsBoundToItsDays(t *testing.T) {
	dir := t.TempDir()
	r := buildReport(t)
	b := writeJSON(t, filepath.Join(dir, date+".json"), r)
	sum := sha256.Sum256(b)
	s, err := BuildSummary([]DayInput{{Report: r, SHA256: hex.EncodeToString(sum[:])}}, "2026-10-12", 3, 30, "https://aliakbari.dev", gen)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.MissingDays) != 2 || s.Origin.Coverage.WindowSeconds != 3*86400 {
		t.Fatalf("summary window %+v missing %v", s.Origin.Coverage, s.MissingDays)
	}
	// Two days without probe data still had 48 slots each.
	if s.Probe.ExpectedSlots != 48*3 {
		t.Fatalf("expected slots %d", s.Probe.ExpectedSlots)
	}
	if s.Origin.AvailabilityBudget.Eligible != 53 || !s.Origin.AvailabilityBudget.Defined {
		t.Fatalf("budget %+v", s.Origin.AvailabilityBudget)
	}
	sp := filepath.Join(dir, "summary.json")
	writeJSON(t, sp, s)
	if res, _ := Recompute(dir); !res.OK() {
		t.Fatalf("summary did not recompute: %+v", res.Discrepancies)
	}

	// Editing the summary is detected.
	mutate(t, sp, func(m map[string]any) { at(m, "origin", "availability")["bad"] = json.Number("0") })
	if res, _ := Recompute(dir); res.OK() {
		t.Fatal("edited summary recomputed cleanly")
	}
	writeJSON(t, sp, s)

	// Replacing a daily report after the summary was computed is detected, even if the
	// replacement recomputes on its own.
	r2 := buildReport(t)
	r2.Probe.Runs = r2.Probe.Runs[:3]
	Derive(r2)
	writeJSON(t, filepath.Join(dir, date+".json"), r2)
	res, _ := Recompute(dir)
	found := false
	for _, d := range res.Discrepancies {
		found = found || strings.Contains(d.Problem, "sha256 mismatch")
	}
	if !found {
		t.Fatalf("replacement not detected: %+v", res.Discrepancies)
	}
}

func TestReportWithoutOriginOrProbe(t *testing.T) {
	r, err := New(date, "https://aliakbari.dev", gen, nil, "missing: ledger not deployed", nil, "missing: no probe runs")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, date+".json"), r)
	if res, _ := Recompute(dir); !res.OK() {
		t.Fatalf("%+v", res.Discrepancies)
	}
}

func TestEmbeddedSchemasMatchPublishedSchemas(t *testing.T) {
	for _, name := range []string{"ops-rollup-v1.json", "ops-summary-v1.json", "probe-run-v1.json"} {
		published, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "schemas", name))
		if err != nil {
			t.Skipf("repository schemas not available: %v", err)
		}
		embedded, _ := schemaFS.ReadFile("schemas/" + name)
		if !bytes.Equal(published, embedded) {
			t.Errorf("%s: embedded copy differs from /schemas; run `cp schemas/%s services/ledger/internal/report/schemas/`", name, name)
		}
	}
}
