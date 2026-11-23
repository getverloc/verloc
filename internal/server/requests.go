package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type RequestSummary struct {
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	TokIn         uint64  `json:"tok_in"`
	TokOut        uint64  `json:"tok_out"`
	TokCache      uint64  `json:"tok_cache"`
	Images        int64   `json:"images"`
	UnknownImages int64   `json:"unknown_images"`
	Cost          float64 `json:"cost"`
	UnknownUsage  int64   `json:"unknown_usage"`
	UnknownCost   int64   `json:"unknown_cost"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	AvgTTFTMs     float64 `json:"avg_ttft_ms"`
}

type UsageGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	RequestSummary
}

type StatusGroup struct {
	Status   int   `json:"status"`
	Requests int64 `json:"requests"`
}
type RequestFilter struct {
	Since, Until                                   int64
	Model, Backend, ClientKeyID, Status, Q, Cursor string
	Endpoint                                       string
	Limit                                          int
}
type RequestPage struct {
	Requests   []ReqRecord    `json:"requests"`
	Total      int64          `json:"total"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Summary    RequestSummary `json:"summary"`
}

func (f RequestFilter) where(rollup bool) (string, []any) {
	var terms []string
	var args []any
	col := "ts"
	if rollup {
		col = "hour"
	}
	if f.Since > 0 {
		start := f.Since
		if rollup {
			start = ((start + 3599999) / 3600000) * 3600000
		}
		terms = append(terms, col+">=?")
		args = append(args, start)
	}
	if f.Until > 0 {
		if rollup {
			terms = append(terms, "hour+3600000<=?")
		} else {
			terms = append(terms, "ts<=?")
		}
		args = append(args, f.Until)
	}
	for _, v := range []struct{ col, val string }{{"model", f.Model}, {"backend", f.Backend}, {"client_key_id", f.ClientKeyID}} {
		if v.val != "" {
			terms = append(terms, v.col+"=?")
			args = append(args, v.val)
		}
	}
	if f.Endpoint != "" {
		if rollup {
			if f.Endpoint != "unknown" {
				terms = append(terms, "0=1")
			}
		} else {
			terms = append(terms, "endpoint=?")
			args = append(args, f.Endpoint)
		}
	}
	switch f.Status {
	case "ok":
		terms = append(terms, "ok=1")
	case "error":
		terms = append(terms, "ok=0")
	case "":
	default:
		terms = append(terms, "status=?")
		args = append(args, f.Status)
	}
	if f.Q != "" {
		search := `instr(lower(model),?)>0 OR instr(lower(upstream),?)>0 OR instr(lower(backend),?)>0 OR instr(lower(client_name),?)>0 OR instr(lower(client_key_id),?)>0`
		for range 5 {
			args = append(args, strings.ToLower(f.Q))
		}
		if !rollup {
			search += ` OR instr(lower(request_id),?)>0`
			args = append(args, strings.ToLower(f.Q))
		}
		terms = append(terms, "("+search+")")
	}
	if len(terms) == 0 {
		return "1=1", args
	}
	return strings.Join(terms, " AND "), args
}

func encodeCursor(ts, seq int64) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d:%d", ts, seq))
}
func decodeCursor(v string) (int64, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return 0, 0, errors.New("invalid cursor")
	}
	p := strings.Split(string(raw), ":")
	if len(p) != 2 {
		return 0, 0, errors.New("invalid cursor")
	}
	ts, e1 := strconv.ParseInt(p[0], 10, 64)
	seq, e2 := strconv.ParseInt(p[1], 10, 64)
	if e1 != nil || e2 != nil || ts < 0 || seq <= 0 {
		return 0, 0, errors.New("invalid cursor")
	}
	return ts, seq, nil
}

const summarySQL = `COUNT(*),COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0),
 COALESCE(SUM(tok_in),0),COALESCE(SUM(tok_out),0),COALESCE(SUM(tok_cache),0),
 COALESCE(SUM(cost),0),COALESCE(SUM(CASE WHEN usage_known=0 THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN cost_known=0 THEN 1 ELSE 0 END),0),COALESCE(AVG(lat_ms),0),
 COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END),0),
 COALESCE(SUM(CASE WHEN images_known=1 THEN images ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN endpoint IN ('images','images/generations','images/edits') AND images_known=0 THEN 1 ELSE 0 END),0)`

