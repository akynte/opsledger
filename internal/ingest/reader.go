package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"syscall"
)

// Counters are the reader's cumulative counts since start. They are read
// by the aggregator, which attributes their per-interval deltas to minutes.
type Counters struct {
	Received  atomic.Uint64 // datagrams read from the socket
	Accepted  atomic.Uint64 // parsed and queued
	Malformed atomic.Uint64 // failed to parse (includes truncated datagrams)
	Dropped   atomic.Uint64 // parsed but discarded because the queue was full
}

// Snapshot is a point-in-time copy of Counters.
type Snapshot struct {
	Received, Accepted, Malformed, Dropped uint64
}

// Load returns the current values.
func (c *Counters) Load() Snapshot {
	return Snapshot{c.Received.Load(), c.Accepted.Load(), c.Malformed.Load(), c.Dropped.Load()}
}

// Reader receives datagrams on a Unix socket.
type Reader struct {
	conn     *net.UnixConn
	path     string
	out      chan<- Record
	counters *Counters
	log      *slog.Logger
}

// Listen creates the socket at path (removing a stale one), makes it
// writable by the nginx user and returns a Reader that sends records to out.
//
// The socket is created in a directory shared only with nginx (a Docker
// volume), so mode 0666 grants access to nginx and nothing else.
func Listen(path string, rcvBuf int, out chan<- Record, counters *Counters, log *slog.Logger) (*Reader, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		conn.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	if rcvBuf > 0 {
		// Best effort: the kernel caps it at net.core.rmem_max.
		_ = conn.SetReadBuffer(rcvBuf)
	}
	return &Reader{conn: conn, path: path, out: out, counters: counters, log: log}, nil
}

// Run reads until ctx is cancelled. It never blocks on the queue: when the
// aggregator falls behind, records are dropped and counted, so the socket
// keeps draining and nginx never has to wait. Run closes the socket and
// removes its file before returning.
func (r *Reader) Run(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { r.conn.Close() })
	defer stop()
	defer os.Remove(r.path)
	defer r.conn.Close()

	// One extra byte detects datagrams larger than the limit even where
	// MSG_TRUNC is not reported.
	buf := make([]byte, MaxRecordBytes+1)
	for {
		n, _, flags, _, err := r.conn.ReadMsgUnix(buf, nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		r.counters.Received.Add(1)
		if flags&syscall.MSG_TRUNC != 0 || n > MaxRecordBytes {
			r.malformed(errors.New("datagram truncated: larger than the record limit"))
			continue
		}
		rec, err := ParseDatagram(buf[:n])
		if err != nil {
			r.malformed(err)
			continue
		}
		select {
		case r.out <- rec:
			r.counters.Accepted.Add(1)
		default:
			r.counters.Dropped.Add(1)
		}
	}
}

func (r *Reader) malformed(err error) {
	n := r.counters.Malformed.Add(1)
	// Log the first few and then every 1000th, so a flood cannot fill the logs.
	if n <= 5 || n%1000 == 0 {
		r.log.Warn("malformed record", "err", err, "malformed_total", n)
	}
}
