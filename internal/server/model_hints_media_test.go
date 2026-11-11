package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func hintFromList(t *testing.T, body, id string) *modelHint {
	t.Helper()
	entries, err := decodeModelList([]byte(body), "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i].modelHint
		}
	}
	t.Fatalf("catalog entry %q missing", id)
	return nil
}

func TestCatalogDetectsMediaKindsAndPrices(t *testing.T) {
	for _, tc := range []struct {
		name, body, id string
		kind           string
		ops            []string
		image          *float64
		variants       int
		inputKnown     bool
		pricingKnown   bool
	}{
		{
			name: "vercel image flat", id: "recraft/flash",
			body: `{"data":[{"id":"recraft/flash","type":"image","pricing":{"image":"0.007"},"modalities":{"input":["text"],"output":["image"]}}]}`,
			kind: "images", ops: []string{"images"}, image: mediaRate(0.007),
		},
		{
			name: "vercel embedding input only", id: "alibaba/embed",
			body: `{"data":[{"id":"alibaba/embed","type":"embedding","pricing":{"input":"0.00000001"},"modalities":{"input":["text"],"output":["text"]}}]}`,
			kind: "embeddings", ops: []string{"embeddings"}, inputKnown: true, pricingKnown: true,
		},
		{
			name: "vercel image token pricing", id: "openai/gpt-image",
			body: `{"data":[{"id":"openai/gpt-image","type":"image","pricing":{"input":"0.000005","output":"0.00003"},"modalities":{"input":["text"],"output":["image"]}}]}`,
			kind: "images", ops: []string{"images"}, inputKnown: true, pricingKnown: true,
		},
		{
			name: "vercel image variants", id: "bfl/flux",
			body: `{"data":[{"id":"bfl/flux","type":"image","pricing":{"image":"0.048","image_dimension_quality_pricing":[{"size":"1024x1024","cost":"0.048"},{"size":"2048x2048","quality":"high","cost":"0.1"},{"operation":"web-grounding","cost":"0.014"}]},"modalities":{"input":["text"],"output":["image"]}}]}`,
			kind: "images", ops: []string{"images"}, image: mediaRate(0.048), variants: 2,
		},
		{
			name: "vercel chat", id: "openai/gpt",
			body: `{"data":[{"id":"openai/gpt","type":"language","pricing":{"input":"0.000002","output":"0.000008"},"modalities":{"input":["text"],"output":["text"]}}]}`,
			kind: "chat", ops: []string{"chat"}, inputKnown: true, pricingKnown: true,
		},
		{
			name: "unsupported kind stays explicit", id: "bfl/video",
			body: `{"data":[{"id":"bfl/video","type":"video","pricing":{},"modalities":{"input":["text"],"output":["video"]}}]}`,
			kind: "video",
		},
		{
			name: "openrouter modality", id: "org/picture",
			body: `{"data":[{"id":"org/picture","architecture":{"modality":"text->image"},"pricing":{"image":"0.02"}}]}`,
			kind: "images", ops: []string{"images"}, image: mediaRate(0.02),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := hintFromList(t, tc.body, tc.id)
			if h.Type != tc.kind {
				t.Fatalf("type=%q want %q", h.Type, tc.kind)
			}
			if strings.Join(h.Operations, ",") != strings.Join(tc.ops, ",") {
				t.Fatalf("operations=%v want %v", h.Operations, tc.ops)
			}
			if (h.PricingImage == nil) != (tc.image == nil) || h.PricingImage != nil && *h.PricingImage != *tc.image {
				t.Fatalf("image price=%v want %v", h.PricingImage, tc.image)
			}
			variants := 0
			if h.ImagePrices != nil {
				variants = len(h.ImagePrices.Variants)
			}
			if variants != tc.variants {
				t.Fatalf("variants=%d want %d", variants, tc.variants)
			}
			if h.inputKnown != tc.inputKnown || h.PricingKnown != tc.pricingKnown {
				t.Fatalf("pricing known=%v input=%v want %v/%v", h.PricingKnown, h.inputKnown, tc.pricingKnown, tc.inputKnown)
			}
		})
	}
}

