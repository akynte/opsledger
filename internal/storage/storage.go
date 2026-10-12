// Package storage persists aggregated minutes, ingestion counters, nginx
// reconciliation samples, ledger run intervals and release events in
// SQLite (WAL mode).
//
// Concurrency: one goroutine (the aggregator) writes; HTTP handlers read.
// WAL lets readers proceed while the writer commits. All timestamps are UTC
// Unix seconds; minutes are Unix seconds / 60.
//
// Durability model: the database holds at most the retention window
// (default 35 days) of minute data. Published daily reports are the
// long-term record, so losing the database loses only days that were not
// yet published. On corruption the file is moved aside and a new database
// is created (see Open); the reset is recorded so that reports show the gap.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/akynte/opsledger/internal/histogram"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
)

// Options configure Open.
type Options struct {
	// MaxBytes caps the database size (SQLite max_page_count). Writes beyond
	// it fail with "database or disk is full"; the aggregator counts them.
	MaxBytes int64
	// Now is the clock (tests).
	Now func() time.Time
}

// Store is an open ledger database.
type Store struct {
	db       *sql.DB
	path     string
	now      func() time.Time
	maxPages int64
	// Reset is non-empty when Open replaced a corrupt or unreadable
	// database; it names the file the old one was moved to.
	Reset string
}

// migrations are applied in order; PRAGMA user_version records progress.
var migrations = []string{
	// 1: initial schema.
	`CREATE TABLE minute_series (
		minute        INTEGER NOT NULL,
		class         TEXT    NOT NULL,
		observer      TEXT    NOT NULL,
		agent         TEXT    NOT NULL,
		total         INTEGER NOT NULL,
		ok            INTEGER NOT NULL,
		blocked       INTEGER NOT NULL,
		rate_limited  INTEGER NOT NULL,
		client_closed INTEGER NOT NULL,
		server_error  INTEGER NOT NULL,
		s1xx INTEGER NOT NULL, s2xx INTEGER NOT NULL, s3xx INTEGER NOT NULL,
		s4xx INTEGER NOT NULL, s5xx INTEGER NOT NULL,
		lat_eligible  INTEGER NOT NULL,
		lat_good      INTEGER NOT NULL,
		latency       TEXT    NOT NULL,
		PRIMARY KEY (minute, class, observer, agent)
	) WITHOUT ROWID;
	CREATE TABLE minute_ingest (
		minute    INTEGER PRIMARY KEY,
		received  INTEGER NOT NULL,
		accepted  INTEGER NOT NULL,
		malformed INTEGER NOT NULL,
		dropped   INTEGER NOT NULL,
		rejected  INTEGER NOT NULL
	);
	CREATE TABLE nginx_samples (
		ts        INTEGER NOT NULL,
		run       INTEGER NOT NULL,
		requests  INTEGER NOT NULL,
		received  INTEGER NOT NULL,
		PRIMARY KEY (ts, run)
	);
	CREATE TABLE runs (
		started   INTEGER PRIMARY KEY,
		last_seen INTEGER NOT NULL,
		stopped   INTEGER,
		version   TEXT NOT NULL,
		note      TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE releases (
		ts          INTEGER NOT NULL,
		release_id  TEXT    NOT NULL,
		info        TEXT    NOT NULL,
		PRIMARY KEY (ts, release_id)
	);`,
}

// Open opens (creating if needed) the database at path. If the file exists
// but is not a healthy ledger database, it is renamed to
// path.corrupt-<unix time> and a new one is created; Store.Reset says so.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s, err := open(ctx, path, opts)
	if err == nil {
		return s, nil
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return nil, err // nothing to move aside: a real failure (permissions, disk)
	}
	aside := fmt.Sprintf("%s.corrupt-%d", path, opts.Now().Unix())
	if renameErr := os.Rename(path, aside); renameErr != nil {
		return nil, fmt.Errorf("%w (and moving it aside failed: %v)", err, renameErr)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Rename(path+suffix, aside+suffix)
	}
	s, err2 := open(ctx, path, opts)
	if err2 != nil {
		return nil, fmt.Errorf("recreate after %v: %w", err, err2)
	}
	s.Reset = filepath.Base(aside)
	return s, nil
}

