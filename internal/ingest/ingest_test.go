package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodMsg = `{"v":1,"t":"1760270400.123","srv":"pub","m":"GET","u":"/proof","st":200,"rt":"0.004","rsc":"","bot":"0","probe":"0","b":5120}`

func datagram(msg string) []byte {
	return []byte("<190>Oct 12 12:00:00 nginx-host ledger: " + msg)
}

func TestParseDatagram(t *testing.T) {
	r, err := ParseDatagram(datagram(goodMsg))
	if err != nil {
		t.Fatal(err)
	}
	want := Record{
		Time: time.Date(2025, 10, 12, 12, 0, 0, 123_000_000, time.UTC), Server: ServerPublic,
		Method: "GET", Path: "/proof", Status: 200, Duration: 4000, Bytes: 5120,
	}
	if r != want {
		t.Fatalf("got %+v\nwant %+v", r, want)
	}
}

func TestParseFlagsAndDurations(t *testing.T) {
	msg := strings.NewReplacer(`"rsc":""`, `"rsc":"1"`, `"bot":"0"`, `"bot":"1"`, `"probe":"0"`, `"probe":"1"`,
		`"rt":"0.004"`, `"rt":"12.345"`).Replace(goodMsg)
	r, err := ParseRecord([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if !r.RSC || !r.Bot || !r.Probe || r.Duration != 12_345_000 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	replace := func(old, new string) string { return strings.Replace(goodMsg, old, new, 1) }
	cases := map[string][]byte{
		"empty":            {},
		"no priority":      []byte("Oct 12 12:00:00 h ledger: " + goodMsg),
		"bad priority":     []byte("<1x0>Oct 12 12:00:00 h ledger: " + goodMsg),
		"short header":     []byte("<190>Oct 12"),
		"no tag":           []byte("<190>Oct 12 12:00:00 host " + goodMsg),
		"not json":         datagram("hello"),
		"trailing":         datagram(goodMsg + ` {}`),
		"unknown field":    datagram(replace(`"v":1`, `"v":1,"ip":"203.0.113.9"`)),
		"missing field":    datagram(replace(`"b":5120`, `"x":0`)),
		"version":          datagram(replace(`"v":1`, `"v":2`)),
		"timestamp":        datagram(replace(`1760270400.123`, `1760270400`)),
		"timestamp sign":   datagram(replace(`1760270400.123`, `-760270400.123`)),
		"server":           datagram(replace(`"srv":"pub"`, `"srv":"evil"`)),
		"method":           datagram(replace(`"m":"GET"`, `"m":"get"`)),
		"status low":       datagram(replace(`"st":200`, `"st":99`)),
		"status high":      datagram(replace(`"st":200`, `"st":1000`)),
		"status string":    datagram(replace(`"st":200`, `"st":"200"`)),
		"duration":         datagram(replace(`"rt":"0.004"`, `"rt":"4"`)),
		"duration minus":   datagram(replace(`"rt":"0.004"`, `"rt":"-0.004"`)),
		"duration huge":    datagram(replace(`"rt":"0.004"`, `"rt":"99999999.000"`)),
		"rsc":              datagram(replace(`"rsc":""`, `"rsc":"yes"`)),
		"bot":              datagram(replace(`"bot":"0"`, `"bot":""`)),
		"probe":            datagram(replace(`"probe":"0"`, `"probe":"true"`)),
		"bytes":            datagram(replace(`"b":5120`, `"b":-1`)),
		"long path":        datagram(replace(`"/proof"`, `"/`+strings.Repeat("a", 192)+`"`)),
		"oversize":         datagram(goodMsg + strings.Repeat(" ", MaxRecordBytes)),
		"duplicate object": datagram(goodMsg + goodMsg),
	}
	for name, in := range cases {
		if _, err := ParseDatagram(in); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", name, err)
		}
	}
}

func FuzzParseDatagram(f *testing.F) {
	f.Add(datagram(goodMsg))
	f.Add([]byte("<0>Jan  1 00:00:00 h t: {}"))
	f.Add(datagram(`{"v":1,"t":"0000000000.000","srv":"int","m":"A","u":"","st":100,"rt":"0.000","rsc":"0","bot":"1","probe":"1","b":0}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseDatagram(b)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error does not wrap ErrMalformed: %v", err)
			}
			return
		}
		// Anything accepted must satisfy the documented invariants.
		if r.Status < 100 || r.Status > 999 || len(r.Path) > 192 || r.Method == "" || r.Time.Location() != time.UTC {
			t.Fatalf("accepted record violates invariants: %+v", r)
		}
	})
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startReader(t *testing.T, queue int) (*Reader, chan Record, *Counters, string, context.CancelFunc, chan error) {
	t.Helper()
	// Unix socket paths are limited to ~108 bytes; t.TempDir() can be longer.
	dir, err := shortTempDir(t)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "in.sock")
	out := make(chan Record, queue)
	c := new(Counters)
	r, err := Listen(path, 1<<20, out, c, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return r, out, c, path, cancel, done
}

func send(t *testing.T, path string, msgs ...[]byte) {
	t.Helper()
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, m := range msgs {
		if _, err := conn.Write(m); err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReaderAcceptsCountsAndShutsDown(t *testing.T) {
	_, out, c, path, cancel, done := startReader(t, 16)
	send(t, path, datagram(goodMsg), []byte("garbage"), datagram(goodMsg))
	big := append(datagram(goodMsg), make([]byte, MaxRecordBytes)...)
	send(t, path, big)
	waitFor(t, func() bool { return c.Received.Load() == 4 })
	s := c.Load()
	if s.Accepted != 2 || s.Malformed != 2 || s.Dropped != 0 {
		t.Fatalf("counters %+v", s)
	}
	if len(out) != 2 {
		t.Fatalf("%d records queued", len(out))
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not stop after cancel")
	}
	if _, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"}); err == nil {
		t.Fatal("socket still accepts after shutdown")
	}
}

func TestReaderDropsWhenQueueIsFullInsteadOfBlocking(t *testing.T) {
	_, out, c, path, cancel, done := startReader(t, 3)
	defer func() { cancel(); <-done }()
	msgs := make([][]byte, 50)
	for i := range msgs {
		msgs[i] = datagram(goodMsg)
	}
	send(t, path, msgs...)
	waitFor(t, func() bool { return c.Received.Load() == 50 })
	s := c.Load()
	if s.Accepted != 3 || s.Dropped != 47 {
		t.Fatalf("counters %+v; want 3 accepted, 47 dropped", s)
	}
	if s.Accepted+s.Dropped+s.Malformed != s.Received {
		t.Fatal("every datagram must be accounted for exactly once")
	}
	if len(out) != 3 {
		t.Fatalf("queue holds %d", len(out))
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	_, _, _, path, cancel, done := startReader(t, 1)
	// Simulate a crash: the file stays behind.
	cancel()
	<-done
	// A datagram socket's file outlives its Close: this leaves a stale file without a reader.
	stale, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected a stale socket file: %v", err)
	}
	out := make(chan Record, 1)
	r, err := Listen(path, 0, out, new(Counters), quietLogger())
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	r.conn.Close()
}

func shortTempDir(t *testing.T) (string, error) {
	dir := t.TempDir()
	if len(dir) < 80 {
		return dir, nil
	}
	d, err := mkShort()
	if err != nil {
		return "", fmt.Errorf("short temp dir: %w", err)
	}
	t.Cleanup(func() { removeAll(d) })
	return d, nil
}
