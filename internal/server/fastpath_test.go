package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestValidateJSONParity(t *testing.T) {
	cases := []string{
		``,
		`   `,
		`null`,
		`true`,
		`false`,
		`0`,
		`-0`,
		`1234567890`,
		`-123.456e+7`,
		`0.0E-0`,
		`01`,
		`-01`,
		`1.`,
		`.1`,
		`1e`,
		`1e+`,
		`+1`,
		`-`,
		`""`,
		`"hello world"`,
		`"escaped \" \\ \/ \b \f \n \r \t \u0041 \u00e9"`,
		`"bad escape \a"`,
		`"bad unicode \u123g"`,
		`"short unicode \u123"`,
		"\"raw\nnewline\"",
		"\"raw\x00nul\"",
		`"unterminated`,
		`"utf8 café 🚀"`,
		`{}`,
		`[]`,
		`{"a":1,"b":[true,false,null,{"c":"d"}]}`,
		`{"a":1,}`,
		`[1,2,]`,
		`{"a":1} trailing`,
		`{"a":1}   `,
		`  {"a":1}`,
		`{1:2}`,
		`{"a" 1}`,
		`{"a":}`,
		`tru`,
		`fals`,
		`nul`,
		`true false`,
		strings.Repeat("[", 100) + strings.Repeat("]", 100),
		strings.Repeat("[", 10000) + strings.Repeat("]", 10000),
		strings.Repeat("[", 10001) + strings.Repeat("]", 10001),
		strings.Repeat(`{"a":`, 10000) + `1` + strings.Repeat(`}`, 10000),
		strings.Repeat(`{"a":`, 10001) + `1` + strings.Repeat(`}`, 10001),
	}
	for i, tc := range cases {
		b := []byte(tc)
		got := validateJSON(b)
		want := json.Valid(b)
		if got != want {
			label := tc
			if len(label) > 64 {
				label = label[:64] + "..."
			}
			t.Errorf("case %d (%q): validateJSON=%v, json.Valid=%v", i, label, got, want)
		}
	}
}

func TestParseRequestMetadataParity(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantScanV bool
	}{
		{"plain", `{"model":"gpt-4o","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"{\"model\":\"inner\"}"}]}`, true},
		{"nested_model", `{"messages":[{"model":"nested","max_tokens":999999}],"model":"real-model","stream":false}`, true},
		{"all_caps", `{"model":"m1","max_tokens":100,"max_completion_tokens":200,"max_output_tokens":300}`, true},
		{"null_values", `{"model":null,"stream":null,"max_tokens":null,"max_completion_tokens":null,"max_output_tokens":null}`, true},
		{"empty_obj", `{}`, true},
		{"escaped_model_val", `{"model":"foo\u002dbar","stream":true}`, true},
		{"escaped_key", `{"\u006dodel":"escaped","stream":true}`, false},
		{"case_folded_model", `{"Model":"upper","stream":true}`, false},
		{"case_folded_stream", `{"model":"m","Stream":true}`, false},
		{"case_folded_max_tokens", `{"model":"m","Max_Tokens":50}`, false},
		{"duplicate_model", `{"model":"first","model":"second"}`, false},
		{"duplicate_max_tokens", `{"model":"m","max_tokens":10,"max_tokens":20}`, false},
		{"model_wrong_type", `{"model":123}`, false},
		{"stream_wrong_type", `{"model":"m","stream":"yes"}`, false},
		{"max_tokens_float", `{"model":"m","max_tokens":100.5}`, false},
		{"max_tokens_neg", `{"model":"m","max_tokens":-5}`, false},
		{"non_object_array", `[1,2,3]`, false},
		{"non_object_null", `null`, false},
		{"malformed", `{"model":"m",`, false},
		{"trailing_garbage", `{"model":"m"} oops`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)
			gotMD, scan, gotErr := parseRequestMetadata(raw)
			var wantMD requestMetadata
			wantErr := json.Unmarshal(raw, &wantMD)
			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("err mismatch: got %v, want %v", gotErr, wantErr)
			}
			if gotErr == nil && gotMD != wantMD {
				t.Fatalf("metadata mismatch: got %+v, want %+v", gotMD, wantMD)
			}
			if scan.valid != tc.wantScanV {
				t.Fatalf("scan.valid=%v, want %v", scan.valid, tc.wantScanV)
			}
		})
	}
}