func TestCloudflareMediaHints(t *testing.T) {
	body := `{"success":true,"result":[
		{"name":"@cf/baai/bge-m3","task":{"name":"Text Embeddings"},"properties":[{"property_id":"context_window","value":"60000"},{"property_id":"price","value":[{"unit":"per M input tokens","price":0.0118,"currency":"USD"}]}]},
		{"name":"@cf/black-forest-labs/flux-1-schnell","task":{"name":"Text-to-Image"},"properties":[{"property_id":"price","value":[{"unit":"per image","price":0.000053,"currency":"USD"}]}]},
		{"name":"@cf/meta/llama-3.1-8b-instruct","task":{"name":"Text Generation"},"properties":[]}
	]}`
	models := parseModelsList([]byte(body))
	byID := map[string]*modelHint{}
	for _, m := range models {
		byID[m.ID] = m.Hint
	}
	embed := byID["@cf/baai/bge-m3"]
	if embed == nil || embed.Type != "embeddings" || strings.Join(embed.Operations, ",") != "embeddings" || !embed.PricingKnown || embed.PricingInput != 0.0118 || embed.ContextWindow != 60000 {
		t.Fatalf("cloudflare embedding hint wrong: %+v", embed)
	}
	image := byID["@cf/black-forest-labs/flux-1-schnell"]
	if image == nil || image.Type != "images" || image.PricingImage == nil || *image.PricingImage != 0.000053 {
		t.Fatalf("cloudflare image hint wrong: %+v", image)
	}
	chat := byID["@cf/meta/llama-3.1-8b-instruct"]
	if chat == nil || chat.Type != "chat" || strings.Join(chat.Operations, ",") != "chat" {
		t.Fatalf("cloudflare chat hint wrong: %+v", chat)
	}
}

