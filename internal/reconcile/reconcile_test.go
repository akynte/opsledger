package reconcile

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/storage"
)

const status = "Active connections: 2 \nserver accepts handled requests\n 10 10 25 \nReading: 0 Writing: 1 Waiting: 1 \n"

func TestParse(t *testing.T) {
	n, err := Parse([]byte(status))
	if err != nil || n != 25 {
		t.Fatalf("%d %v", n, err)
	}
	for _, bad := range []string{"", "Active connections: 1\n", "server accepts handled requests\n 1 2\n", "server accepts handled requests\n a b c\n"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

type memStore struct {
	mu sync.Mutex
	s  []storage.Sample
}

func (m *memStore) AddSample(_ context.Context, s storage.Sample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s = append(m.s, s)
	return nil
}

func TestPollerRecordsNginxAndLedgerCountersTogether(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests += 7
		io.WriteString(w, "Active connections: 1\nserver accepts handled requests\n 1 1 "+itoa(requests)+"\nReading: 0 Writing: 1 Waiting: 0\n")
	}))
	defer srv.Close()
	c := new(ingest.Counters)
	c.Received.Store(5)
	st := &memStore{}
	p := &Poller{URL: srv.URL, Every: 20 * time.Millisecond, Counters: c, Store: st, RunID: 9,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.s) < 2 || st.s[0].Requests != 7 || st.s[1].Requests != 14 || st.s[0].Received != 5 || st.s[0].Run != 9 {
		t.Fatalf("samples %+v", st.s)
	}
}

func TestPollerCountsFailuresAndKeepsRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 502) }))
	defer srv.Close()
	p := &Poller{URL: srv.URL, Every: 10 * time.Millisecond, Counters: new(ingest.Counters), Store: &memStore{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	if p.Failures.Load() < 2 || p.Samples.Load() != 0 {
		t.Fatalf("failures %d samples %d", p.Failures.Load(), p.Samples.Load())
	}
}

func itoa(n int) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
