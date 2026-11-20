package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"sort"
)

// hourAnalyticsAcc holds one pre-aggregated hour of request-level analytics
// matching Store.Analytics's exact semantics (dimensions, latency/TTFT
// histograms, image counters, exact min/max timestamps, and latest client
// display name tie-breaking).
type hourAnalyticsAcc struct {
	minTS, maxTS int64
	total        usageAccumulator
	models       map[string]*usageAccumulator
	providers    map[string]*usageAccumulator
	clients      map[string]*usageAccumulator
	clientNames  map[string]string
	clientNameTS map[string]int64
	endpoints    map[string]*usageAccumulator
	statuses     map[int]int64
}

func newHourAnalyticsAcc() *hourAnalyticsAcc {
	return &hourAnalyticsAcc{
		models:       map[string]*usageAccumulator{},
		providers:    map[string]*usageAccumulator{},
		clients:      map[string]*usageAccumulator{},
		clientNames:  map[string]string{},
		clientNameTS: map[string]int64{},
		endpoints:    map[string]*usageAccumulator{},
		statuses:     map[int]int64{},
	}
}

func (h *hourAnalyticsAcc) addRow(
	ts int64,
	model, backend, clientKeyID, clientName, endpoint string,
	status int,
	ok, usageKnown, costKnown bool,
	tokIn, tokOut, tokCache uint64,
	cost float64,
	images int64,
	imagesKnown bool,
	latMs, ttftMs float64,
) {
	if h.minTS == 0 || ts < h.minTS {
		h.minTS = ts
	}
	if ts > h.maxTS {
		h.maxTS = ts
	}
	if endpoint == "" {
		endpoint = "unknown"
	}
	var errN, unkUsage, unkCost, imgs, unkImgs int64
	if !ok {
		errN = 1
	}
	if !usageKnown {
		unkUsage = 1
	}
	if !costKnown {
		unkCost = 1
	}
	if imagesKnown {
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
	addOne(&h.total)
	getOrNew := func(m map[string]*usageAccumulator, k string) *usageAccumulator {
		a := m[k]
		if a == nil {
			a = &usageAccumulator{}
			m[k] = a
		}
		return a
	}
	addOne(getOrNew(h.models, model))
	addOne(getOrNew(h.providers, backend))
	addOne(getOrNew(h.clients, clientKeyID))
	addOne(getOrNew(h.endpoints, endpoint))
	if curTS, exists := h.clientNameTS[clientKeyID]; !exists || ts > curTS || (ts == curTS && clientName > h.clientNames[clientKeyID]) {
		h.clientNames[clientKeyID] = clientName
		h.clientNameTS[clientKeyID] = ts
	}
	h.statuses[status]++
}

func (h *hourAnalyticsAcc) addReqRecord(r ReqRecord) {
	h.addRow(
		r.TS, r.Model, r.Backend, r.ClientKeyID, r.ClientName, r.Endpoint,
		r.Status, r.OK, r.UsageKnown, r.CostKnown,
		r.TokIn, r.TokOut, r.TokCache, r.Cost,
		int64(r.Images), r.ImagesKnown, r.LatMs, r.TTFTMs,
	)
}

func (h *hourAnalyticsAcc) merge(other *hourAnalyticsAcc) {
	if other == nil || other.total.Requests == 0 {
		return
	}
	if other.minTS > 0 && (h.minTS == 0 || other.minTS < h.minTS) {
		h.minTS = other.minTS
	}
	if other.maxTS > h.maxTS {
		h.maxTS = other.maxTS
	}
	h.total.merge(&other.total)
	mergeMap := func(dst, src map[string]*usageAccumulator) {
		for k, v := range src {
			d := dst[k]
			if d == nil {
				d = &usageAccumulator{}
				dst[k] = d
			}
			d.merge(v)
		}
	}
	mergeMap(h.models, other.models)
	mergeMap(h.providers, other.providers)
	mergeMap(h.clients, other.clients)
	mergeMap(h.endpoints, other.endpoints)
	for id, name := range other.clientNames {
		ts := other.clientNameTS[id]
		if curTS, exists := h.clientNameTS[id]; !exists || ts > curTS || (ts == curTS && name > h.clientNames[id]) {
			h.clientNames[id] = name
			h.clientNameTS[id] = ts
		}
	}
	for st, n := range other.statuses {
		h.statuses[st] += n
	}
}

const hourAnalyticsCodecVersion byte = 0x51

var errHourAnalyticsDecode = errors.New("analytics hour cache blob is malformed")

func putUsageAcc(dst *bytes.Buffer, a *usageAccumulator) {
	putU64(dst, uint64(a.Requests))
	putU64(dst, uint64(a.Errors))
	putU64(dst, a.TokIn)
	putU64(dst, a.TokOut)
	putU64(dst, a.TokCache)
	putF64(dst, a.Cost)
	putU64(dst, uint64(a.UnknownUsage))
	putU64(dst, uint64(a.UnknownCost))
	putU64(dst, uint64(a.Images))
	putU64(dst, uint64(a.UnknownImages))
	putF64(dst, a.latSum)
	putF64(dst, a.ttftSum)
	putU64(dst, uint64(a.ttftN))
	for _, v := range a.latHist {
		putU64(dst, v)
	}
	for _, v := range a.ttftHist {
		putU64(dst, v)
	}
}

func encodeHourAnalytics(dst *bytes.Buffer, h *hourAnalyticsAcc) []byte {
	dst.Reset()
	dst.WriteByte(hourAnalyticsCodecVersion)
	putU64(dst, uint64(h.minTS))
	putU64(dst, uint64(h.maxTS))
	putUsageAcc(dst, &h.total)

	writeGroup := func(m map[string]*usageAccumulator) {
		putU64(dst, uint64(len(m)))
		for _, k := range sortedKeys(m) {
			putName(dst, k)
			putUsageAcc(dst, m[k])
		}
	}
	writeGroup(h.models)
	writeGroup(h.providers)
	writeGroup(h.endpoints)

	// Clients include latest (name, nameTS).
	putU64(dst, uint64(len(h.clients)))
	for _, k := range sortedKeys(h.clients) {
		putName(dst, k)
		putName(dst, h.clientNames[k])
		putU64(dst, uint64(h.clientNameTS[k]))
		putUsageAcc(dst, h.clients[k])
	}

	// Status counts sorted by status code.
	stKeys := make([]int, 0, len(h.statuses))
	for st := range h.statuses {
		stKeys = append(stKeys, st)
	}
	sort.Ints(stKeys)
	putU64(dst, uint64(len(stKeys)))
	for _, st := range stKeys {
		putU64(dst, uint64(st))
		putU64(dst, uint64(h.statuses[st]))
	}

	out := make([]byte, dst.Len())
	copy(out, dst.Bytes())
	return out
}

func readUvarintSlice(b []byte) (uint64, []byte, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, nil, errHourAnalyticsDecode
	}
	return v, b[n:], nil
}

