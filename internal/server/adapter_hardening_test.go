package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMalformedUpstreamAdapterResponsesAreNotForwarded(t *testing.T) {
	fixtures := []struct {
		name string
		body string
	}{
		{name: "empty choices", body: `{"choices":[],"private":"upstream-secret"}`},
		{name: "wrong shape choice", body: `{"choices":[{"message":"upstream-secret"}]}`},
	}
	for _, fixture := range fixtures {
		for _, protocol := range []string{"anthropic", "responses"} {
			t.Run(protocol+"/"+fixture.name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, fixture.body)
				}))
				t.Cleanup(upstream.Close)

				cfg := runtimeConfig(upstream.URL)
				if protocol == "anthropic" {
					cfg.ModelMap["nv/test"] = "primary:upstream-test"
				}
				s := runtimeTestServer(t, cfg)
				var w *httptest.ResponseRecorder
				if protocol == "anthropic" {
					w = anthropicCall(s, `{"model":"nv/test","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, false)
				} else {
					w = responsesCall(s, `{"model":"test","input":"hello"}`)
				}
				if w.Code != http.StatusBadGateway {
					t.Fatalf("status=%d body=%q, want 502", w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), fixture.body) || strings.Contains(w.Body.String(), "upstream-secret") {
					t.Fatalf("raw upstream response leaked to client: %q", w.Body.String())
				}
				var envelope map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
					t.Fatalf("error envelope is not JSON: %v", err)
				}
				var message, errorType string
				if protocol == "anthropic" {
					if envelope["type"] != "error" {
						t.Fatalf("Anthropic envelope type=%v, want error", envelope["type"])
					}
					errObject, _ := envelope["error"].(map[string]any)
					message, _ = errObject["message"].(string)
					errorType, _ = errObject["type"].(string)
					if errorType != "api_error" {
						t.Fatalf("Anthropic error type=%q, want api_error", errorType)
					}
				} else {
					errObject, _ := envelope["error"].(map[string]any)
					message, _ = errObject["message"].(string)
					errorType, _ = errObject["type"].(string)
					if errorType != "server_error" {
						t.Fatalf("Responses error type=%q, want server_error", errorType)
					}
				}
				if message != "upstream response could not be translated" {
					t.Fatalf("error message=%q, want generic translation failure", message)
				}
				rec := runtimeRecord(t, s)
				if rec.OK || rec.Status != http.StatusBadGateway {
					t.Fatalf("record=%+v, want failed 502 request", rec)
				}
			})
		}
	}
}

func TestTruncatePreservesUTF8WithinByteLimit(t *testing.T) {
	for _, tc := range []struct {
		input string
		n     int
		want  string
	}{
		{input: "é日🙂x", n: 3, want: "é..."},
		// Two runes fit, but their five UTF-8 bytes exceed the two-byte budget.
		{input: "é日", n: 2, want: "é..."},
	} {
		got := truncate(tc.input, tc.n)
		if got != tc.want {
			t.Errorf("truncate(%q, %d)=%q, want %q", tc.input, tc.n, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) returned invalid UTF-8: %q", tc.input, tc.n, got)
		}
	}
}