func TestSpliceAndCapAndUsageParity(t *testing.T) {
	t.Run("remapBody_fast_vs_map", func(t *testing.T) {
		bodies := []string{
			`{"model":"a","messages":[{"role":"user","content":"{\"model\":\"inner\"}"}]}`,
			`{"messages":[{"role":"user","content":"hi"}]}`,
			`{}`,
			`{"model":"target"}`,
			`{"\u006dodel":"a","x":1}`,
			`{"Model":"upper","x":1}`,
			`{"model":"a","model":"b","x":1}`,
			`[1,2,3]`,
			`{bad json`,
		}
		models := []string{"target", `special<>&"model\name`}
		for _, upstream := range models {
			for _, body := range bodies {
				raw := []byte(body)
				got := remapBody(raw, upstream, false, "openai")

				// Reference map[string]json.RawMessage implementation.
				want := raw
				var top map[string]json.RawMessage
				if json.Unmarshal(raw, &top) == nil && top != nil {
					if mj, err := json.Marshal(upstream); err == nil {
						top["model"] = mj
						if b, err := json.Marshal(top); err == nil {
							want = b
						}
					}
				}
				if !json.Valid(raw) {
					if !bytes.Equal(got, raw) {
						t.Fatalf("malformed input modified: %q -> %q", body, got)
					}
					continue
				}
				var gotObj, wantObj any
				if err := json.Unmarshal(got, &gotObj); err != nil {
					t.Fatalf("remapBody produced invalid JSON for %q: %v (%s)", body, err, got)
				}
				_ = json.Unmarshal(want, &wantObj)
				if !reflect.DeepEqual(gotObj, wantObj) {
					t.Fatalf("body=%q model=%q:\ngot  %s\nwant %s", body, upstream, got, want)
				}
			}
		}
	})

	t.Run("capMaxTokens_edge_cases", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			lim  int
		}{
			{"below", `{"model":"m","max_tokens":1000}`, 32768},
			{"equal", `{"model":"m","max_tokens":32768}`, 32768},
			{"above", `{"model":"m","max_tokens":65536}`, 32768},
			{"multiple_mixed", `{"model":"m","max_tokens":100,"max_completion_tokens":50000,"max_output_tokens":99999}`, 32768},
			{"nested_ignored", `{"model":"m","messages":[{"max_tokens":999999}],"max_tokens":100}`, 32768},
			{"float_above", `{"model":"m","max_tokens":65536.5}`, 32768},
			{"negative", `{"model":"m","max_tokens":-10}`, 32768},
			{"null_val", `{"model":"m","max_tokens":null}`, 32768},
			{"escaped_key", `{"model":"m","\u006dax_tokens":65536}`, 32768},
			{"duplicate_key", `{"model":"m","max_tokens":10,"max_tokens":65536}`, 32768},
			{"malformed", `{"max_tokens":65536`, 32768},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				raw := []byte(tc.body)
				got := capMaxTokens(raw, tc.lim)
				if !json.Valid(raw) {
					if !bytes.Equal(got, raw) {
						t.Fatalf("malformed body altered: %s", got)
					}
					return
				}
				parsed, ok := decodeChunkMap(raw)
				if !ok {
					t.Fatal("decode failed")
				}
				want := raw
				if capMaxTokensMap(parsed, tc.lim) {
					want, _ = json.Marshal(parsed)
				}
				var gotObj, wantObj any
				if err := json.Unmarshal(got, &gotObj); err != nil {
					t.Fatalf("invalid json: %v (%s)", err, got)
				}
				_ = json.Unmarshal(want, &wantObj)
				if !reflect.DeepEqual(gotObj, wantObj) {
					t.Fatalf("got  %s\nwant %s", got, want)
				}
			})
		}
	})

	t.Run("prepareChatBody_matches_sequential", func(t *testing.T) {
		bodies := []string{
			`{"model":"pub","max_tokens":65536,"messages":[{"role":"user","content":"hello"}]}`,
			`{"model":"pub","max_tokens":1024,"messages":[{"role":"assistant","content":"ok","reasoning_content":"secret"},{"role":"user","content":"next"}]}`,
			`{"model":"pub","max_completion_tokens":50000,"reasoning_effort":"high","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			`{"model":"pub","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		}
		for _, isMistral := range []bool{false, true} {
			for _, strip := range []bool{true, false} {
				for _, body := range bodies {
					raw := []byte(body)
					_, scan, _ := parseRequestMetadata(raw)
					scope := reasoningScope{clientID: "c1", model: "pub", provider: "p", upstream: "up"}
					cache := newReasoningCache(16)
					got, gotCtx := prepareChatBody(raw, scan, "up", isMistral, "openai", strip, 32768, cache, scope)

					// Sequential reference pipeline.
					seq := remapBody(raw, "up", isMistral, "openai")
					var wantCtx reasoningContext
					if strip {
						seq = stripReasoningContent(seq, true)
					} else {
						seq, wantCtx = ensureReasoningReplay(seq, cache, scope)
					}
					seq = capMaxTokens(seq, 32768)

					var gotObj, wantObj any
					if err := json.Unmarshal(got, &gotObj); err != nil {
						t.Fatalf("invalid json: %v", err)
					}
					_ = json.Unmarshal(seq, &wantObj)
					if !reflect.DeepEqual(gotObj, wantObj) {
						t.Fatalf("isMistral=%v strip=%v body=%s:\ngot  %s\nwant %s", isMistral, strip, body, got, seq)
					}
					if !strip && gotCtx != wantCtx {
						t.Fatalf("reasoningContext mismatch: got %+v, want %+v", gotCtx, wantCtx)
					}
				}
			}
		}
	})

	t.Run("usageOf_variants", func(t *testing.T) {
		cases := []struct {
			body                  string
			in, out, cache        uint64
			ok                    bool
		}{
			{`{"id":"1","usage":{"prompt_tokens":10,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":4}}}`, 10, 20, 4, true},
			{`{"type":"response.completed","response":{"usage":{"input_tokens":15,"output_tokens":25,"input_tokens_details":{"cached_tokens":7}}}}`, 15, 25, 7, true},
			{`{"id":"1","usage":null}`, 0, 0, 0, false},
			{`{"id":"1","Usage":{"prompt_tokens":5,"completion_tokens":6}}`, 5, 6, 0, true},
			{`{"id":"1","choices":[{"delta":{"content":"usage"}}]}`, 0, 0, 0, false},
			{`{"id":"1","usage":{"prompt_tokens":10}}`, 0, 0, 0, false},
		}
		for _, tc := range cases {
			in, out, cache, ok := usageOf([]byte(tc.body))
			if in != tc.in || out != tc.out || cache != tc.cache || ok != tc.ok {
				t.Errorf("usageOf(%s) = (%d,%d,%d,%v), want (%d,%d,%d,%v)", tc.body, in, out, cache, ok, tc.in, tc.out, tc.cache, tc.ok)
			}
		}
	})
}

func TestStreamCopyWithUsageFastPathEdgeCases(t *testing.T) {
	t.Run("finish_reason_variants", func(t *testing.T) {
		cases := []struct {
			name    string
			sse     string
			wantErr bool
		}{
			{
				name:    "null_only_interrupted",
				sse:     "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
				wantErr: true,
			},
			{
				name:    "finish_reason_with_spaces",
				sse:     "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\" : \"length\"}]}\n\n",
				wantErr: false,
			},
			{
				name:    "multi_choice_second_finishes",
				sse:     "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":null},{\"index\":1,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
				wantErr: false,
			},
			{
				name:    "error_null_not_failed",
				sse:     "data: {\"error\": null, \"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
				wantErr: false,
			},
			{
				name:    "error_object_fails",
				sse:     "data: {\"error\":{\"message\":\"boom\"}}\n\n",
				wantErr: true,
			},
			{
				name:    "response_completed",
				sse:     "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n",
				wantErr: false,
			},
			{
				name:    "response_failed",
				sse:     "data: {\"type\":\"response.failed\"}\n\n",
				wantErr: true,
			},
			{
				name:    "type_error",
				sse:     "data: {\"type\":\"error\"}\n\n",
				wantErr: true,
			},
			{
				name:    "done_without_trailing_blank_line",
				sse:     "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n",
				wantErr: false,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				var gotIn, gotOut uint64
				err := streamCopyWithUsage(rec, strings.NewReader(tc.sse), func(in, out, _ uint64) {
					gotIn, gotOut = in, out
				}, nil, streamOptions{})
				if (err != nil) != tc.wantErr {
					t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
				}
				if tc.name == "response_completed" && (gotIn != 3 || gotOut != 4) {
					t.Fatalf("usage=(%d,%d), want (3,4)", gotIn, gotOut)
				}
			})
		}
	})

	t.Run("oversized_line_pool_cap", func(t *testing.T) {
		big := strings.Repeat("x", 96<<10)
		sse := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + big + "\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		for range 3 {
			rec := httptest.NewRecorder()
			if err := streamCopyWithUsage(rec, strings.NewReader(sse), nil, nil, streamOptions{}); err != nil {
				t.Fatalf("oversized stream failed: %v", err)
			}
			if !strings.Contains(rec.Body.String(), big) {
				t.Fatal("oversized payload was truncated")
			}
		}
	})

	t.Run("ingestBytes_matches_ingestMap", func(t *testing.T) {
		chunks := []string{
			`{"choices":[{"index":0,"delta":{"reasoning_content":"step 1\n","content":"hi "}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning":"ignored because reasoning_content already empty? no, fallback","content":"caf\u00e9"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`,
			// Choice 1 gets invalidated by non-string content.
			`{"choices":[{"index":1,"delta":{"reasoning_content":"secret","content":[{"type":"text","text":"arr"}]}}]}`,
		}
		accBytes := &sseReasoningAcc{}
		accMap := &sseReasoningAcc{}
		var scratch sseIngestChunk
		for _, c := range chunks {
			raw := []byte(c)
			accBytes.ingestBytes(raw, &scratch)
			m, ok := decodeChunkMap(raw)
			if !ok {
				t.Fatalf("decode failed: %s", c)
			}
			accMap.ingestMap(m)
		}
		scope := reasoningScope{clientID: "c1", model: "m", provider: "p", upstream: "u"}
		ctx := reasoningContext{key: reasoningKey{scope: scope}, valid: true}
		cacheBytes := newReasoningCache(8)
		cacheMap := newReasoningCache(8)
		accBytes.store(cacheBytes, ctx)
		accMap.store(cacheMap, ctx)
		if !reflect.DeepEqual(cacheBytes.m, cacheMap.m) {
			t.Fatalf("cache mismatch:\ningestBytes=%+v\ningestMap=%+v", cacheBytes.m, cacheMap.m)
		}
		if len(cacheBytes.m) != 1 {
			t.Fatalf("expected 1 cached entry (choice 0 valid, choice 1 invalid), got %d", len(cacheBytes.m))
		}
	})
}

