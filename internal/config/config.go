// Package config reads and validates `ledger serve` configuration from the
// environment. Every problem is reported at once, at startup.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config is the validated serve configuration.
type Config struct {
	Socket          string        // Unix datagram socket nginx logs to
	DB              string        // SQLite database path
	HTTPAddr        string        // public listener (proxied by nginx)
	AdminAddr       string        // private listener: /healthz, /readyz, /metrics
	NginxStatusURL  string        // nginx stub_status for reconciliation ("" disables it)
	ReleaseFile     string        // deployment record written by deploy.sh
	Service         string        // public origin, e.g. https://aliakbari.dev
	ReportsURL      string        // where signed reports are published
	Queue           int           // ingestion queue capacity
	RcvBuf          int           // socket receive buffer request (bytes)
	RetentionDays   int           // minute data kept in the database
	MaxDBBytes      int64         // database size limit
	FlushEvery      time.Duration // aggregator flush interval
	SampleEvery     time.Duration // reconciliation sample interval
	ShutdownTimeout time.Duration
	LogLevel        slog.Level
	LogFormat       string
}

// Load reads configuration through getenv.
func Load(getenv func(string) string) (Config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	var errs []error
	fail := func(k, f string, a ...any) { errs = append(errs, fmt.Errorf("%s: %s", k, fmt.Sprintf(f, a...))) }
	intIn := func(k, def string, lo, hi int) int {
		n, err := strconv.Atoi(get(k, def))
		if err != nil || n < lo || n > hi {
			fail(k, "must be an integer in [%d, %d]", lo, hi)
		}
		return n
	}
	durIn := func(k, def string, lo, hi time.Duration) time.Duration {
		d, err := time.ParseDuration(get(k, def))
		if err != nil || d < lo || d > hi {
			fail(k, "must be a duration in [%s, %s]", lo, hi)
		}
		return d
	}
	c := Config{
		Socket:         get("LEDGER_SOCKET", "/srv/ledger-socket/ingest.sock"),
		DB:             get("LEDGER_DB", "/var/lib/ledger/ledger.db"),
		HTTPAddr:       get("LEDGER_HTTP_ADDR", ":8090"),
		AdminAddr:      get("LEDGER_ADMIN_ADDR", ":9091"),
		NginxStatusURL: get("LEDGER_NGINX_STATUS_URL", ""),
		ReleaseFile:    get("LEDGER_RELEASE_FILE", "/srv/release/release.json"),
		Service:        get("LEDGER_SERVICE", "https://aliakbari.dev"),
		ReportsURL:     get("LEDGER_REPORTS_URL", "https://github.com/akynte/portfolio/tree/ops-reports"),
		LogFormat:      get("LOG_FORMAT", "json"),
	}
	c.Queue = intIn("LEDGER_QUEUE", "8192", 16, 1<<20)
	c.RcvBuf = intIn("LEDGER_RCVBUF", "4194304", 0, 64<<20)
	c.RetentionDays = intIn("LEDGER_RETENTION_DAYS", "35", 2, 400)
	c.MaxDBBytes = int64(intIn("LEDGER_MAX_DB_MB", "512", 16, 16384)) << 20
	c.FlushEvery = durIn("LEDGER_FLUSH_EVERY", "10s", time.Second, time.Minute)
	c.SampleEvery = durIn("LEDGER_SAMPLE_EVERY", "1m", 5*time.Second, 5*time.Minute)
	c.ShutdownTimeout = durIn("SHUTDOWN_TIMEOUT", "10s", time.Second, time.Minute)

	if !strings.HasPrefix(c.Socket, "/") || len(c.Socket) > 100 {
		fail("LEDGER_SOCKET", "must be an absolute path shorter than 100 bytes")
	}
	if !strings.HasPrefix(c.DB, "/") && !strings.HasPrefix(c.DB, "./") {
		fail("LEDGER_DB", "must be a path")
	}
	for k, v := range map[string]string{"LEDGER_HTTP_ADDR": c.HTTPAddr, "LEDGER_ADMIN_ADDR": c.AdminAddr} {
		if _, _, err := net.SplitHostPort(v); err != nil {
			fail(k, "must be host:port")
		}
	}
	if c.HTTPAddr == c.AdminAddr {
		fail("LEDGER_ADMIN_ADDR", "must differ from LEDGER_HTTP_ADDR")
	}
	if c.NginxStatusURL != "" {
		if u, err := url.Parse(c.NginxStatusURL); err != nil || u.Scheme != "http" || u.Host == "" {
			fail("LEDGER_NGINX_STATUS_URL", "must be an http:// URL on the private network")
		}
	}
	if u, err := url.Parse(c.Service); err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		fail("LEDGER_SERVICE", "must be an https origin without a path")
	}
	switch lvl := get("LOG_LEVEL", "info"); lvl {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		fail("LOG_LEVEL", "must be debug, info, warn or error")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		fail("LOG_FORMAT", "must be json or text")
	}
	return c, errors.Join(errs...)
}
