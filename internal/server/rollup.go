package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"
)

func mergeMinuteAcc(a, b *minuteAcc) *minuteAcc {
	a.req += b.req
	a.errN += b.errN
	a.tin += b.tin
	a.tout += b.tout
	a.tcache += b.tcache
	a.cost += b.cost
	a.ms += b.ms
	a.latW += b.latW
	a.latSum += b.latSum
	a.ttftW += b.ttftW
	a.ttftSum += b.ttftSum
	a.histN += b.histN
	a.missingN += b.missingN
	for i := range a.hist {
		a.hist[i] += b.hist[i]
		a.ttftHist[i] += b.ttftHist[i]
	}
	for k, v := range b.models {
		g := a.models[k]
		g.Requests += v.Requests
		g.TokIn += v.TokIn
		g.TokOut += v.TokOut
		g.TokCache += v.TokCache
		g.Err += v.Err
		g.Failover += v.Failover
		g.Cost += v.Cost
		a.models[k] = g
	}
	for k, v := range b.backends {
		g := a.backends[k]
		g.Requests += v.Requests
		g.TokIn += v.TokIn
		g.TokOut += v.TokOut
		g.TokCache += v.TokCache
		g.Err += v.Err
		g.Failover += v.Failover
		g.Cost += v.Cost
		a.backends[k] = g
	}
	return a
}

