package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"strconv"
)

const seriesCacheMaxEntries = 32

type seriesCacheKey struct {
	sinceMs   int64
	untilMs   int64
	maxPoints int
}

// seriesOpenEnd is the inclusive upper bound used when a series window has no
// explicit end, so every query can share one `<= ?` shape.
const seriesOpenEnd = int64(math.MaxInt64)

func normalizeSeriesUntil(untilMs int64) int64 {
	if untilMs <= 0 {
		return seriesOpenEnd
	}
	return untilMs
}

type seriesTier uint8

const (
	seriesTierScan seriesTier = iota
	seriesTierHour
	seriesTier10m
	seriesTierMinute
	seriesTierBuckets
)

type seriesCacheEntry struct {
	rev       uint64
	snapN     int
	snapMin   int64
	snapMax   int64
	tier      seriesTier
	tierFirst int64
	tierLast  int64
	rows      []SeriesRow
	jsonBytes []byte
	gzipBytes []byte
}

type rollupTierSpec struct {
	tier     seriesTier
	table    string
	col      string
	widthMs  int64
	boundsQ  string
	minmaxQ  string
	selectQ  string
	leadingQ string
}

var (
	tierSpecHour = newRollupTierSpec(seriesTierHour, "snapshot_hour", "hour", hourMs)
	tierSpec10m  = newRollupTierSpec(seriesTier10m, "snapshot_10m", "bucket", tenMinMs)
	tierSpecMin  = newRollupTierSpec(seriesTierMinute, "snapshot_minute", "minute", minuteMs)
)

func newRollupTierSpec(tier seriesTier, table, col string, widthMs int64) rollupTierSpec {
	return rollupTierSpec{
		tier:    tier,
		table:   table,
		col:     col,
		widthMs: widthMs,
		boundsQ: `SELECT
			(SELECT COUNT(*) FROM (SELECT 1 FROM ` + table + ` WHERE ` + col + ` >= ? AND ` + col + ` <= ? LIMIT ?)),
			COALESCE((SELECT ` + col + ` FROM ` + table + ` ORDER BY ` + col + ` ASC LIMIT 1), 0),
			COALESCE((SELECT ` + col + ` FROM ` + table + ` WHERE ` + col + ` >= ? AND ` + col + ` <= ? ORDER BY ` + col + ` ASC LIMIT 1), 0),
			COALESCE((SELECT ` + col + ` FROM ` + table + ` ORDER BY ` + col + ` DESC LIMIT 1), 0)`,
		minmaxQ: `SELECT
			COALESCE((SELECT ` + col + ` FROM ` + table + ` ORDER BY ` + col + ` ASC LIMIT 1), 0),
			COALESCE((SELECT ` + col + ` FROM ` + table + ` ORDER BY ` + col + ` DESC LIMIT 1), 0)`,
		selectQ: `SELECT ` + col + `, requests, tok_in, tok_out, tok_cache, cost, ms, err,
			       lat_w, lat_sum, ttft_w, ttft_sum,
			       CASE WHEN requests=0 AND err=0 AND cost=0 THEN '' WHEN substr(models,1,1)=X'7F' THEN '' ELSE hist END,
			       CASE WHEN requests=0 AND err=0 AND cost=0 THEN '' WHEN substr(models,1,1)=X'7F' THEN '' ELSE ttft_hist END,
			       CASE WHEN requests=0 AND err=0 AND cost=0 THEN X'' ELSE models END,
			       missing_n
			FROM ` + table + `
			WHERE ` + col + ` >= ? AND ` + col + ` <= ?
			ORDER BY ` + col + ` ASC`,
		leadingQ: `SELECT minute, requests, tok_in, tok_out, tok_cache, cost, ms, err,
			       lat_w, lat_sum, ttft_w, ttft_sum,
			       CASE WHEN requests=0 AND err=0 AND cost=0 THEN '' WHEN substr(models,1,1)=X'7F' THEN '' ELSE hist END,
			       CASE WHEN requests=0 AND err=0 AND cost=0 THEN '' WHEN substr(models,1,1)=X'7F' THEN '' ELSE ttft_hist END,
			       CASE WHEN requests=0 AND err=0 AND cost=0 THEN X'' ELSE models END,
			       missing_n
			FROM snapshot_minute
			WHERE minute >= ? AND minute < ? AND minute <= ?
			ORDER BY minute ASC`,
	}
}