func TestClientKeysLockFreeConcurrentRace(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key1, view1 := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{
		Name:          "k1",
		AllowedModels: []string{"*"},
		RPM:           100000,
		TPM:           100000000,
		MaxConcurrent: 64,
	})
	key2, view2 := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{
		Name:          "k2",
		AllowedModels: []string{"*"},
		RPM:           100000,
		TPM:           100000000,
		MaxConcurrent: 64,
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Parallel hot-path inference readers hammering ClientMiddleware + AcquireClient + Finish.
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := key1
			if id%2 == 1 {
				key = key2
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				var authedReq *http.Request
				e.security.ClientMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					authedReq = r
					w.WriteHeader(http.StatusOK)
				})).ServeHTTP(w, r)
				if authedReq != nil {
					if lease, err := e.security.AcquireClient(authedReq, "gpt-4o", 10); err == nil {
						lease.Finish(10, false)
					}
				}
			}
		}(g)
	}

	// Concurrent admin mutations: create, edit, rotate, revoke.
	path1 := "/dash/api/admin/client-keys/" + view1.ID
	path2 := "/dash/api/admin/client-keys/" + view2.ID
	for i := range 6 {
		if w := e.request("PUT", path1, clientKeyRequest{
			Name:          "k1-edited",
			AllowedModels: []string{"*"},
			RPM:           100000,
			TPM:           100000000,
			MaxConcurrent: 64,
		}, cookie, csrf); w.Code != 200 {
			t.Fatalf("edit %d failed: %d %s", i, w.Code, w.Body.String())
		}
		if i == 2 {
			if w := e.request("POST", path2+"/rotate", nil, cookie, csrf); w.Code != 200 {
				t.Fatalf("rotate failed: %d %s", w.Code, w.Body.String())
			}
		}
		if i == 4 {
			if w := e.request("DELETE", path2, nil, cookie, csrf); w.Code != 200 {
				t.Fatalf("revoke failed: %d %s", w.Code, w.Body.String())
			}
		}
	}

	close(stop)
	wg.Wait()

	// Revoked/rotated key2 must now be rejected.
	assertClientKeyRejected(t, e.security, key2)
	// No active leases leaked on key1.
	e.security.mu.Lock()
	rec1 := e.security.clients[view1.ID]
	e.security.mu.Unlock()
	rec1.usage.mu.Lock()
	active := rec1.usage.active
	rec1.usage.mu.Unlock()
	if active != 0 {
		t.Fatalf("leaked active leases on k1: %d", active)
	}
}