// RebuildRollups rebuilds snapshot_minute and snapshot_hour from the raw tick
// table in a single ascending pass. This is the migration for history that
// predates the rollups (and the repair path if a rollup is ever lost).
func (s *Store) RebuildRollups(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT ts,payload,d_requests,d_tok_in,d_tok_out,d_tok_cache,d_cost,d_ms,d_err,d_models,d_backends
	                                     FROM snapshots ORDER BY ts ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM snapshot_minute`); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`DELETE FROM snapshot_hour`); err != nil {
		return 0, err
	}

	var (
		curMinute int64 = -1
		curHour   int64 = -1
		minAcc          = newMinuteAcc()
		hourAcc         = newMinuteAcc()
		written   int
	)
	flushHour := func() error {
		if curHour < 0 {
			return nil
		}
		if err := hourAcc.insertDirect(tx, "snapshot_hour", "hour", curHour); err != nil {
			return err
		}
		hourAcc = newMinuteAcc()
		return nil
	}
	flushMinute := func() error {
		if curMinute < 0 {
			return nil
		}
		hr := curMinute / 3600000 * 3600000
		if hr != curHour {
			if err := flushHour(); err != nil {
				return err
			}
			curHour = hr
		}
		mergeMinuteAcc(hourAcc, minAcc)
		if err := minAcc.insertDirect(tx, "snapshot_minute", "minute", curMinute); err != nil {
			return err
		}
		written++
		minAcc = newMinuteAcc()
		return nil
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var d Delta
		var payload, mj, bj string
		if err := rows.Scan(&d.TS, &payload, &d.Requests, &d.TokIn, &d.TokOut, &d.TokCache,
			&d.Cost, &d.MS, &d.Err, &mj, &bj); err != nil {
			return 0, err
		}
		d.Payload = payload
		_ = json.Unmarshal([]byte(mj), &d.Models)
		_ = json.Unmarshal([]byte(bj), &d.Backends)
		if d.Models == nil {
			d.Models = map[string]deltaModel{}
		}
		if d.Backends == nil {
			d.Backends = map[string]deltaBackend{}
		}
		minute := d.TS / 60000 * 60000
		if minute != curMinute {
			if err := flushMinute(); err != nil {
				return 0, err
			}
			curMinute = minute
		}
		minAcc.addDelta(&d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := flushMinute(); err != nil {
		return 0, err
	}
	if err := flushHour(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// EnsureRollupsBackfilled repairs derived state for history that predates it.
// It stays O(1) when everything is consistent. Runs in the background so
// startup is never blocked by a large rebuild.
func (s *Store) EnsureRollupsBackfilled(ctx context.Context) {
	s.mu.Lock()
	var minutes, hours, ticks, totalTicks, reqCount, reqCacheReady int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM snapshot_minute`).Scan(&minutes)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM snapshot_hour`).Scan(&hours)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&ticks)
	_ = s.db.QueryRow(`SELECT ticks FROM analytics_totals WHERE id=1`).Scan(&totalTicks)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&reqCount)
	_ = s.db.QueryRow(`SELECT COALESCE((SELECT value FROM analytics_meta WHERE name='request_hour_cache_ready'), 0)`).Scan(&reqCacheReady)
	s.mu.Unlock()
	// A database upgraded from a schema without analytics_totals (or one that
	// briefly ran a build that never seeded it) has ticks but an empty/partial
	// totals row, so LifetimeTotals and the retained-window Range would read
	// zero for all pre-existing history. The healthy invariant is exactly one
	// totals increment per snapshot row, so any mismatch marks the row as
	// unseeded. RebuildTotals takes s.mu like every tick insert, so it cannot
	// interleave with the live writer.
	needsTotals := ticks > 0 && totalTicks != ticks
	needsRollups := ticks > 0 && (minutes == 0 || hours == 0)
	needsReqCache := reqCount > 0 && reqCacheReady == 0
	if !needsTotals && !needsRollups && !needsReqCache {
		return
	}
	// All rebuilds are whole-history scans. Deferring the repair to this
	// goroutine removes any boot-time stall while keeping reads correct via
	// transparent fallback until each derived table is populated.
	go func() {
		if needsTotals {
			if err := s.RebuildTotals(); err != nil {
				log.Printf("[analytics] totals backfill failed: %v", err)
			} else {
				log.Printf("[analytics] totals backfill complete: %d ticks", ticks)
			}
		}
		if needsRollups {
			start := time.Now()
			n, err := s.RebuildRollups(ctx)
			if err != nil {
				log.Printf("[analytics] rollup backfill failed: %v", err)
			} else {
				log.Printf("[analytics] rollup backfill complete: %d minutes in %v", n, time.Since(start).Round(time.Millisecond))
			}
		}
		if needsReqCache {
			start := time.Now()
			n, err := s.RebuildRequestHourCache(ctx)
			if err != nil {
				log.Printf("[analytics] request hour cache backfill failed: %v", err)
			} else {
				log.Printf("[analytics] request hour cache backfill complete: %d hours in %v", n, time.Since(start).Round(time.Millisecond))
			}
		}
	}()
}

// rollTick folds one tick into both its minute and hour rollup buckets inside
// the caller's transaction, parsing d.Payload only once.
func (s *Store) rollTick(tx *sql.Tx, d *Delta) error {
	acc := newMinuteAcc()
	acc.addDelta(d)
	if err := acc.writeTable(tx, "snapshot_minute", "minute", d.TS/60000*60000); err != nil {
		return err
	}
	return acc.writeTable(tx, "snapshot_hour", "hour", d.TS/3600000*3600000)
}

// rollMinute folds one tick into its minute bucket.
func (s *Store) rollMinute(tx *sql.Tx, d *Delta) error {
	acc := newMinuteAcc()
	acc.addDelta(d)
	return acc.writeTable(tx, "snapshot_minute", "minute", d.TS/60000*60000)
}

// rollHour folds one tick into its hour bucket.
func (s *Store) rollHour(tx *sql.Tx, d *Delta) error {
	acc := newMinuteAcc()
	acc.addDelta(d)
	return acc.writeTable(tx, "snapshot_hour", "hour", d.TS/3600000*3600000)
}

// minuteAcc accumulates one minute (or hour) of ticks. Both the live per-tick
// rollup and the one-time backfill fold through this type, so the two paths
// cannot drift.
type minuteAcc struct {
	blobBuf                          bytes.Buffer
	req, errN, tin, tout, tcache, ms int64
	cost                             float64
	latW, latSum, ttftW, ttftSum     float64
	hist, ttftHist                   [len(latBuckets) + 1]uint64
	histN, missingN                  int64
	models                           map[string]deltaModel
	backends                         map[string]deltaBackend
}

func newMinuteAcc() *minuteAcc {
	return &minuteAcc{models: map[string]deltaModel{}, backends: map[string]deltaBackend{}}
}

// addDelta folds one tick. A tick that carried traffic but no usable histogram
// marks the bucket unavailable: snapshot percentiles are averages over the tick
// and must never be folded into a window distribution, matching the raw path.
func (a *minuteAcc) addDelta(d *Delta) {
	a.req += d.Requests
	a.errN += d.Err
	a.tin += d.TokIn
	a.tout += d.TokOut
	a.tcache += d.TokCache
	a.cost += d.Cost
	a.ms += d.MS
	if d.Requests > 0 {
		usable := false
		if d.Payload != "" {
			var st TickStats
			if json.Unmarshal([]byte(d.Payload), &st) == nil {
				a.latW += float64(d.Requests)
				a.latSum += st.Avg * float64(d.Requests)
				a.ttftW += float64(st.TTFTN)
				a.ttftSum += st.TTFT * float64(st.TTFTN)
				if len(st.LatHist) == len(latBuckets)+1 {
					for i, v := range st.LatHist {
						a.hist[i] += v
					}
					usable = true
				}
			}
		}
		if usable {
			a.histN++
		} else {
			a.missingN++
		}
	}
	for k, v := range d.Models {
		g := a.models[k]
		g.Requests += v.Requests
		g.TokIn += v.TokIn
		g.TokOut += v.TokOut
		g.TokCache += v.TokCache
		g.Cost += v.Cost
		a.models[k] = g
	}
	for k, v := range d.Backends {
		g := a.backends[k]
		g.Requests += v.Requests
		g.TokIn += v.TokIn
		g.TokOut += v.TokOut
		g.TokCache += v.TokCache
		g.Err += v.Err
		g.Cost += v.Cost
		a.backends[k] = g
	}
}

func (a *minuteAcc) insertDirect(tx *sql.Tx, table, keyCol string, keyVal int64) error {
	blob := encodeMinutes(&a.blobBuf, a.models, a.backends, a.hist[:], a.ttftHist[:])
	hj, _ := json.Marshal(a.hist[:])
	thj, _ := json.Marshal(a.ttftHist[:])
	_, err := tx.Exec(`INSERT INTO `+table+`
	   (`+keyCol+`,requests,err,tok_in,tok_out,tok_cache,cost,ms,lat_w,lat_sum,ttft_w,ttft_sum,hist,ttft_hist,models,backends,hist_n,missing_n)
	   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		keyVal, a.req, a.errN, a.tin, a.tout, a.tcache, a.cost, a.ms,
		a.latW, a.latSum, a.ttftW, a.ttftSum, string(hj), string(thj), blob, blob,
		a.histN, a.missingN)
	return err
}

