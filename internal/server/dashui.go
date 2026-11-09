package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// acceptsGzip selects gzip unless it is excluded or identity is explicitly preferred.
func acceptsGzip(r *http.Request) bool {
	gzipQ, wildcardQ, identityQ := -1.0, -1.0, -1.0
	for _, header := range r.Header.Values("Accept-Encoding") {
		for header != "" {
			var part string
			part, header, _ = strings.Cut(header, ",")
			coding, params, _ := strings.Cut(part, ";")
			coding = strings.TrimSpace(coding)
			if !strings.EqualFold(coding, "gzip") && !strings.EqualFold(coding, "identity") && coding != "*" {
				continue
			}
			quality := 1.0
			for params != "" {
				var param string
				param, params, _ = strings.Cut(params, ";")
				name, value, ok := strings.Cut(param, "=")
				if strings.EqualFold(strings.TrimSpace(name), "q") {
					q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
					if !ok || err != nil || !(q >= 0 && q <= 1) {
						quality = 0
					} else {
						quality = q
					}
				}
			}
			switch {
			case strings.EqualFold(coding, "gzip"):
				gzipQ = quality
			case strings.EqualFold(coding, "identity"):
				identityQ = quality
			case coding == "*":
				wildcardQ = quality
			}
		}
	}
	if gzipQ < 0 {
		gzipQ = wildcardQ
	}
	return gzipQ > 0 && (identityQ < 0 || gzipQ >= identityQ)
}

func varyAcceptEncoding(h http.Header) {
	for _, value := range h.Values("Vary") {
		for value != "" {
			var field string
			field, value, _ = strings.Cut(value, ",")
			field = strings.TrimSpace(field)
			if field == "*" || strings.EqualFold(field, "Accept-Encoding") {
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

// compressibleType reports whether a Content-Type is worth gzipping.
// Already-compressed formats (png, woff2, …) are excluded.
func compressibleType(ct string) bool {
	ct = strings.TrimSpace(strings.Split(ct, ";")[0])
	switch ct {
	case "text/html", "text/css", "text/plain", "application/javascript",
		"text/javascript", "application/json", "image/svg+xml":
		return true
	}
	return false
}

//go:embed all:static
var dashDist embed.FS

// dashFS is the built Vite app, served under /dash/.
var dashFS = func() fs.FS {
	sub, err := fs.Sub(dashDist, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

// dashAsset is one embedded file, precomputed at startup: raw bytes, an
// optional precompressed gzip variant, content type and a strong ETag. The
// embedded bytes never change for the life of the process, so reading and
// compressing them per request was pure waste.
type dashAsset struct {
	data        []byte
	gz          []byte // nil unless the type compresses well and is >= 1 KiB
	contentType string
	etag        string
}

var dashAssets = func() map[string]dashAsset {
	out := map[string]dashAsset{}
	_ = fs.WalkDir(dashFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(dashFS, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		a := dashAsset{
			data:        data,
			contentType: dashContentType(p),
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
		}
		if len(data) >= 1024 && compressibleType(a.contentType) {
			var buf bytes.Buffer
			// Compressed once at startup, served forever: pay for the
			// best ratio here; the dynamic middleware uses the default.
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			_, _ = zw.Write(data)
			_ = zw.Close()
			if buf.Len() < len(data) {
				a.gz = buf.Bytes()
			}
		}
		out[p] = a
		return nil
	})
	return out
}()

func dashContentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		return "application/javascript"
	case strings.HasSuffix(p, ".css"):
		return "text/css"
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(p, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(p, ".json"):
		return "application/json"
	default:
		return "text/plain"
	}
}

// handleDashUI serves the SPA. /dash/ and /dash -> index.html; other paths
// fall back to index.html too (client-side routing), except real files
// under /dash/assets/.
func (s *Server) handleDashUI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/dash")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		p = "index.html"
	}
	clean := path.Clean(p)
	if clean == "index.html" || strings.HasPrefix(clean, "assets/") {
		serveDashFile(w, r, clean)
		return
	}
	// Serve real root assets (favicon.ico, logo.png, …) when they exist.
	if _, ok := dashAssets[clean]; ok {
		serveDashFile(w, r, clean)
		return
	}
	// SPA fallback: unknown client routes get the shell.
	serveDashFile(w, r, "index.html")
}

func serveDashFile(w http.ResponseWriter, r *http.Request, p string) {
	a, ok := dashAssets[path.Clean(p)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	// Content-hashed bundles under assets/ can be cached forever: a changed
	// file gets a new name. Everything else (index.html above all) must
	// revalidate so a deploy takes effect immediately.
	if strings.HasPrefix(p, "assets/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	body := a.data
	etag := a.etag
	if a.gz != nil {
		varyAcceptEncoding(h)
	}
	if a.gz != nil && acceptsGzip(r) {
		body = a.gz
		etag = a.etag[:len(a.etag)-1] + `-gz"`
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}
