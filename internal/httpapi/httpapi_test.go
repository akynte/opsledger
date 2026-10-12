package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/release"
	"github.com/akynte/opsledger/internal/report"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
	"github.com/akynte/opsledger/internal/storage"
)

var now = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func setup(t *testing.T) (http.Handler, string, *release.Watcher) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := storage.Open(ctx, filepath.Join(dir, "l.db"), storage.Options{Now: func() time.Time { return now.Add(-30 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	run, _ := st.StartRun(ctx, "test", "")
	cells := map[storage.MinuteKey]*series.Cell{}
	k := sli.Key{Class: sli.Page, Observer: sli.Organic, Agent: sli.Human}
	// Yesterday and today: today's data must never be served.
	for _, at := range []time.Time{now.Add(-20 * time.Hour), now.Add(-time.Hour)} {
		c := series.New()
		c.Add(k, ingest.Record{Status: 200, Duration: 5000})
		cells[storage.MinuteKey{Minute: at.Unix() / 60, Key: k}] = c
	}
	if err := st.Write(ctx, storage.Flush{Cells: cells, Run: run, Heartbeat: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	releaseFile := filepath.Join(dir, "release.json")
	w := &release.Watcher{Path: releaseFile, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.Check(ctx)
	h := NewPublic(Options{Store: st, Releases: w, Service: "https://aliakbari.dev",
		Generator: report.Generator{Name: "opsledger", Version: "test", Commit: "unknown"},
		Now:       func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return h, releaseFile, w
}

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCompletedDayIsServedAndRecomputes(t *testing.T) {
	h, _, _ := setup(t)
	rec := get(h, "/api/v1/ops/daily/2026-10-11")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("ETag") == "" || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") {
		t.Fatal("missing caching headers")
	}
	// What the API serves must pass the same verification a reviewer runs.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "2026-10-11.json"), rec.Body.Bytes(), 0o644)
	res, err := report.Recompute(dir)
	if err != nil || !res.OK() {
		t.Fatalf("served report does not recompute: %v %+v", err, res.Discrepancies)
	}
	var r report.Report
	json.Unmarshal(rec.Body.Bytes(), &r)
	if r.Origin == nil || r.Origin.Derived.Availability.N != 1 || r.Probe != nil {
		t.Fatalf("unexpected content: %+v", r.Origin)
	}
	// Conditional request.
	again := get(h, "/api/v1/ops/daily/2026-10-11", "If-None-Match", rec.Header().Get("ETag"))
	if again.Code != http.StatusNotModified {
		t.Fatalf("conditional request: %d", again.Code)
	}
}

func TestCurrentAndFutureDaysAreNeverServed(t *testing.T) {
	h, _, _ := setup(t)
	for _, d := range []string{"2026-10-12", "2026-10-13", "2025-01-01"} {
		if rec := get(h, "/api/v1/ops/daily/"+d); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", d, rec.Code)
		}
	}
}

func TestStrictRequests(t *testing.T) {
	h, _, _ := setup(t)
	cases := map[string]int{
		"/api/v1/ops/daily/yesterday":      400,
		"/api/v1/ops/daily/2026-10-11?x=1": 400,
		"/api/v1/ops/summary?refresh=1":    400,
		"/api/v1/ops/unknown":              404,
		"/api/v1/ops":                      404,
	}
	for path, want := range cases {
		if rec := get(h, path); rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/status", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing on errors")
	}
}

func TestSummaryAndStatusExposeNoCurrentTraffic(t *testing.T) {
	h, _, _ := setup(t)
	rec := get(h, "/api/v1/ops/summary")
	if rec.Code != 200 {
		t.Fatalf("summary %d: %s", rec.Code, rec.Body)
	}
	var s report.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Window.LastDate != "2026-10-11" || s.Origin == nil || s.Origin.Availability.N != 1 {
		t.Fatalf("summary must cover completed days only: last=%s origin=%+v", s.Window.LastDate, s.Origin)
	}
	st := get(h, "/api/v1/ops/status")
	if st.Code != 200 || strings.Contains(st.Body.String(), `"availability"`) {
		t.Fatalf("status leaks traffic data or failed: %d %s", st.Code, st.Body)
	}
}

func TestReleaseEndpoint(t *testing.T) {
	h, file, w := setup(t)
	if rec := get(h, "/api/v1/release"); rec.Code != 404 {
		t.Fatalf("no record: %d", rec.Code)
	}
	commit := strings.Repeat("a", 40)
	img := func(s string, d string) string {
		return "ghcr.io/akynte/aliakbari-" + s + "@sha256:" + strings.Repeat(d, 64)
	}
	r := release.Record{Schema: release.RecordSchema, ReleaseID: commit, Repository: "akynte/portfolio", Commit: commit,
		DeployedAt: now, Images: map[string]string{"web": img("web", "1"), "api": img("api", "2"), "nginx": img("nginx", "3"), "ledger": img("ledger", "4")},
		Verification: release.Verification{Status: "unverified", Method: "none"}}
	b, _ := json.Marshal(r)
	os.WriteFile(file, b, 0o644)
	w.Check(context.Background())
	rec := get(h, "/api/v1/release")
	if rec.Code != 200 {
		t.Fatalf("release: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"reported_by": "host"`, "not independently attested", commit, "cosign verify-blob"} {
		if !strings.Contains(body, want) {
			t.Errorf("release response lacks %q", want)
		}
	}
	for _, secret := range []string{"ghp_", "ghs_", "bearer", "password", "/srv/", "62.171", "deployer"} {
		if strings.Contains(strings.ToLower(body), secret) {
			t.Errorf("release response contains %q", secret)
		}
	}
}

func TestOverloadIsRejectedNotQueued(t *testing.T) {
	block := make(chan struct{})
	a := &api{o: Options{RequestTimeout: time.Second, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, sem: make(chan struct{}, 1)}
	h := a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	time.Sleep(20 * time.Millisecond)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	close(block)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("overload: %d", rec.Code)
	}
}

func TestAdmin(t *testing.T) {
	ready := errors.New("starting")
	h := NewAdmin(func() error { return ready }, []Gauge{{Name: "ledger_x_total", Help: "x", Type: "counter", Value: func() float64 { return 3 }}},
		map[string]string{"version": "v", "commit": "c", "go": "go1"})
	if rec := get(h, "/readyz"); rec.Code != 503 {
		t.Fatalf("readyz while starting: %d", rec.Code)
	}
	ready = nil
	if rec := get(h, "/readyz"); rec.Code != 200 {
		t.Fatalf("readyz: %d", rec.Code)
	}
	m := get(h, "/metrics").Body.String()
	if !strings.Contains(m, "ledger_x_total 3") || !strings.Contains(m, `ledger_build_info{version="v",commit="c",go="go1"} 1`) {
		t.Fatalf("metrics:\n%s", m)
	}
}
