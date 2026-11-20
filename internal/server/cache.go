package server

import (
	"container/list"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ResponseCacheConfig configures the opt-in deterministic response cache.
// Disabled by default so existing deployments see zero behavior change.
type ResponseCacheConfig struct {
	Enabled    bool     `json:"enabled,omitempty"`
	MaxEntries int      `json:"max_entries,omitempty"` // default 1024
	MaxBytes   int64    `json:"max_bytes,omitempty"`   // default 64 MiB
	TTLSeconds int      `json:"ttl_seconds,omitempty"` // default 300s
	Models     []string `json:"models,omitempty"`      // empty = all models
}

// ResponseCacheStats reports live cache utilization for the dashboard.
type ResponseCacheStats struct {
	Hits    uint64 `json:"hits"`
	Misses  uint64 `json:"misses"`
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
}

func validateResponseCacheConfig(c *ResponseCacheConfig) (*ResponseCacheConfig, error) {
	if c == nil {
		return nil, nil
	}
	if c.MaxEntries < 0 || c.MaxEntries > 100_000 {
		return nil, fmt.Errorf("cache max_entries must be between 0 and 100000")
	}
	if c.MaxBytes < 0 || c.MaxBytes > 1<<30 {
		return nil, fmt.Errorf("cache max_bytes must be between 0 and 1073741824")
	}
	if c.TTLSeconds < 0 || c.TTLSeconds > 86400 {
		return nil, fmt.Errorf("cache ttl_seconds must be between 0 and 86400")
	}
	if len(c.Models) > 256 {
		return nil, fmt.Errorf("cache models allows at most 256 entries")
	}
	var models []string
	seen := make(map[string]bool, len(c.Models))
	for _, raw := range c.Models {
		m := strings.TrimSpace(raw)
		if m == "" || seen[m] {
			continue
		}
		if len(m) > 256 || strings.ContainsAny(m, "\r\n\t ") {
			return nil, fmt.Errorf("invalid model id in cache.models")
		}
		seen[m] = true
		models = append(models, m)
	}
	out := &ResponseCacheConfig{
		Enabled:    c.Enabled,
		MaxEntries: c.MaxEntries,
		MaxBytes:   c.MaxBytes,
		TTLSeconds: c.TTLSeconds,
		Models:     models,
	}
	if !out.Enabled && out.MaxEntries == 0 && out.MaxBytes == 0 && out.TTLSeconds == 0 && len(out.Models) == 0 {
		return nil, nil
	}
	return out, nil
}

func effectiveResponseCacheConfig(c *ResponseCacheConfig) ResponseCacheConfig {
	out := ResponseCacheConfig{
		MaxEntries: 1024,
		MaxBytes:   64 << 20,
		TTLSeconds: 300,
		Models:     []string{},
	}
	if c == nil {
		return out
	}
	out.Enabled = c.Enabled
	if c.MaxEntries > 0 {
		out.MaxEntries = c.MaxEntries
	}
	if c.MaxBytes > 0 {
		out.MaxBytes = c.MaxBytes
	}
	if c.TTLSeconds > 0 {
		out.TTLSeconds = c.TTLSeconds
	}
	if len(c.Models) > 0 {
		out.Models = append([]string(nil), c.Models...)
	}
	return out
}

type cachedResponse struct {
	key         [32]byte
	body        []byte
	contentType string
	tokIn       uint64
	tokOut      uint64
	tokCache    uint64
	usageKnown  bool
	expiresAt   time.Time
	size        int64
}

type responseCache struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64
	ttl        time.Duration
	models     map[string]bool // nil = allow all models
	curBytes   int64
	order      *list.List // front = MRU, back = LRU
	entries    map[[32]byte]*list.Element
	hits       uint64
	misses     uint64
}

func newResponseCache(cfg *ResponseCacheConfig) *responseCache {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	ttlSec := cfg.TTLSeconds
	if ttlSec <= 0 {
		ttlSec = 300
	}
	var allowed map[string]bool
	if len(cfg.Models) > 0 {
		allowed = make(map[string]bool, len(cfg.Models))
		for _, m := range cfg.Models {
			if m = strings.TrimSpace(m); m != "" {
				allowed[m] = true
			}
		}
	}
	return &responseCache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		ttl:        time.Duration(ttlSec) * time.Second,
		models:     allowed,
		order:      list.New(),
		entries:    make(map[[32]byte]*list.Element, min(maxEntries, 256)),
	}
}

