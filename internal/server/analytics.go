package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
)

type usageAccumulator struct {
	RequestSummary
	latSum, ttftSum   float64
	ttftN             int64
	latHist, ttftHist [len(latBuckets) + 1]uint64
}

func (a *usageAccumulator) merge(b *usageAccumulator) {
	a.Images += b.Images
	a.UnknownImages += b.UnknownImages
	a.Requests += b.Requests
	a.Errors += b.Errors
	a.TokIn += b.TokIn
	a.TokOut += b.TokOut
	a.TokCache += b.TokCache
	a.Cost += b.Cost
	a.UnknownUsage += b.UnknownUsage
	a.UnknownCost += b.UnknownCost
	a.latSum += b.latSum
	a.ttftSum += b.ttftSum
	a.ttftN += b.ttftN
	for i := range a.latHist {
		a.latHist[i] += b.latHist[i]
		a.ttftHist[i] += b.ttftHist[i]
	}
}
func (a *usageAccumulator) summary() RequestSummary {
	s := a.RequestSummary
	if s.Requests > 0 {
		s.AvgLatencyMs = a.latSum / float64(s.Requests)
	}
	if a.ttftN > 0 {
		s.AvgTTFTMs = a.ttftSum / float64(a.ttftN)
	}
	return s
}

type Distribution struct {
	P50         float64   `json:"p50"`
	P95         float64   `json:"p95"`
	P99         float64   `json:"p99"`
	Buckets     []uint64  `json:"buckets,omitempty"`
	Bounds      []float64 `json:"bounds,omitempty"`
	Approximate bool      `json:"approximate"`
	Overflow    uint64    `json:"overflow"`
}

func histogramPercentile(h [len(latBuckets) + 1]uint64, p int) float64 {
	var n uint64
	for _, v := range h {
		n += v
	}
	if n == 0 {
		return 0
	}
	target := (n*uint64(p) + 99) / 100
	var cumulative uint64
	for i, v := range h {
		cumulative += v
		if cumulative >= target {
			if i == len(latBuckets) {
				return latBuckets[len(latBuckets)-1]
			}
			return latBuckets[i]
		}
	}
	return 0
}
func distribution(h [len(latBuckets) + 1]uint64) Distribution {
	return Distribution{P50: histogramPercentile(h, 50), P95: histogramPercentile(h, 95), P99: histogramPercentile(h, 99), Buckets: append([]uint64(nil), h[:]...), Bounds: append([]float64(nil), latBuckets[:]...), Approximate: true, Overflow: h[len(latBuckets)]}
}

type AnalyticsCoverage struct {
	OldestTS                       int64           `json:"oldest_ts"`
	NewestTS                       int64           `json:"newest_ts"`
	LegacyAggregatesOnly           bool            `json:"legacy_aggregates_only"`
	DetailOldestTS                 int64           `json:"detail_oldest_ts"`
	DetailNewestTS                 int64           `json:"detail_newest_ts"`
	AggregateOnlyRequests          int64           `json:"aggregate_only_requests"`
	AggregateBucketMs              int64           `json:"aggregate_bucket_ms"`
	PartialBucketsExcluded         bool            `json:"partial_buckets_excluded"`
	LegacyDistributionsUnavailable bool            `json:"legacy_distributions_unavailable"`
	LegacySummary                  *RequestSummary `json:"legacy_summary,omitempty"`
}
type AnalyticsResult struct {
	Summary   RequestSummary    `json:"summary"`
	Models    []UsageGroup      `json:"models"`
	Providers []UsageGroup      `json:"providers"`
	Clients   []UsageGroup      `json:"clients"`
	Endpoints []UsageGroup      `json:"endpoints"`
	Status    []StatusGroup     `json:"status"`
	Latency   Distribution      `json:"latency"`
	TTFT      Distribution      `json:"ttft"`
	Coverage  AnalyticsCoverage `json:"coverage"`
}