func readF64Slice(b []byte) (float64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, errHourAnalyticsDecode
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b[:8])), b[8:], nil
}

func readBytesSlice(b []byte) ([]byte, []byte, error) {
	n, b, err := readUvarintSlice(b)
	if err != nil || n > uint64(len(b)) {
		return nil, nil, errHourAnalyticsDecode
	}
	return b[:n], b[n:], nil
}

func decodeUsageAccInto(b []byte, dst *usageAccumulator) ([]byte, error) {
	var v uint64
	var f float64
	var err error
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.Requests += int64(v)
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.Errors += int64(v)
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.TokIn += v
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.TokOut += v
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.TokCache += v
	if f, b, err = readF64Slice(b); err != nil {
		return nil, err
	}
	dst.Cost += f
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.UnknownUsage += int64(v)
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.UnknownCost += int64(v)
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.Images += int64(v)
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.UnknownImages += int64(v)
	if f, b, err = readF64Slice(b); err != nil {
		return nil, err
	}
	dst.latSum += f
	if f, b, err = readF64Slice(b); err != nil {
		return nil, err
	}
	dst.ttftSum += f
	if v, b, err = readUvarintSlice(b); err != nil {
		return nil, err
	}
	dst.ttftN += int64(v)
	for i := range dst.latHist {
		if v, b, err = readUvarintSlice(b); err != nil {
			return nil, err
		}
		dst.latHist[i] += v
	}
	for i := range dst.ttftHist {
		if v, b, err = readUvarintSlice(b); err != nil {
			return nil, err
		}
		dst.ttftHist[i] += v
	}
	return b, nil
}

