package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type site struct {
	homeStatus int
	snapshot   string
	redirect   bool
	seenProbe  string
}

func (s *site) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.seenProbe = r.Header.Get("X-Opsledger-Probe")
		switch r.URL.Path {
		case "/":
			if s.redirect {
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(s.homeStatus)
			w.Write([]byte(`<html><h1>Ali</h1><footer>snapshot ` + id[:12] + `</footer></html>`))
		case "/review", "/proof":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<h1>x</h1>`))
		case "/resume.pdf":
			w.Write([]byte("%PDF-1.7 ..."))
		case "/api/v1/snapshot":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"snapshot_id":"` + s.snapshot + `"}`))
		case "/api/v1/":
			w.Write([]byte(`{"service":"x"}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func runAgainst(t *testing.T, s *site, mod func(*Config)) map[string]string {
	t.Helper()
	srv := httptest.NewTLSServer(s.handler())
	t.Cleanup(srv.Close)
	cfg := Config{
		Target: "https://aliakbari.dev", IntervalMinutes: 30, RunID: "42", Revision: strings.Repeat("a", 40),
		RunnerEnvironment: "local", ResolveTo: strings.TrimPrefix(srv.URL, "https://"), InsecureTLS: true,
		Now: func() time.Time { return time.Date(2026, 10, 12, 10, 47, 3, 0, time.UTC) },
	}
	if mod != nil {
		mod(&cfg)
	}
	run := Run(context.Background(), cfg)
	if !run.Slot.Equal(time.Date(2026, 10, 12, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("slot %v", run.Slot)
	}
	out := map[string]string{}
	all := true
	for _, c := range run.Checks {
		out[c.Name] = c.Error
		all = all && c.OK
		if c.OK != (c.Error == "") {
			t.Fatalf("check %s: ok=%v but error %q", c.Name, c.OK, c.Error)
		}
	}
	if run.OK != all {
		t.Fatal("run.OK disagrees with its checks")
	}
	if s.seenProbe != "42" {
		t.Fatalf("probe header %q", s.seenProbe)
	}
	return out
}

func TestHealthySite(t *testing.T) {
	got := runAgainst(t, &site{homeStatus: 200, snapshot: id}, nil)
	for name, e := range got {
		if e != "" {
			t.Errorf("%s failed: %s", name, e)
		}
	}
	if len(got) != 8 {
		t.Fatalf("%d checks", len(got))
	}
}

func TestDetectsFailures(t *testing.T) {
	if got := runAgainst(t, &site{homeStatus: 502, snapshot: id}, nil); got["home"] == "" || got["snapshot-consistency"] == "" {
		t.Fatalf("502 not detected: %v", got)
	}
	other := strings.Repeat("f", 64)
	if got := runAgainst(t, &site{homeStatus: 200, snapshot: other}, nil); !strings.Contains(got["snapshot-consistency"], "different snapshots") {
		t.Fatalf("snapshot mismatch not detected: %v", got)
	}
	if got := runAgainst(t, &site{homeStatus: 200, snapshot: "bad"}, nil); got["api-snapshot"] == "" {
		t.Fatalf("invalid snapshot id not detected: %v", got)
	}
	if got := runAgainst(t, &site{homeStatus: 200, snapshot: id, redirect: true}, nil); got["home"] == "" {
		t.Fatalf("redirect accepted: %v", got)
	}
	got := runAgainst(t, &site{homeStatus: 200, snapshot: id}, func(c *Config) { c.MinCertDays = 365 * 100 })
	if !strings.Contains(got["tls"], "certificate expires") {
		t.Fatalf("short certificate validity not detected: %v", got)
	}
}

func TestUnreachableSite(t *testing.T) {
	run := Run(context.Background(), Config{Target: "https://aliakbari.dev", RunID: "1", ResolveTo: "127.0.0.1:1",
		Timeout: 2 * time.Second, Revision: "unknown", RunnerEnvironment: "local"})
	if run.OK || len(run.Checks) == 0 {
		t.Fatalf("unreachable site reported OK: %+v", run)
	}
	for _, c := range run.Checks {
		if c.OK {
			t.Errorf("check %s passed against an unreachable site", c.Name)
		}
	}
}
