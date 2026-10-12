// Package httpapi serves opsledger's read-only public endpoints and its
// private admin endpoints.
//
// Public (proxied by nginx, see openapi.json):
//
//	GET /api/v1/ops/               index
//	GET /api/v1/ops/status         collection status (no traffic data)
//	GET /api/v1/ops/summary        rolling 28-day origin summary of completed days
//	GET /api/v1/ops/daily/{date}   one completed UTC day (origin observations)
//	GET /api/v1/release            the host's deployment record and how to verify it
//
// Only completed days are published: nothing here reveals the current
// day's traffic. Nothing here contains per-request or per-visitor data.
//
// Admin (never proxied): /healthz, /readyz, /metrics.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/akynte/opsledger/internal/release"
	"github.com/akynte/opsledger/internal/report"
	"github.com/akynte/opsledger/internal/storage"
)

// Options configure the public handler.
type Options struct {
	Store          *storage.Store
	Releases       *release.Watcher
	Service        string
	Generator      report.Generator
	RetentionDays  int
	ReportsURL     string // where signed daily reports are published
	RequestTimeout time.Duration
	MaxInflight    int
	Now            func() time.Time
	Log            *slog.Logger
}

type api struct {
	o        Options
	sem      chan struct{}
	mu       sync.Mutex
	daily    map[string]cached
	summary  cached
	sumUntil time.Time
}

type cached struct {
	body []byte
	etag string
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// NewPublic returns the public handler.
func NewPublic(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 10 * time.Second
	}
	if o.MaxInflight <= 0 {
		o.MaxInflight = 16
	}
	if o.RetentionDays <= 0 {
		o.RetentionDays = 35
	}
	a := &api{o: o, sem: make(chan struct{}, o.MaxInflight), daily: map[string]cached{}}
	return a.middleware(http.HandlerFunc(a.route))
}