// decodeHourAnalyticsInto decodes a binary hourly analytics blob directly into
// the caller's accumulators with minimal allocations.
func decodeHourAnalyticsInto(
	blob []byte,
	minTS, maxTS *int64,
	total *usageAccumulator,
	models, providers, clients, endpoints map[string]*usageAccumulator,
	names map[string]string,
	nameTS map[string]int64,
	statuses map[int]int64,
) error {
	if len(blob) == 0 || blob[0] != hourAnalyticsCodecVersion {
		return errHourAnalyticsDecode
	}
	b := blob[1:]
	var v uint64
	var err error
	if v, b, err = readUvarintSlice(b); err != nil {
		return err
	}
	rowMin := int64(v)
	if v, b, err = readUvarintSlice(b); err != nil {
		return err
	}
	rowMax := int64(v)
	if rowMin > 0 && (*minTS == 0 || rowMin < *minTS) {
		*minTS = rowMin
	}
	if rowMax > *maxTS {
		*maxTS = rowMax
	}
	if b, err = decodeUsageAccInto(b, total); err != nil {
		return err
	}

	readGroup := func(dst map[string]*usageAccumulator) error {
		var n uint64
		if n, b, err = readUvarintSlice(b); err != nil {
			return err
		}
		for range n {
			var kb []byte
			if kb, b, err = readBytesSlice(b); err != nil {
				return err
			}
			a := dst[string(kb)]
			if a == nil {
				a = &usageAccumulator{}
				dst[string(kb)] = a
			}
			if b, err = decodeUsageAccInto(b, a); err != nil {
				return err
			}
		}
		return nil
	}
	if err = readGroup(models); err != nil {
		return err
	}
	if err = readGroup(providers); err != nil {
		return err
	}
	if err = readGroup(endpoints); err != nil {
		return err
	}

	var nClients uint64
	if nClients, b, err = readUvarintSlice(b); err != nil {
		return err
	}
	for range nClients {
		var idB, nameB []byte
		var tsU uint64
		if idB, b, err = readBytesSlice(b); err != nil {
			return err
		}
		if nameB, b, err = readBytesSlice(b); err != nil {
			return err
		}
		if tsU, b, err = readUvarintSlice(b); err != nil {
			return err
		}
		ts := int64(tsU)
		a := clients[string(idB)]
		if a == nil {
			id := string(idB)
			a = &usageAccumulator{}
			clients[id] = a
			names[id] = string(nameB)
			nameTS[id] = ts
		} else {
			curTS := nameTS[string(idB)]
			if ts > curTS || (ts == curTS && string(nameB) > names[string(idB)]) {
				id := string(idB)
				names[id] = string(nameB)
				nameTS[id] = ts
			}
		}
		if b, err = decodeUsageAccInto(b, a); err != nil {
			return err
		}
	}

	var nStatus uint64
	if nStatus, b, err = readUvarintSlice(b); err != nil {
		return err
	}
	for range nStatus {
		var st, cnt uint64
		if st, b, err = readUvarintSlice(b); err != nil {
			return err
		}
		if cnt, b, err = readUvarintSlice(b); err != nil {
			return err
		}
		statuses[int(st)] += int64(cnt)
	}
	return nil
}