func (c *responseCache) Stats() ResponseCacheStats {
	if c == nil {
		return ResponseCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return ResponseCacheStats{
		Hits:    c.hits,
		Misses:  c.misses,
		Entries: c.order.Len(),
		Bytes:   c.curBytes,
	}
}

func (c *responseCache) allowsModel(model string) bool {
	if c == nil {
		return false
	}
	if len(c.models) == 0 {
		return true
	}
	return c.models[model]
}

// isCacheEligible reports whether a request may be served from and stored into
// the deterministic response cache:
//   - Cache must be enabled and allow the model.
//   - Caller must not pass Cache-Control: no-store / no-cache.
//   - /embeddings and /systemone are deterministic by nature.
//   - Chat/Responses/Messages endpoints are eligible only when non-streaming
//     and temperature is explicitly 0.
func (c *responseCache) isCacheEligible(endpoint string, model string, stream bool, raw []byte, reqHeader http.Header) bool {
	if c == nil || stream || !c.allowsModel(model) {
		return false
	}
	if cc := reqHeader.Get("Cache-Control"); cc != "" {
		lower := strings.ToLower(cc)
		if strings.Contains(lower, "no-store") || strings.Contains(lower, "no-cache") {
			return false
		}
	}
	switch endpoint {
	case "/embeddings", "/systemone":
		return true
	case "/chat/completions":
		return hasZeroTemperature(raw)
	default:
		return false
	}
}

// hasZeroTemperature checks top-level "temperature" == 0 without decoding the
// full request body.
func hasZeroTemperature(raw []byte) bool {
	val, found, ok := findTopLevelField(raw, "temperature")
	if !ok || !found || len(val) == 0 {
		return false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(string(val)), 64)
	return err == nil && f == 0
}

// responseCacheKey builds a tenant-, endpoint-, model-, target-, and body-scoped
// SHA-256 key so cached responses never leak across client keys, protocols, or
// remapped upstreams.
func responseCacheKey(clientKeyID, endpointName, model string, primary RouteTarget, raw []byte) [32]byte {
	h := sha256.New()
	writePart := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	writePart(clientKeyID)
	writePart(endpointName)
	writePart(model)
	writePart(primary.Backend)
	writePart(primary.Upstream)
	h.Write(raw)
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

func (c *responseCache) Get(key [32]byte) (*cachedResponse, bool) {
	if c == nil {
		return nil, false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	entry := el.Value.(*cachedResponse)
	if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
		c.removeLocked(el)
		c.misses++
		return nil, false
	}
	c.order.MoveToFront(el)
	c.hits++
	return entry, true
}

func (c *responseCache) Put(key [32]byte, body []byte, contentType string, tokIn, tokOut, tokCache uint64, usageKnown bool) {
	if c == nil || len(body) == 0 {
		return
	}
	maxSingle := min(c.maxBytes/4, int64(4<<20))
	if maxSingle <= 0 {
		maxSingle = c.maxBytes
	}
	size := int64(len(body))
	if size > maxSingle || size > c.maxBytes {
		return
	}
	copied := make([]byte, len(body))
	copy(copied, body)
	if contentType == "" {
		contentType = "application/json"
	}
	entry := &cachedResponse{
		key:         key,
		body:        copied,
		contentType: contentType,
		tokIn:       tokIn,
		tokOut:      tokOut,
		tokCache:    tokCache,
		usageKnown:  usageKnown,
		expiresAt:   time.Now().Add(c.ttl),
		size:        size,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.removeLocked(old)
	}
	el := c.order.PushFront(entry)
	c.entries[key] = el
	c.curBytes += size
	for c.order.Len() > c.maxEntries || c.curBytes > c.maxBytes {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.removeLocked(back)
	}
}

func (c *responseCache) removeLocked(el *list.Element) {
	entry := el.Value.(*cachedResponse)
	c.order.Remove(el)
	delete(c.entries, entry.key)
	c.curBytes -= entry.size
	if c.curBytes < 0 {
		c.curBytes = 0
	}
}