func scanSummary(row interface{ Scan(...any) error }, s *RequestSummary) error {
	return row.Scan(&s.Requests, &s.Errors, &s.TokIn, &s.TokOut, &s.TokCache, &s.Cost, &s.UnknownUsage, &s.UnknownCost, &s.AvgLatencyMs, &s.AvgTTFTMs, &s.Images, &s.UnknownImages)
}

// requestHourCacheTarget reports whether f can be satisfied from
// analytics_hour_summary (either unfiltered or with a single dimension
// filter: model, backend, endpoint, or client_key_id).
func (f RequestFilter) requestHourCacheTarget() (kind int, key string, ok bool) {
	if f.Status != "" || f.Q != "" {
		return 0, "", false
	}
	n := 0
	if f.Model != "" {
		n++
		kind, key = 1, f.Model
	}
	if f.Backend != "" {
		n++
		kind, key = 2, f.Backend
	}
	if f.Endpoint != "" {
		n++
		kind, key = 3, f.Endpoint
	}
	if f.ClientKeyID != "" {
		n++
		kind, key = 4, f.ClientKeyID
	}
	return kind, key, n <= 1
}

func foldRequestsSummaryWhere(ctx context.Context, tx *sql.Tx, f RequestFilter, timeWhere string, timeArgs []any, dst *usageAccumulator) error {
	where, args := f.where(false)
	if timeWhere != "" {
		where, args = timeWhere, append([]any(nil), timeArgs...)
		for _, v := range []struct{ col, val string }{{"model", f.Model}, {"backend", f.Backend}, {"client_key_id", f.ClientKeyID}, {"endpoint", f.Endpoint}} {
			if v.val != "" {
				where += " AND " + v.col + "=?"
				args = append(args, v.val)
			}
		}
	}
	var (
		reqs, errs, unkU, unkC, ttftN, imgs, unkImgs int64
		tokIn, tokOut, tokCache                      uint64
		cost, latSum, ttftSum                        float64
	)
	err := tx.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(tok_in),0),
		COALESCE(SUM(tok_out),0),
		COALESCE(SUM(tok_cache),0),
		COALESCE(SUM(cost),0),
		COALESCE(SUM(CASE WHEN usage_known=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cost_known=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(lat_ms),0),
		COALESCE(SUM(CASE WHEN ttft_ms>0 THEN ttft_ms ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ttft_ms>0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN images_known=1 THEN images ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN endpoint IN ('images','images/generations','images/edits') AND images_known=0 THEN 1 ELSE 0 END),0)
		FROM requests WHERE `+where, args...).
		Scan(&reqs, &errs, &tokIn, &tokOut, &tokCache, &cost, &unkU, &unkC, &latSum, &ttftSum, &ttftN, &imgs, &unkImgs)
	if err != nil {
		return err
	}
	dst.Requests += reqs
	dst.Errors += errs
	dst.TokIn += tokIn
	dst.TokOut += tokOut
	dst.TokCache += tokCache
	dst.Cost += cost
	dst.UnknownUsage += unkU
	dst.UnknownCost += unkC
	dst.latSum += latSum
	dst.ttftSum += ttftSum
	dst.ttftN += ttftN
	dst.Images += imgs
	dst.UnknownImages += unkImgs
	return nil
}

func foldHourCacheSummaryWhere(ctx context.Context, tx *sql.Tx, where string, args []any, targetKind int, targetKey string, dst *usageAccumulator) error {
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
		if err := decodeHourSummaryInto(blob, targetKind, targetKey, dst); err != nil {
			return err
		}
	}
	return rows.Err()
}

func computeRequestSummaryFromHourCache(ctx context.Context, tx *sql.Tx, f RequestFilter, targetKind int, targetKey string) (RequestSummary, bool, error) {
	var cacheReady int64
	_ = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM analytics_meta WHERE name='request_hour_cache_ready'), 0)`).Scan(&cacheReady)
	if cacheReady != 1 {
		return RequestSummary{}, false, nil
	}
	var total usageAccumulator
	switch {
	case f.Since <= 0 && f.Until <= 0:
		if err := foldHourCacheSummaryWhere(ctx, tx, "1=1", nil, targetKind, targetKey, &total); err != nil {
			return RequestSummary{}, false, err
		}
	case f.Since > 0 && f.Until <= 0:
		fullStart := ((f.Since + 3599999) / 3600000) * 3600000
		if f.Since < fullStart {
			if err := foldRequestsSummaryWhere(ctx, tx, f, "ts>=? AND ts<?", []any{f.Since, fullStart}, &total); err != nil {
				return RequestSummary{}, false, err
			}
		}
		if err := foldHourCacheSummaryWhere(ctx, tx, "hour>=?", []any{fullStart}, targetKind, targetKey, &total); err != nil {
			return RequestSummary{}, false, err
		}
	case f.Since <= 0 && f.Until > 0:
		fullEnd := ((f.Until + 1) / 3600000) * 3600000
		if fullEnd > 0 {
			if err := foldHourCacheSummaryWhere(ctx, tx, "hour<?", []any{fullEnd}, targetKind, targetKey, &total); err != nil {
				return RequestSummary{}, false, err
			}
			if fullEnd <= f.Until {
				if err := foldRequestsSummaryWhere(ctx, tx, f, "ts>=? AND ts<=?", []any{fullEnd, f.Until}, &total); err != nil {
					return RequestSummary{}, false, err
				}
			}
		} else {
			if err := foldRequestsSummaryWhere(ctx, tx, f, "ts<=?", []any{f.Until}, &total); err != nil {
				return RequestSummary{}, false, err
			}
		}
	default: // f.Since > 0 && f.Until > 0
		if f.Until >= f.Since {
			fullStart := ((f.Since + 3599999) / 3600000) * 3600000
			fullEnd := ((f.Until + 1) / 3600000) * 3600000
			if fullStart < fullEnd {
				if f.Since < fullStart {
					if err := foldRequestsSummaryWhere(ctx, tx, f, "ts>=? AND ts<?", []any{f.Since, fullStart}, &total); err != nil {
						return RequestSummary{}, false, err
					}
				}
				if err := foldHourCacheSummaryWhere(ctx, tx, "hour>=? AND hour<?", []any{fullStart, fullEnd}, targetKind, targetKey, &total); err != nil {
					return RequestSummary{}, false, err
				}
				if fullEnd <= f.Until {
					if err := foldRequestsSummaryWhere(ctx, tx, f, "ts>=? AND ts<=?", []any{fullEnd, f.Until}, &total); err != nil {
						return RequestSummary{}, false, err
					}
				}
			} else {
				if err := foldRequestsSummaryWhere(ctx, tx, f, "ts>=? AND ts<=?", []any{f.Since, f.Until}, &total); err != nil {
					return RequestSummary{}, false, err
				}
			}
		}
	}
	return total.summary(), true, nil
}

