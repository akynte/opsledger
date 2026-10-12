package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func record(at time.Time, path string, status int) []byte {
	return fmt.Appendf(nil, `<190>Oct 12 12:00:00 h ledger: {"v":1,"t":"%d.%03d","srv":"pub","m":"GET","u":"%s","st":%d,"rt":"0.012","rsc":"","bot":"0","probe":"0","b":100}`,
		at.Unix(), at.Nanosecond()/1e6, path, status)
}

// The real command, end to end: records sent to the socket are counted,
// aggregated and persisted; SIGTERM shuts down cleanly without losing what
// was accepted; the offline `day` and `recompute` commands then reproduce
// and verify a report from the database.
func TestServeEndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "lg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	httpAddr, adminAddr := freePort(t), freePort(t)
	env := map[string]string{
		"LEDGER_SOCKET": filepath.Join(dir, "in.sock"), "LEDGER_DB": filepath.Join(dir, "ledger.db"),
		"LEDGER_HTTP_ADDR": httpAddr, "LEDGER_ADMIN_ADDR": adminAddr, "LEDGER_FLUSH_EVERY": "1s",
		"LEDGER_RELEASE_FILE": filepath.Join(dir, "release.json"), "LOG_FORMAT": "text",
	}
	getenv := func(k string) string { return env[k] }
	var logs bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"serve"}, getenv, &logs, &logs) }()

	waitHTTP(t, "http://"+adminAddr+"/readyz")
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: env["LEDGER_SOCKET"], Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 100 {
		status := 200
		if i%25 == 0 {
			status = 503
		}
		conn.Write(record(now, "/proof", status))
	}
	conn.Write([]byte("not a record"))
	conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(get(t, "http://"+adminAddr+"/metrics"), "ledger_ingest_received_total 101") {
		if time.Now().After(deadline) {
			t.Fatalf("metrics never showed 101 received:\n%s", get(t, "http://"+adminAddr+"/metrics"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	m := get(t, "http://"+adminAddr+"/metrics")
	if !strings.Contains(m, "ledger_ingest_malformed_total 1") {
		t.Fatalf("malformed not counted:\n%s", m)
	}
	// Public API is up; today's data is never published.
	if code := status(t, "http://"+httpAddr+"/api/v1/ops/daily/"+now.UTC().Format("2006-01-02")); code != 404 {
		t.Fatalf("today's report was served (%d)", code)
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exited %d:\n%s", code, logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not shut down")
	}
	if !strings.Contains(logs.String(), "received=101 accepted=100 malformed=1 dropped=0") {
		t.Fatalf("shutdown summary missing:\n%s", logs.String())
	}

	// Offline: build today's origin report from the database and verify it.
	reports := filepath.Join(dir, "reports")
	date := now.UTC().Format("2006-01-02")
	var out, errb bytes.Buffer
	if code := run([]string{"day", "-db", env["LEDGER_DB"], "-date", date, "-out", filepath.Join(reports, date+".json")}, getenv, &out, &errb); code != 0 {
		t.Fatalf("day: %d %s", code, errb.String())
	}
	b, _ := os.ReadFile(filepath.Join(reports, date+".json"))
	for _, want := range []string{`"n": 100`, `"bad": 4`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("report lacks %s", want)
		}
	}
	out.Reset()
	if code := run([]string{"recompute", reports}, getenv, &out, &errb); code != 0 {
		t.Fatalf("recompute: %d\n%s%s", code, out.String(), errb.String())
	}
	// Tampering makes recompute fail with a nonzero exit.
	os.WriteFile(filepath.Join(reports, date+".json"), bytes.Replace(b, []byte(`"bad": 4`), []byte(`"bad": 0`), 1), 0o644)
	out.Reset()
	if code := run([]string{"recompute", reports}, getenv, &out, &errb); code != 1 {
		t.Fatalf("recompute of a tampered report exited %d:\n%s", code, out.String())
	}
}

func TestUsageErrors(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{}, {"bogus"}, {"recompute"}, {"assemble"}, {"verify-release", "-manifest", "x"}, {"serve", "extra"}} {
		if code := run(args, func(string) string { return "" }, &out, &out); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code := run([]string{"serve"}, func(k string) string {
		if k == "LEDGER_QUEUE" {
			return "-1"
		}
		return ""
	}, &out, &out); code != 2 {
		t.Errorf("invalid configuration: exit %d", code)
	}
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became ready", url)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func get(t *testing.T, url string) string {
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func status(t *testing.T, url string) int {
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