func open(ctx context.Context, path string, opts Options) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(time.Minute)
	s := &Store{db: db, path: path, now: opts.Now}
	if err := s.init(ctx, opts.MaxBytes); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context, maxBytes int64) error {
	var check string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("quick_check: %s", check)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this ledger (%d)", version, len(migrations))
	}
	if version == 0 {
		// Only effective before the first table exists; lets Prune return space with incremental_vacuum.
		if _, err := s.db.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
			return err
		}
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA cannot take a bound parameter; the value is a trusted integer.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if maxBytes > 0 {
		var pageSize int64
		if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
			return err
		}
		s.maxPages = maxBytes / pageSize
	}
	return nil
}

// applyLimit sets max_page_count on the connection used by tx. The pragma
// is per connection, so it is applied inside each write transaction.
func (s *Store) applyLimit(ctx context.Context, tx *sql.Tx) error {
	if s.maxPages <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count = %d", s.maxPages))
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// SizeBytes returns the current database file size (pages in use × page size).
func (s *Store) SizeBytes(ctx context.Context) (int64, error) {
	var pages, size int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}

// MinuteKey identifies one stored row.
type MinuteKey struct {
	Minute int64
	Key    sli.Key
}

// IngestDelta is the ingestion counters' change attributed to one minute.
type IngestDelta struct {
	Minute                                           int64
	Received, Accepted, Malformed, Dropped, Rejected uint64
}

// Flush is one aggregator write: closed minutes, ingestion deltas and a
// run heartbeat, committed atomically.
type Flush struct {
	Cells     map[MinuteKey]*series.Cell
	Ingest    []IngestDelta
	Run       int64 // run start (see StartRun)
	Heartbeat int64
}

// Write merges a flush into the database. Cells for minutes that already
// have rows (late records) are merged with them, not replaced.
func (s *Store) Write(ctx context.Context, f Flush) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.applyLimit(ctx, tx); err != nil {
		return err
	}
	sel, err := tx.PrepareContext(ctx, `SELECT total, ok, blocked, rate_limited, client_closed, server_error,
		s1xx, s2xx, s3xx, s4xx, s5xx, lat_eligible, lat_good, latency
		FROM minute_series WHERE minute = ? AND class = ? AND observer = ? AND agent = ?`)
	if err != nil {
		return err
	}
	defer sel.Close()
	up, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO minute_series
		(minute, class, observer, agent, total, ok, blocked, rate_limited, client_closed, server_error,
		 s1xx, s2xx, s3xx, s4xx, s5xx, lat_eligible, lat_good, latency)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer up.Close()
	for mk, c := range f.Cells {
		cell := c.Clone()
		existing, err := scanCell(sel.QueryRowContext(ctx, mk.Minute, mk.Key.Class, mk.Key.Observer, mk.Key.Agent))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			existing.Merge(cell)
			cell = existing
		}
		cell.Latency.Normalize()
		lat, err := json.Marshal(cell.Latency)
		if err != nil {
			return err
		}
		if _, err := up.ExecContext(ctx, mk.Minute, mk.Key.Class, mk.Key.Observer, mk.Key.Agent,
			cell.Total, cell.OK, cell.Blocked, cell.RateLimited, cell.ClientClosed, cell.ServerError,
			cell.S1xx, cell.S2xx, cell.S3xx, cell.S4xx, cell.S5xx, cell.LatEligible, cell.LatGood, string(lat)); err != nil {
			return err
		}
	}
	for _, d := range f.Ingest {
		if _, err := tx.ExecContext(ctx, `INSERT INTO minute_ingest (minute, received, accepted, malformed, dropped, rejected)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (minute) DO UPDATE SET received = received + excluded.received,
			  accepted = accepted + excluded.accepted, malformed = malformed + excluded.malformed,
			  dropped = dropped + excluded.dropped, rejected = rejected + excluded.rejected`,
			d.Minute, d.Received, d.Accepted, d.Malformed, d.Dropped, d.Rejected); err != nil {
			return err
		}
	}
	if f.Run != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET last_seen = ? WHERE started = ?`, f.Heartbeat, f.Run); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanCell(r rowScanner) (*series.Cell, error) {
	c := &series.Cell{}
	var lat string
	if err := r.Scan(&c.Total, &c.OK, &c.Blocked, &c.RateLimited, &c.ClientClosed, &c.ServerError,
		&c.S1xx, &c.S2xx, &c.S3xx, &c.S4xx, &c.S5xx, &c.LatEligible, &c.LatGood, &lat); err != nil {
		return nil, err
	}
	c.Latency = &histogram.Histogram{}
	if err := json.Unmarshal([]byte(lat), c.Latency); err != nil {
		return nil, fmt.Errorf("stored histogram: %w", err)
	}
	c.Latency.MaxBuckets = histogram.DefaultMaxBuckets
	if err := c.Latency.Validate(); err != nil {
		return nil, fmt.Errorf("stored histogram: %w", err)
	}
	return c, nil
}

// StartRun records the start of a ledger process and returns its id.
func (s *Store) StartRun(ctx context.Context, version, note string) (int64, error) {
	now := s.now().Unix()
	// Two starts in the same second (a crash loop) must not collide.
	for {
		_, err := s.db.ExecContext(ctx, `INSERT INTO runs (started, last_seen, version, note) VALUES (?, ?, ?, ?)`, now, now, version, note)
		if err == nil {
			return now, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			return 0, err
		}
		now++
	}
}

// StopRun records a clean stop.
func (s *Store) StopRun(ctx context.Context, run int64) error {
	now := s.now().Unix()
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET last_seen = ?, stopped = ? WHERE started = ?`, now, now, run)
	return err
}

