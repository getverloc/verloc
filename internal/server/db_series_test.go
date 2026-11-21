package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSeriesSinceBucketsKeepTotals(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-2 * time.Hour).UnixMilli()
	for i := 0; i < 100; i++ {
		d := &Delta{
			TS:       base + int64(i)*1000,
			Requests: 1,
			TokIn:    10,
			Cost:     0.01,
			MS:       3000,
			Models:   map[string]deltaModel{"hy/a": {Requests: 1, TokIn: 10, Cost: 0.01}},
			Backends: map[string]deltaBackend{"hyper": {Requests: 1, Cost: 0.01, Err: 0}},
			Payload:  `{"avg":12,"p50":10,"p95":20,"p99":30,"ttft":5}`,
		}
		if i%2 == 0 {
			d.Requests = 0
			d.Cost = 0
			d.Models = map[string]deltaModel{}
			d.Backends = map[string]deltaBackend{}
			d.Payload = ""
		}
		if err := s.InsertDelta(d); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.SeriesSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || len(rows) > 10 {
		t.Fatalf("buckets = %d, want 1..10", len(rows))
	}
	var req int64
	var cost float64
	for _, r := range rows {
		req += r.Requests
		cost += r.Cost
	}
	if req != 50 {
		t.Fatalf("requests = %d, want 50", req)
	}
	if cost < 0.49 || cost > 0.51 {
		t.Fatalf("cost = %v, want 0.50", cost)
	}
	var modelReq int64
	for _, r := range rows {
		modelReq += r.Models["hy/a"].Requests
	}
	if modelReq != 50 {
		t.Fatalf("model requests = %d, want 50", modelReq)
	}
	if !rows[0].DistributionsUnavailable || rows[0].LatP95 != 0 {
		t.Fatal("legacy percentiles cannot be averaged into a window distribution")
	}
}

func TestSeriesSinceEmptyIsArray(t *testing.T) {
	s := testKeyEventStore(t)
	rows, err := s.SeriesSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil {
		t.Fatal("empty window must be an empty slice, not nil")
	}
}

func TestSeriesRangeEncodedRespectsUntilBound(t *testing.T) {
	s := testKeyEventStore(t)
	base := int64(1_800_000_000_000)
	for i := 0; i < 6; i++ {
		d := &Delta{
			TS:       base + int64(i)*60_000,
			Requests: 1,
			Cost:     0.1,
			MS:       1000,
			Models:   map[string]deltaModel{"m/a": {Requests: 1, Cost: 0.1}},
			Backends: map[string]deltaBackend{"p": {Requests: 1, Cost: 0.1}},
		}
		if err := s.InsertDelta(d); err != nil {
			t.Fatal(err)
		}
	}
	raw, gz, err := s.SeriesRangeEncoded(context.Background(), base+60_000, base+240_000, 100, false)
	if err != nil || gz {
		t.Fatalf("SeriesRangeEncoded=(%v, %v)", gz, err)
	}
	var rows []SeriesRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows=%d want 4", len(rows))
	}
	if rows[0].TS != base+60_000 || rows[len(rows)-1].TS != base+240_000 {
		t.Fatalf("bounds=[%d..%d] want [%d..%d]", rows[0].TS, rows[len(rows)-1].TS, base+60_000, base+240_000)
	}
}
