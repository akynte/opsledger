// Package aggregate turns the stream of parsed records into per-minute
// cells and writes closed minutes to storage.
//
// One goroutine owns all aggregation state, so there are no locks: the
// reader hands records over through a bounded channel, and the aggregator
// alone touches the open minutes. Writes happen on a timer, never per
// record, so a slow disk delays a flush but never stalls the socket reader.
package aggregate

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
	"github.com/akynte/opsledger/internal/storage"
)

// Writer is the storage the aggregator flushes to.
type Writer interface {
	Write(ctx context.Context, f storage.Flush) error
}

// Config tunes the aggregator. Zero values take the defaults.
type Config struct {
	FlushEvery     time.Duration // how often closed minutes are written (default 10s)
	Grace          time.Duration // how long after its end a minute stays open (default 5s)
	MaxOpenMinutes int           // memory bound while storage is failing (default 180)
	MaxFuture      time.Duration // records further in the future are rejected (default 2m)
	MaxAge         time.Duration // records older than this are rejected (default 24h)
	Now            func() time.Time
}

func (c *Config) defaults() {
	if c.FlushEvery <= 0 {
		c.FlushEvery = 10 * time.Second
	}
	if c.Grace <= 0 {
		c.Grace = 5 * time.Second
	}
	if c.MaxOpenMinutes <= 0 {
		c.MaxOpenMinutes = 180
	}
	if c.MaxFuture <= 0 {
		c.MaxFuture = 2 * time.Minute
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 24 * time.Hour
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Stats are cumulative counters for the admin /metrics endpoint.
type Stats struct {
	Records       atomic.Uint64 // records aggregated
	Rejected      atomic.Uint64 // records with implausible timestamps
	Flushes       atomic.Uint64 // successful writes
	FlushFailures atomic.Uint64 // failed writes (data kept in memory and retried)
	Lost          atomic.Uint64 // records discarded because storage failed for too long
	OpenMinutes   atomic.Int64  // minutes held in memory
	LastFlush     atomic.Int64  // Unix time of the last successful write
}

// Aggregator builds minute cells. Create it with New and call Run once.
type Aggregator struct {
	cfg      Config
	in       <-chan ingest.Record
	counters *ingest.Counters
	store    Writer
	run      int64
	log      *slog.Logger
	Stats    Stats

	open        map[int64]map[sli.Key]*series.Cell
	pending     map[int64]*storage.IngestDelta // ingestion deltas not yet written
	last        ingest.Snapshot
	lastRejects uint64
}

// New returns an aggregator reading from in. counters are the reader's;
// their deltas are attributed to the minute in which they are observed.
func New(cfg Config, in <-chan ingest.Record, counters *ingest.Counters, store Writer, run int64, log *slog.Logger) *Aggregator {
	cfg.defaults()
	return &Aggregator{
		cfg: cfg, in: in, counters: counters, store: store, run: run, log: log,
		open:    map[int64]map[sli.Key]*series.Cell{},
		pending: map[int64]*storage.IngestDelta{},
	}
}

// Run aggregates until ctx is cancelled, then drains the queue, writes
// everything it holds (including the current, partial minute) and returns.
// Records arriving later for a written minute are merged by storage.
func (a *Aggregator) Run(ctx context.Context) error {
	t := time.NewTicker(a.cfg.FlushEvery)
	defer t.Stop()
	for {
		select {
		case r := <-a.in:
			a.add(r)
		case <-t.C:
			a.flush(ctx, false)
		case <-ctx.Done():
			a.drain()
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			a.flush(fctx, true)
			cancel()
			return nil
		}
	}
}

func (a *Aggregator) drain() {
	for {
		select {
		case r := <-a.in:
			a.add(r)
		default:
			return
		}
	}
}

func (a *Aggregator) add(r ingest.Record) {
	now := a.cfg.Now()
	if r.Time.After(now.Add(a.cfg.MaxFuture)) || r.Time.Before(now.Add(-a.cfg.MaxAge)) {
		a.Stats.Rejected.Add(1)
		return
	}
	m := r.Time.Unix() / 60
	cells := a.open[m]
	if cells == nil {
		cells = map[sli.Key]*series.Cell{}
		a.open[m] = cells
	}
	k := sli.Classify(r)
	c := cells[k]
	if c == nil {
		c = series.New()
		cells[k] = c
	}
	c.Add(k, r)
	a.Stats.Records.Add(1)
}

// flush writes closed minutes (or all minutes when final) and the
// ingestion deltas observed since the last flush.
func (a *Aggregator) flush(ctx context.Context, final bool) {
	now := a.cfg.Now()
	nowMinute := now.Unix() / 60

	// Attribute the counters' change since the last flush to the current minute.
	cur := a.counters.Load()
	rej := a.Stats.Rejected.Load()
	d := storage.IngestDelta{
		Minute:    nowMinute,
		Received:  cur.Received - a.last.Received,
		Accepted:  cur.Accepted - a.last.Accepted,
		Malformed: cur.Malformed - a.last.Malformed,
		Dropped:   cur.Dropped - a.last.Dropped,
		Rejected:  rej - a.lastRejects,
	}
	a.last, a.lastRejects = cur, rej
	if d.Received+d.Accepted+d.Malformed+d.Dropped+d.Rejected > 0 {
		p := a.pending[nowMinute]
		if p == nil {
			p = &storage.IngestDelta{Minute: nowMinute}
			a.pending[nowMinute] = p
		}
		p.Received += d.Received
		p.Accepted += d.Accepted
		p.Malformed += d.Malformed
		p.Dropped += d.Dropped
		p.Rejected += d.Rejected
	}

	closedBefore := now.Add(-a.cfg.Grace).Unix()/60 - 1 // minutes <= this have ended at least Grace ago
	f := storage.Flush{Cells: map[storage.MinuteKey]*series.Cell{}, Run: a.run, Heartbeat: now.Unix()}
	var minutes []int64
	for m, cells := range a.open {
		if final || m <= closedBefore {
			minutes = append(minutes, m)
			for k, c := range cells {
				f.Cells[storage.MinuteKey{Minute: m, Key: k}] = c
			}
		}
	}
	for _, p := range a.pending {
		f.Ingest = append(f.Ingest, *p)
	}
	sort.Slice(f.Ingest, func(i, j int) bool { return f.Ingest[i].Minute < f.Ingest[j].Minute })

	if err := a.store.Write(ctx, f); err != nil {
		a.Stats.FlushFailures.Add(1)
		a.log.Error("flush failed; keeping data in memory", "err", err, "open_minutes", len(a.open))
		a.enforceBound()
		a.Stats.OpenMinutes.Store(int64(len(a.open)))
		return
	}
	for _, m := range minutes {
		delete(a.open, m)
	}
	clear(a.pending)
	a.Stats.Flushes.Add(1)
	a.Stats.LastFlush.Store(now.Unix())
	a.Stats.OpenMinutes.Store(int64(len(a.open)))
}

// enforceBound discards the oldest minutes when storage has been failing
// for long enough that memory would grow without limit. The loss is
// counted and recorded as dropped records once storage recovers.
func (a *Aggregator) enforceBound() {
	if len(a.open) <= a.cfg.MaxOpenMinutes {
		return
	}
	minutes := make([]int64, 0, len(a.open))
	for m := range a.open {
		minutes = append(minutes, m)
	}
	slices.Sort(minutes)
	for _, m := range minutes[:len(minutes)-a.cfg.MaxOpenMinutes] {
		var lost uint64
		for _, c := range a.open[m] {
			lost += c.Total
		}
		delete(a.open, m)
		a.Stats.Lost.Add(lost)
		p := a.pending[m]
		if p == nil {
			p = &storage.IngestDelta{Minute: m}
			a.pending[m] = p
		}
		p.Dropped += lost
	}
	if len(a.pending) > 2*a.cfg.MaxOpenMinutes {
		// Ingestion deltas are tiny, but bound them too: keep the newest.
		keys := make([]int64, 0, len(a.pending))
		for m := range a.pending {
			keys = append(keys, m)
		}
		slices.Sort(keys)
		for _, m := range keys[:len(keys)-2*a.cfg.MaxOpenMinutes] {
			delete(a.pending, m)
		}
	}
}
