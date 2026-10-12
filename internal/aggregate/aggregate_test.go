package aggregate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/storage"
)

type fakeStore struct {
	mu     sync.Mutex
	fail   bool
	writes []storage.Flush
}

func (f *fakeStore) Write(_ context.Context, fl storage.Flush) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("disk full")
	}
	f.writes = append(f.writes, fl)
	return nil
}

func (f *fakeStore) totals() (records, received uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writes {
		for _, c := range w.Cells {
			records += c.Total
		}
		for _, d := range w.Ingest {
			received += d.Received
		}
	}
	return
}

var base = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newAgg(store Writer, clk *clock, maxOpen int) (*Aggregator, *ingest.Counters) {
	counters := new(ingest.Counters)
	a := New(Config{Now: clk.now, MaxOpenMinutes: maxOpen}, make(chan ingest.Record), counters, store, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return a, counters
}

func req(at time.Time, status int) ingest.Record {
	return ingest.Record{Time: at, Server: ingest.ServerPublic, Method: "GET", Path: "/", Status: status, Duration: 2000}
}

func TestOnlyClosedMinutesAreFlushed(t *testing.T) {
	st := &fakeStore{}
	clk := &clock{base.Add(90 * time.Second)} // 10:01:30
	a, _ := newAgg(st, clk, 0)
	a.add(req(base.Add(10*time.Second), 200)) // minute 10:00
	a.add(req(base.Add(70*time.Second), 200)) // minute 10:01 (still open)
	a.flush(context.Background(), false)
	if len(st.writes) != 1 || len(st.writes[0].Cells) != 1 {
		t.Fatalf("writes %+v", st.writes)
	}
	for mk := range st.writes[0].Cells {
		if mk.Minute != base.Unix()/60 {
			t.Fatalf("flushed minute %d, want the closed 10:00", mk.Minute)
		}
	}
	if len(a.open) != 1 {
		t.Fatalf("%d open minutes; the current minute must stay open", len(a.open))
	}
	// Within the grace period after 10:01 ends, it stays open for late records.
	clk.t = base.Add(122 * time.Second)
	a.flush(context.Background(), false)
	if len(a.open) != 1 {
		t.Fatal("minute closed before its grace period ended")
	}
	clk.t = base.Add(126 * time.Second)
	a.flush(context.Background(), false)
	if len(a.open) != 0 {
		t.Fatal("minute not closed after grace")
	}
}

func TestFinalFlushWritesPartialMinute(t *testing.T) {
	st := &fakeStore{}
	clk := &clock{base.Add(30 * time.Second)}
	a, _ := newAgg(st, clk, 0)
	a.add(req(base.Add(20*time.Second), 200))
	a.flush(context.Background(), true)
	if n, _ := st.totals(); n != 1 {
		t.Fatalf("final flush wrote %d records", n)
	}
}

func TestFailedWritesAreRetriedAndBounded(t *testing.T) {
	st := &fakeStore{fail: true}
	clk := &clock{base.Add(10 * time.Minute)}
	a, _ := newAgg(st, clk, 3)
	for i := range 6 {
		a.add(req(base.Add(time.Duration(i)*time.Minute), 200))
	}
	a.flush(context.Background(), false)
	if a.Stats.FlushFailures.Load() != 1 {
		t.Fatal("failure not counted")
	}
	if len(a.open) != 3 || a.Stats.Lost.Load() != 3 {
		t.Fatalf("open=%d lost=%d; want the 3 oldest minutes discarded and counted", len(a.open), a.Stats.Lost.Load())
	}
	st.fail = false
	a.flush(context.Background(), false)
	records, _ := st.totals()
	if records != 3 || len(a.open) != 0 {
		t.Fatalf("after recovery wrote %d records, %d still open", records, len(a.open))
	}
	// The loss is persisted as dropped records, so reports show it.
	var dropped uint64
	for _, w := range st.writes {
		for _, d := range w.Ingest {
			dropped += d.Dropped
		}
	}
	if dropped != 3 {
		t.Fatalf("dropped persisted = %d, want 3", dropped)
	}
}

func TestImplausibleTimestampsAreRejectedAndCounted(t *testing.T) {
	st := &fakeStore{}
	clk := &clock{base}
	a, _ := newAgg(st, clk, 0)
	a.add(req(base.Add(10*time.Minute), 200))
	a.add(req(base.Add(-48*time.Hour), 200))
	a.add(req(base.Add(-time.Minute), 200))
	if a.Stats.Rejected.Load() != 2 || a.Stats.Records.Load() != 1 {
		t.Fatalf("rejected=%d records=%d", a.Stats.Rejected.Load(), a.Stats.Records.Load())
	}
	a.flush(context.Background(), true)
	var rejected uint64
	for _, w := range st.writes {
		for _, d := range w.Ingest {
			rejected += d.Rejected
		}
	}
	if rejected != 2 {
		t.Fatalf("rejected persisted = %d", rejected)
	}
}

func TestIngestCounterDeltas(t *testing.T) {
	st := &fakeStore{}
	clk := &clock{base}
	a, c := newAgg(st, clk, 0)
	c.Received.Add(10)
	c.Accepted.Add(9)
	c.Malformed.Add(1)
	a.flush(context.Background(), false)
	c.Received.Add(5)
	c.Accepted.Add(5)
	clk.t = base.Add(time.Minute)
	a.flush(context.Background(), false)
	_, received := st.totals()
	if received != 15 {
		t.Fatalf("received across flushes = %d, want 15 (deltas, not cumulative values)", received)
	}
}

// End to end through the real reader: concurrent senders, a running
// aggregator, and a clean shutdown that loses nothing it accepted.
func TestRunWithRealReaderUnderConcurrency(t *testing.T) {
	dir, err := shortDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeDir(dir) })
	path := dir + "/in.sock"
	q := make(chan ingest.Record, 4096)
	counters := new(ingest.Counters)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := ingest.Listen(path, 4<<20, q, counters, log)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{}
	a := New(Config{FlushEvery: 20 * time.Millisecond}, q, counters, st, 1, log)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.Run(ctx) }()
	aggCtx, aggCancel := context.WithCancel(context.Background())
	go func() { defer wg.Done(); a.Run(aggCtx) }()

	const senders, each = 8, 250
	var sw sync.WaitGroup
	for range senders {
		sw.Add(1)
		go func() {
			defer sw.Done()
			for range each {
				sendDatagram(t, path, nowRecord())
			}
		}()
	}
	sw.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for counters.Received.Load() < senders*each && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // stop reading first, then let the aggregator drain
	time.Sleep(50 * time.Millisecond)
	aggCancel()
	wg.Wait()

	s := counters.Load()
	if s.Received != senders*each {
		t.Fatalf("received %d of %d", s.Received, senders*each)
	}
	records, received := st.totals()
	if records != s.Accepted {
		t.Fatalf("stored %d records, accepted %d: accepted records were lost", records, s.Accepted)
	}
	if received != s.Received || s.Accepted+s.Dropped+s.Malformed != s.Received {
		t.Fatalf("accounting mismatch: counters %+v, persisted received %d", s, received)
	}
}