// Run is one ledger process lifetime. A run without Stopped ended
// uncleanly (crash, kill) or is the current one.
type Run struct {
	Started, LastSeen int64
	Stopped           *int64
	Version, Note     string
}

// Runs returns runs that overlap [from, to).
func (s *Store) Runs(ctx context.Context, from, to int64) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT started, last_seen, stopped, version, note FROM runs
		WHERE started < ? AND last_seen >= ? ORDER BY started`, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var stopped sql.NullInt64
		if err := rows.Scan(&r.Started, &r.LastSeen, &stopped, &r.Version, &r.Note); err != nil {
			return nil, err
		}
		if stopped.Valid {
			r.Stopped = &stopped.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FirstRun returns the start of the earliest run, or 0.
func (s *Store) FirstRun(ctx context.Context) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(started) FROM runs`).Scan(&v)
	return v.Int64, err
}

// Sample is one nginx stub_status reading together with the ledger's
// received-datagram counter at the same moment.
type Sample struct {
	TS       int64
	Run      int64
	Requests uint64 // nginx's cumulative request count (resets when nginx restarts)
	Received uint64 // datagrams received by this ledger run so far
}

// AddSample stores a reconciliation sample.
func (s *Store) AddSample(ctx context.Context, smp Sample) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO nginx_samples (ts, run, requests, received) VALUES (?, ?, ?, ?)`,
		smp.TS, smp.Run, smp.Requests, smp.Received)
	return err
}

// Samples returns samples in [from, to) ordered by time.
func (s *Store) Samples(ctx context.Context, from, to int64) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, run, requests, received FROM nginx_samples
		WHERE ts >= ? AND ts < ? ORDER BY ts, run`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var smp Sample
		if err := rows.Scan(&smp.TS, &smp.Run, &smp.Requests, &smp.Received); err != nil {
			return nil, err
		}
		out = append(out, smp)
	}
	return out, rows.Err()
}

// Row is one stored minute row.
type Row struct {
	Minute int64
	Key    sli.Key
	Cell   *series.Cell
}

