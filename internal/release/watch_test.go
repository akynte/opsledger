package release

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akynte/opsledger/internal/storage"
)

type events struct {
	mu sync.Mutex
	e  []storage.ReleaseEvent
}

func (s *events) AddRelease(_ context.Context, e storage.ReleaseEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.e = append(s.e, e)
	return nil
}

func TestWatcherRecordsChangesOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "release.json")
	st := &events{}
	w := &Watcher{Path: path, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	w.Check(ctx)
	if _, _, err := w.Current(); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("missing file: %v", err)
	}
	rec := Record{Schema: RecordSchema, ReleaseID: commit, Repository: "akynte/portfolio", Commit: commit,
		DeployedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), Images: clone(images),
		Verification: Verification{Status: "unverified", Method: "none"}}
	os.WriteFile(path, encode(t, rec), 0o644)
	w.Check(ctx)
	w.Check(ctx) // unchanged: no second event
	if len(st.e) != 1 || st.e[0].ReleaseID != commit {
		t.Fatalf("events %+v", st.e)
	}
	cur, raw, err := w.Current()
	if err != nil || cur.ReleaseID != commit || len(raw) == 0 {
		t.Fatalf("current %+v %v", cur, err)
	}

	os.WriteFile(path, []byte(`{"schema":"bogus"}`), 0o644)
	w.Check(ctx)
	if cur, _, err := w.Current(); cur != nil || err == nil {
		t.Fatal("an invalid record must not be served")
	}

	rec.ReleaseID, rec.Commit = strings.Repeat("b", 40), strings.Repeat("b", 40)
	rec.DeployedAt = rec.DeployedAt.Add(time.Hour)
	os.WriteFile(path, encode(t, rec), 0o644)
	w.Check(ctx)
	if len(st.e) != 2 {
		t.Fatalf("new release not recorded: %+v", st.e)
	}
}