func (s *Store) seriesSnapshotBounds(ctx context.Context, sinceMs, untilMs int64, maxPoints int) (n int, minTs, maxTs int64, err error) {
	// O(log N) min/max seeks + capped count stopping at maxPoints+1 rows.
	// Replaces `SELECT COUNT(*), MIN(ts), MAX(ts)` which walked every matching
	// index entry (174ms on ~1M ticks).
	err = s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM (SELECT 1 FROM snapshots WHERE ts >= ? AND ts <= ? LIMIT ?)),
		COALESCE((SELECT ts FROM snapshots WHERE ts >= ? AND ts <= ? ORDER BY ts ASC LIMIT 1), 0),
		COALESCE((SELECT ts FROM snapshots WHERE ts >= ? AND ts <= ? ORDER BY ts DESC LIMIT 1), 0)`,
		sinceMs, untilMs, maxPoints+1, sinceMs, untilMs, sinceMs, untilMs).Scan(&n, &minTs, &maxTs)
	return
}

func (s *Store) tierMinMax(ctx context.Context, tier seriesTier) (first, last int64, ok bool) {
	var q string
	switch tier {
	case seriesTierHour:
		q = tierSpecHour.minmaxQ
	case seriesTier10m:
		q = tierSpec10m.minmaxQ
	case seriesTierMinute:
		q = tierSpecMin.minmaxQ
	default:
		return 0, 0, true
	}
	if err := s.db.QueryRowContext(ctx, q).Scan(&first, &last); err != nil {
		return 0, 0, false
	}
	return first, last, true
}

func (s *Store) lookupSeriesCache(ctx context.Context, key seriesCacheKey, rev uint64, n int, minTs, maxTs int64) *seriesCacheEntry {
	s.seriesMu.Lock()
	e := s.seriesCache[key]
	s.seriesMu.Unlock()
	if e == nil || e.rev != rev || e.snapN != n || e.snapMin != minTs || e.snapMax != maxTs {
		return nil
	}
	// Confirm the backing rollup tier wasn't cleared/modified out-of-band via
	// raw SQL (O(log N) primary-key seek, ~20µs).
	if e.tier == seriesTierHour || e.tier == seriesTier10m || e.tier == seriesTierMinute {
		tf, tl, ok := s.tierMinMax(ctx, e.tier)
		if !ok || tf != e.tierFirst || tl != e.tierLast {
			return nil
		}
	}
	return e
}

func (s *Store) storeSeriesCache(key seriesCacheKey, e *seriesCacheEntry) {
	s.seriesMu.Lock()
	if s.seriesRev.Load() != e.rev {
		s.seriesMu.Unlock()
		return
	}
	if s.seriesCache == nil {
		s.seriesCache = make(map[seriesCacheKey]*seriesCacheEntry, 8)
	} else if len(s.seriesCache) >= seriesCacheMaxEntries {
		clear(s.seriesCache)
	}
	s.seriesCache[key] = e
	s.seriesMu.Unlock()
}

// SeriesSince returns snapshots in [sinceMs, +∞) oldest-first. When the
// result exceeds maxPoints it is downsampled into equal-width buckets, using
// the coarsest pre-aggregated rollup tier (snapshot_hour -> snapshot_10m ->
// snapshot_minute -> raw snapshots) whose resolution fits the requested bucket
// width. ctx cancels the scan so a newer window can take the DB lock.
func (s *Store) SeriesSince(ctx context.Context, sinceMs int64, maxPoints int) ([]SeriesRow, error) {
	e, err := s.seriesSinceEntry(ctx, sinceMs, 0, maxPoints, false, false)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return []SeriesRow{}, nil
	}
	return e.rows, nil
}

// SeriesSinceJSON returns the JSON-encoded response for SeriesSince, caching
// the serialized bytes across identical requests within the same tick revision.
func (s *Store) SeriesSinceJSON(ctx context.Context, sinceMs int64, maxPoints int) ([]byte, error) {
	b, _, err := s.SeriesSinceEncoded(ctx, sinceMs, maxPoints, false)
	return b, err
}

// SeriesSinceEncoded returns the serialized JSON response for SeriesSince,
// optionally pre-compressed with gzip when acceptGzip is true. Both plain JSON
// and gzipped payloads are cached per tick revision so repeated/concurrent
// dashboard polls skip SQL scanning, JSON encoding, and gzip compression.
func (s *Store) SeriesSinceEncoded(ctx context.Context, sinceMs int64, maxPoints int, acceptGzip bool) ([]byte, bool, error) {
	return s.SeriesRangeEncoded(ctx, sinceMs, 0, maxPoints, acceptGzip)
}

// SeriesRangeEncoded is SeriesSinceEncoded bounded to [sinceMs, untilMs].
// untilMs <= 0 leaves the window open-ended (live).
func (s *Store) SeriesRangeEncoded(ctx context.Context, sinceMs, untilMs int64, maxPoints int, acceptGzip bool) ([]byte, bool, error) {
	e, err := s.seriesSinceEntry(ctx, sinceMs, untilMs, maxPoints, true, acceptGzip)
	if err != nil {
		return nil, false, err
	}
	if e == nil {
		return []byte("[]\n"), false, nil
	}
	if acceptGzip && len(e.gzipBytes) > 0 {
		return e.gzipBytes, true, nil
	}
	return e.jsonBytes, false, nil
}

func (s *Store) seriesSinceEntry(ctx context.Context, sinceMs, untilMs int64, maxPoints int, needJSON, needGzip bool) (*seriesCacheEntry, error) {
	if maxPoints < 1 {
		maxPoints = 500
	}
	untilMs = normalizeSeriesUntil(untilMs)
	rev := s.seriesRev.Load()
	n, minTs, maxTs, err := s.seriesSnapshotBounds(ctx, sinceMs, untilMs, maxPoints)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}

	key := seriesCacheKey{sinceMs: sinceMs, untilMs: untilMs, maxPoints: maxPoints}
	if cached := s.lookupSeriesCache(ctx, key, rev, n, minTs, maxTs); cached != nil {
		hasJSON := !needJSON || cached.jsonBytes != nil
		hasGzip := !needGzip || cached.gzipBytes != nil
		if hasJSON && hasGzip {
			return cached, nil
		}
		next := *cached
		if needJSON && next.jsonBytes == nil {
			next.jsonBytes = marshalSeriesRows(cached.rows)
		}
		if needGzip && next.gzipBytes == nil {
			next.gzipBytes = gzipSeriesBytes(next.jsonBytes)
		}
		s.storeSeriesCache(key, &next)
		return &next, nil
	}

	var (
		rows      []SeriesRow
		tier      seriesTier
		tierFirst int64
		tierLast  int64
	)
	if n <= maxPoints {
		rows, err = s.seriesScan(ctx, sinceMs, untilMs)
		tier = seriesTierScan
	} else {
		rows, tier, tierFirst, tierLast, err = s.seriesDownsampled(ctx, sinceMs, untilMs, minTs, maxTs, maxPoints)
	}
	if err != nil {
		return nil, err
	}

	entry := &seriesCacheEntry{
		rev:       rev,
		snapN:     n,
		snapMin:   minTs,
		snapMax:   maxTs,
		tier:      tier,
		tierFirst: tierFirst,
		tierLast:  tierLast,
		rows:      rows,
	}
	if needJSON || needGzip {
		entry.jsonBytes = marshalSeriesRows(rows)
	}
	if needGzip {
		entry.gzipBytes = gzipSeriesBytes(entry.jsonBytes)
	}
	s.storeSeriesCache(key, entry)
	return entry, nil
}

func gzipSeriesBytes(raw []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(raw)/6 + 64)
	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(&buf)
	_, err := gz.Write(raw)
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	gz.Reset(io.Discard)
	gzipWriterPool.Put(gz)
	if err != nil {
		return nil
	}
	return buf.Bytes()
}

// marshalSeriesRows encodes SeriesRow slices directly without reflection,
// cutting JSON serialization cost ~8-10x on 500-800 point dashboard payloads.
func marshalSeriesRows(rows []SeriesRow) []byte {
	if len(rows) == 0 {
		return []byte("[]\n")
	}
	buf := make([]byte, 0, len(rows)*240+4)
	buf = append(buf, '[')
	for i := range rows {
		if i > 0 {
			buf = append(buf, ',')
		}
		r := &rows[i]
		buf = append(buf, `{"ts":`...)
		buf = strconv.AppendInt(buf, r.TS, 10)
		buf = append(buf, `,"d_requests":`...)
		buf = strconv.AppendInt(buf, r.Requests, 10)
		buf = append(buf, `,"d_tok_in":`...)
		buf = strconv.AppendInt(buf, r.TokIn, 10)
		buf = append(buf, `,"d_tok_out":`...)
		buf = strconv.AppendInt(buf, r.TokOut, 10)
		buf = append(buf, `,"d_tok_cache":`...)
		buf = strconv.AppendInt(buf, r.TokCache, 10)
		buf = append(buf, `,"d_cost":`...)
		buf = appendJSONFloat(buf, r.Cost)
		buf = append(buf, `,"d_ms":`...)
		buf = strconv.AppendInt(buf, r.MS, 10)
		buf = append(buf, `,"d_err":`...)
		buf = strconv.AppendInt(buf, r.Err, 10)

		if len(r.Models) > 0 {
			buf = append(buf, `,"d_models":{`...)
			first := true
			for k, v := range r.Models {
				if !first {
					buf = append(buf, ',')
				}
				first = false
				buf = appendJSONString(buf, k)
				buf = append(buf, `:{"requests":`...)
				buf = strconv.AppendInt(buf, v.Requests, 10)
				buf = append(buf, `,"tok_in":`...)
				buf = strconv.AppendInt(buf, v.TokIn, 10)
				buf = append(buf, `,"tok_out":`...)
				buf = strconv.AppendInt(buf, v.TokOut, 10)
				buf = append(buf, `,"tok_cache":`...)
				buf = strconv.AppendInt(buf, v.TokCache, 10)
				buf = append(buf, `,"err":`...)
				buf = strconv.AppendInt(buf, v.Err, 10)
				buf = append(buf, `,"failover":`...)
				buf = strconv.AppendInt(buf, v.Failover, 10)
				buf = append(buf, `,"cost":`...)
				buf = appendJSONFloat(buf, v.Cost)
				buf = append(buf, '}')
			}
			buf = append(buf, '}')
		}

		if len(r.Backends) > 0 {
			buf = append(buf, `,"d_backends":{`...)
			first := true
			for k, v := range r.Backends {
				if !first {
					buf = append(buf, ',')
				}
				first = false
				buf = appendJSONString(buf, k)
				buf = append(buf, `:{"requests":`...)
				buf = strconv.AppendInt(buf, v.Requests, 10)
				buf = append(buf, `,"tok_in":`...)
				buf = strconv.AppendInt(buf, v.TokIn, 10)
				buf = append(buf, `,"tok_out":`...)
				buf = strconv.AppendInt(buf, v.TokOut, 10)
				buf = append(buf, `,"tok_cache":`...)
				buf = strconv.AppendInt(buf, v.TokCache, 10)
				buf = append(buf, `,"err":`...)
				buf = strconv.AppendInt(buf, v.Err, 10)
				buf = append(buf, `,"failover":`...)
				buf = strconv.AppendInt(buf, v.Failover, 10)
				buf = append(buf, `,"cost":`...)
				buf = appendJSONFloat(buf, v.Cost)
				buf = append(buf, '}')
			}
			buf = append(buf, '}')
		}

		if r.LatAvg != 0 {
			buf = append(buf, `,"lat_avg_ms":`...)
			buf = appendJSONFloat(buf, r.LatAvg)
		}
		if r.LatP50 != 0 {
			buf = append(buf, `,"lat_p50_ms":`...)
			buf = appendJSONFloat(buf, r.LatP50)
		}
		if r.LatP95 != 0 {
			buf = append(buf, `,"lat_p95_ms":`...)
			buf = appendJSONFloat(buf, r.LatP95)
		}
		if r.LatP99 != 0 {
			buf = append(buf, `,"lat_p99_ms":`...)
			buf = appendJSONFloat(buf, r.LatP99)
		}
		if r.TTFTAvg != 0 {
			buf = append(buf, `,"ttft_avg_ms":`...)
			buf = appendJSONFloat(buf, r.TTFTAvg)
		}
		if r.DistributionsUnavailable {
			buf = append(buf, `,"distributions_unavailable":true`...)
		} else {
			buf = append(buf, `,"distributions_unavailable":false`...)
		}
		if r.DistributionsApproximate {
			buf = append(buf, `,"distributions_approximate":true`...)
		} else {
			buf = append(buf, `,"distributions_approximate":false`...)
		}
		if r.AggregateBucketMs != 0 {
			buf = append(buf, `,"aggregate_bucket_ms":`...)
			buf = strconv.AppendInt(buf, r.AggregateBucketMs, 10)
		}
		buf = append(buf, '}')
	}
	buf = append(buf, ']', '\n')
	return buf
}

func appendJSONFloat(dst []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(dst, '0')
	}
	abs := math.Abs(f)
	fmtByte := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		fmtByte = 'e'
	}
	return strconv.AppendFloat(dst, f, fmtByte, -1, 64)
}

func (s *Store) seriesDownsampled(ctx context.Context, sinceMs, untilMs, minTs, maxTs int64, maxPoints int) ([]SeriesRow, seriesTier, int64, int64, error) {
	span := maxTs - minTs
	if span < 0 {
		span = 0
	}
	// Prefer the coarsest rollup tier that delivers at least ~75% of the
	// requested point budget (or exact bucket width >= tier width):
	//   - snapshot_hour (1h): 30d (720 rows even at limit=800) & all-time
	//   - snapshot_10m (10m): 7d (1,008 rows at limit=500/800) & 14d
	//   - snapshot_minute (1m): 1h, 6h, 24h (60..1,440 rows)
	// Always fall back transparently to finer tiers if a coarse tier is not
	// yet populated or does not cover the requested window.
	useHour := span/int64(maxPoints) >= hourMs || (span >= hourMs && span >= int64(maxPoints)*hourMs*3/4)
	use10m := useHour || span/int64(maxPoints) >= tenMinMs || (span >= tenMinMs && span >= int64(maxPoints)*tenMinMs*3/4)

	if useHour {
		if rows, tf, tl, ok, err := s.seriesFromTier(ctx, tierSpecHour, sinceMs, untilMs, minTs, maxPoints); ok {
			return rows, seriesTierHour, tf, tl, err
		} else if err != nil {
			return nil, 0, 0, 0, err
		}
	}
	if use10m {
		if rows, tf, tl, ok, err := s.seriesFromTier(ctx, tierSpec10m, sinceMs, untilMs, minTs, maxPoints); ok {
			return rows, seriesTier10m, tf, tl, err
		} else if err != nil {
			return nil, 0, 0, 0, err
		}
	}
	if rows, tf, tl, ok, err := s.seriesFromTier(ctx, tierSpecMin, sinceMs, untilMs, minTs, maxPoints); ok {
		return rows, seriesTierMinute, tf, tl, err
	} else if err != nil {
		return nil, 0, 0, 0, err
	}
	rows, err := s.seriesBuckets(ctx, sinceMs, untilMs, minTs, maxTs, maxPoints)
	return rows, seriesTierBuckets, 0, 0, err
}

// seriesScan reads every tick in the window. Lock-free: WAL readers never block the single writer.
func (s *Store) seriesScan(ctx context.Context, sinceMs, untilMs int64) ([]SeriesRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, payload, d_requests, d_tok_in, d_tok_out, d_tok_cache, d_cost, d_ms, d_err, d_models, d_backends
	                        FROM snapshots WHERE ts >= ? AND ts <= ? ORDER BY ts ASC`, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []SeriesRow
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var r SeriesRow
		var payload, mj, bj string
		if err := rows.Scan(&r.TS, &payload, &r.Requests, &r.TokIn, &r.TokOut, &r.TokCache, &r.Cost, &r.MS, &r.Err, &mj, &bj); err != nil {
			return nil, err
		}
		r.DistributionsUnavailable = r.Requests > 0
		if payload != "" {
			var ts TickStats
			if json.Unmarshal([]byte(payload), &ts) == nil {
				r.LatAvg, r.LatP50, r.LatP95, r.LatP99, r.TTFTAvg = ts.Avg, ts.P50, ts.P95, ts.P99, ts.TTFT
				r.DistributionsUnavailable = ts.DistributionsUnavailable || len(ts.LatHist) == 0
				r.DistributionsApproximate = len(ts.LatHist) > 0
				r.AggregateBucketMs = ts.AggregateBucketMs
			}
		}
		var m map[string]deltaModel
		var b map[string]deltaBackend
		_ = json.Unmarshal([]byte(mj), &m)
		_ = json.Unmarshal([]byte(bj), &b)
		r.Models = m
		r.Backends = b
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if all == nil {
		return []SeriesRow{}, nil
	}
	return all, nil
}

type seriesRollupAcc struct {
	row                          SeriesRow
	latW, latSum, ttftW, ttftSum float64
	hist, ttftHist               [len(latBuckets) + 1]uint64
	missingN                     int64
	models                       map[string]deltaModel
	backends                     map[string]deltaBackend
}

func (a *seriesRollupAcc) finalize(bucketMs int64) SeriesRow {
	a.row.Models = a.models
	a.row.Backends = a.backends
	a.row.AggregateBucketMs = bucketMs
	a.row.DistributionsUnavailable = a.missingN > 0
	a.row.DistributionsApproximate = a.missingN == 0
	if a.latW > 0 {
		a.row.LatAvg = a.latSum / a.latW
	}
	if a.ttftW > 0 {
		a.row.TTFTAvg = a.ttftSum / a.ttftW
	}
	if !a.row.DistributionsUnavailable {
		a.row.LatP50 = histogramPercentile(a.hist, 50)
		a.row.LatP95 = histogramPercentile(a.hist, 95)
		a.row.LatP99 = histogramPercentile(a.hist, 99)
	}
	return a.row
}

// foldRollupRows streams ordered rollup rows (from snapshot_minute,
// snapshot_10m, or snapshot_hour) into equal-width SeriesRow buckets using
// zero-alloc decodeMinutesInto. Rows must arrive in ascending timestamp order.
// Idle rows (empty dimension blob) skip decoding and lazy-map allocation.
func foldRollupRows(
	ctx context.Context,
	rows *sql.Rows,
	anchor, bucketMs int64,
	curIdx *int64,
	cur **seriesRollupAcc,
	out *[]SeriesRow,
) error {
	var (
		rowTS, req, tin, tout, tcache, ms, errN, missingN int64
		cost, latW, latSum, ttftW, ttftSum                float64
		hj, thj, mj                                       sql.RawBytes
	)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rows.Scan(&rowTS, &req, &tin, &tout, &tcache, &cost, &ms, &errN,
			&latW, &latSum, &ttftW, &ttftSum, &hj, &thj, &mj, &missingN); err != nil {
			return err
		}
		idx := (rowTS - anchor) / bucketMs
		if *cur == nil || idx != *curIdx {
			if *cur != nil {
				*out = append(*out, (*cur).finalize(bucketMs))
			}
			a := &seriesRollupAcc{}
			a.row.TS = rowTS
			*cur = a
			*curIdx = idx
		}
		a := *cur
		a.row.Requests += req
		a.row.TokIn += tin
		a.row.TokOut += tout
		a.row.TokCache += tcache
		a.row.Cost += cost
		a.row.MS += ms
		a.row.Err += errN
		a.latW += latW
		a.latSum += latSum
		a.ttftW += ttftW
		a.ttftSum += ttftSum
		a.missingN += missingN

		if len(mj) > 0 || len(hj) > 0 || len(thj) > 0 {
			if a.models == nil {
				a.models = make(map[string]deltaModel, 4)
				a.backends = make(map[string]deltaBackend, 4)
			}
			gotH, gotTH, _ := decodeMinutesInto(mj, a.models, a.backends, &a.hist, &a.ttftHist)
			// Legacy JSON-era rows keep their histograms in the separate hist
			// columns, not in the dimension blob: fall back to them so those
			// buckets do not silently report zeroed percentiles.
			if !gotH && len(hj) > 0 {
				var h []uint64
				if json.Unmarshal(hj, &h) == nil && len(h) == len(latBuckets)+1 {
					for i, v := range h {
						a.hist[i] += v
					}
				}
			}
			if !gotTH && len(thj) > 0 {
				var th []uint64
				if json.Unmarshal(thj, &th) == nil && len(th) == len(latBuckets)+1 {
					for i, v := range th {
						a.ttftHist[i] += v
					}
				}
			}
		}
	}
	return rows.Err()
}

// seriesFromTier aggregates one rollup tier (snapshot_hour, snapshot_10m, or
// snapshot_minute), blending leading partial-bucket minutes from
// snapshot_minute when sinceMs falls mid-bucket on a coarse tier.
func (s *Store) seriesFromTier(ctx context.Context, spec rollupTierSpec, sinceMs, untilMs, oldestTick int64, maxPoints int) ([]SeriesRow, int64, int64, bool, error) {
	var winCount, firstOverall, firstInWin, lastOverall int64
	if err := s.db.QueryRowContext(ctx, spec.boundsQ, sinceMs, untilMs, maxPoints+1, sinceMs, untilMs).
		Scan(&winCount, &firstOverall, &firstInWin, &lastOverall); err != nil {
		return nil, 0, 0, false, err
	}
	if firstOverall == 0 {
		return nil, 0, 0, false, nil
	}
	rollupComplete := oldestTick == 0 || oldestTick >= firstOverall-spec.widthMs
	windowInsideRollup := sinceMs > 0 && sinceMs >= firstOverall-spec.widthMs
	if !rollupComplete && !windowInsideRollup {
		return nil, 0, 0, false, nil
	}

	needLeadingMinutes := spec.widthMs > minuteMs && sinceMs > firstOverall && sinceMs%spec.widthMs != 0
	var firstMinInWin, lastMin int64
	if needLeadingMinutes {
		var firstMin int64
		if err := s.db.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT minute FROM snapshot_minute ORDER BY minute ASC LIMIT 1), 0),
			COALESCE((SELECT minute FROM snapshot_minute WHERE minute >= ? ORDER BY minute ASC LIMIT 1), 0),
			COALESCE((SELECT minute FROM snapshot_minute ORDER BY minute DESC LIMIT 1), 0)`,
			sinceMs).Scan(&firstMin, &firstMinInWin, &lastMin); err != nil {
			return nil, 0, 0, false, err
		}
		if firstMin == 0 || sinceMs < firstMin-minuteMs {
			return nil, 0, 0, false, nil
		}
	}

	if maxPoints < 1 {
		maxPoints = 500
	}

	firstFullBucket := sinceMs
	if needLeadingMinutes {
		firstFullBucket = ((sinceMs + spec.widthMs - 1) / spec.widthMs) * spec.widthMs
	}

	// Align anchor to the coarse bucket boundary when blending leading minutes
	// so partial leading minutes stay in bucket 0 without pulling the first
	// full coarse bucket into bucket 0 when bucketMs == spec.widthMs.
	var anchor int64
	hasLeading := needLeadingMinutes && firstMinInWin > 0 && firstMinInWin < firstFullBucket
	if hasLeading {
		anchor = firstFullBucket - spec.widthMs
	} else if firstInWin > 0 {
		anchor = firstInWin
	} else {
		return []SeriesRow{}, firstOverall, lastOverall, true, nil
	}
	endTS := lastOverall
	if endTS < lastMin {
		endTS = lastMin
	}
	if endTS > untilMs {
		endTS = untilMs
	}

	bucketMs := spec.widthMs
	effectiveCount := winCount
	if hasLeading {
		effectiveCount++
	}
	if effectiveCount > int64(maxPoints) {
		span := endTS - anchor
		if span < 0 {
			span = 0
		}
		bucketMs = (span/int64(maxPoints)/spec.widthMs + 1) * spec.widthMs
	}

	out := make([]SeriesRow, 0, min(int(effectiveCount), maxPoints)+1)
	var curIdx int64 = -1
	var cur *seriesRollupAcc

	tierStart := sinceMs
	if needLeadingMinutes {
		mrows, err := s.db.QueryContext(ctx, spec.leadingQ, sinceMs, firstFullBucket, untilMs)
		if err != nil {
			return nil, 0, 0, false, err
		}
		err = foldRollupRows(ctx, mrows, anchor, bucketMs, &curIdx, &cur, &out)
		mrows.Close()
		if err != nil {
			return nil, 0, 0, false, err
		}
		tierStart = firstFullBucket
	}

	rows, err := s.db.QueryContext(ctx, spec.selectQ, tierStart, untilMs)
	if err != nil {
		return nil, 0, 0, false, err
	}
	err = foldRollupRows(ctx, rows, anchor, bucketMs, &curIdx, &cur, &out)
	rows.Close()
	if err != nil {
		return nil, 0, 0, false, err
	}
	if cur != nil {
		out = append(out, cur.finalize(bucketMs))
	}
	return out, firstOverall, lastOverall, true, nil
}

// seriesBuckets aggregates a long window in SQL. Lock-free read (WAL).
func (s *Store) seriesBuckets(ctx context.Context, sinceMs, untilMs, minTs, maxTs int64, maxPoints int) ([]SeriesRow, error) {
	span := maxTs - minTs
	if span < 1 {
		span = 1
	}
	// +1 so the last tick stays inside the final bucket (integer division
	// otherwise emits maxPoints+1 groups).
	bucket := span/int64(maxPoints) + 1
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ts - ?) / ?, MIN(ts),
		       SUM(d_requests), SUM(d_tok_in), SUM(d_tok_out), SUM(d_tok_cache),
		       SUM(d_cost), SUM(d_ms), SUM(d_err)
		FROM snapshots
		WHERE ts >= ? AND ts <= ?
		GROUP BY (ts - ?) / ?
		ORDER BY 1`, minTs, bucket, sinceMs, untilMs, minTs, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byIdx := map[int64]*SeriesRow{}
	var order []int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var idx int64
		r := &SeriesRow{Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}
		if err := rows.Scan(&idx, &r.TS, &r.Requests, &r.TokIn, &r.TokOut, &r.TokCache, &r.Cost, &r.MS, &r.Err); err != nil {
			return nil, err
		}
		byIdx[idx] = r
		order = append(order, idx)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	arows, err := s.db.QueryContext(ctx, `
		SELECT ts, payload, d_requests, d_models, d_backends
		FROM snapshots
		WHERE ts >= ? AND ts <= ? AND (d_requests > 0 OR d_cost != 0 OR d_err > 0)`, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	latW := map[int64]float64{}
	latSum := map[int64]float64{}
	hists := map[int64][len(latBuckets) + 1]uint64{}
	missing := map[int64]bool{}
	ttftW := map[int64]float64{}
	ttftSum := map[int64]float64{}
	for arows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var ts, req int64
		var payload, mj, bj string
		if err := arows.Scan(&ts, &payload, &req, &mj, &bj); err != nil {
			return nil, err
		}
		b := byIdx[(ts-minTs)/bucket]
		if b == nil {
			continue
		}
		var m map[string]deltaModel
		var backends map[string]deltaBackend
		_ = json.Unmarshal([]byte(mj), &m)
		_ = json.Unmarshal([]byte(bj), &backends)
		for k, v := range m {
			got := b.Models[k]
			got.Requests += v.Requests
			got.TokIn += v.TokIn
			got.TokOut += v.TokOut
			got.TokCache += v.TokCache
			got.Cost += v.Cost
			b.Models[k] = got
		}
		for k, v := range backends {
			got := b.Backends[k]
			got.Requests += v.Requests
			got.TokIn += v.TokIn
			got.TokOut += v.TokOut
			got.TokCache += v.TokCache
			got.Err += v.Err
			got.Cost += v.Cost
			b.Backends[k] = got
		}
		if req > 0 && payload == "" {
			missing[(ts-minTs)/bucket] = true
		}
		if req > 0 && payload != "" {
			var stats TickStats
			if json.Unmarshal([]byte(payload), &stats) == nil {
				w := float64(req)
				latW[(ts-minTs)/bucket] += w
				latSum[(ts-minTs)/bucket] += stats.Avg * w
				idx := (ts - minTs) / bucket
				if len(stats.LatHist) == len(latBuckets)+1 {
					h := hists[idx]
					for i, v := range stats.LatHist {
						h[i] += v
					}
					hists[idx] = h
				} else {
					missing[idx] = true
				}
				ttftSum[idx] += stats.TTFT * float64(stats.TTFTN)
				ttftW[idx] += float64(stats.TTFTN)
			} else {
				missing[(ts-minTs)/bucket] = true
			}
		}
	}
	if err := arows.Err(); err != nil {
		return nil, err
	}
	out := make([]SeriesRow, 0, len(order))
	for _, idx := range order {
		b := byIdx[idx]
		b.DistributionsUnavailable = missing[idx]
		b.DistributionsApproximate = !missing[idx]
		b.AggregateBucketMs = bucket
		if latW[idx] > 0 {
			b.LatAvg = latSum[idx] / latW[idx]
			if !missing[idx] {
				h := hists[idx]
				b.LatP50 = histogramPercentile(h, 50)
				b.LatP95 = histogramPercentile(h, 95)
				b.LatP99 = histogramPercentile(h, 99)
			}
			if ttftW[idx] > 0 {
				b.TTFTAvg = ttftSum[idx] / ttftW[idx]
			}
		}
		out = append(out, *b)
	}
	return out, nil
}

// SeriesRow is one chartable point.
type SeriesRow struct {
	TS                       int64                   `json:"ts"`
	Requests                 int64                   `json:"d_requests"`
	TokIn                    int64                   `json:"d_tok_in"`
	TokOut                   int64                   `json:"d_tok_out"`
	TokCache                 int64                   `json:"d_tok_cache"`
	Cost                     float64                 `json:"d_cost"`
	MS                       int64                   `json:"d_ms"`
	Err                      int64                   `json:"d_err"`
	Models                   map[string]deltaModel   `json:"d_models,omitempty"`
	Backends                 map[string]deltaBackend `json:"d_backends,omitempty"`
	LatAvg                   float64                 `json:"lat_avg_ms,omitempty"`
	LatP50                   float64                 `json:"lat_p50_ms,omitempty"`
	LatP95                   float64                 `json:"lat_p95_ms,omitempty"`
	LatP99                   float64                 `json:"lat_p99_ms,omitempty"`
	TTFTAvg                  float64                 `json:"ttft_avg_ms,omitempty"`
	DistributionsUnavailable bool                    `json:"distributions_unavailable"`
	DistributionsApproximate bool                    `json:"distributions_approximate"`
	AggregateBucketMs        int64                   `json:"aggregate_bucket_ms,omitempty"`
}
