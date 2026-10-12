package report

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/akynte/opsledger/internal/sli"
)

// The schemas are copies of /schemas/*.json in the repository (a test keeps
// them identical), embedded so the verifier works offline.
//
//go:embed schemas/*.json
var schemaFS embed.FS

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

func schemas() (map[string]*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		c := jsonschema.NewCompiler()
		names, err := fs.Glob(schemaFS, "schemas/*.json")
		if err != nil {
			compileErr = err
			return
		}
		for _, n := range names {
			b, err := schemaFS.ReadFile(n)
			if err != nil {
				compileErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				compileErr = fmt.Errorf("%s: %w", n, err)
				return
			}
			if err := c.AddResource("https://aliakbari.dev/"+n, doc); err != nil {
				compileErr = err
				return
			}
		}
		compiled = map[string]*jsonschema.Schema{}
		for _, n := range names {
			url := "https://aliakbari.dev/" + n
			s, err := c.Compile(url)
			if err != nil {
				compileErr = fmt.Errorf("%s: %w", n, err)
				return
			}
			compiled[url] = s
		}
	})
	return compiled, compileErr
}

// ValidateDocument validates b against the schema with the given URL.
func ValidateDocument(schemaURL string, b []byte) error {
	all, err := schemas()
	if err != nil {
		return fmt.Errorf("schemas: %w", err)
	}
	s, ok := all[schemaURL]
	if !ok {
		return fmt.Errorf("unknown schema %s", schemaURL)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if err := s.Validate(doc); err != nil {
		return fmt.Errorf("schema %s: %w", schemaURL, err)
	}
	return nil
}

// Discrepancy is one difference between a published and a recomputed value,
// or one inconsistency in the raw data.
type Discrepancy struct {
	File       string `json:"file"`
	Path       string `json:"path"`
	Published  string `json:"published,omitempty"`
	Recomputed string `json:"recomputed,omitempty"`
	Problem    string `json:"problem"`
}

func (d Discrepancy) String() string {
	if d.Published != "" || d.Recomputed != "" {
		return fmt.Sprintf("%s: %s: %s (published %s, recomputed %s)", d.File, d.Path, d.Problem, d.Published, d.Recomputed)
	}
	return fmt.Sprintf("%s: %s: %s", d.File, d.Path, d.Problem)
}

// Result is the outcome of Recompute.
type Result struct {
	DailyReports  int           `json:"daily_reports"`
	Summaries     int           `json:"summaries"`
	Discrepancies []Discrepancy `json:"discrepancies"`
}

// OK reports whether everything recomputed identically.
func (r Result) OK() bool { return len(r.Discrepancies) == 0 }

var dailyName = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.json$`)

// Recompute verifies every daily report and summary under dir:
//
//  1. each file validates against its JSON Schema and decodes strictly;
//  2. its raw data is internally consistent (counts sum up, histograms are
//     valid, probe runs agree with their checks, the date matches the window);
//  3. every derived value equals the value recomputed from the raw data;
//  4. each summary lists daily reports whose SHA-256 matches the files
//     present, and its values equal those recomputed from those files.
//
// Signature bundles (*.sigstore.json) are skipped here; verify them with
// `ledger verify-bundle`.
func Recompute(dir string) (Result, error) {
	res := Result{Discrepancies: []Discrepancy{}}
	add := func(file, path, problem string) {
		res.Discrepancies = append(res.Discrepancies, Discrepancy{File: file, Path: path, Problem: problem})
	}
	daily := map[string]DayInput{}
	var summaries []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		switch {
		case d.IsDir(), !strings.HasSuffix(name, ".json"), strings.HasSuffix(name, ".sigstore.json"):
			return nil
		case strings.HasPrefix(name, "summary"):
			summaries = append(summaries, path)
			return nil
		}
		m := dailyName.FindStringSubmatch(name)
		if m == nil {
			add(rel(dir, path), "", "unexpected file: daily reports are named YYYY-MM-DD.json")
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		r, ds := checkDaily(rel(dir, path), m[1], b)
		res.Discrepancies = append(res.Discrepancies, ds...)
		res.DailyReports++
		if r != nil {
			if prev, dup := daily[r.Date]; dup {
				add(rel(dir, path), "date", "a second report for "+prev.Report.Date)
			} else {
				sum := sha256.Sum256(b)
				daily[r.Date] = DayInput{Report: r, SHA256: hex.EncodeToString(sum[:])}
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	sort.Strings(summaries)
	for _, p := range summaries {
		res.Summaries++
		res.Discrepancies = append(res.Discrepancies, checkSummary(rel(dir, p), p, daily)...)
	}
	return res, nil
}

func rel(dir, path string) string {
	if r, err := filepath.Rel(dir, path); err == nil {
		return r
	}
	return path
}

// checkDaily validates one daily report. It returns the decoded report if
// it could be decoded (even with discrepancies) so summaries can be checked.
func checkDaily(file, nameDate string, b []byte) (*Report, []Discrepancy) {
	var ds []Discrepancy
	add := func(path, problem string) { ds = append(ds, Discrepancy{File: file, Path: path, Problem: problem}) }
	if err := ValidateDocument(RollupSchema, b); err != nil {
		add("", err.Error())
		return nil, ds
	}
	var r Report
	if err := decodeStrict(b, &r); err != nil {
		add("", "decode: "+err.Error())
		return nil, ds
	}
	if r.Date != nameDate {
		add("date", fmt.Sprintf("file is named %s but reports %s", nameDate, r.Date))
	}
	if w, err := DayWindow(r.Date); err != nil || !w.Start.Equal(r.Window.Start) || !w.End.Equal(r.Window.End) {
		add("window", "window is not the UTC day of date")
	}
	if r.SLIVersion != sli.Version {
		add("sli_version", fmt.Sprintf("this verifier implements SLI version %d, the report uses %d", sli.Version, r.SLIVersion))
		return &r, ds
	}
	if r.Origin != nil {
		ds = append(ds, checkOriginRaw(file, r.Origin, r.Window)...)
	}
	if r.Probe != nil {
		ds = append(ds, checkProbeRaw(file, r.Probe, r.Window)...)
	}
	// Recompute the derived layer and compare.
	re := r
	if r.Origin != nil {
		o := *r.Origin
		re.Origin = &o
	}
	if r.Probe != nil {
		p := *r.Probe
		re.Probe = &p
	}
	Derive(&re)
	if r.Origin != nil {
		ds = append(ds, compareJSON(file, "origin.derived", r.Origin.Derived, re.Origin.Derived)...)
	}
	if r.Probe != nil {
		ds = append(ds, compareJSON(file, "probe.derived", r.Probe.Derived, re.Probe.Derived)...)
	}
	return &r, ds
}

func checkOriginRaw(file string, o *Origin, w Window) []Discrepancy {
	var ds []Discrepancy
	add := func(path, problem string) { ds = append(ds, Discrepancy{File: file, Path: path, Problem: problem}) }
	for i, h := range o.Hours {
		p := fmt.Sprintf("origin.hours[%d]", i)
		if h.Hour != i {
			add(p+".hour", fmt.Sprintf("hours must be listed 0–23 in order; found %d at position %d", h.Hour, i))
		}
		seen := map[sli.Key]bool{}
		for j, c := range h.Cells {
			k := keyOf(c)
			if seen[k] {
				add(fmt.Sprintf("%s.cells[%d]", p, j), "duplicate key")
			}
			seen[k] = true
			cc := c.Cell
			if msg := cc.Consistent(); msg != "" {
				add(fmt.Sprintf("%s.cells[%d]", p, j), msg)
			}
			if cc.LatEligible > 0 {
				if _, ok := sli.LatencyThreshold(k.Class); !ok || !sli.AvailabilityEligible(k) {
					add(fmt.Sprintf("%s.cells[%d]", p, j), "latency counts on a key without a latency SLI")
				}
			}
		}
		in := h.Ingest
		if in.Accepted+in.Malformed > in.Received {
			add(p+".ingest", "accepted + malformed exceeds received")
		}
		if h.Reconciliation.ReconciledSeconds+h.Reconciliation.UnreconciledSeconds > 3600+MaxSampleGap {
			add(p+".reconciliation", "more reconciliation time than the hour (plus one sample interval)")
		}
	}
	for i, r := range o.Runs {
		if r.End.Before(r.Start) {
			add(fmt.Sprintf("origin.runs[%d]", i), "run ends before it starts")
		}
		if r.End.Before(w.Start) || !r.Start.Before(w.End) {
			add(fmt.Sprintf("origin.runs[%d]", i), "run does not overlap the window")
		}
	}
	return ds
}

func checkProbeRaw(file string, p *Probe, w Window) []Discrepancy {
	var ds []Discrepancy
	add := func(path, problem string) { ds = append(ds, Discrepancy{File: file, Path: path, Problem: problem}) }
	interval := time.Duration(p.IntervalMinutes) * time.Minute
	for i, r := range p.Runs {
		path := fmt.Sprintf("probe.runs[%d]", i)
		if r.Slot.Before(w.Start) || !r.Slot.Before(w.End) {
			add(path+".slot", "slot outside the report window")
		}
		if interval > 0 && r.Slot.Sub(w.Start)%interval != 0 {
			add(path+".slot", "slot is not aligned to the probe interval")
		}
		if r.FinishedAt.Before(r.StartedAt) {
			add(path, "finished before it started")
		}
		all := len(r.Checks) > 0
		for _, c := range r.Checks {
			all = all && c.OK
			if !c.OK && c.Error == "" {
				add(path, "failed check "+c.Name+" has no error message")
			}
		}
		if r.OK != all {
			add(path+".ok", "run result disagrees with its checks")
		}
	}
	return ds
}

func checkSummary(file, path string, daily map[string]DayInput) []Discrepancy {
	var ds []Discrepancy
	add := func(p, problem string) { ds = append(ds, Discrepancy{File: file, Path: p, Problem: problem}) }
	b, err := os.ReadFile(path)
	if err != nil {
		add("", err.Error())
		return ds
	}
	if err := ValidateDocument(SummarySchema, b); err != nil {
		add("", err.Error())
		return ds
	}
	var s Summary
	if err := decodeStrict(b, &s); err != nil {
		add("", "decode: "+err.Error())
		return ds
	}
	var inputs []DayInput
	for _, ref := range s.Days {
		d, ok := daily[ref.Date]
		switch {
		case !ok:
			add("days", "summary lists "+ref.Date+" but that daily report is missing")
			continue
		case d.SHA256 != ref.SHA256:
			add("days", "daily report "+ref.Date+" differs from the one the summary was computed from (sha256 mismatch)")
			continue
		}
		inputs = append(inputs, d)
	}
	interval := 0
	if s.Probe != nil {
		// Recover the interval used for days without probe data from the newest day that has it.
		for i := len(inputs) - 1; i >= 0; i-- {
			if p := inputs[i].Report.Probe; p != nil {
				interval = p.IntervalMinutes
				break
			}
		}
	}
	re, err := BuildSummary(inputs, s.Window.LastDate, s.Window.Days, interval, s.Service, s.Generator)
	if err != nil {
		add("", err.Error())
		return ds
	}
	// Missing days must be exactly those without a listed report.
	if !slices.Equal(s.MissingDays, re.MissingDays) {
		add("missing_days", "missing days do not match the window and the listed reports")
	}
	ds = append(ds, compareJSON(file, "", s, re)...)
	return ds
}

// compareJSON compares two values through their JSON encodings, so that
// every published field is checked, and reports differing leaves by path.
// Numbers are compared with a tolerance of one unit in the last published
// decimal place (they are rounded to stats.Decimals).
func compareJSON(file, root string, published, recomputed any) []Discrepancy {
	a, b := toTree(published), toTree(recomputed)
	var ds []Discrepancy
	diff(root, a, b, func(path, pub, rec, problem string) {
		ds = append(ds, Discrepancy{File: file, Path: path, Published: pub, Recomputed: rec, Problem: problem})
	})
	return ds
}

func toTree(v any) any {
	b, _ := json.Marshal(v)
	var t any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	dec.Decode(&t)
	return t
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func short(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}

func diff(path string, a, b any, report func(path, pub, rec, problem string)) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			report(path, short(a), short(b), "type differs")
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			diff(join(path, k), av[k], bv[k], report)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			report(path, short(a), short(b), "type differs")
			return
		}
		if len(av) != len(bv) {
			report(path, fmt.Sprintf("%d items", len(av)), fmt.Sprintf("%d items", len(bv)), "length differs")
			return
		}
		for i := range av {
			diff(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], report)
		}
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			report(path, short(a), short(b), "type differs")
			return
		}
		if av == bv {
			return
		}
		x, err1 := av.Float64()
		y, err2 := bv.Float64()
		if err1 != nil || err2 != nil || math.Abs(x-y) > math.Pow(10, -9)*1.5 {
			report(path, string(av), string(bv), "value differs")
		}
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			report(path, short(a), short(b), "value differs")
		}
	}
}
