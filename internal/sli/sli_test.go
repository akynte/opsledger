package sli

import (
	"testing"

	"github.com/akynte/opsledger/internal/ingest"
)

func rec(server ingest.Server, method, path string) ingest.Record {
	return ingest.Record{Server: server, Method: method, Path: path, Status: 200}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		r    ingest.Record
		want Class
	}{
		{rec(ingest.ServerPublic, "GET", "/"), Page},
		{rec(ingest.ServerPublic, "HEAD", "/proof/x"), Page},
		{ingest.Record{Server: ingest.ServerPublic, Method: "GET", Path: "/work", RSC: true}, RSC},
		{rec(ingest.ServerPublic, "GET", "/api/v1/evidence"), API},
		{rec(ingest.ServerPublic, "GET", "/api/v1/ops/summary"), API},
		{rec(ingest.ServerPublic, "GET", "/resume.pdf"), Page}, // never singled out in public reports
		{rec(ingest.ServerPublic, "GET", "/_next/static/chunks/a.js"), Static},
		{rec(ingest.ServerPublic, "GET", "/site-analytics.js"), Static},
		{rec(ingest.ServerPublic, "GET", "/demos/fluxa/x.webp"), Media},
		{rec(ingest.ServerPublic, "GET", "/x/s.js"), Analytics},
		{rec(ingest.ServerPublic, "POST", "/x/api/send"), Analytics},
		{rec(ingest.ServerPublic, "POST", "/proof"), Other},
		{rec(ingest.ServerPublic, "DELETE", "/x/api/send"), Other},
		{rec(ingest.ServerPublic, "GET", "/.well-known/acme-challenge/t"), WellKnown},
		{rec(ingest.ServerWAF, "GET", "/"), Internal},
		{rec(ingest.ServerInternal, "GET", "/stub_status"), Internal},
		{rec(ingest.ServerPlain, "GET", "/"), PlainHTTP},
		{rec(ingest.ServerOther, "GET", "/"), Unknown},
		// An RSC header on an API path does not turn it into navigation.
		{ingest.Record{Server: ingest.ServerPublic, Method: "GET", Path: "/api/v1/", RSC: true}, API},
	}
	for _, c := range cases {
		if got := Classify(c.r).Class; got != c.want {
			t.Errorf("Classify(%s %s %s rsc=%v) = %s, want %s", c.r.Server, c.r.Method, c.r.Path, c.r.RSC, got, c.want)
		}
	}
}

func TestObserverAndAgent(t *testing.T) {
	r := rec(ingest.ServerPublic, "GET", "/")
	r.Probe, r.Bot = true, true
	k := Classify(r)
	if k.Observer != Probe || k.Agent != Bot {
		t.Fatalf("got %+v", k)
	}
	if AvailabilityEligible(k) {
		t.Fatal("probe and bot requests must not count toward the organic SLI")
	}
}

func TestOutcomes(t *testing.T) {
	cases := map[int]Outcome{200: OK, 204: OK, 304: OK, 301: OK, 400: OK, 404: OK, 405: OK, 403: Blocked,
		429: RateLimited, 499: ClientClosed, 500: ServerError, 502: ServerError, 503: ServerError, 504: ServerError}
	for status, want := range cases {
		if got := OutcomeOf(status); got != want {
			t.Errorf("OutcomeOf(%d) = %d, want %d", status, got, want)
		}
	}
}

func TestAvailabilityRules(t *testing.T) {
	for o, want := range map[Outcome][2]bool{
		OK: {true, true}, ServerError: {true, false},
		Blocked: {false, false}, RateLimited: {false, false}, ClientClosed: {false, false},
	} {
		counted, good := AvailabilityCounts(o)
		if counted != want[0] || good != want[1] {
			t.Errorf("outcome %d: counted=%v good=%v, want %v", o, counted, good, want)
		}
	}
	for _, c := range Classes {
		k := Key{Class: c, Observer: Organic, Agent: Human}
		want := c == Page || c == RSC || c == API
		if AvailabilityEligible(k) != want {
			t.Errorf("class %s eligible = %v, want %v", c, !want, want)
		}
	}
}

func TestLatencyEligibility(t *testing.T) {
	page := Key{Class: Page, Observer: Organic, Agent: Human}
	if !LatencyEligible(page, OK) {
		t.Fatal("a served page is latency-eligible")
	}
	if LatencyEligible(page, ServerError) {
		t.Fatal("a 5xx already counts against availability, not latency")
	}
	if LatencyEligible(Key{Class: Static, Observer: Organic, Agent: Human}, OK) {
		t.Fatal("static files have no latency SLI")
	}
	if d, ok := LatencyThreshold(API); !ok || d.Milliseconds() != 100 {
		t.Fatal("API threshold is 100 ms")
	}
}