func (a *api) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = newID()
		}
		h.Set("X-Request-ID", id)
		defer func() {
			if p := recover(); p != nil {
				a.o.Log.Error("panic", "path", r.URL.Path, "panic", p)
				writeError(w, id, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		select {
		case a.sem <- struct{}{}:
			defer func() { <-a.sem }()
		default:
			h.Set("Retry-After", "1")
			writeError(w, id, http.StatusServiceUnavailable, "overloaded", "too many concurrent requests")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), a.o.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *api) route(w http.ResponseWriter, r *http.Request) {
	id := w.Header().Get("X-Request-ID")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, id, http.StatusMethodNotAllowed, "method_not_allowed", "only GET and HEAD are supported")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, id, http.StatusBadRequest, "bad_request", "these endpoints take no query parameters")
		return
	}
	if r.ContentLength > 0 {
		writeError(w, id, http.StatusBadRequest, "bad_request", "request bodies are not accepted")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/api/v1/ops/":
		a.index(w, r)
	case p == "/api/v1/ops/status":
		a.status(w, r)
	case p == "/api/v1/ops/summary":
		a.summaryHandler(w, r)
	case strings.HasPrefix(p, "/api/v1/ops/daily/"):
		a.dailyHandler(w, r, strings.TrimPrefix(p, "/api/v1/ops/daily/"))
	case p == "/api/v1/release":
		a.releaseHandler(w, r)
	default:
		writeError(w, id, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func (a *api) index(w http.ResponseWriter, r *http.Request) {
	base := a.o.Service
	writeJSON(w, r, http.StatusOK, "public, max-age=300", map[string]any{
		"service":     "opsledger",
		"description": "Operational evidence for " + a.o.Service + ": origin observations of completed UTC days, and the deployment record.",
		"endpoints": map[string]string{
			"status":  base + "/api/v1/ops/status",
			"summary": base + "/api/v1/ops/summary",
			"daily":   base + "/api/v1/ops/daily/{YYYY-MM-DD}",
			"release": base + "/api/v1/release",
		},
		"schemas": map[string]string{
			"daily":   report.RollupSchema,
			"summary": report.SummarySchema,
			"release": release.RecordSchema,
		},
		"signed_reports": a.o.ReportsURL,
		"note":           "These endpoints are computed by the production host from its own observations and are not signed. Signed daily reports, which add an external probe, are published separately.",
	})
}

func (a *api) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	first, err := a.o.Store.FirstRun(ctx)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	out := map[string]any{
		"generator":      a.o.Generator,
		"retention_days": a.o.RetentionDays,
		"published_days": "completed UTC days only; the current day is never published",
	}
	if first > 0 {
		out["collecting_since"] = time.Unix(first, 0).UTC()
	} else {
		out["collecting_since"] = nil
	}
	if a.o.Store.Reset != "" {
		out["store_reset"] = "the database was unreadable at startup and was replaced; earlier unpublished data is unavailable"
	}
	if rec, _, err := a.o.Releases.Current(); err == nil {
		out["release"] = rec.ReleaseID
	} else {
		out["release"] = nil
	}
	writeJSON(w, r, http.StatusOK, "public, max-age=60", out)
}

// completedDay reports whether date is a full UTC day that has ended, with
// a margin for late records, and is still within retention.
func (a *api) completedDay(date string) (report.Window, bool) {
	wnd, err := report.DayWindow(date)
	if err != nil {
		return wnd, false
	}
	now := a.o.Now().UTC()
	if now.Before(wnd.End.Add(10 * time.Minute)) {
		return wnd, false
	}
	if wnd.Start.Before(now.AddDate(0, 0, -(a.o.RetentionDays - 1))) {
		return wnd, false
	}
	return wnd, true
}

func (a *api) buildDay(ctx context.Context, date string) (*report.Report, []byte, error) {
	wnd, _ := report.DayWindow(date)
	o, err := report.BuildOrigin(ctx, a.o.Store, wnd, a.o.Generator)
	if err != nil {
		return nil, nil, err
	}
	status := "complete"
	if o == nil {
		status = "missing: the ledger has no data for this day"
	} else if o.Derived.Coverage.Ratio < 1 {
		status = "partial"
	}
	rep, err := report.New(date, a.o.Service, a.o.Generator, o, status, nil, "not collected by the host: the external probe is added when reports are published")
	if err != nil {
		return nil, nil, err
	}
	if rep.Origin != nil && rep.Origin.Derived.Coverage.Ratio < 1 {
		rep.Status.Origin = "partial"
	}
	b, err := report.Marshal(rep)
	return rep, b, err
}

func (a *api) dailyHandler(w http.ResponseWriter, r *http.Request, date string) {
	id := w.Header().Get("X-Request-ID")
	if !dateRE.MatchString(date) {
		writeError(w, id, http.StatusBadRequest, "bad_request", "date must be YYYY-MM-DD")
		return
	}
	if _, ok := a.completedDay(date); !ok {
		writeError(w, id, http.StatusNotFound, "not_found", "only completed UTC days within the retention window are published")
		return
	}
	a.mu.Lock()
	c, ok := a.daily[date]
	a.mu.Unlock()
	if !ok {
		_, b, err := a.buildDay(r.Context(), date)
		if err != nil {
			a.internal(w, r, err)
			return
		}
		c = cached{body: b, etag: etag(b)}
		a.mu.Lock()
		a.daily[date] = c
		// Bound the cache to the retention window.
		if len(a.daily) > a.o.RetentionDays+2 {
			for k := range a.daily {
				if _, ok := a.completedDay(k); !ok {
					delete(a.daily, k)
				}
			}
		}
		a.mu.Unlock()
	}
	writeCached(w, r, c, "public, max-age=3600")
}

func (a *api) summaryHandler(w http.ResponseWriter, r *http.Request) {
	now := a.o.Now()
	a.mu.Lock()
	c, fresh := a.summary, now.Before(a.sumUntil) && a.summary.body != nil
	a.mu.Unlock()
	if !fresh {
		last := now.UTC().Add(-10*time.Minute).AddDate(0, 0, -1).Format("2006-01-02")
		days := min(28, a.o.RetentionDays-1)
		var inputs []report.DayInput
		lastT, _ := time.Parse("2006-01-02", last)
		for i := range days {
			date := lastT.AddDate(0, 0, -i).Format("2006-01-02")
			rep, b, err := a.buildDay(r.Context(), date)
			if err != nil {
				a.internal(w, r, err)
				return
			}
			if rep.Origin == nil {
				continue // no data that day: listed as missing
			}
			sum := sha256.Sum256(b)
			inputs = append(inputs, report.DayInput{Report: rep, SHA256: hex.EncodeToString(sum[:])})
		}
		s, err := report.BuildSummary(inputs, last, days, 0, a.o.Service, a.o.Generator)
		if err != nil {
			a.internal(w, r, err)
			return
		}
		b, err := report.Marshal(s)
		if err != nil {
			a.internal(w, r, err)
			return
		}
		c = cached{body: b, etag: etag(b)}
		a.mu.Lock()
		a.summary, a.sumUntil = c, now.Add(5*time.Minute)
		a.mu.Unlock()
	}
	writeCached(w, r, c, "public, max-age=300")
}

// ReleaseTrust explains what /api/v1/release proves.
const ReleaseTrust = "Reported by the production host. The image digests can be checked independently: each was " +
	"signed by CI on a GitHub-hosted runner (see verify). That this host is running exactly these images is the " +
	"host's own statement; it is not independently attested."

func (a *api) releaseHandler(w http.ResponseWriter, r *http.Request) {
	id := w.Header().Get("X-Request-ID")
	rec, _, err := a.o.Releases.Current()
	if err != nil {
		if errors.Is(err, release.ErrNoRecord) {
			writeError(w, id, http.StatusNotFound, "not_found", "no deployment record on this host yet")
			return
		}
		writeError(w, id, http.StatusServiceUnavailable, "unavailable", "the deployment record is unreadable")
		return
	}
	verify := map[string]string{
		"manifest": "docker pull ghcr.io/akynte/aliakbari-release:" + rec.Commit +
			"  (contains release.json and release.json.sigstore.json; requires access to the package while the repository is private)",
		"signature": "cosign verify-blob --bundle release.json.sigstore.json --certificate-identity " +
			"https://github.com/akynte/portfolio/.github/workflows/ci.yml@refs/heads/main --certificate-oidc-issuer " +
			"https://token.actions.githubusercontent.com --certificate-github-workflow-sha " + rec.Commit + " release.json",
		"digests": "compare the images in release.json with the images listed here",
	}
	writeJSON(w, r, http.StatusOK, "no-store", map[string]any{
		"reported_by": "host",
		"trust":       ReleaseTrust,
		"release":     rec,
		"verify":      verify,
	})
}

func (a *api) internal(w http.ResponseWriter, r *http.Request, err error) {
	id := w.Header().Get("X-Request-ID")
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, id, http.StatusServiceUnavailable, "timeout", "the request took too long")
		return
	}
	a.o.Log.Error("request failed", "path", r.URL.Path, "err", err)
	writeError(w, id, http.StatusInternalServerError, "internal", "internal error")
}