// foldRequestsWhere scans matching detail rows from `requests` in ONE pass
// (eliminating the previous 7-query GROUP BY / ROW_NUMBER fan-out that caused
// SQLite's planner to pick covering scans or full sorts on filtered queries).
func foldRequestsWhere(
	ctx context.Context,
	tx *sql.Tx,
	where string,
	args []any,
	detOld, detNew *int64,
	total *usageAccumulator,
	models, providers, clients, endpoints map[string]*usageAccumulator,
	names map[string]string,
	nameTS map[string]int64,
	statuses map[int]int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT ts, model, backend, client_key_id, client_name, endpoint, status, ok,
		usage_known, cost_known, tok_in, tok_out, tok_cache, cost, images, images_known, lat_ms, ttft_ms
		FROM requests WHERE `+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	var (
		ts, images                    int64
		model, backend, cid, cname    string
		endpoint                      string
		status                        int
		okI, usageKI, costKI, imgsKI  int
		tokIn, tokOut, tokCache       uint64
		cost, latMs, ttftMs           float64
	)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rows.Scan(&ts, &model, &backend, &cid, &cname, &endpoint, &status, &okI,
			&usageKI, &costKI, &tokIn, &tokOut, &tokCache, &cost, &images, &imgsKI, &latMs, &ttftMs); err != nil {
			return err
		}
		if *detOld == 0 || ts < *detOld {
			*detOld = ts
		}
		if ts > *detNew {
			*detNew = ts
		}
		if endpoint == "" {
			endpoint = "unknown"
		}
		var errN, unkUsage, unkCost, imgs, unkImgs int64
		if okI == 0 {
			errN = 1
		}
		if usageKI == 0 {
			unkUsage = 1
		}
		if costKI == 0 {
			unkCost = 1
		}
		if imgsKI != 0 {
			imgs = images
		} else if endpoint == "images" || endpoint == "images/generations" || endpoint == "images/edits" {
			unkImgs = 1
		}
		latB := bucketOf(latMs)
		ttftB := -1
		if ttftMs > 0 {
			ttftB = bucketOf(ttftMs)
		}
		addOne := func(a *usageAccumulator) {
			a.Requests++
			a.Errors += errN
			a.TokIn += tokIn
			a.TokOut += tokOut
			a.TokCache += tokCache
			a.Cost += cost
			a.UnknownUsage += unkUsage
			a.UnknownCost += unkCost
			a.Images += imgs
			a.UnknownImages += unkImgs
			a.latSum += latMs
			a.latHist[latB]++
			if ttftB >= 0 {
				a.ttftSum += ttftMs
				a.ttftN++
				a.ttftHist[ttftB]++
			}
		}
		addOne(total)
		for _, pair := range [4]struct {
			m map[string]*usageAccumulator
			k string
		}{{models, model}, {providers, backend}, {clients, cid}, {endpoints, endpoint}} {
			a := pair.m[pair.k]
			if a == nil {
				a = &usageAccumulator{}
				pair.m[pair.k] = a
			}
			addOne(a)
		}
		if curTS, exists := nameTS[cid]; !exists || ts > curTS || (ts == curTS && cname > names[cid]) {
			names[cid] = cname
			nameTS[cid] = ts
		}
		statuses[status]++
	}
	return rows.Err()
}

// foldHourCacheWhere reads pre-aggregated hourly summary blobs from
// analytics_hour_summary directly into the caller's accumulators.
func foldHourCacheWhere(
	ctx context.Context,
	tx *sql.Tx,
	where string,
	args []any,
	detOld, detNew *int64,
	total *usageAccumulator,
	models, providers, clients, endpoints map[string]*usageAccumulator,
	names map[string]string,
	nameTS map[string]int64,
	statuses map[int]int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT blob FROM analytics_hour_summary WHERE `+where+` ORDER BY hour ASC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var blob sql.RawBytes
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rows.Scan(&blob); err != nil {
			return err
		}
		if err := decodeHourAnalyticsInto(blob, detOld, detNew, total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) Analytics(ctx context.Context, f RequestFilter) (AnalyticsResult, error) {
	out := AnalyticsResult{Models: []UsageGroup{}, Providers: []UsageGroup{}, Clients: []UsageGroup{}, Status: []StatusGroup{}, Coverage: AnalyticsCoverage{AggregateBucketMs: 3600000, PartialBucketsExcluded: true, LegacyDistributionsUnavailable: true}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	total := &usageAccumulator{}
	models, providers, clients := map[string]*usageAccumulator{}, map[string]*usageAccumulator{}, map[string]*usageAccumulator{}
	names := map[string]string{}
	endpoints := map[string]*usageAccumulator{}
	nameTS := map[string]int64{}
	statuses := map[int]int64{}
	add := func(model, provider, client, name string, status int, ts int64, a *usageAccumulator) {
		total.merge(a)
		legacy := endpoints["unknown"]
		if legacy == nil {
			legacy = &usageAccumulator{}
			endpoints["unknown"] = legacy
		}
		legacy.merge(a)
		for _, v := range []struct {
			m map[string]*usageAccumulator
			k string
		}{{models, model}, {providers, provider}, {clients, client}} {
			x := v.m[v.k]
			if x == nil {
				x = &usageAccumulator{}
				v.m[v.k] = x
			}
			x.merge(a)
		}
		if curTS, exists := nameTS[client]; !exists || ts > curTS || (ts == curTS && name > names[client]) {
			names[client] = name
			nameTS[client] = ts
		}
		statuses[status] += a.Requests
	}
	cover := func(ts int64) {
		if out.Coverage.OldestTS == 0 || ts < out.Coverage.OldestTS {
			out.Coverage.OldestTS = ts
		}
		if ts > out.Coverage.NewestTS {
			out.Coverage.NewestTS = ts
		}
	}

	var detOld, detNew int64
	unfiltered := f.Model == "" && f.Backend == "" && f.ClientKeyID == "" && f.Endpoint == "" && f.Status == "" && f.Q == ""
	var cacheReady int64
	if unfiltered {
		_ = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM analytics_meta WHERE name='request_hour_cache_ready'), 0)`).Scan(&cacheReady)
	}

	if unfiltered && cacheReady == 1 {
		// Unfiltered time-range query: read complete interior hours from
		// analytics_hour_summary (O(hours), ~720 rows/30d) and fold at most
		// two partial boundary hours from raw `requests`.
		switch {
		case f.Since <= 0 && f.Until <= 0:
			if err = foldHourCacheWhere(ctx, tx, "1=1", nil, &detOld, &detNew,
				total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
				return out, err
			}
		case f.Since > 0 && f.Until <= 0:
			fullStart := ((f.Since + 3599999) / 3600000) * 3600000
			if f.Since < fullStart {
				if err = foldRequestsWhere(ctx, tx, "ts>=? AND ts<?", []any{f.Since, fullStart}, &detOld, &detNew,
					total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
					return out, err
				}
			}
			if err = foldHourCacheWhere(ctx, tx, "hour>=?", []any{fullStart}, &detOld, &detNew,
				total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
				return out, err
			}
		case f.Since <= 0 && f.Until > 0:
			fullEnd := ((f.Until + 1) / 3600000) * 3600000
			if fullEnd > 0 {
				if err = foldHourCacheWhere(ctx, tx, "hour<?", []any{fullEnd}, &detOld, &detNew,
					total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
					return out, err
				}
				if fullEnd <= f.Until {
					if err = foldRequestsWhere(ctx, tx, "ts>=? AND ts<=?", []any{fullEnd, f.Until}, &detOld, &detNew,
						total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
						return out, err
					}
				}
			} else {
				if err = foldRequestsWhere(ctx, tx, "ts<=?", []any{f.Until}, &detOld, &detNew,
					total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
					return out, err
				}
			}
		default: // f.Since > 0 && f.Until > 0
			if f.Until >= f.Since {
				fullStart := ((f.Since + 3599999) / 3600000) * 3600000
				fullEnd := ((f.Until + 1) / 3600000) * 3600000
				if fullStart < fullEnd {
					if f.Since < fullStart {
						if err = foldRequestsWhere(ctx, tx, "ts>=? AND ts<?", []any{f.Since, fullStart}, &detOld, &detNew,
							total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
							return out, err
						}
					}
					if err = foldHourCacheWhere(ctx, tx, "hour>=? AND hour<?", []any{fullStart, fullEnd}, &detOld, &detNew,
						total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
						return out, err
					}
					if fullEnd <= f.Until {
						if err = foldRequestsWhere(ctx, tx, "ts>=? AND ts<=?", []any{fullEnd, f.Until}, &detOld, &detNew,
							total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
							return out, err
						}
					}
				} else {
					if err = foldRequestsWhere(ctx, tx, "ts>=? AND ts<=?", []any{f.Since, f.Until}, &detOld, &detNew,
						total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
						return out, err
					}
				}
			}
		}
	} else {
		where, args := f.where(false)
		if err = foldRequestsWhere(ctx, tx, where, args, &detOld, &detNew,
			total, models, providers, clients, endpoints, names, nameTS, statuses); err != nil {
			return out, err
		}
	}

	if total.Requests > 0 {
		cover(detOld)
		cover(detNew)
		out.Coverage.DetailOldestTS = detOld
		out.Coverage.DetailNewestTS = detNew
	}

	// request_hourly is a COVERAGE TIER whose rows are aggregates of detail that
	// was PRUNED, so they are additive with any surviving detail for the same
	// hour (the documented legacy behaviour, and covered by
	// TestLegacyHourlyHistoryAndWindowBoundaries).
	//
	// The hazard this excludes: if a future writer mirrors detail into this
	// table WITHOUT deleting it, those rows would be counted twice. Such a row
	// must declare itself with mirrors_detail=1. Legacy rows default to 0 and
	// keep contributing exactly as before.
	hourlyWhere, hourlyArgs := f.where(true)
	// The mirrors_detail exclusion must apply to EVERY query shape — the
	// old string replacement only fired when an Until bound happened to
	// produce that exact substring, so since-only or model-only queries
	// would double-count a future mirroring writer's rows.
	if hourlyWhere == "1=1" {
		hourlyWhere = "mirrors_detail=0"
	} else {
		hourlyWhere += " AND mirrors_detail=0"
	}
	rows, qerr := tx.QueryContext(ctx, `SELECT hour,model,backend,client_key_id,client_name,status,requests,errors,tok_in,tok_out,tok_cache,cost,unknown_usage,unknown_cost,lat_sum,ttft_sum,ttft_n,lat_hist,ttft_hist FROM request_hourly WHERE `+hourlyWhere, hourlyArgs...)
	if qerr != nil {
		return out, qerr
	}
	for rows.Next() {
		var hour int64
		var model, provider, client, name, lh, th string
		var status int
		a := &usageAccumulator{}
		if err = rows.Scan(&hour, &model, &provider, &client, &name, &status, &a.Requests, &a.Errors, &a.TokIn, &a.TokOut, &a.TokCache, &a.Cost, &a.UnknownUsage, &a.UnknownCost, &a.latSum, &a.ttftSum, &a.ttftN, &lh, &th); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal([]byte(lh), &a.latHist); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal([]byte(th), &a.ttftHist); err != nil {
			rows.Close()
			return out, err
		}
		add(model, provider, client, name, status, hour, a)
		cover(hour)
		out.Coverage.AggregateOnlyRequests += a.Requests
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Summary = total.summary()
	out.Latency = distribution(total.latHist)
	out.TTFT = distribution(total.ttftHist)
	groups := func(m map[string]*usageAccumulator, client bool) []UsageGroup {
		list := make([]UsageGroup, 0, len(m))
		for id, a := range m {
			name := id
			if client {
				name = names[id]
				if name == "" {
					name = "Unattributed"
				}
			}
			list = append(list, UsageGroup{id, name, a.summary()})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Requests == list[j].Requests {
				return list[i].ID < list[j].ID
			}
			return list[i].Requests > list[j].Requests
		})
		return list
	}
	out.Models = groups(models, false)
	out.Providers = groups(providers, false)
	out.Clients = groups(clients, true)
	out.Endpoints = groups(endpoints, false)
	for status, n := range statuses {
		out.Status = append(out.Status, StatusGroup{status, n})
	}
	sort.Slice(out.Status, func(i, j int) bool { return out.Status[i].Status < out.Status[j].Status })
	if f.Model == "" && f.Backend == "" && f.ClientKeyID == "" && f.Status == "" && f.Q == "" && (f.Endpoint == "" || f.Endpoint == "unknown") {
		start := ((f.Since + 3599999) / 3600000) * 3600000
		until := int64(math.MaxInt64)
		if f.Until > 0 {
			until = f.Until
		}
		legacy := &RequestSummary{}
		var oldest, newest int64
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0),COALESCE(SUM(errors),0),COALESCE(SUM(tok_in),0),COALESCE(SUM(tok_out),0),COALESCE(SUM(tok_cache),0),COALESCE(SUM(cost),0),COALESCE(MIN(hour),0),COALESCE(MAX(hour),0) FROM legacy_hourly WHERE hour>=? AND hour+3600000<=?`, start, until).Scan(&legacy.Requests, &legacy.Errors, &legacy.TokIn, &legacy.TokOut, &legacy.TokCache, &legacy.Cost, &oldest, &newest)
		if err != nil {
			return out, err
		}
		if legacy.Requests > 0 {
			legacy.UnknownUsage = legacy.Requests
			legacy.UnknownCost = legacy.Requests
			out.Coverage.LegacySummary = legacy
			out.Coverage.LegacyAggregatesOnly = out.Summary.Requests == 0
			cover(oldest)
			cover(newest)
		}
	}
	return out, nil
}
