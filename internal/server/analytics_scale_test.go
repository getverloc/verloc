package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestSnapshotHourEquivalenceAndSeriesSinceHourlyTier(t *testing.T) {
	s := testKeyEventStore(t)
	ctx := context.Background()

	// Seed 24 hours of ticks (1 tick every 2 minutes = 720 ticks) so
	// (maxTs - minTs) / maxPoints >= 3600000 when maxPoints = 10, exercising
	// the snapshot_hour tier as well as partial leading-hour minute blending.
	base := int64(1700000000000 / 3600000 * 3600000) // exact hour boundary
	const totalTicks = 720
	for i := range totalTicks {
		ts := base + int64(i)*120_000 // every 2 minutes -> spans 24 hours
		d := &Delta{
			TS:       ts,
			Requests: 2,
			TokIn:    500,
			TokOut:   100,
			TokCache: 50,
			Cost:     0.002,
			MS:       120_000,
			Err:      int64(i % 5 / 4),
			Models: map[string]deltaModel{
				fmt.Sprintf("m/%d", i%3): {Requests: 2, TokIn: 500, TokOut: 100, TokCache: 50, Cost: 0.002},
			},
			Backends: map[string]deltaBackend{
				fmt.Sprintf("b/%d", i%2): {Requests: 2, TokIn: 500, TokOut: 100, TokCache: 50, Err: int64(i % 5 / 4), Cost: 0.002},
			},
			Payload: `{"avg":800,"p50":600,"p95":2000,"p99":5000,"ttft":200,"ttft_n":2,"lat_hist":[0,0,0,0,1,1,0,0,0,0,0]}`,
		}
		if err := s.InsertDelta(d); err != nil {
			t.Fatal(err)
		}
	}

	type hourRow struct {
		req, tin, tout, tcache, errN, ms int64
		cost                             float64
		latW, latSum, ttftW, ttftSum     float64
		histN, missingN                  int64
		models                           []byte
	}
	readHours := func() map[int64]hourRow {
		rows, err := s.db.Query(`SELECT hour,requests,tok_in,tok_out,tok_cache,err,ms,cost,lat_w,lat_sum,ttft_w,ttft_sum,hist_n,missing_n,models FROM snapshot_hour ORDER BY hour`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]hourRow{}
		for rows.Next() {
			var h int64
			var r hourRow
			if err := rows.Scan(&h, &r.req, &r.tin, &r.tout, &r.tcache, &r.errN, &r.ms, &r.cost,
				&r.latW, &r.latSum, &r.ttftW, &r.ttftSum, &r.histN, &r.missingN, &r.models); err != nil {
				t.Fatal(err)
			}
			out[h] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	incr := readHours()
	if len(incr) != 24 {
		t.Fatalf("incremental snapshot_hour rows=%d, want 24", len(incr))
	}
	if _, err := s.RebuildRollups(ctx); err != nil {
		t.Fatal(err)
	}
	rebuilt := readHours()
	if len(rebuilt) != len(incr) {
		t.Fatalf("rebuilt snapshot_hour rows=%d, want %d", len(rebuilt), len(incr))
	}
	for h, a := range incr {
		b, ok := rebuilt[h]
		if !ok {
			t.Fatalf("hour %d missing after RebuildRollups", h)
		}
		if a.req != b.req || a.tin != b.tin || a.tout != b.tout || a.tcache != b.tcache ||
			a.errN != b.errN || a.ms != b.ms || a.histN != b.histN || a.missingN != b.missingN ||
			diffFloat(a.cost, b.cost) || diffFloat(a.latW, b.latW) || diffFloat(a.latSum, b.latSum) ||
			diffFloat(a.ttftW, b.ttftW) || diffFloat(a.ttftSum, b.ttftSum) || string(a.models) != string(b.models) {
			t.Fatalf("hour %d mismatch:\n incr=%+v\n rebuilt=%+v", h, a, b)
		}
	}

	// Verify SeriesSince uses hourly tier (AggregateBucketMs >= 3600000) and
	// preserves exact totals, dimensions, and histograms for both full-range
	// and mid-hour sinceMs queries.
	for _, since := range []int64{0, base + 20*60_000} {
		rows, err := s.SeriesSince(ctx, since, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 || len(rows) > 10 {
			t.Fatalf("since=%d: len(rows)=%d, want 1..10", since, len(rows))
		}
		if rows[0].AggregateBucketMs < 3600000 {
			t.Fatalf("since=%d: expected hourly tier (bucket>=3600000), got %d", since, rows[0].AggregateBucketMs)
		}
		// Compare against raw ticks directly.
		var wantReq, wantIn, wantOut, wantErr int64
		var wantCost float64
		if err := s.db.QueryRow(`SELECT COALESCE(SUM(d_requests),0), COALESCE(SUM(d_tok_in),0), COALESCE(SUM(d_tok_out),0), COALESCE(SUM(d_err),0), COALESCE(SUM(d_cost),0) FROM snapshots WHERE ts >= ?`, since).
			Scan(&wantReq, &wantIn, &wantOut, &wantErr, &wantCost); err != nil {
			t.Fatal(err)
		}
		var gotReq, gotIn, gotOut, gotErr, modelReq, backendReq int64
		var gotCost float64
		for _, r := range rows {
			gotReq += r.Requests
			gotIn += r.TokIn
			gotOut += r.TokOut
			gotErr += r.Err
			gotCost += r.Cost
			for _, m := range r.Models {
				modelReq += m.Requests
			}
			for _, b := range r.Backends {
				backendReq += b.Requests
			}
			if r.DistributionsUnavailable || r.LatP95 == 0 {
				t.Fatalf("since=%d: missing distributions on bucket ts=%d", since, r.TS)
			}
		}
		if gotReq != wantReq || gotIn != wantIn || gotOut != wantOut || gotErr != wantErr || diffFloat(gotCost, wantCost) {
			t.Fatalf("since=%d totals mismatch: got (%d,%d,%d,%d,%v) want (%d,%d,%d,%d,%v)",
				since, gotReq, gotIn, gotOut, gotErr, gotCost, wantReq, wantIn, wantOut, wantErr, wantCost)
		}
		if modelReq != wantReq || backendReq != wantReq {
			t.Fatalf("since=%d dimensions lost: modelReq=%d backendReq=%d want=%d", since, modelReq, backendReq, wantReq)
		}
	}
}

func assertAnalyticsEqual(t *testing.T, label string, got, want AnalyticsResult) {
	t.Helper()
	approx := func(a, b float64) bool { return math.Abs(a-b) <= 1e-6 }
	eqSummary := func(a, b RequestSummary) bool {
		return a.Requests == b.Requests && a.Errors == b.Errors &&
			a.TokIn == b.TokIn && a.TokOut == b.TokOut && a.TokCache == b.TokCache &&
			a.Images == b.Images && a.UnknownImages == b.UnknownImages &&
			a.UnknownUsage == b.UnknownUsage && a.UnknownCost == b.UnknownCost &&
			approx(a.Cost, b.Cost) && approx(a.AvgLatencyMs, b.AvgLatencyMs) && approx(a.AvgTTFTMs, b.AvgTTFTMs)
	}
	if !eqSummary(got.Summary, want.Summary) {
		t.Fatalf("%s summary mismatch:\n got =%+v\n want=%+v", label, got.Summary, want.Summary)
	}
	checkGroups := func(kind string, a, b []UsageGroup) {
		if len(a) != len(b) {
			t.Fatalf("%s %s len=%d want %d", label, kind, len(a), len(b))
		}
		for i := range a {
			if a[i].ID != b[i].ID || a[i].Name != b[i].Name || !eqSummary(a[i].RequestSummary, b[i].RequestSummary) {
				t.Fatalf("%s %s[%d] mismatch:\n got =%+v\n want=%+v", label, kind, i, a[i], b[i])
			}
		}
	}
	checkGroups("models", got.Models, want.Models)
	checkGroups("providers", got.Providers, want.Providers)
	checkGroups("clients", got.Clients, want.Clients)
	checkGroups("endpoints", got.Endpoints, want.Endpoints)
	if !reflect.DeepEqual(got.Status, want.Status) {
		t.Fatalf("%s status mismatch:\n got =%+v\n want=%+v", label, got.Status, want.Status)
	}
	if !reflect.DeepEqual(got.Latency, want.Latency) {
		t.Fatalf("%s latency mismatch:\n got =%+v\n want=%+v", label, got.Latency, want.Latency)
	}
	if !reflect.DeepEqual(got.TTFT, want.TTFT) {
		t.Fatalf("%s ttft mismatch:\n got =%+v\n want=%+v", label, got.TTFT, want.TTFT)
	}
	if got.Coverage.OldestTS != want.Coverage.OldestTS ||
		got.Coverage.NewestTS != want.Coverage.NewestTS ||
		got.Coverage.DetailOldestTS != want.Coverage.DetailOldestTS ||
		got.Coverage.DetailNewestTS != want.Coverage.DetailNewestTS ||
		got.Coverage.AggregateOnlyRequests != want.Coverage.AggregateOnlyRequests {
		t.Fatalf("%s coverage mismatch:\n got =%+v\n want=%+v", label, got.Coverage, want.Coverage)
	}
}

func TestAnalyticsHourSummaryParityAndEdgeCases(t *testing.T) {
	s := testKeyEventStore(t)
	ctx := context.Background()
	const hour int64 = 3600000
	base := int64(10) * hour

	// Seed rich dataset spanning 5 hours with mixed endpoints, images, unknown
	// usage/cost, errors, and client display name renames + same-ts ties.
	batch := []ReqRecord{
		{TS: base + 100, ID: "r1", Model: "gpt-4o", Upstream: "up1", Backend: "openai", ClientKeyID: "ck_1", ClientName: "Alpha-Old", Endpoint: "chat", Status: 200, OK: true, Outcome: "completed", UsageKnown: true, CostKnown: true, TokIn: 100, TokOut: 50, TokCache: 20, Cost: 0.12, LatMs: 250, TTFTMs: 80},
		{TS: base + 100, ID: "r2", Model: "gpt-4o", Upstream: "up1", Backend: "openai", ClientKeyID: "ck_1", ClientName: "Alpha-TieZ", Endpoint: "chat", Status: 502, OK: false, Outcome: "upstream_error", UsageKnown: false, CostKnown: false, LatMs: 1500},
		{TS: base + hour + 500, ID: "r3", Model: "text-emb", Upstream: "up2", Backend: "groq", ClientKeyID: "ck_2", ClientName: "Beta", Endpoint: "embeddings", Status: 200, OK: true, Outcome: "completed", UsageKnown: true, CostKnown: true, TokIn: 400, Cost: 0.01, LatMs: 40},
		{TS: base + 2*hour + 1234, ID: "r4", Model: "img-gen", Upstream: "up3", Backend: "openai", ClientKeyID: "ck_1", ClientName: "Alpha-New", Endpoint: "images/generations", Status: 200, OK: true, Outcome: "completed", UsageKnown: false, CostKnown: true, Cost: 0.08, Images: 2, ImagesKnown: true, LatMs: 3200},
		{TS: base + 2*hour + 5000, ID: "r5", Model: "img-gen", Upstream: "up3", Backend: "openai", ClientKeyID: "ck_3", ClientName: "", Endpoint: "images/edits", Status: 200, OK: true, Outcome: "completed", UsageKnown: false, CostKnown: false, ImagesKnown: false, LatMs: 4100},
		{TS: base + 3*hour + 900, ID: "r6", Model: "claude", Upstream: "up4", Backend: "anthropic", ClientKeyID: "ck_2", ClientName: "Beta-V2", Endpoint: "", Status: 200, OK: true, Outcome: "completed", UsageKnown: true, CostKnown: true, TokIn: 200, TokOut: 80, Cost: 0.35, LatMs: 900, TTFTMs: 120},
		{TS: base + 4*hour + 200, ID: "r7", Model: "gpt-4o", Upstream: "up1", Backend: "openai", ClientKeyID: "ck_1", ClientName: "Alpha-Latest", Endpoint: "chat", Status: 429, OK: false, Outcome: "upstream_rejected", UsageKnown: false, CostKnown: false, LatMs: 55},
	}
	if err := s.insertRequests(batch); err != nil {
		t.Fatal(err)
	}

	// Add one legacy request_hourly row (mirrors_detail=0) and one mirroring row
	// (mirrors_detail=1, which must be ignored).
	var lh, th [len(latBuckets) + 1]uint64
	lh[bucketOf(500)] = 1
	th[bucketOf(100)] = 1
	lhJSON, _ := json.Marshal(lh)
	thJSON, _ := json.Marshal(th)
	if _, err := s.db.Exec(`INSERT INTO request_hourly (hour,model,upstream,backend,client_key_id,client_name,status,ok,requests,errors,tok_in,tok_out,tok_cache,cost,unknown_usage,unknown_cost,lat_sum,ttft_sum,ttft_n,lat_hist,ttft_hist,mirrors_detail) VALUES
		(?, 'gpt-4o','up1','openai','ck_1','LegacyName',200,1,1,0,50,25,0,0.05,0,0,500,100,1,?,?,0),
		(?, 'gpt-4o','up1','openai','ck_1','MirroredIgnored',200,1,9,0,999,999,0,9.99,0,0,9000,900,9,?,?,1)`,
		base+hour, string(lhJSON), string(thJSON),
		base+2*hour, string(lhJSON), string(thJSON)); err != nil {
		t.Fatal(err)
	}

	filters := []struct {
		name string
		f    RequestFilter
	}{
		{"all_unfiltered", RequestFilter{}},
		{"since_partial", RequestFilter{Since: base + 100}},
		{"since_mid_hour", RequestFilter{Since: base + hour + 200}},
		{"until_partial", RequestFilter{Until: base + 3*hour + 900}},
		{"both_partial_multi_hour", RequestFilter{Since: base + 100, Until: base + 4*hour + 100}},
		{"single_hour_narrow", RequestFilter{Since: base + 2*hour + 1000, Until: base + 2*hour + 5000}},
		{"exact_hour_aligned", RequestFilter{Since: base + hour, Until: base + 3*hour - 1}},
		{"filter_model", RequestFilter{Model: "gpt-4o"}},
		{"filter_backend", RequestFilter{Backend: "openai"}},
		{"filter_client", RequestFilter{ClientKeyID: "ck_1"}},
		{"filter_endpoint_images", RequestFilter{Endpoint: "images/generations"}},
		{"filter_endpoint_unknown", RequestFilter{Endpoint: "unknown"}},
		{"filter_status_error", RequestFilter{Status: "error"}},
		{"filter_status_code", RequestFilter{Status: "502"}},
		{"filter_q", RequestFilter{Q: "beta"}},
	}

	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			// Cache path (request_hour_cache_ready = 1)
			if _, err := s.db.Exec(`UPDATE analytics_meta SET value=1 WHERE name='request_hour_cache_ready'`); err != nil {
				t.Fatal(err)
			}
			got, err := s.Analytics(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			// Raw single-pass fallback (request_hour_cache_ready = 0)
			if _, err := s.db.Exec(`UPDATE analytics_meta SET value=0 WHERE name='request_hour_cache_ready'`); err != nil {
				t.Fatal(err)
			}
			want, err := s.Analytics(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			assertAnalyticsEqual(t, tc.name, got, want)
		})
	}

	// Restore cache ready and verify PruneFailedRequests keeps cache exact.
	if _, err := s.db.Exec(`UPDATE analytics_meta SET value=1 WHERE name='request_hour_cache_ready'`); err != nil {
		t.Fatal(err)
	}
	stats, err := s.PruneFailedRequests(false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 2 {
		t.Fatalf("pruned failed=%d, want 2", stats.Failed)
	}
	gotAfterPrune, err := s.Analytics(ctx, RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE analytics_meta SET value=0 WHERE name='request_hour_cache_ready'`); err != nil {
		t.Fatal(err)
	}
	wantAfterPrune, err := s.Analytics(ctx, RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertAnalyticsEqual(t, "after_prune", gotAfterPrune, wantAfterPrune)
	if gotAfterPrune.Summary.Errors != 0 {
		t.Fatalf("expected 0 errors after pruning failed requests, got %d", gotAfterPrune.Summary.Errors)
	}

	// Simulate an upgraded DB: clear derived caches and ready flag, then invoke
	// EnsureRollupsBackfilled and verify it rebuilds both in the background.
	if _, err := s.db.Exec(`DELETE FROM analytics_hour_summary`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE analytics_meta SET value=0 WHERE name='request_hour_cache_ready'`); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertDelta(&Delta{TS: base + 1000, Requests: 1, Payload: `{"avg":100}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM snapshot_hour`); err != nil {
		t.Fatal(err)
	}
	s.EnsureRollupsBackfilled(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var ready, hrs int64
		_ = s.db.QueryRow(`SELECT COALESCE((SELECT value FROM analytics_meta WHERE name='request_hour_cache_ready'),0)`).Scan(&ready)
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM snapshot_hour`).Scan(&hrs)
		if ready == 1 && hrs > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var ready, hrs int64
	_ = s.db.QueryRow(`SELECT COALESCE((SELECT value FROM analytics_meta WHERE name='request_hour_cache_ready'),0)`).Scan(&ready)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM snapshot_hour`).Scan(&hrs)
	if ready != 1 || hrs == 0 {
		t.Fatalf("EnsureRollupsBackfilled did not complete: ready=%d snapshot_hour=%d", ready, hrs)
	}
	gotRebuilt, err := s.Analytics(ctx, RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertAnalyticsEqual(t, "after_backfill", gotRebuilt, wantAfterPrune)
}