func etag(b []byte) string {
	s := sha256.Sum256(b)
	return `"` + hex.EncodeToString(s[:16]) + `"`
}

func writeCached(w http.ResponseWriter, r *http.Request, c cached, cacheControl string) {
	h := w.Header()
	h.Set("ETag", c.etag)
	h.Set("Cache-Control", cacheControl)
	if match := r.Header.Get("If-None-Match"); match != "" && match == c.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(c.body)
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, cacheControl string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		writeError(w, w.Header().Get("X-Request-ID"), http.StatusInternalServerError, "internal", "encoding failed")
		return
	}
	b = append(b, '\n')
	if status == http.StatusOK {
		writeCached(w, r, cached{body: b, etag: etag(b)}, cacheControl)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(b)
	}
}

func writeError(w http.ResponseWriter, id string, status int, code, msg string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": msg, "request_id": id}})
	w.Write(append(b, '\n'))
}

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

func validRequestID(s string) bool { return requestIDRE.MatchString(s) }

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Admin

// Gauge is a named value for /metrics.
type Gauge struct {
	Name, Help, Type string
	Value            func() float64
}

// NewAdmin returns the admin handler. ready reports readiness; metrics are
// rendered in the Prometheus text exposition format.
func NewAdmin(ready func() error, metrics []Gauge, buildInfo map[string]string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := ready(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			b, _ := json.Marshal(map[string]string{"status": "not_ready", "reason": err.Error()})
			w.Write(append(b, '\n'))
			return
		}
		w.Write([]byte(`{"status":"ready"}` + "\n"))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var sb strings.Builder
		for _, g := range metrics {
			fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", g.Name, g.Help, g.Name, g.Type, g.Name, g.Value())
		}
		labels := make([]string, 0, len(buildInfo))
		for _, k := range []string{"version", "commit", "go"} {
			labels = append(labels, fmt.Sprintf("%s=%q", k, buildInfo[k]))
		}
		fmt.Fprintf(&sb, "# HELP ledger_build_info Build information.\n# TYPE ledger_build_info gauge\nledger_build_info{%s} 1\n", strings.Join(labels, ","))
		w.Write([]byte(sb.String()))
	})
	return mux
}