// Rows returns minute rows with minute in [fromMinute, toMinute).
func (s *Store) Rows(ctx context.Context, fromMinute, toMinute int64) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT minute, class, observer, agent, total, ok, blocked, rate_limited,
		client_closed, server_error, s1xx, s2xx, s3xx, s4xx, s5xx, lat_eligible, lat_good, latency
		FROM minute_series WHERE minute >= ? AND minute < ? ORDER BY minute, class, observer, agent`, fromMinute, toMinute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		c := &series.Cell{}
		var lat, class, observer, agent string
		if err := rows.Scan(&r.Minute, &class, &observer, &agent, &c.Total, &c.OK, &c.Blocked, &c.RateLimited,
			&c.ClientClosed, &c.ServerError, &c.S1xx, &c.S2xx, &c.S3xx, &c.S4xx, &c.S5xx, &c.LatEligible, &c.LatGood, &lat); err != nil {
			return nil, err
		}
		c.Latency = &histogram.Histogram{}
		if err := json.Unmarshal([]byte(lat), c.Latency); err != nil {
			return nil, fmt.Errorf("minute %d: stored histogram: %w", r.Minute, err)
		}
		c.Latency.MaxBuckets = histogram.DefaultMaxBuckets
		r.Key = sli.Key{Class: sli.Class(class), Observer: sli.Observer(observer), Agent: sli.Agent(agent)}
		r.Cell = c
		out = append(out, r)
	}
	return out, rows.Err()
}

// IngestTotals sums the ingestion counters for minutes in [fromMinute, toMinute).
func (s *Store) IngestTotals(ctx context.Context, fromMinute, toMinute int64) (IngestDelta, error) {
	var d IngestDelta
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(received),0), COALESCE(SUM(accepted),0), COALESCE(SUM(malformed),0),
		COALESCE(SUM(dropped),0), COALESCE(SUM(rejected),0) FROM minute_ingest WHERE minute >= ? AND minute < ?`,
		fromMinute, toMinute).Scan(&d.Received, &d.Accepted, &d.Malformed, &d.Dropped, &d.Rejected)
	return d, err
}

// ReleaseEvent records that a release was observed as deployed.
type ReleaseEvent struct {
	TS        int64           // deployment time as reported by the host
	ReleaseID string          // commit SHA
	Info      json.RawMessage // the release document as read
}

// AddRelease stores a release event (idempotent per time and release).
func (s *Store) AddRelease(ctx context.Context, e ReleaseEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO releases (ts, release_id, info) VALUES (?, ?, ?)`,
		e.TS, e.ReleaseID, string(e.Info))
	return err
}

// Releases returns the release active at from (if any) and every release
// event in [from, to), in time order.
func (s *Store) Releases(ctx context.Context, from, to int64) ([]ReleaseEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, release_id, info FROM (SELECT ts, release_id, info FROM releases WHERE ts < ? ORDER BY ts DESC LIMIT 1)
		UNION ALL
		SELECT ts, release_id, info FROM releases WHERE ts >= ? AND ts < ?
		ORDER BY ts`, from, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseEvent
	for rows.Next() {
		var e ReleaseEvent
		var info string
		if err := rows.Scan(&e.TS, &e.ReleaseID, &info); err != nil {
			return nil, err
		}
		e.Info = json.RawMessage(info)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestRelease returns the most recent release event.
func (s *Store) LatestRelease(ctx context.Context) (ReleaseEvent, bool, error) {
	var e ReleaseEvent
	var info string
	err := s.db.QueryRowContext(ctx, `SELECT ts, release_id, info FROM releases ORDER BY ts DESC LIMIT 1`).Scan(&e.TS, &e.ReleaseID, &info)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	e.Info = json.RawMessage(info)
	return e, err == nil, err
}

// Prune deletes minute, ingestion, sample and run data older than keep.
// Release events are tiny and kept so reports can always name the release.
func (s *Store) Prune(ctx context.Context, keep time.Duration) (int64, error) {
	cutoff := s.now().Add(-keep).Unix()
	var total int64
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM minute_series WHERE minute < ?`, cutoff / 60},
		{`DELETE FROM minute_ingest WHERE minute < ?`, cutoff / 60},
		{`DELETE FROM nginx_samples WHERE ts < ?`, cutoff},
		{`DELETE FROM runs WHERE last_seen < ?`, cutoff},
	} {
		res, err := s.db.ExecContext(ctx, q.sql, q.arg)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if total > 0 {
		// Return freed pages to the OS gradually; WAL mode needs a checkpoint first.
		s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	}
	return total, nil
}

// Checkpoint flushes the WAL into the database file (used at shutdown).
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
