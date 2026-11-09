package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipMiddleware(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat(`{"ok":true}`, 50)))
	}))
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("JSON response was not gzipped")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatal("gzipped body does not round-trip")
	}

	// Streaming path: never compressed even when accepted.
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("streaming path was gzipped")
	}
}

// C2/C3: hashed assets are immutable, index.html revalidates, ETag works.
func TestDashAssetCaching(t *testing.T) {
	s := &Server{}
	var jsPath string
	for p := range dashAssets {
		if strings.HasPrefix(p, "assets/") && strings.HasSuffix(p, ".js") {
			jsPath = p
			break
		}
	}
	if jsPath == "" {
		t.Skip("no JS asset in this build")
	}
	rec := httptest.NewRecorder()
	s.handleDashUI(rec, httptest.NewRequest("GET", "/dash/"+jsPath, nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed asset Cache-Control = %q, want immutable", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("hashed asset has no ETag")
	}
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dash/"+jsPath, nil)
	req.Header.Set("If-None-Match", etag)
	s.handleDashUI(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: got %d, want 304", rec2.Code)
	}
	rec3 := httptest.NewRecorder()
	s.handleDashUI(rec3, httptest.NewRequest("GET", "/dash/", nil))
	if cc := rec3.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Fatalf("index.html Cache-Control = %q, must revalidate", cc)
	}
}

func TestAcceptEncodingRepresentations(t *testing.T) {
	payload := []byte(strings.Repeat("const answer = 42;\n", 100))
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	const assetPath = "assets/encoding-regression.js"
	previous, existed := dashAssets[assetPath]
	dashAssets[assetPath] = dashAsset{data: payload, gz: compressed.Bytes(), contentType: "application/javascript", etag: `"encoding-regression"`}
	t.Cleanup(func() {
		if existed {
			dashAssets[assetPath] = previous
		} else {
			delete(dashAssets, assetPath)
		}
	})
	tests := []struct {
		name    string
		headers []string
		gzip    bool
	}{
		{name: "absent"},
		{name: "identity", headers: []string{"identity"}},
		{name: "gzip", headers: []string{"gzip"}, gzip: true},
		{name: "explicit exclusion", headers: []string{"gzip;q=0"}},
		{name: "decimal exclusion", headers: []string{"gzip;q=0.000"}},
		{name: "fractional", headers: []string{"gzip;q=0.25"}, gzip: true},
		{name: "whitespace and case", headers: []string{" GZip ; Q = 0.500 "}, gzip: true},
		{name: "identity preferred", headers: []string{"gzip;q=0.2, identity;q=0.8"}},
		{name: "gzip preferred", headers: []string{"gzip;q=0.8, identity;q=0.2"}, gzip: true},
		{name: "equal preference", headers: []string{"gzip;q=0.5, identity;q=0.5"}, gzip: true},
		{name: "identity excluded", headers: []string{"gzip;q=0.1, identity;q=0"}, gzip: true},
		{name: "wildcard", headers: []string{"br, *;q=0.5"}, gzip: true},
		{name: "wildcard excluded", headers: []string{"*;q=0"}},
		{name: "specific exclusion", headers: []string{"*;q=1, gzip;q=0"}},
		{name: "specific exclusion first", headers: []string{"gzip;q=0, *;q=1"}},
		{name: "specific overrides wildcard", headers: []string{"*;q=0, gzip;q=0.3"}, gzip: true},
		{name: "specific quality overrides wildcard", headers: []string{"*;q=1, gzip;q=0.2, identity;q=0.5"}},
		{name: "wildcard identity preference", headers: []string{"*;q=0.2, identity;q=0.5"}},
		{name: "multiple header lines", headers: []string{"br", "gzip;q=0.7"}, gzip: true},
		{name: "invalid quality", headers: []string{"gzip;q=invalid"}},
		{name: "out of range quality", headers: []string{"gzip;q=1.5"}},
		{name: "negative quality", headers: []string{"gzip;q=-0.1"}},
		{name: "nonfinite quality", headers: []string{"gzip;q=NaN"}},
	}
	for _, kind := range []string{"dynamic", "static", "static direct"} {
		static := kind != "dynamic"
		t.Run(kind, func(t *testing.T) {
			var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Vary", "Origin")
				if static {
					serveDashFile(w, r, assetPath)
					return
				}
				w.Header().Set("Content-Type", "application/javascript")
				_, _ = w.Write(payload)
			})
			if kind != "static direct" {
				handler = gzipMiddleware(handler)
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					request := func(etag string) *httptest.ResponseRecorder {
						req := httptest.NewRequest(http.MethodGet, "/dash/"+assetPath, nil)
						for _, value := range tc.headers {
							req.Header.Add("Accept-Encoding", value)
						}
						if etag != "" {
							req.Header.Set("If-None-Match", etag)
						}
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, req)
						return rec
					}
					rec := request("")
					if rec.Code != http.StatusOK {
						t.Fatalf("status = %d, want 200", rec.Code)
					}
					if got := rec.Header().Get("Content-Encoding"); (got == "gzip") != tc.gzip || (got != "" && got != "gzip") {
						t.Fatalf("Content-Encoding = %q, want gzip=%v", got, tc.gzip)
					}
					body := rec.Body.Bytes()
					if tc.gzip {
						zr, err := gzip.NewReader(bytes.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						body, err = io.ReadAll(zr)
						if err != nil {
							t.Fatal(err)
						}
						if err := zr.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if !bytes.Equal(body, payload) {
						t.Fatal("representation did not decode to the original content")
					}
					assertEncodingVary(t, rec.Header(), "Origin")
					if static {
						etag := `"encoding-regression"`
						otherETag := `"encoding-regression-gz"`
						if tc.gzip {
							etag, otherETag = otherETag, etag
						}
						if got := rec.Header().Get("ETag"); got != etag {
							t.Fatalf("ETag = %q, want %q", got, etag)
						}
						cached := request(etag)
						if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
							t.Fatalf("matching representation: status=%d bytes=%d", cached.Code, cached.Body.Len())
						}
						assertEncodingVary(t, cached.Header(), "Origin")
						if got := request(otherETag).Code; got != http.StatusOK {
							t.Fatalf("other representation ETag status = %d, want 200", got)
						}
					}
				})
			}
		})
	}
}

func assertEncodingVary(t *testing.T, h http.Header, preserved string) {
	t.Helper()
	encoding, original := 0, 0
	for _, value := range h.Values("Vary") {
		for _, field := range strings.Split(value, ",") {
			switch {
			case strings.EqualFold(strings.TrimSpace(field), "Accept-Encoding"):
				encoding++
			case strings.TrimSpace(field) == preserved:
				original++
			}
		}
	}
	if encoding != 1 || original != 1 {
		t.Fatalf("Vary = %v, want exactly one Accept-Encoding and preserved %q", h.Values("Vary"), preserved)
	}
}

func TestGzipVaryPreservesExistingEncodingAndWildcard(t *testing.T) {
	for _, vary := range []string{"Origin, accept-encoding", "*"} {
		for _, encoding := range []string{"gzip", "gzip;q=0"} {
			t.Run(vary+"/"+encoding, func(t *testing.T) {
				h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Vary", vary)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"ok":true}`))
				}))
				req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				req.Header.Set("Accept-Encoding", encoding)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if got := rec.Header().Values("Vary"); len(got) != 1 || got[0] != vary {
					t.Fatalf("Vary = %v, want unchanged %q", got, vary)
				}
			})
		}
	}
}