// rollRequestBatch folds a batch of newly inserted ReqRecords into
// analytics_hour_summary inside the caller's transaction. Only runs when the
// cache is marked ready (brand-new DBs or after backfill).
func (s *Store) rollRequestBatch(tx *sql.Tx, batch []ReqRecord) error {
	if len(batch) == 0 {
		return nil
	}
	var ready int64
	if err := tx.QueryRow(`SELECT COALESCE((SELECT value FROM analytics_meta WHERE name='request_hour_cache_ready'), 0)`).Scan(&ready); err != nil || ready != 1 {
		return err
	}
	byHour := map[int64]*hourAnalyticsAcc{}
	var hours []int64
	for _, r := range batch {
		hr := (r.TS / 3600000) * 3600000
		acc := byHour[hr]
		if acc == nil {
			acc = newHourAnalyticsAcc()
			byHour[hr] = acc
			hours = append(hours, hr)
		}
		acc.addReqRecord(r)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
	var buf bytes.Buffer
	for _, hr := range hours {
		acc := byHour[hr]
		var prevBlob []byte
		err := tx.QueryRow(`SELECT blob FROM analytics_hour_summary WHERE hour=?`, hr).Scan(&prevBlob)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && len(prevBlob) > 0 {
			merged := newHourAnalyticsAcc()
			if derr := decodeHourAnalyticsInto(prevBlob, &merged.minTS, &merged.maxTS, &merged.total,
				merged.models, merged.providers, merged.clients, merged.endpoints,
				merged.clientNames, merged.clientNameTS, merged.statuses); derr != nil {
				return derr
			}
			merged.merge(acc)
			acc = merged
		}
		blob := encodeHourAnalytics(&buf, acc)
		if _, err := tx.Exec(`INSERT INTO analytics_hour_summary(hour,min_ts,max_ts,requests,blob) VALUES(?,?,?,?,?)
			ON CONFLICT(hour) DO UPDATE SET min_ts=excluded.min_ts, max_ts=excluded.max_ts, requests=excluded.requests, blob=excluded.blob`,
			hr, acc.minTS, acc.maxTS, acc.total.Requests, blob); err != nil {
			return err
		}
	}
	return nil
}

// rebuildRequestHourCacheTx rebuilds analytics_hour_summary from raw requests
// inside an existing write transaction.
func rebuildRequestHourCacheTx(ctx context.Context, tx *sql.Tx) (int, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_hour_summary`); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT ts, model, backend, client_key_id, client_name, endpoint, status, ok,
		usage_known, cost_known, tok_in, tok_out, tok_cache, cost, images, images_known, lat_ms, ttft_ms
		FROM requests ORDER BY ts ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var (
		curHour int64 = -1
		acc     *hourAnalyticsAcc
		buf     bytes.Buffer
		written int
	)
	flush := func() error {
		if curHour < 0 || acc == nil {
			return nil
		}
		blob := encodeHourAnalytics(&buf, acc)
		if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_hour_summary(hour,min_ts,max_ts,requests,blob) VALUES(?,?,?,?,?)`,
			curHour, acc.minTS, acc.maxTS, acc.total.Requests, blob); err != nil {
			return err
		}
		written++
		acc = nil
		return nil
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var (
			ts, images                  int64
			model, backend, cid, cname  string
			endpoint                    string
			status                      int
			okI, unkU, unkC, imgsKnownI int
			tokIn, tokOut, tokCache     uint64
			cost, latMs, ttftMs         float64
		)
		if err := rows.Scan(&ts, &model, &backend, &cid, &cname, &endpoint, &status, &okI,
			&unkU, &unkC, &tokIn, &tokOut, &tokCache, &cost, &images, &imgsKnownI, &latMs, &ttftMs); err != nil {
			return 0, err
		}
		hr := (ts / 3600000) * 3600000
		if hr != curHour {
			if err := flush(); err != nil {
				return 0, err
			}
			curHour = hr
			acc = newHourAnalyticsAcc()
		}
		acc.addRow(ts, model, backend, cid, cname, endpoint, status,
			okI != 0, unkU != 0, unkC != 0,
			tokIn, tokOut, tokCache, cost,
			images, imgsKnownI != 0, latMs, ttftMs)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := flush(); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_meta(name,value) VALUES('request_hour_cache_ready',1)
		ON CONFLICT(name) DO UPDATE SET value=1`); err != nil {
		return 0, err
	}
	return written, nil
}

// RebuildRequestHourCache rebuilds analytics_hour_summary from the raw requests
// table and marks the cache ready.
func (s *Store) RebuildRequestHourCache(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	written, err := rebuildRequestHourCacheTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}