func TestGzipMiddlewarePoolReuse(t *testing.T) {
	payload := strings.Repeat(`{"hello":"world","n":12345}`, 64)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})
	mux.HandleFunc("GET /empty", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("raw-binary-bytes"))
	})
	h := gzipMiddleware(mux)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				// Compressible JSON
				req := httptest.NewRequest(http.MethodGet, "/json", nil)
				req.Header.Set("Accept-Encoding", "gzip")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Header().Get("Content-Encoding") != "gzip" {
					t.Errorf("expected gzip Content-Encoding, got %q", rec.Header().Get("Content-Encoding"))
					return
				}
				gr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
				if err != nil {
					t.Errorf("gzip.NewReader: %v", err)
					return
				}
				got, err := io.ReadAll(gr)
				_ = gr.Close()
				if err != nil || string(got) != payload {
					t.Errorf("decompressed body mismatch: err=%v len=%d", err, len(got))
					return
				}

				// Empty 204 must not emit gzip bytes
				req2 := httptest.NewRequest(http.MethodGet, "/empty", nil)
				req2.Header.Set("Accept-Encoding", "gzip")
				rec2 := httptest.NewRecorder()
				h.ServeHTTP(rec2, req2)
				if rec2.Code != http.StatusNoContent || rec2.Body.Len() != 0 {
					t.Errorf("204 unexpected: code=%d len=%d", rec2.Code, rec2.Body.Len())
					return
				}

				// Non-compressible binary passes through raw
				req3 := httptest.NewRequest(http.MethodGet, "/bin", nil)
				req3.Header.Set("Accept-Encoding", "gzip")
				rec3 := httptest.NewRecorder()
				h.ServeHTTP(rec3, req3)
				if rec3.Header().Get("Content-Encoding") != "" || rec3.Body.String() != "raw-binary-bytes" {
					t.Errorf("binary unexpected: ce=%q body=%q", rec3.Header().Get("Content-Encoding"), rec3.Body.String())
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestHubZeroAllocAndMetricsParallel(t *testing.T) {
	t.Run("hub_subcount", func(t *testing.T) {
		h := NewHub()
		if h.HasSubscribers() || h.SubCount() != 0 {
			t.Fatalf("new hub should have 0 subs")
		}
		// Publish with 0 subs must be a no-op.
		h.Publish(map[string]any{"type": "req"})
		ch := h.Subscribe()
		if !h.HasSubscribers() || h.SubCount() != 1 {
			t.Fatalf("expected 1 sub, got %d", h.SubCount())
		}
		h.Publish(map[string]any{"type": "req", "v": 1})
		select {
		case msg := <-ch:
			if msg["v"] != 1 {
				t.Fatalf("unexpected msg: %+v", msg)
			}
		default:
			t.Fatal("subscriber did not receive message")
		}
		h.Unsubscribe(ch)
		if h.HasSubscribers() || h.SubCount() != 0 {
			t.Fatalf("expected 0 subs after unsubscribe, got %d", h.SubCount())
		}
		ch2 := h.Subscribe()
		h.Close()
		if h.HasSubscribers() || h.SubCount() != 0 {
			t.Fatalf("expected 0 subs after close, got %d", h.SubCount())
		}
		if _, open := <-ch2; open {
			t.Fatal("channel should be closed after hub.Close")
		}
	})

	t.Run("metrics_parallel_hot_and_cold_status", func(t *testing.T) {
		mt := NewMetrics()
		statuses := []int{200, 400, 401, 429, 500, 502, 418, 201}
		const workers = 8
		const perWorker = 50
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := range perWorker {
					st := statuses[(id+i)%len(statuses)]
					mt.RecordRequest(ReqRecord{
						Model:      "m1",
						Backend:    "b1",
						Status:     st,
						OK:         st < 400,
						TokIn:      10,
						TokOut:     20,
						UsageKnown: true,
						LatMs:      12.5,
					})
					if i%10 == 0 {
						_ = mt.Snapshot(nil)
						_ = mt.DrainDelta()
					}
				}
			}(w)
		}
		wg.Wait()
		snap := mt.Snapshot(nil)
		global := snap["global"].(map[string]any)
		if got := global["requests"].(uint64); got != workers*perWorker {
			t.Fatalf("requests=%d, want %d", got, workers*perWorker)
		}
		stMap := global["status"].(map[string]uint64)
		var stTotal uint64
		for _, v := range stMap {
			stTotal += v
		}
		if stTotal != workers*perWorker {
			t.Fatalf("status sum=%d, want %d (%+v)", stTotal, workers*perWorker, stMap)
		}
		if stMap["418"] == 0 || stMap["201"] == 0 || stMap["200"] == 0 {
			t.Fatalf("missing hot/cold status counts: %+v", stMap)
		}
	})
}