func TestCatalogDetectsLiteLLMEmbedding(t *testing.T) {
	body := `{"org/embed":{"mode":"embedding","input_cost_per_token":0.00000002,"max_input_tokens":8192}}`
	entries, err := decodeLiteLLM([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d", len(entries))
	}
	h := entries[0].modelHint
	if h.Type != "embeddings" || strings.Join(h.Operations, ",") != "embeddings" || !h.PricingKnown || h.PricingInput != 0.02 {
		t.Fatalf("embedding hint wrong: %+v", h)
	}
}

func TestHintMergeKeepsDetectedKind(t *testing.T) {
	native := &modelHint{Name: "Picture", Type: "images", Operations: []string{"images"}, PricingImage: mediaRate(0.02)}
	merged := mergeDiscoveredModelHint(native, nil)
	if merged == nil || merged.Type != "images" || len(merged.Operations) != 1 || merged.PricingImage == nil {
		t.Fatalf("native media metadata lost: %+v", merged)
	}
}

func TestAdminTestUsesDetectedEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, operation string
		confirm         bool
		path            string
		status          int
		response        string
		check           func(*testing.T, map[string]any)
	}{
		{
			name: "embeddings", operation: "embeddings", path: "/embeddings", status: 200,
			response: `{"data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":3}}`,
			check: func(t *testing.T, got map[string]any) {
				if got["kind"] != "embeddings" || got["dimensions"] != float64(2) || got["tok_in"] != float64(3) {
					t.Fatalf("embedding result=%v", got)
				}
			},
		},
		{
			name: "image requires confirmation", operation: "images", path: "", status: 400,
		},
		{name: "unknown operation is not chat", status: 400},
		{name: "unsupported operation", operation: "video", status: 400},
		{
			name: "image confirmed", operation: "images", confirm: true, path: "/images/generations", status: 200,
			response: `{"data":[{"b64_json":"AA=="}],"usage":{"input_tokens":1,"output_tokens":2}}`,
			check: func(t *testing.T, got map[string]any) {
				if got["kind"] != "images" || got["images"] != float64(1) || got["images_known"] != true {
					t.Fatalf("image result=%v", got)
				}
				if _, leaked := got["b64_json"]; leaked {
					t.Fatal("image bytes returned by test")
				}
			},
		},
		{
			name: "chat", operation: "chat", path: "/chat/completions", status: 200,
			response: `{"choices":[{"message":{"content":"PONG"}}]}`,
			check: func(t *testing.T, got map[string]any) {
				if got["kind"] != "chat" || got["reply"] != "PONG" {
					t.Fatalf("chat result=%v", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != tc.path {
					t.Errorf("upstream path=%q want %q", r.URL.Path, tc.path)
				}
				io.WriteString(w, tc.response)
			}))
			defer upstream.Close()
			e := newSecurityTestEnv(t)
			cfg := &Config{
				DefaultProvider: "mock",
				ModelMap:        map[string]string{"img/test": "mock:img"},
				Providers:       map[string]*ProviderConfig{"mock": {BaseURL: upstream.URL, Keys: []KeyEntry{{Key: "k"}}, AllowPrivateUpstreams: true}},
			}
			cfg.applyDefaults()
			cfg, err := e.store.LoadOrImportConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			srv, err := NewServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			srv.control, srv.security = e.store, e.security
			body, _ := json.Marshal(map[string]any{
				"provider": "mock", "upstream": "img", "operation": tc.operation, "confirm_image_spend": tc.confirm,
			})
			req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/test", bytes.NewReader(body))
			authorizeAdminTest(t, srv, req)
			rec := httptest.NewRecorder()
			srv.handleAdminTest(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.status != 200 {
				if calls.Load() != 0 {
					t.Fatalf("rejected test still reached upstream %d times", calls.Load())
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["ok"] != true {
				t.Fatalf("test result=%v", got)
			}
			tc.check(t, got)
			if strings.Contains(rec.Body.String(), "b64_json") || strings.Contains(rec.Body.String(), `"embedding":`) {
				t.Fatal("test response leaked media payload")
			}
		})
	}
}

func TestCatalogPricingConditionsStayUnknown(t *testing.T) {
	for _, rows := range []string{
		`[{"size":"1024x1024","style":"vivid","cost":"0.1"}]`,
		`[{"size":"1024x1024","operation":"edit","cost":"0.1"}]`,
		`[{"size":"1024x1024","cost":"0.1"},{"size":"1024x1024","cost":"0.2"}]`,
		`[{"quality":"high","cost":null}]`,
	} {
		h := hintFromList(t, `{"data":[{"id":"image","type":"image","pricing":{"image":"0.01","image_dimension_quality_pricing":`+rows+`}}]}`, "image")
		if h.ImagePrices == nil || !h.ImagePrices.UnsupportedConditions {
			t.Fatalf("unsafe table accepted: %+v", h)
		}
		s := &MediaPriceSnapshot{Image: h.PricingImage, ImagePrices: h.ImagePrices}
		if cost, known, _ := s.cost(mediaResponse{ImagesKnown: true, Images: 1}, mediaRequest{size: "1024x1024", quality: "high"}, "images"); known || cost != 0 {
			t.Fatalf("unsupported conditions priced as %v", cost)
		}
	}
}

func TestCloudflarePricesRequireExplicitUSDUnitsAndValues(t *testing.T) {
	var h modelHint
	cloudflarePrices(json.RawMessage(`[
		{"unit":"per image steps","price":0.5,"currency":"USD"},
		{"unit":"per image","currency":"USD"},
		{"unit":"per image","price":null,"currency":"USD"},
		{"unit":"per image","price":1,"currency":"EUR"},
		{"unit":"per M input tokens","price":0,"currency":"USD"},
		{"unit":"per M output tokens","price":0.5,"currency":"USD"}
	]`), &h)
	if !h.inputKnown || h.PricingInput != 0 || !h.outputKnown || h.PricingOutput != 0.5 || h.PricingImage != nil {
		t.Fatalf("prices=%+v", h)
	}
}

func TestCatalogImageTokenComponents(t *testing.T) {
	h := hintFromList(t, `{"data":[{"id":"image","type":"image","pricing":{"input_text":"0","input_image":"0.000002","output_image":"0.000008"}}]}`, "image")
	if h.ImagePrices == nil || h.ImagePrices.TextInput == nil || *h.ImagePrices.TextInput != "0" || h.ImagePrices.ImageInput == nil || *h.ImagePrices.ImageInput != "2" || h.ImagePrices.ImageOutput == nil || *h.ImagePrices.ImageOutput != "8" || h.ImagePrices.TextOutput != nil {
		t.Fatalf("image components=%+v", h.ImagePrices)
	}
}

func TestConflictingKindsDoNotMixOperationsOrPrices(t *testing.T) {
	native := &modelHint{Type: "video"}
	chat := &modelHint{Type: "chat", Operations: []string{"chat"}, PricingInput: 1, inputKnown: true}
	merged := mergeDiscoveredModelHint(native, chat)
	if merged.Type != "video" || len(merged.Operations) != 0 || merged.inputKnown {
		t.Fatalf("mixed kinds: %+v", merged)
	}
	consensus := consensusHints([]*catalogEntry{{modelHint: *native}, {modelHint: *chat}})
	if consensus.Type != "conflicting" || len(consensus.Operations) != 0 {
		t.Fatalf("conflicting consensus: %+v", consensus)
	}
}

func TestConflictingImageTablesCannotFallBackToFlatPrice(t *testing.T) {
	a := &catalogEntry{modelHint: modelHint{Type: "images", Operations: []string{"images"}, PricingImage: mediaRate(.1), ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Price: "0.1"}}}}}
	b := &catalogEntry{modelHint: modelHint{Type: "images", Operations: []string{"images"}, PricingImage: mediaRate(.1), ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Price: "0.2"}}}}}
	h := consensusHints([]*catalogEntry{a, b})
	if h.ImagePrices == nil || !h.ImagePrices.UnsupportedConditions {
		t.Fatalf("conflicting table was dropped: %+v", h)
	}
	s := &MediaPriceSnapshot{Image: h.PricingImage, ImagePrices: h.ImagePrices}
	if _, known, _ := s.cost(mediaResponse{ImagesKnown: true, Images: 1}, mediaRequest{size: "1024x1024"}, "images"); known {
		t.Fatal("flat rate hid conflicting conditions")
	}
}

