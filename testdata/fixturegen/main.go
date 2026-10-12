//go:build ignore

// Generates a 15-day database (packages/evidence/test/fixtures/ops-summary-15d.json was built from it:
// go run ./testdata/fixturegen/main.go DB, then ledger day / assemble / summary) with synthetic traffic, for building fixture reports with the real
// pipeline (ledger day → assemble → summary). Not part of the ledger.
package main

import (
	"context"
	"os"
	"time"

	"github.com/akynte/opsledger/internal/ingest"
	"github.com/akynte/opsledger/internal/series"
	"github.com/akynte/opsledger/internal/sli"
	"github.com/akynte/opsledger/internal/storage"
)

func main() {
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	st, err := storage.Open(ctx, os.Args[1], storage.Options{Now: func() time.Time { return start }})
	if err != nil {
		panic(err)
	}
	run, _ := st.StartRun(ctx, "fixture", "")
	for d := 0; d < 15; d++ {
		day := start.AddDate(0, 0, d)
		cells := map[storage.MinuteKey]*series.Cell{}
		for h := 0; h < 24; h++ {
			at := day.Add(time.Duration(h)*time.Hour + 7*time.Minute)
			r := ingest.Record{Time: at, Server: ingest.ServerPublic, Method: "GET", Path: "/", Status: 200, Duration: uint64(3000 + h*100)}
			k := sli.Classify(r)
			c := series.New()
			for i := 0; i < 10; i++ {
				c.Add(k, r)
			}
			cells[storage.MinuteKey{Minute: at.Unix() / 60, Key: k}] = c
		}
		if err := st.Write(ctx, storage.Flush{Cells: cells, Run: run, Heartbeat: day.Add(24 * time.Hour).Unix()}); err != nil {
			panic(err)
		}
	}
	st.Close()
}
