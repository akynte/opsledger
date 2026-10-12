// Package ingest receives nginx access-log records over a Unix datagram
// socket (nginx's syslog transport), parses them strictly and hands them to
// the aggregator through a bounded queue.
//
// The records carry no visitor data: nginx sends the request time, server,
// method, a length-capped path, status, durations and three flags it
// computes itself (RSC prefetch, bot user agent, probe). It never sends
// addresses, user agents, query strings or referrers. See
// infra/nginx/snippets/ledger-log.conf.
package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RecordVersion is the version of the log format this parser accepts.
const RecordVersion = 1

// MaxRecordBytes bounds one datagram. nginx caps the path at 192 bytes, so a
// valid record is far smaller; anything larger is malformed.
const MaxRecordBytes = 2048

// Server identifies which nginx server block handled the request.
type Server string

const (
	ServerPublic   Server = "pub"  // https://aliakbari.dev (the only server visitors use)
	ServerWAF      Server = "waf"  // inner firewall tier, one per proxied public request
	ServerPlain    Server = "http" // :80 redirects and ACME challenges
	ServerInternal Server = "int"  // health and status endpoints on the private network
	ServerOther    Server = "oth"  // unknown host names (closed with 444)
)

// Record is one parsed access-log line.
type Record struct {
	Time     time.Time // request completion, UTC, millisecond precision
	Server   Server
	Method   string
	Path     string // URI path, at most 192 bytes, no query string
	Status   int
	Duration uint64 // $request_time in microseconds (nginx resolution: 1 ms)
	RSC      bool   // Next.js RSC fetch (client navigation or prefetch)
	Bot      bool   // user agent matched nginx's bot map, or was empty
	Probe    bool   // carried a well-formed X-Opsledger-Probe header
	Bytes    int64  // response body bytes
}

type wireRecord struct {
	V     *int    `json:"v"`
	T     *string `json:"t"`
	Srv   *string `json:"srv"`
	M     *string `json:"m"`
	U     *string `json:"u"`
	St    *int    `json:"st"`
	Rt    *string `json:"rt"`
	RSC   *string `json:"rsc"`
	Bot   *string `json:"bot"`
	Probe *string `json:"probe"`
	B     *int64  `json:"b"`
}

// ErrMalformed wraps every parse failure.
var ErrMalformed = errors.New("malformed record")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// ParseDatagram parses one syslog datagram as nginx sends it:
//
//	<PRI>Mmm dd hh:mm:ss HOST TAG: MESSAGE
//
// and returns the record in MESSAGE. The syslog timestamp is ignored (it
// has second resolution and no year); the record's own $msec is used.
func ParseDatagram(b []byte) (Record, error) {
	if len(b) > MaxRecordBytes {
		return Record{}, malformed("datagram of %d bytes exceeds %d", len(b), MaxRecordBytes)
	}
	if len(b) < 2 || b[0] != '<' {
		return Record{}, malformed("missing syslog priority")
	}
	end := bytes.IndexByte(b, '>')
	if end < 2 || end > 4 {
		return Record{}, malformed("bad syslog priority")
	}
	for _, c := range b[1:end] {
		if c < '0' || c > '9' {
			return Record{}, malformed("bad syslog priority")
		}
	}
	rest := b[end+1:]
	// "Mmm dd hh:mm:ss " is 16 bytes.
	if len(rest) < 16 || rest[3] != ' ' || rest[15] != ' ' {
		return Record{}, malformed("bad syslog timestamp")
	}
	rest = rest[16:]
	// HOST TAG: MESSAGE
	sp := bytes.IndexByte(rest, ' ')
	if sp <= 0 {
		return Record{}, malformed("missing syslog host")
	}
	rest = rest[sp+1:]
	colon := bytes.Index(rest, []byte(": "))
	if colon <= 0 || colon > 32 {
		return Record{}, malformed("missing syslog tag")
	}
	return ParseRecord(rest[colon+2:])
}

// ParseRecord parses the JSON access-log record. Every field is required,
// unknown fields are rejected, and every value is range-checked.
func ParseRecord(msg []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.DisallowUnknownFields()
	var w wireRecord
	if err := dec.Decode(&w); err != nil {
		return Record{}, malformed("json: %v", err)
	}
	if dec.More() {
		return Record{}, malformed("trailing data after record")
	}
	if w.V == nil || w.T == nil || w.Srv == nil || w.M == nil || w.U == nil || w.St == nil ||
		w.Rt == nil || w.RSC == nil || w.Bot == nil || w.Probe == nil || w.B == nil {
		return Record{}, malformed("missing field")
	}
	if *w.V != RecordVersion {
		return Record{}, malformed("unsupported record version %d", *w.V)
	}
	var r Record
	var err error
	if r.Time, err = parseMsec(*w.T); err != nil {
		return Record{}, err
	}
	switch s := Server(*w.Srv); s {
	case ServerPublic, ServerWAF, ServerPlain, ServerInternal, ServerOther:
		r.Server = s
	default:
		return Record{}, malformed("unknown server %q", *w.Srv)
	}
	if !validMethod(*w.M) {
		return Record{}, malformed("bad method")
	}
	r.Method = *w.M
	if len(*w.U) > 192 {
		return Record{}, malformed("path longer than 192 bytes")
	}
	r.Path = *w.U
	if *w.St < 100 || *w.St > 999 {
		return Record{}, malformed("status %d out of range", *w.St)
	}
	r.Status = *w.St
	if r.Duration, err = parseSeconds(*w.Rt); err != nil {
		return Record{}, err
	}
	switch *w.RSC {
	case "", "0":
	case "1":
		r.RSC = true
	default:
		return Record{}, malformed("bad rsc flag")
	}
	if r.Bot, err = flag(*w.Bot, "bot"); err != nil {
		return Record{}, err
	}
	if r.Probe, err = flag(*w.Probe, "probe"); err != nil {
		return Record{}, err
	}
	if *w.B < 0 || *w.B > 1<<40 {
		return Record{}, malformed("body bytes out of range")
	}
	r.Bytes = *w.B
	return r, nil
}

func flag(s, name string) (bool, error) {
	switch s {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, malformed("bad %s flag", name)
}

func validMethod(m string) bool {
	if m == "" || len(m) > 16 {
		return false
	}
	for _, c := range m {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// parseMsec parses nginx $msec ("1728730000.123") into a UTC time.
func parseMsec(s string) (time.Time, error) {
	sec, frac, ok := strings.Cut(s, ".")
	if !ok || len(sec) < 9 || len(sec) > 11 || len(frac) != 3 || !digits(sec) || !digits(frac) {
		return time.Time{}, malformed("bad timestamp %q", s)
	}
	secs, _ := strconv.ParseInt(sec, 10, 64)
	ms, _ := strconv.ParseInt(frac, 10, 64)
	return time.Unix(secs, ms*int64(time.Millisecond)).UTC(), nil
}

// parseSeconds parses nginx's "%d.%03d" seconds into microseconds.
func parseSeconds(s string) (uint64, error) {
	sec, frac, ok := strings.Cut(s, ".")
	if !ok || sec == "" || len(sec) > 6 || len(frac) != 3 || !digits(sec) || !digits(frac) {
		return 0, malformed("bad duration %q", s)
	}
	secs, _ := strconv.ParseUint(sec, 10, 64)
	ms, _ := strconv.ParseUint(frac, 10, 64)
	return secs*1_000_000 + ms*1_000, nil
}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}