func TestCatalogMediaMetadataRoundTrip(t *testing.T) {
	s, _, _ := controlTestStore(t)
	cfg := runtimeConfig("https://upstream.example/v1")
	cfg.applyDefaults()
	cfg, err := s.LoadOrImportConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hintFromList(t, `{"data":[{"id":"embed","type":"embedding","pricing":{"input":"0"},"attachment":false,"reasoning":false,"temperature":false,"modalities":{"input":["text"],"output":["vector"]}}]}`, "embed")
	input := h.PricingInput
	if err := applyModelRequest(cfg, modelUpsertReq{ID: "emb/new", Backend: "primary", Upstream: "embed", Kind: h.Type, Operations: h.Operations, Modalities: h.Modalities, Attachment: h.Attachment, Reasoning: h.Reasoning, Temperature: h.Temperature, PricingInput: &input}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.LoadOrImportConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	view := srv.modelView("emb/new", reloaded)
	if view.Kind != "embeddings" || view.Attachment == nil || *view.Attachment || !view.PricingKnown || len(view.PricingFields) != 2 || view.Modalities == nil {
		t.Fatalf("lost view metadata: %+v", view)
	}
	w := httptest.NewRecorder()
	srv.handleModels(w, httptest.NewRequest("GET", "/v1/models", nil))
	var out struct {
		Data []modelInfo `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Data {
		if m.ID == "emb/new" {
			if m.Type != "embedding" || m.Pricing.Prompt != "0" || m.Pricing.Completion != "" || m.Modalities == nil || m.Attachment == nil || *m.Attachment {
				t.Fatalf("wrong public model: %+v", m)
			}
			return
		}
	}
	t.Fatal("saved model missing from public API")
}

func TestModelKindValidation(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		ops    []string
		native bool
		valid  bool
	}{
		{"video", nil, false, false},
		{"images", []string{"chat"}, false, false},
		{"images", nil, false, true},
		{"systemone", nil, false, false},
		{"systemone", nil, true, true},
	} {
		cfg := runtimeConfig("https://upstream.example/v1")
		cfg.applyDefaults()
		if tc.native {
			cfg.Providers["primary"].APIFormat = "systemone"
		}
		err := applyModelRequest(cfg, modelUpsertReq{ID: "test-kind", Backend: "primary", Upstream: "test", Kind: tc.kind, Operations: tc.ops}, false)
		if (err == nil) != tc.valid {
			t.Fatalf("kind=%s ops=%v native=%v err=%v", tc.kind, tc.ops, tc.native, err)
		}
		if err != nil {
			if _, ok := cfg.ModelMap["test-kind"]; ok {
				t.Fatal("rejected kind modified routing")
			}
		}
	}
}

func TestAdminProbeRejectsIncompatibleSavedOperation(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer up.Close()
	srv, _ := testAdminServer(t)
	cfg := srv.runtime().cfg
	cfg.Providers["image-probe"] = &ProviderConfig{BaseURL: up.URL, NoAuth: true, AllowPrivateUpstreams: true}
	cfg.ModelMap["image-probe/model"] = "image-probe:picture"
	cfg.ModelMeta["image-probe/model"] = &ModelMeta{Operations: []string{"images"}}
	req := httptest.NewRequest("POST", "/dash/api/admin/test", strings.NewReader(`{"provider":"image-probe","upstream":"picture","operation":"chat"}`))
	authorizeAdminTest(t, srv, req)
	w := httptest.NewRecorder()
	srv.handleAdminTest(w, req)
	if w.Code != 400 || calls.Load() != 0 {
		t.Fatalf("incompatible probe status=%d calls=%d", w.Code, calls.Load())
	}
}