func (a *minuteAcc) writeTable(tx *sql.Tx, table, keyCol string, keyVal int64) error {
	// Read-merge-write in the caller's transaction. ALL scalar columns are
	// loaded, not just the dimensions: omitting them silently dropped every
	// previous tick in the bucket (caught by the rebuild-equivalence test).
	var (
		pReq, pErr, pIn, pOut, pCache, pMS int64
		pCost                              float64
		pLatW, pLatSum, pTTFTW, pTTFTSum   float64
		pHist, pTTFTHist, pModels, pBack   string
		pHistN, pMissingN                  int64
	)
	err := tx.QueryRow(`SELECT requests,err,tok_in,tok_out,tok_cache,cost,ms,lat_w,lat_sum,ttft_w,ttft_sum,hist,ttft_hist,models,backends,hist_n,missing_n
	                    FROM `+table+` WHERE `+keyCol+`=?`, keyVal).Scan(
		&pReq, &pErr, &pIn, &pOut, &pCache, &pCost, &pMS, &pLatW, &pLatSum, &pTTFTW, &pTTFTSum,
		&pHist, &pTTFTHist, &pModels, &pBack, &pHistN, &pMissingN)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	target := a
	if err == nil {
		prev := newMinuteAcc()
		prev.req, prev.errN, prev.tin, prev.tout, prev.tcache = pReq, pErr, pIn, pOut, pCache
		prev.cost, prev.ms = pCost, pMS
		prev.latW, prev.latSum, prev.ttftW, prev.ttftSum = pLatW, pLatSum, pTTFTW, pTTFTSum
		prev.histN, prev.missingN = pHistN, pMissingN
		var h, th []uint64
		_ = decodeMinutes([]byte(pModels), &prev.models, &prev.backends, &h, &th)
		if len(h) != len(latBuckets)+1 {
			var fh [len(latBuckets) + 1]uint64
			if json.Unmarshal([]byte(pHist), &fh) == nil {
				h = fh[:]
			}
		}
		if len(th) != len(latBuckets)+1 {
			var fth [len(latBuckets) + 1]uint64
			if json.Unmarshal([]byte(pTTFTHist), &fth) == nil {
				th = fth[:]
			}
		}
		for i := range prev.hist {
			if i < len(h) {
				prev.hist[i] = h[i]
			}
			if i < len(th) {
				prev.ttftHist[i] = th[i]
			}
		}
		target = mergeMinuteAcc(prev, a)
	}
	blob := encodeMinutes(&target.blobBuf, target.models, target.backends, target.hist[:], target.ttftHist[:])
	hj, _ := json.Marshal(target.hist[:])
	thj, _ := json.Marshal(target.ttftHist[:])
	_, err = tx.Exec(`INSERT INTO `+table+`
	   (`+keyCol+`,requests,err,tok_in,tok_out,tok_cache,cost,ms,lat_w,lat_sum,ttft_w,ttft_sum,hist,ttft_hist,models,backends,hist_n,missing_n)
	   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	   ON CONFLICT(`+keyCol+`) DO UPDATE SET
	     requests=excluded.requests, err=excluded.err, tok_in=excluded.tok_in, tok_out=excluded.tok_out,
	     tok_cache=excluded.tok_cache, cost=excluded.cost, ms=excluded.ms,
	     lat_w=excluded.lat_w, lat_sum=excluded.lat_sum, ttft_w=excluded.ttft_w, ttft_sum=excluded.ttft_sum,
	     hist=excluded.hist, ttft_hist=excluded.ttft_hist, models=excluded.models, backends=excluded.backends,
	     hist_n=excluded.hist_n, missing_n=excluded.missing_n`,
		keyVal, target.req, target.errN, target.tin, target.tout, target.tcache, target.cost, target.ms,
		target.latW, target.latSum, target.ttftW, target.ttftSum, string(hj), string(thj), blob, blob,
		target.histN, target.missingN)
	return err
}

