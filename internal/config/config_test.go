package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Socket != "/srv/ledger-socket/ingest.sock" || c.Queue != 8192 || c.RetentionDays != 35 || c.MaxDBBytes != 512<<20 {
		t.Fatalf("%+v", c)
	}
}

func TestEveryProblemIsReported(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LEDGER_SOCKET": "relative.sock", "LEDGER_QUEUE": "0", "LEDGER_HTTP_ADDR": ":9091",
		"LEDGER_NGINX_STATUS_URL": "https://public.example/status", "LEDGER_SERVICE": "http://aliakbari.dev",
		"LOG_LEVEL": "loud", "LEDGER_FLUSH_EVERY": "1h",
	}))
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	for _, k := range []string{"LEDGER_SOCKET", "LEDGER_QUEUE", "LEDGER_ADMIN_ADDR", "LEDGER_NGINX_STATUS_URL", "LEDGER_SERVICE", "LOG_LEVEL", "LEDGER_FLUSH_EVERY"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not mention %s: %v", k, err)
		}
	}
}