// summaryCache memoises the full-scan summary aggregate. The dashboard
// recomputes it on every pagination click (the same filter, a new cursor), so
// without this a 300k-row history paid 138ms per click. Keyed by the filter
// identity (which excludes the cursor), so paging within one filter is free and
// any filter change recomputes. Bounded to a few entries; the dashboard uses
// one filter at a time per tab.
type summaryCacheEntry struct {
	at int64
	s  RequestSummary
}

// summaryMemoTTLMs is the memo's ONLY freshness mechanism: a summary is
// honoured for at most 3s — one dashboard poll cycle — after which it is
// recomputed. (Write-triggered invalidation cleared the cache every
// ≤100ms batch under load, so it never survived to be useful.)
const summaryMemoTTLMs = 3000

func (s *Store) Requests(ctx context.Context, f RequestFilter) (RequestPage, error) {
	out := RequestPage{Requests: []ReqRecord{}}
	where, args := f.where(false)
	// One read transaction makes totals and the page a consistent snapshot.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Serve the summary from cache when the same filter is being paged: the
	// aggregate covers the whole filtered set, so it cannot change while only
	// the cursor moves. This removes a full scan from every "next page" click.
	// cacheKey deliberately excludes the cursor and the limit.
	cacheKey := f.Endpoint + "\x00" + f.Model + "\x00" + f.Backend + "\x00" + f.ClientKeyID + "\x00" + f.Status + "\x00" + f.Q +
		"\x00" + strconv.FormatInt(f.Since, 10) + "\x00" + strconv.FormatInt(f.Until, 10)
	s.summaryMu.Lock()
	cached, ok := s.summaryCache[cacheKey]
	if ok && nowMs()-cached.at > summaryMemoTTLMs {
		ok = false
	}
	s.summaryMu.Unlock()
	if ok {
		out.Summary = cached.s
	} else {
		usedHourCache := false
		if targetKind, targetKey, canUse := f.requestHourCacheTarget(); canUse {
			sSummary, okCache, cerr := computeRequestSummaryFromHourCache(ctx, tx, f, targetKind, targetKey)
			if cerr != nil {
				return out, cerr
			}
			if okCache {
				out.Summary = sSummary
				usedHourCache = true
			}
		}
		if !usedHourCache {
			if err = scanSummary(tx.QueryRowContext(ctx, `SELECT `+summarySQL+` FROM requests WHERE `+where, args...), &out.Summary); err != nil {
				return out, err
			}
		}
		s.summaryMu.Lock()
		if s.summaryCache == nil {
			s.summaryCache = map[string]summaryCacheEntry{}
		}
		if len(s.summaryCache) >= 8 {
			for k := range s.summaryCache {
				delete(s.summaryCache, k)
			}
		}
		s.summaryCache[cacheKey] = summaryCacheEntry{at: nowMs(), s: out.Summary}
		s.summaryMu.Unlock()
	}
	out.Total = out.Summary.Requests
	if f.Cursor != "" {
		ts, seq, err := decodeCursor(f.Cursor)
		if err != nil {
			return out, err
		}
		where += ` AND (ts<? OR (ts=? AND seq<?))`
		args = append(args, ts, ts, seq)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT ts,seq,record FROM requests WHERE `+where+` ORDER BY ts DESC,seq DESC LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var lastTS, lastSeq int64
	for rows.Next() {
		var ts, seq int64
		var raw string
		if err = rows.Scan(&ts, &seq, &raw); err != nil {
			return out, err
		}
		if len(out.Requests) == limit {
			out.NextCursor = encodeCursor(lastTS, lastSeq)
			break
		}
		var r ReqRecord
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return out, err
		}
		out.Requests = append(out.Requests, sanitizeRecord(r))
		lastTS, lastSeq = ts, seq
	}
	return out, rows.Err()
}

