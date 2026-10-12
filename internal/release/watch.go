package release

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/akynte/opsledger/internal/storage"
)

// EventWriter stores release events.
type EventWriter interface {
	AddRelease(ctx context.Context, e storage.ReleaseEvent) error
}

// Watcher reads the deployment record written by deploy.sh and records a
// release event whenever it changes. The file is mounted read-only into
// the container; it is re-read periodically because a bind-mounted file is
// replaced, not modified, by a deployment.
type Watcher struct {
	Path  string
	Every time.Duration
	Store EventWriter
	Log   *slog.Logger

	mu      sync.RWMutex
	current *Record
	raw     []byte
	err     error
}

// Current returns the last valid record and its bytes, or the reason
// there is none.
func (w *Watcher) Current() (*Record, []byte, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current, w.raw, w.err
}

// Run polls until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	if w.Every <= 0 {
		w.Every = 30 * time.Second
	}
	t := time.NewTicker(w.Every)
	defer t.Stop()
	w.Check(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check(ctx)
		}
	}
}

// ErrNoRecord means no deployment record exists (local runs, first deploy).
var ErrNoRecord = errors.New("no deployment record")

// Check reads the file once.
func (w *Watcher) Check(ctx context.Context) {
	b, err := os.ReadFile(w.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = ErrNoRecord
		}
		w.set(nil, nil, err)
		return
	}
	r, err := ParseRecord(b)
	if err != nil {
		w.Log.Warn("ignoring invalid deployment record", "path", w.Path, "err", err)
		w.set(nil, nil, err)
		return
	}
	prev, _, _ := w.Current()
	if prev == nil || prev.ReleaseID != r.ReleaseID || !prev.DeployedAt.Equal(r.DeployedAt) {
		info, _ := json.Marshal(r)
		if err := w.Store.AddRelease(ctx, storage.ReleaseEvent{TS: r.DeployedAt.Unix(), ReleaseID: r.ReleaseID, Info: info}); err != nil {
			w.Log.Warn("cannot store release event", "err", err)
			return // retry on the next check
		}
		w.Log.Info("release observed", "release", r.ReleaseID, "deployed_at", r.DeployedAt, "verification", r.Verification.Status)
	}
	w.set(r, b, nil)
}

func (w *Watcher) set(r *Record, raw []byte, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.current, w.raw, w.err = r, raw, err
}
