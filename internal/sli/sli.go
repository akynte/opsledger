// Package sli classifies requests and defines the service level indicators.
//
// The definitions here are the contract that published reports refer to by
// Version. Changing what counts as eligible, good or bad requires a new
// Version, so that reports produced under different rules are never
// silently compared. docs/operations/slo.md explains each choice.
package sli

import (
	"strings"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
)

// Version identifies the SLI definitions below.
const Version = 1

// Class is the kind of request, derived from the server and path.
type Class string

const (
	Page      Class = "page"      // HTML document (and the résumé PDF: per-file counts are private analytics, not ops data)
	RSC       Class = "rsc"       // Next.js client navigation or prefetch payload
	API       Class = "api"       // /api/* (evidence API, release and ops endpoints)
	Static    Class = "static"    // /_next/static/* and /site-analytics.js, served from disk by nginx
	Media     Class = "media"     // /demos/* images served from disk by nginx
	Analytics Class = "analytics" // first-party analytics script and collection endpoint
	WellKnown Class = "wellknown" // /.well-known/* (ACME, security.txt)
	Other     Class = "other"     // anything else on the public server (non-GET/HEAD, ...)
	Internal  Class = "internal"  // health, status and firewall-tier requests; never user traffic
	PlainHTTP Class = "plainhttp" // port 80: redirects to HTTPS and ACME
	Unknown   Class = "unknownhost"
)

// Classes lists every class in a stable order.
var Classes = []Class{Page, RSC, API, Static, Media, Analytics, WellKnown, Other, Internal, PlainHTTP, Unknown}

// Observer says whether a request came from the external probe.
type Observer string

const (
	Organic Observer = "organic"
	Probe   Observer = "probe"
)

// Agent says whether nginx classified the user agent as automated.
type Agent string

const (
	Human Agent = "human"
	Bot   Agent = "bot"
)

// Key is the aggregation key below the minute.
type Key struct {
	Class    Class
	Observer Observer
	Agent    Agent
}

// AnalyticsPrefix is the first-party path of the analytics script and
// collection endpoint (see infra/nginx/conf.d/aliakbari.conf).
const AnalyticsPrefix = "/x/"

// Classify derives the aggregation key of a record.
func Classify(r ingest.Record) Key {
	k := Key{Class: classOf(r), Observer: Organic, Agent: Human}
	if r.Probe {
		k.Observer = Probe
	}
	if r.Bot {
		k.Agent = Bot
	}
	return k
}

func classOf(r ingest.Record) Class {
	switch r.Server {
	case ingest.ServerWAF, ingest.ServerInternal:
		return Internal
	case ingest.ServerPlain:
		return PlainHTTP
	case ingest.ServerOther:
		return Unknown
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		// The collection endpoint is the only path that accepts POST.
		if r.Method == "POST" && strings.HasPrefix(r.Path, AnalyticsPrefix) {
			return Analytics
		}
		return Other
	}
	p := r.Path
	switch {
	case strings.HasPrefix(p, "/_next/static/"), p == "/site-analytics.js":
		return Static
	case strings.HasPrefix(p, "/demos/"):
		return Media
	case strings.HasPrefix(p, "/api/"):
		return API
	case strings.HasPrefix(p, AnalyticsPrefix):
		return Analytics
	case strings.HasPrefix(p, "/.well-known/"):
		return WellKnown
	case r.RSC:
		return RSC
	}
	return Page
}

// Outcome is the SLI-relevant outcome of a status code.
type Outcome int

const (
	OK           Outcome = iota // 1xx–4xx other than the codes below: served as intended
	Blocked                     // 403: refused by the firewall (the site has no other 403s)
	RateLimited                 // 429: refused by a rate limit
	ClientClosed                // 499: the client went away before the response
	ServerError                 // 5xx
)

// OutcomeOf maps a status code to its outcome.
func OutcomeOf(status int) Outcome {
	switch {
	case status >= 500:
		return ServerError
	case status == 403:
		return Blocked
	case status == 429:
		return RateLimited
	case status == 499:
		return ClientClosed
	}
	return OK
}

// AvailabilityEligible reports whether a request counts toward the origin
// availability SLI: organic, non-bot requests for pages (including the
// résumé PDF), navigation payloads and the API on the public server. Static files and
// images are served from local disk and mostly from the CDN edge, so they
// are reported but not part of the SLI; probe requests have their own
// external SLI; bots and internal requests are not users.
func AvailabilityEligible(k Key) bool {
	if k.Observer != Organic || k.Agent != Human {
		return false
	}
	switch k.Class {
	case Page, RSC, API:
		return true
	}
	return false
}

// AvailabilityCounts reports whether an outcome counts in the availability
// denominator and, if so, whether it is good. 5xx is bad; firewall blocks,
// rate-limit refusals and client disconnects are excluded (counted and
// reported separately): they are deliberate policy or the client's choice,
// not the service failing.
func AvailabilityCounts(o Outcome) (counted, good bool) {
	switch o {
	case OK:
		return true, true
	case ServerError:
		return true, false
	}
	return false, false
}

// LatencyThreshold is the origin latency objective per class: the request
// is good if nginx's $request_time is at most this. ok is false for classes
// without a latency SLI.
func LatencyThreshold(c Class) (time.Duration, bool) {
	switch c {
	case Page, RSC:
		return 300 * time.Millisecond, true
	case API:
		return 100 * time.Millisecond, true
	}
	return 0, false
}

// LatencyEligible reports whether a request counts toward the latency SLI:
// availability-eligible and not a server error (a failed request has no
// meaningful latency; it already counts against availability).
func LatencyEligible(k Key, o Outcome) bool {
	_, ok := LatencyThreshold(k.Class)
	return ok && AvailabilityEligible(k) && o == OK
}

// Objectives over the rolling window.
const (
	AvailabilityTarget = 0.995 // origin: 99.5% of eligible requests not 5xx
	LatencyTarget      = 0.99  // origin: 99% of eligible requests within the class threshold
	ProbeTarget        = 0.995 // external: 99.5% of probe checks succeed
	CoverageTarget     = 0.99  // completeness: 99% of nginx's requests reach the ledger
	WindowDays         = 28
)