func requestFilter(values url.Values) (RequestFilter, error) {
	f := RequestFilter{Model: values.Get("model"), Backend: values.Get("backend"), ClientKeyID: values.Get("client_key_id"), Status: values.Get("status"), Q: values.Get("q"), Cursor: values.Get("cursor"), Limit: 100}
	f.Endpoint = values.Get("endpoint")
	switch f.Endpoint {
	case "", "unknown", "chat", "responses", "systemone", "embeddings", "images", "images/generations", "images/edits":
	default:
		return f, errors.New("invalid endpoint")
	}
	for _, v := range []struct {
		k string
		p *int64
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if raw := values.Get(v.k); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				return f, errors.New("invalid time filter")
			}
			*v.p = n
		}
	}
	if f.Until > 0 && f.Until < f.Since {
		return f, errors.New("until precedes since")
	}
	if f.Status != "" && f.Status != "ok" && f.Status != "error" {
		n, err := strconv.Atoi(f.Status)
		if err != nil || n < 0 || n > 599 {
			return f, errors.New("invalid status")
		}
	}
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return f, errors.New("invalid limit")
		}
		f.Limit = n
	}
	if f.Cursor != "" {
		if _, _, err := decodeCursor(f.Cursor); err != nil {
			return f, err
		}
	}
	return f, nil
}