func (s *Store) Range() (oldest, newest int64, count int) {
	row := s.db.QueryRow(`SELECT oldest_ts, newest_ts, ticks FROM analytics_totals WHERE id=1`)
	_ = row.Scan(&oldest, &newest, &count)
	return
}

// RebuildTotals recomputes the running totals from the raw tick table. The
// maintained row is exact by construction (same transaction as the insert),
// so this is a repair/verification path — not something the request path runs.
// Use it after a manual import, or in tests asserting the invariant holds.
func (s *Store) RebuildTotals() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE analytics_totals SET
	   cost=(SELECT COALESCE(SUM(d_cost),0) FROM snapshots),
	   requests=(SELECT COALESCE(SUM(d_requests),0) FROM snapshots),
	   tok_in=(SELECT COALESCE(SUM(d_tok_in),0) FROM snapshots),
	   tok_out=(SELECT COALESCE(SUM(d_tok_out),0) FROM snapshots),
	   tok_cache=(SELECT COALESCE(SUM(d_tok_cache),0) FROM snapshots),
	   err=(SELECT COALESCE(SUM(d_err),0) FROM snapshots),
	   oldest_ts=(SELECT COALESCE(MIN(ts),0) FROM snapshots),
	   newest_ts=(SELECT COALESCE(MAX(ts),0) FROM snapshots),
	   ticks=(SELECT COUNT(*) FROM snapshots)
	 WHERE id=1`)
	return err
}

// LifetimeTotals sums the retained window. This is the dashboard's
// headline lifetime cost — it survives verloc restarts.
// LifetimeTotals reads the running totals row maintained by InsertDelta.
// It is an O(1) primary-key lookup: previously this SUMmed the entire snapshots
// table on every /dash/api/state poll (250ms at 1M ticks) while holding s.mu,
// which blocked the analytics writer for 241ms each time.
func (s *Store) LifetimeTotals() (cost float64, requests, tokIn, tokOut, tokCache, errN int64) {
	row := s.db.QueryRow(
		`SELECT cost, requests, tok_in, tok_out, tok_cache, err FROM analytics_totals WHERE id=1`)
	_ = row.Scan(&cost, &requests, &tokIn, &tokOut, &tokCache, &errN)
	return
}
