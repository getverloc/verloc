package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func mediaDecimal(v string) *string { return &v }

func TestMediaResponseAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, operation, body string
		in, out, cache        uint64
		images                int
		tokens, breakdown     bool
	}{
		{"count is not tokens", "images", `{"data":[{"b64_json":"AA=="}]}`, 0, 0, 0, 1, false, false},
		{"prompt completion", "images", `{"data":[{"url":"https://example.com/a"}],"usage":{"prompt_tokens":10,"completion_tokens":400,"prompt_tokens_details":{"cached_tokens":2}}}`, 10, 400, 2, 1, true, false},
		{"usage before data", "images", `{"usage":{"input_tokens":5,"output_tokens":20},"data":[{"b64_json":"AA==","url":null}]}`, 5, 20, 0, 1, true, false},
		{"input only is partial", "images", `{"data":[{"url":"a"}],"usage":{"input_tokens":5}}`, 0, 0, 0, 1, false, false},
		{"explicit zeros", "images", `{"data":[{"url":"a"}],"usage":{"input_tokens":0,"output_tokens":0}}`, 0, 0, 0, 1, true, false},
		{"embedding input only", "embeddings", `{"usage":{"prompt_tokens":12},"data":[{"embedding":[0.1,-1e-3,2]}]}`, 12, 0, 0, 0, true, false},
		{"embedding base64", "embeddings", `{"data":[{"embedding":"AAAA"}],"usage":{"input_tokens":8}}`, 8, 0, 0, 0, true, false},
		{"escaped keys", "images", `{"d\u0061ta":[{"b64_json":"AA=="}],"us\u0061ge":{"input_tokens":1,"output_tokens":2}}`, 1, 2, 0, 1, true, false},
		{"breakdown", "images", `{"data":[{"url":"a"}],"usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"text_tokens":3,"image_tokens":7},"output_tokens_details":{"text_tokens":2,"image_tokens":18}}}`, 10, 20, 0, 1, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := readMediaResponse(strings.NewReader(tc.body), tc.operation)
			if err != nil {
				t.Fatal(err)
			}
			if u.Input != tc.in || u.Output != tc.out || u.Cache != tc.cache || u.Images != tc.images || u.TokensKnown != tc.tokens || u.BreakdownKnown != tc.breakdown {
				t.Fatalf("wrong quantities: %+v", u)
			}
		})
	}
}

func TestMediaResponseRejectsMalformed(t *testing.T) {
	for _, body := range []string{
		`not-json`, `{"data":[]}`, `{"data":[{}]}`, `{"data":[{"url":""}]}`,
		`{"data":[{"url":"a"}]} {}`, `{"data":[{"url":"a"}],"data":[{"url":"b"}]}`,
		`{"error":{"message":"bad"},"data":[{"url":"a"}]}`, `{"data":[{"url":"a"}],"usage":{"input_tokens":-1}}`,
		`{"data":[{"url":"a"}],"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}}`,
		`{"data":[{"url":"a"}],"usage":{"input_tokens":18446744073709551615,"output_tokens":2}}`,
		`{"data":[{"url":"a"}],"usage":{"padding":"` + strings.Repeat("A", 65<<10) + `"}}`,
		`{"data":[{"url":"a\q"}]}`, `{"data":[{"url":"a"}],"nested":` + strings.Repeat("[", 70) + `0` + strings.Repeat("]", 70) + `}`,
		`{"data":[{"url":"a"}],"` + strings.Repeat(`\u0061`, 1000) + `":0}`,
	} {
		if _, err := readMediaResponse(strings.NewReader(body), "images"); err == nil {
			t.Fatalf("accepted malformed response: %.200s", body)
		}
	}
	for _, vector := range []string{`[]`, `[true]`, `[{}]`, `["1"]`, `[1,]`, `[1e999]`, `""`} {
		if _, err := readMediaResponse(strings.NewReader(`{"data":[{"embedding":`+vector+`}]}`), "embeddings"); err == nil {
			t.Fatalf("accepted vector %s", vector)
		}
	}
}

