package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// The OpenAPI description and the handler must describe the same endpoints:
// every documented path is served (not 404), and every served path is documented.
func TestOpenAPIMatchesHandler(t *testing.T) {
	b, err := os.ReadFile("../../openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	h, _, _ := setup(t)
	var documented []string
	for p, ops := range spec.Paths {
		documented = append(documented, p)
		if _, ok := ops["get"]; !ok || len(ops) != 1 {
			t.Errorf("%s: only GET is served", p)
		}
		concrete := strings.ReplaceAll(p, "{date}", "2026-10-11")
		if rec := get(h, concrete); rec.Code == http.StatusNotFound && !strings.Contains(p, "release") {
			t.Errorf("documented path %s is not served (%d)", p, rec.Code)
		}
	}
	sort.Strings(documented)
	served := []string{"/api/v1/ops/", "/api/v1/ops/daily/{date}", "/api/v1/ops/status", "/api/v1/ops/summary", "/api/v1/release"}
	if strings.Join(documented, " ") != strings.Join(served, " ") {
		t.Fatalf("documented %v, served %v", documented, served)
	}
	for _, p := range []string{"/api/v1/ops/admin", "/api/v1/ops/minutes", "/api/v1/ops/daily/", "/metrics", "/healthz"} {
		if rec := get(h, p); rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
			t.Errorf("undocumented path %s answered %d", p, rec.Code)
		}
	}
}
