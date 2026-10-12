package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/akynte/opsledger/internal/aggregate"
	"github.com/akynte/opsledger/internal/config"
	"github.com/akynte/opsledger/internal/httpapi"
	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/reconcile"
	"github.com/akynte/opsledger/internal/release"
	"github.com/akynte/opsledger/internal/report"
	"github.com/akynte/opsledger/internal/storage"
)

func newLogger(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// serve runs until SIGINT/SIGTERM. Shutdown order matters:
//
//  1. HTTP servers stop accepting and drain;
//  2. the socket reader stops (nginx's records now fail fast on its side);
//  3. the aggregator drains the queue and writes everything it holds,
//     including the current partial minute;
//  4. the run is marked as cleanly stopped and the WAL is checkpointed.
func serve(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "ledger serve takes no arguments; configure it with environment variables")
		return exitUsage
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "ledger: invalid configuration:\n%v\n", err)
		return exitUsage
	}
	log := newLogger(stdout, cfg.LogLevel, cfg.LogFormat)
	gen := report.Generator{Name: "opsledger", Version: version, Commit: commitOrUnknown()}
	log.Info("starting", "version", version, "commit", commit)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := storage.Open(ctx, cfg.DB, storage.Options{MaxBytes: cfg.MaxDBBytes})
	if err != nil {
		log.Error("cannot open database", "path", cfg.DB, "err", err)
		return exitFailure
	}
	defer st.Close()
	note := ""
	if st.Reset != "" {
		note = "database replaced at startup; previous file kept as " + st.Reset
		log.Error("database was unreadable and has been replaced", "kept_as", st.Reset)
	}
	runID, err := st.StartRun(ctx, version, note)
	if err != nil {
		log.Error("cannot record run", "err", err)
		return exitFailure
	}

	queue := make(chan ingest.Record, cfg.Queue)
	counters := new(ingest.Counters)
	reader, err := ingest.Listen(cfg.Socket, cfg.RcvBuf, queue, counters, log)
	if err != nil {
		log.Error("cannot listen on socket", "path", cfg.Socket, "err", err)
		return exitFailure
	}
	agg := aggregate.New(aggregate.Config{FlushEvery: cfg.FlushEvery}, queue, counters, st, runID, log)
	watcher := &release.Watcher{Path: cfg.ReleaseFile, Store: st, Log: log}
	var poller *reconcile.Poller
	if cfg.NginxStatusURL != "" {
		poller = &reconcile.Poller{URL: cfg.NginxStatusURL, Every: cfg.SampleEvery, Counters: counters, Store: st, RunID: runID, Log: log,
			Client: &http.Client{Timeout: 5 * time.Second}}
	}

	public := httpapi.NewPublic(httpapi.Options{
		Store: st, Releases: watcher, Service: cfg.Service, Generator: gen, RetentionDays: cfg.RetentionDays,
		ReportsURL: cfg.ReportsURL, Log: log,
	})
	started := time.Now()
	ready := func() error {
		last := agg.Stats.LastFlush.Load()
		limit := 3*cfg.FlushEvery + 10*time.Second
		if last == 0 {
			if time.Since(started) > limit {
				return errors.New("no successful write since start")
			}
			return nil
		}
		if age := time.Since(time.Unix(last, 0)); age > limit {
			return fmt.Errorf("last successful write %s ago", age.Round(time.Second))
		}
		return nil
	}
	admin := httpapi.NewAdmin(ready, gauges(counters, queue, agg, poller, st), map[string]string{
		"version": version, "commit": commit, "go": runtime.Version(),
	})

	pubLn, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		log.Error("cannot listen", "addr", cfg.HTTPAddr, "err", err)
		return exitFailure
	}
	admLn, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		pubLn.Close()
		log.Error("cannot listen", "addr", cfg.AdminAddr, "err", err)
		return exitFailure
	}
	mkServer := func(h http.Handler) *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	}
	pubSrv, admSrv := mkServer(public), mkServer(admin)

	// Background work. The reader and aggregator have their own contexts so
	// they can be stopped in order; the rest stop with ctx.
	readerCtx, stopReader := context.WithCancel(context.Background())
	aggCtx, stopAgg := context.WithCancel(context.Background())
	fatal := make(chan error, 4)
	var readerWG, aggWG, bgWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		if err := reader.Run(readerCtx); err != nil {
			fatal <- fmt.Errorf("reader: %w", err)
		}
	}()
	aggWG.Add(1)
	go func() { defer aggWG.Done(); agg.Run(aggCtx) }()
	bgWG.Add(2)
	go func() { defer bgWG.Done(); watcher.Run(ctx) }()
	go func() { defer bgWG.Done(); prune(ctx, st, cfg.RetentionDays, log) }()
	if poller != nil {
		bgWG.Add(1)
		go func() { defer bgWG.Done(); poller.Run(ctx) }()
	}
	for _, s := range []struct {
		srv *http.Server
		ln  net.Listener
	}{{pubSrv, pubLn}, {admSrv, admLn}} {
		go func() {
			if err := s.srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
				fatal <- fmt.Errorf("http: %w", err)
			}
		}()
	}
	log.Info("serving", "socket", cfg.Socket, "http", pubLn.Addr().String(), "admin", admLn.Addr().String(),
		"reconcile", cfg.NginxStatusURL != "")

	exit := exitOK
	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-fatal:
		log.Error("fatal", "err", err)
		exit = exitFailure
		stop()
	}

	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	pubSrv.Shutdown(sctx)
	stopReader()
	readerWG.Wait()
	stopAgg()
	aggWG.Wait()
	bgWG.Wait()
	admSrv.Shutdown(sctx)
	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fcancel()
	if err := st.StopRun(fctx, runID); err != nil {
		log.Warn("cannot record clean stop", "err", err)
	}
	st.Checkpoint(fctx)
	s := counters.Load()
	log.Info("stopped", "received", s.Received, "accepted", s.Accepted, "malformed", s.Malformed, "dropped", s.Dropped,
		"flush_failures", agg.Stats.FlushFailures.Load(), "lost", agg.Stats.Lost.Load())
	return exit
}

