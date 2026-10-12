// Package reconcile samples nginx's own request counter so that records
// lost between nginx and the ledger can be counted.
//
// nginx's stub_status reports the total number of requests it has handled.
// Every request nginx handles is also logged to the ledger. Comparing the
// counter's growth with the number of records the ledger received over the
// same interval measures loss in transit independently of the ledger's own
// counters. Each sample reads nginx first and the ledger's counter after the
// response, so the sample request itself is counted on both sides.
package reconcile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/storage"
)

// SampleWriter stores samples.
type SampleWriter interface {
	AddSample(ctx context.Context, s storage.Sample) error
}

// Poller samples nginx periodically.
type Poller struct {
	URL      string
	Every    time.Duration
	Client   *http.Client
	Counters *ingest.Counters
	Store    SampleWriter
	RunID    int64
	Log      *slog.Logger
	Now      func() time.Time

	Samples  atomic.Uint64
	Failures atomic.Uint64
}

// Run samples until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	if p.Every <= 0 {
		p.Every = time.Minute
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	// nginx usually starts after the ledger: retry quickly until the first sample succeeds.
	for !p.sample(ctx) && p.Samples.Load() == 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(min(5*time.Second, p.Every)):
		}
	}
	t := time.NewTicker(p.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sample(ctx)
		}
	}
}

func (p *Poller) sample(ctx context.Context) bool {
	reqs, err := Fetch(ctx, p.Client, p.URL)
	if err != nil {
		if p.Failures.Add(1) <= 3 || p.Failures.Load()%60 == 0 {
			p.Log.Warn("nginx status unavailable; reconciliation paused", "err", err)
		}
		return false
	}
	s := storage.Sample{TS: p.Now().Unix(), Run: p.RunID, Requests: reqs, Received: p.Counters.Received.Load()}
	if err := p.Store.AddSample(ctx, s); err != nil {
		p.Failures.Add(1)
		p.Log.Warn("cannot store reconciliation sample", "err", err)
		return false
	}
	p.Samples.Add(1)
	return true
}

// Fetch reads the "requests" counter from an nginx stub_status page:
//
//	Active connections: 2
//	server accepts handled requests
//	 10 10 25
//	Reading: 0 Writing: 1 Waiting: 1
func Fetch(ctx context.Context, c *http.Client, url string) (uint64, error) {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return 0, err
	}
	return Parse(body)
}

// Parse extracts the requests counter from a stub_status body.
func Parse(body []byte) (uint64, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "server accepts handled requests" {
			continue
		}
		if !sc.Scan() {
			break
		}
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			return 0, errors.New("stub_status: malformed counters line")
		}
		return strconv.ParseUint(f[2], 10, 64)
	}
	return 0, errors.New("stub_status: counters not found")
}