func TestMediaDecimalPricing(t *testing.T) {
	tokens := mediaResponse{Input: 10, Output: 20, TokensKnown: true, Images: 3, ImagesKnown: true, TextInput: 3, ImageInput: 7, TextOutput: 2, ImageOutput: 18, BreakdownKnown: true}
	for _, tc := range []struct {
		name      string
		price     MediaPriceSnapshot
		usage     mediaResponse
		req       mediaRequest
		operation string
		cost      float64
		known     bool
		source    string
	}{
		{"missing tokens is not free", MediaPriceSnapshot{Input: "2", Output: "8"}, mediaResponse{Images: 1, ImagesKnown: true}, mediaRequest{}, "images", 0, false, "unknown"},
		{"explicit free image", MediaPriceSnapshot{Image: mediaRate(0)}, mediaResponse{Images: 1, ImagesKnown: true}, mediaRequest{}, "images", 0, true, "calculated"},
		{"decimal flat", MediaPriceSnapshot{Image: mediaRate(.1)}, tokens, mediaRequest{}, "images", .3, true, "calculated"},
		{"input only embeddings", MediaPriceSnapshot{Input: "2", Cache: "2"}, mediaResponse{Input: 1000, TokensKnown: true}, mediaRequest{}, "embeddings", .002, true, "calculated"},
		{"token image", MediaPriceSnapshot{Input: "2", Output: "8", Cache: "1"}, tokens, mediaRequest{}, "images", .00018, true, "calculated"},
		{"exact variant", MediaPriceSnapshot{ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Quality: "high", Price: "0.1"}}}}, tokens, mediaRequest{size: "1024x1024", quality: "high"}, "images", .3, true, "calculated"},
		{"size only variant", MediaPriceSnapshot{ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Price: "0.05"}}}}, tokens, mediaRequest{size: "1024x1024", quality: "high"}, "images", .15, true, "calculated"},
		{"most specific variant wins", MediaPriceSnapshot{ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Price: "0.05"}, {Size: "1024x1024", Quality: "high", Price: "0.1"}}}}, tokens, mediaRequest{size: "1024x1024", quality: "high"}, "images", .3, true, "calculated"},
		{"ambiguous variants", MediaPriceSnapshot{ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Price: "0.05"}, {Quality: "high", Price: "0.1"}}}}, tokens, mediaRequest{size: "1024x1024", quality: "high"}, "images", 0, false, "unknown"},
		{"auto is not a variant", MediaPriceSnapshot{Image: mediaRate(.1), ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Quality: "high", Price: "0.1"}}}}, tokens, mediaRequest{size: "auto", quality: "high"}, "images", 0, false, "unknown"},
		{"breakdown", MediaPriceSnapshot{ImagePrices: &ImagePricing{TextInput: mediaDecimal("1"), ImageInput: mediaDecimal("2"), TextOutput: mediaDecimal("3"), ImageOutput: mediaDecimal("4")}}, tokens, mediaRequest{}, "images", .000095, true, "calculated"},
		{"incomplete breakdown rates", MediaPriceSnapshot{ImagePrices: &ImagePricing{TextInput: mediaDecimal("1")}}, tokens, mediaRequest{}, "images", 0, false, "unknown"},
		{"provider opt in", MediaPriceSnapshot{Image: mediaRate(5), ImagePrices: &ImagePricing{ProviderCost: true}}, mediaResponse{Cost: "0.123"}, mediaRequest{}, "images", .123, true, "provider_reported"},
		{"provider not trusted by default", MediaPriceSnapshot{}, mediaResponse{Cost: "0"}, mediaRequest{}, "images", 0, false, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost, known, source := tc.price.cost(tc.usage, tc.req, tc.operation)
			if cost != tc.cost || known != tc.known || source != tc.source {
				t.Fatalf("got %g %v %s", cost, known, source)
			}
		})
	}
	for _, bad := range []string{"-1", "NaN", "1/2", "1e999999999", "1e-999999999", "\"0\"", "1000000001"} {
		if _, ok := decimalRate(bad); ok {
			t.Fatalf("accepted price %q", bad)
		}
	}
	if err := validateImagePricing(&ImagePricing{Variants: []ImagePriceVariant{{Price: "0"}, {Price: "1"}}}); err == nil {
		t.Fatal("accepted duplicate conditions")
	}
	if err := validateImagePricing(&ImagePricing{Variants: []ImagePriceVariant{{Price: "1"}}}); err == nil {
		t.Fatal("accepted a variant with no size or quality condition")
	}
}

func TestMediaLargeResponseAccounting(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{false: "usage after", true: "usage before"}[before], func(t *testing.T) {
			s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
				usage := `"usage":{"prompt_tokens":10,"completion_tokens":400}`
				io.WriteString(w, `{`)
				if before {
					io.WriteString(w, usage+`,`)
				}
				io.WriteString(w, `"data":[{"b64_json":"`)
				io.CopyN(w, strings.NewReader(strings.Repeat("A", (8<<20)+1)), (8<<20)+1)
				io.WriteString(w, `"}]`)
				if !before {
					io.WriteString(w, `,`+usage)
				}
				io.WriteString(w, `}`)
			}, func(cfg *Config) { cfg.ModelMeta = map[string]*ModelMeta{"test": {PricingImage: mediaRate(.04)}} })
			w := mediaCall(s, "/v1/images/generations", `{"model":"test","prompt":"not persisted"}`, "application/json")
			r := runtimeRecord(t, s)
			if w.Code != 200 || !r.OK || r.Images != 1 || !r.ImagesKnown || !r.UsageKnown || r.TokOut != 400 || !r.CostKnown || r.Cost != .04 {
				t.Fatalf("status=%d record=%+v", w.Code, r)
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Fatal("large payload changed")
			}
		})
	}
}

func FuzzMediaJSONValidation(f *testing.F) {
	for _, s := range []string{`{"a":[1,true,null,"a\\b"]}`, `[1,]`, `{"a":"\u0061"}`, `-1e3`, `{"a":true} junk`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1<<16 {
			return
		}
		p := mediaJSON{r: bufio.NewReader(strings.NewReader(s))}
		if err := p.value(0); err != nil {
			return
		}
		if _, err := p.peek(); err != io.EOF {
			return
		}
		if !json.Valid([]byte(s)) {
			t.Fatalf("scanner accepted invalid JSON: %q", s)
		}
	})
}

func BenchmarkMediaResponseLarge(b *testing.B) {
	payload := strings.Repeat("A", 9<<20)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		r := io.MultiReader(strings.NewReader(`{"usage":{"input_tokens":5,"output_tokens":10},"data":[{"b64_json":"`), strings.NewReader(payload), strings.NewReader(`"}]}`))
		if _, err := readMediaResponse(r, "images"); err != nil {
			b.Fatal(err)
		}
	}
}
