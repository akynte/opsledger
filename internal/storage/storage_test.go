package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
)

var t0 = time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)

func openTemp(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return t0 }
	}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func cellOf(statuses ...int) *series.Cell {
	c := series.New()
	k := sli.Key{Class: sli.Page, Observer: sli.Organic, Agent: sli.Human}
	for i, st := range statuses {
		c.Add(k, ingest.Record{Status: st, Duration: uint64(1000 * (i + 1))})
	}
	return c
}

var pageKey = sli.Key{Class: sli.Page, Observer: sli.Organic, Agent: sli.Human}

func TestWriteMergesLateRecordsIntoExistingMinutes(t *testing.T) {
	s := openTemp(t, Options{})
	ctx := context.Background()
	m := t0.Unix() / 60
	mk := MinuteKey{Minute: m, Key: pageKey}
	if err := s.Write(ctx, Flush{Cells: map[MinuteKey]*series.Cell{mk: cellOf(200, 200, 500)}}); err != nil {
		t.Fatal(err)
	}
	// A late record for the same minute arrives after the first flush.
	if err := s.Write(ctx, Flush{Cells: map[MinuteKey]*series.Cell{mk: cellOf(404)}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Rows(ctx, m, m+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	c := rows[0].Cell
	if c.Total != 4 || c.S5xx != 1 || c.S4xx != 1 || c.S2xx != 2 || c.Latency.Count() != 4 {
		t.Fatalf("merged cell %+v", c)
	}
	if msg := c.Consistent(); msg != "" {
		t.Fatal(msg)
	}
}

func TestIngestCountersAccumulate(t *testing.T) {
	s := openTemp(t, Options{})
	ctx := context.Background()
	m := t0.Unix() / 60
	for range 3 {
		if err := s.Write(ctx, Flush{Ingest: []IngestDelta{{Minute: m, Received: 10, Accepted: 8, Malformed: 1, Dropped: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.IngestTotals(ctx, m, m+1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Received != 30 || d.Accepted != 24 || d.Malformed != 3 || d.Dropped != 3 {
		t.Fatalf("%+v", d)
	}
}

func TestCorruptDatabaseIsMovedAsideAndRecreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("not a database ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatalf("Open must recover from a corrupt file: %v", err)
	}
	defer s.Close()
	if s.Reset == "" {
		t.Fatal("Reset must name the file the corrupt database was moved to")
	}
	if _, err := os.Stat(filepath.Join(dir, s.Reset)); err != nil {
		t.Fatalf("corrupt file not kept for inspection: %v", err)
	}
	if _, err := s.StartRun(context.Background(), "test", ""); err != nil {
		t.Fatalf("recreated database is not usable: %v", err)
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec("PRAGMA user_version = 99")
	s.Close()
	// A database from a newer ledger is not corrupt; it must be moved aside rather than
	// half-migrated by an older binary, and the move must be visible.
	s2, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Reset == "" {
		t.Fatal("expected the newer database to be set aside")
	}
}

func TestSizeLimitMakesWritesFailWithoutCorruptingTheDatabase(t *testing.T) {
	s := openTemp(t, Options{MaxBytes: 64 << 10})
	ctx := context.Background()
	var failed error
	for m := int64(0); m < 5000 && failed == nil; m++ {
		cells := map[MinuteKey]*series.Cell{}
		for _, c := range sli.Classes {
			cells[MinuteKey{Minute: t0.Unix()/60 + m, Key: sli.Key{Class: c, Observer: sli.Organic, Agent: sli.Human}}] = cellOf(200, 301, 404, 500)
		}
		failed = s.Write(ctx, Flush{Cells: cells})
	}
	if failed == nil {
		t.Fatal("expected writes to fail once the size limit is reached")
	}
	if !strings.Contains(failed.Error(), "full") {
		t.Fatalf("unexpected error: %v", failed)
	}
	size, _ := s.SizeBytes(ctx)
	if size > 64<<10 {
		t.Fatalf("database grew to %d bytes past the limit", size)
	}
	var check string
	s.db.QueryRow("PRAGMA quick_check").Scan(&check)
	if check != "ok" {
		t.Fatalf("database damaged by a failed write: %s", check)
	}
}

func TestPruneRemovesOldDataButKeepsReleases(t *testing.T) {
	now := t0
	s := openTemp(t, Options{Now: func() time.Time { return now }})
	ctx := context.Background()
	old := t0.Add(-40 * 24 * time.Hour)
	if err := s.Write(ctx, Flush{
		Cells:  map[MinuteKey]*series.Cell{{Minute: old.Unix() / 60, Key: pageKey}: cellOf(200)},
		Ingest: []IngestDelta{{Minute: old.Unix() / 60, Received: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRelease(ctx, ReleaseEvent{TS: old.Unix(), ReleaseID: strings.Repeat("a", 40), Info: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, Flush{Cells: map[MinuteKey]*series.Cell{{Minute: t0.Unix() / 60, Key: pageKey}: cellOf(200)}}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(ctx, 35*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d rows, want 2", n)
	}
	rows, _ := s.Rows(ctx, 0, 1<<40)
	if len(rows) != 1 {
		t.Fatalf("%d rows left", len(rows))
	}
	rel, _ := s.Releases(ctx, t0.Unix(), t0.Unix()+86400)
	if len(rel) != 1 {
		t.Fatal("the release active at the window start must survive pruning")
	}
}

func TestReleasesIncludesTheOneActiveAtWindowStart(t *testing.T) {
	s := openTemp(t, Options{})
	ctx := context.Background()
	for i, ts := range []int64{100, 200, 300, 400} {
		id := strings.Repeat(string(rune('a'+i)), 40)
		if err := s.AddRelease(ctx, ReleaseEvent{TS: ts, ReleaseID: id, Info: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Releases(ctx, 250, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].TS != 200 || got[1].TS != 300 {
		t.Fatalf("got %+v", got)
	}
	latest, ok, err := s.LatestRelease(ctx)
	if err != nil || !ok || latest.TS != 400 {
		t.Fatalf("latest %+v %v %v", latest, ok, err)
	}
}

func TestRunsAndSamples(t *testing.T) {
	now := t0
	s := openTemp(t, Options{Now: func() time.Time { return now }})
	ctx := context.Background()
	r1, _ := s.StartRun(ctx, "v1", "")
	r2, _ := s.StartRun(ctx, "v1", "") // same second: must not collide
	if r1 == r2 {
		t.Fatal("run ids collide")
	}
	now = t0.Add(time.Hour)
	if err := s.StopRun(ctx, r1); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx, t0.Unix(), t0.Unix()+86400)
	if len(runs) != 2 || runs[0].Stopped == nil || runs[1].Stopped != nil {
		t.Fatalf("runs %+v", runs)
	}
	for i := range 3 {
		if err := s.AddSample(ctx, Sample{TS: t0.Unix() + int64(60*i), Run: r1, Requests: uint64(100 * i), Received: uint64(99 * i)}); err != nil {
			t.Fatal(err)
		}
	}
	smp, _ := s.Samples(ctx, t0.Unix(), t0.Unix()+3600)
	if len(smp) != 3 || smp[2].Requests != 200 {
		t.Fatalf("samples %+v", smp)
	}
}