// prune deletes data beyond retention once an hour.
func prune(ctx context.Context, st *storage.Store, days int, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.Prune(ctx, time.Duration(days)*24*time.Hour); err != nil {
			log.Warn("prune failed", "err", err)
		} else if n > 0 {
			log.Info("pruned expired data", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func gauges(c *ingest.Counters, q chan ingest.Record, a *aggregate.Aggregator, p *reconcile.Poller, st *storage.Store) []httpapi.Gauge {
	f := func(v uint64) float64 { return float64(v) }
	g := []httpapi.Gauge{
		gauge("ledger_ingest_received_total", "Datagrams read from the socket.", "counter", func() float64 { return f(c.Received.Load()) }),
		gauge("ledger_ingest_accepted_total", "Records parsed and queued.", "counter", func() float64 { return f(c.Accepted.Load()) }),
		gauge("ledger_ingest_malformed_total", "Datagrams that failed to parse.", "counter", func() float64 { return f(c.Malformed.Load()) }),
		gauge("ledger_ingest_dropped_total", "Records dropped because the queue was full.", "counter", func() float64 { return f(c.Dropped.Load()) }),
		gauge("ledger_queue_length", "Records waiting for the aggregator.", "gauge", func() float64 { return float64(len(q)) }),
		gauge("ledger_queue_capacity", "Queue capacity.", "gauge", func() float64 { return float64(cap(q)) }),
		gauge("ledger_aggregate_records_total", "Records aggregated.", "counter", func() float64 { return f(a.Stats.Records.Load()) }),
		gauge("ledger_aggregate_rejected_total", "Records with implausible timestamps.", "counter", func() float64 { return f(a.Stats.Rejected.Load()) }),
		gauge("ledger_flushes_total", "Successful database writes.", "counter", func() float64 { return f(a.Stats.Flushes.Load()) }),
		gauge("ledger_flush_failures_total", "Failed database writes.", "counter", func() float64 { return f(a.Stats.FlushFailures.Load()) }),
		gauge("ledger_lost_records_total", "Records discarded after storage failed for too long.", "counter", func() float64 { return f(a.Stats.Lost.Load()) }),
		gauge("ledger_open_minutes", "Minutes held in memory.", "gauge", func() float64 { return float64(a.Stats.OpenMinutes.Load()) }),
		gauge("ledger_last_flush_timestamp_seconds", "Time of the last successful write.", "gauge", func() float64 { return float64(a.Stats.LastFlush.Load()) }),
		gauge("ledger_db_size_bytes", "Database size.", "gauge", func() float64 {
			n, _ := st.SizeBytes(context.Background())
			return float64(n)
		}),
	}
	if p != nil {
		g = append(g,
			httpapi.Gauge{Name: "ledger_reconcile_samples_total", Help: "nginx status samples stored.", Type: "counter", Value: func() float64 { return f(p.Samples.Load()) }},
			httpapi.Gauge{Name: "ledger_reconcile_failures_total", Help: "nginx status samples that failed.", Type: "counter", Value: func() float64 { return f(p.Failures.Load()) }},
		)
	}
	return g
}

func gauge(name, help, typ string, v func() float64) httpapi.Gauge {
	return httpapi.Gauge{Name: name, Help: help, Type: typ, Value: v}
}

func healthcheck(getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "ledger: %v\n", err)
		return exitFailure
	}
	host, port, _ := net.SplitHostPort(cfg.AdminAddr)
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
	if err != nil {
		fmt.Fprintf(stderr, "ledger: healthcheck: %v\n", err)
		return exitFailure
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "ledger: healthcheck: %s\n", resp.Status)
		return exitFailure
	}
	return exitOK
}

func commitOrUnknown() string {
	if len(commit) == 40 {
		return commit
	}
	return "unknown"
}
