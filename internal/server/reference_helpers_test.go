package server

import (
	"encoding/json"
	"strings"
)

// Reference implementations and helpers retained for regression tests.

// SumSnapshots computes the lifetime totals directly from raw ticks. Only used
// to verify the maintained row in tests — never on a request path.
func (s *Store) SumSnapshots() (cost float64, requests, tokIn, tokOut, tokCache, errN int64) {
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(d_cost),0), COALESCE(SUM(d_requests),0), COALESCE(SUM(d_tok_in),0),
		        COALESCE(SUM(d_tok_out),0), COALESCE(SUM(d_tok_cache),0), COALESCE(SUM(d_err),0)
		 FROM snapshots`)
	_ = row.Scan(&cost, &requests, &tokIn, &tokOut, &tokCache, &errN)
	return
}

// merge accumulates o and immediately derives percentiles. Percentiles are
// order-independent over additive histograms, so this is equivalent to
// add followed by finalize and serves as a test reference.
func (t *tickAgg) merge(o tickAgg) {
	t.add(o)
	t.finalize()
}

func decodeModelList(body []byte, source string) ([]catalogEntry, error) {
	rows, err := modelListRows(body)
	if err != nil {
		return nil, err
	}
	return decodeModelRows(rows, source), nil
}

// costOf computes the USD cost of a request given per-1M-token pricing.
// Unknown pricing stays unknown to callers; zero here is only the numeric
// contribution to an explicitly labeled estimated-cost aggregate.
func costOf(modelID string, in, out, cache uint64) float64 {
	catalog.mu.RLock()
	p, ok := catalog.pricing[modelID]
	catalog.mu.RUnlock()
	if !ok {
		return 0
	}
	if cache > in {
		cache = in
	}
	return (float64(in-cache)*p.Input + float64(out)*p.Output + float64(cache)*p.CacheRead) / 1e6
}

func (a *usageAccumulator) addRecord(r ReqRecord) {
	if r.ImagesKnown {
		a.Images += int64(r.Images)
	} else if strings.HasPrefix(r.Endpoint, "images") {
		a.UnknownImages++
	}
	a.Requests++
	if !r.OK {
		a.Errors++
	}
	a.TokIn += r.TokIn
	a.TokOut += r.TokOut
	a.TokCache += r.TokCache
	a.Cost += r.Cost
	if !r.UsageKnown {
		a.UnknownUsage++
	}
	if !r.CostKnown {
		a.UnknownCost++
	}
	a.latSum += r.LatMs
	a.latHist[bucketOf(r.LatMs)]++
	if r.TTFTMs > 0 {
		a.ttftSum += r.TTFTMs
		a.ttftN++
		a.ttftHist[bucketOf(r.TTFTMs)]++
	}
}

// computeDelta diffs two cumulative /metrics snapshots into a per-tick Delta.
// Costs follow the backend that actually served the request, including fallbacks.
func computeDelta(prev, cur map[string]any) Delta {
	d := Delta{TS: nowMs(), Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}
	d.Requests = diffI64(prev, cur, "requests")
	d.Err = diffI64(prev, cur, "err")
	d.Failover = diffI64(prev, cur, "failover")
	d.TokIn = diffI64(prev, cur, "tok_in")
	d.TokOut = diffI64(prev, cur, "tok_out")
	d.TokCache = diffI64(prev, cur, "tok_cache")
	d.MS = 3000
	pm := prev["per_model"].(map[string]any)
	cm := cur["per_model"].(map[string]any)
	for k, cv := range cm {
		cmap := cv.(map[string]any)
		pv, ok := pm[k]
		var pmap map[string]any
		if ok {
			pmap = pv.(map[string]any)
		}
		m := deltaModel{
			Requests: diffMapI64(pmap, cmap, "requests"),
			Err:      diffMapI64(pmap, cmap, "err"),
			Failover: diffMapI64(pmap, cmap, "failover"),
			TokIn:    diffMapI64(pmap, cmap, "tok_in"),
			TokOut:   diffMapI64(pmap, cmap, "tok_out"),
			TokCache: diffMapI64(pmap, cmap, "tok_cache"),
			Cost:     toF64(cmap["cost"]) - toF64(pmap["cost"]),
		}
		if m.Requests > 0 || m.TokIn > 0 || m.TokOut > 0 {
			d.Models[k] = m
		}
	}
	pb := prev["per_backend"].(map[string]any)
	cb := cur["per_backend"].(map[string]any)
	for k, cv := range cb {
		cmap := cv.(map[string]any)
		pv, ok := pb[k]
		var pmap map[string]any
		if ok {
			pmap = pv.(map[string]any)
		}
		b := deltaBackend{
			Requests: diffMapI64(pmap, cmap, "requests"),
			Err:      diffMapI64(pmap, cmap, "err"),
			Failover: diffMapI64(pmap, cmap, "failover"),
			TokIn:    diffMapI64(pmap, cmap, "tok_in"),
			TokOut:   diffMapI64(pmap, cmap, "tok_out"),
			TokCache: diffMapI64(pmap, cmap, "tok_cache"),
			Cost:     toF64(cmap["cost"]) - toF64(pmap["cost"]),
		}
		if b.Requests > 0 || b.Err > 0 {
			d.Backends[k] = b
		}
	}
	for _, m := range d.Models {
		d.Cost += m.Cost
	}
	return d
}

func diffI64(prev, cur map[string]any, key string) int64 {
	var p, c int64
	if pm, ok := prev["global"].(map[string]any); ok {
		p = toI64(pm[key])
	}
	if cm, ok := cur["global"].(map[string]any); ok {
		c = toI64(cm[key])
	}
	return c - p
}

func diffMapI64(pv, cv map[string]any, key string) int64 {
	var p, c int64
	if pv != nil {
		p = toI64(pv[key])
	}
	if cv != nil {
		c = toI64(cv[key])
	}
	return c - p
}

func toI64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

func (s *Store) DroppedKeyEvents() uint64 { return s.keyEvDrop.Load() }

// Record2 records one completed request with explicit ok/err classification.
func (mt *Metrics) Record2(model, backend string, stream bool, status int, tin, tout, tcache, latNs, ttftNs uint64, ok bool) {
	cost := costOf(model, tin, tout, tcache)
	mt.record(model, backend, stream, status, tin, tout, tcache, latNs, ttftNs, ok, cost)
}

func (a *sseReasoningAcc) ingest(payload []byte) {
	if a == nil || len(payload) == 0 {
		return
	}
	chunk, ok := decodeChunkMap(payload)
	if !ok {
		return
	}
	a.ingestMap(chunk)
}

// aliasReasoningInChunk copies delta/message.reasoning onto reasoning_content
// so clients that only look for the DeepSeek field still capture thinking.
func aliasReasoningInChunk(payload []byte) ([]byte, bool) {
	chunk, ok := decodeChunkMap(payload)
	if !ok {
		return payload, false
	}
	if !aliasReasoningMap(chunk) {
		return payload, false
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return payload, false
	}
	return out, true
}
