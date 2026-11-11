package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMediaModelPricesPersistAndClear(t *testing.T) {
	s, path, keyPath := controlTestStore(t)
	cfg := runtimeConfig("https://upstream.example/v1")
	cfg.applyDefaults()
	cfg, err := s.LoadOrImportConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rates := &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Quality: "high", Price: "0"}}, TextInput: mediaDecimal("0"), ImageInput: mediaDecimal("2.50"), ImageOutput: mediaDecimal("8"), ProviderCost: true}
	for _, req := range []modelUpsertReq{
		{ID: "emb/test", Backend: "primary", Upstream: "embedding", Operations: []string{"embeddings"}, PricingInput: mediaRate(2)},
		{ID: "img/test", Backend: "primary", Upstream: "image", Operations: []string{"images"}, PricingImage: mediaRate(0), ImagePrices: rates},
		{ID: "img/unknown", Backend: "primary", Upstream: "unknown", Operations: []string{"images"}},
	} {
		if err := applyModelRequest(cfg, req, false); err != nil {
			t.Fatal(err)
		}
	}
	if !*cfg.ModelMeta["emb/test"].PricingKnown {
		t.Fatal("input-only embedding rate is unknown")
	}
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := OpenControlStore(path, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.LoadOrImportConfig(runtimeConfig("https://ignored.example"))
	if err != nil {
		t.Fatal(err)
	}
	image := got.ModelMeta["img/test"]
	if image.PricingImage == nil || *image.PricingImage != 0 || !reflect.DeepEqual(image.ImagePrices, rates) || !reflect.DeepEqual(image.Operations, []string{"images"}) {
		t.Fatalf("image metadata lost: %+v", image)
	}
	if got.ModelMeta["img/unknown"].PricingImage != nil || *got.ModelMeta["img/unknown"].PricingKnown {
		t.Fatal("unknown became free")
	}
	if !*got.ModelMeta["emb/test"].PricingKnown {
		t.Fatal("embedding rate lost on reload")
	}
	// Ordinary API edits preserve image prices; explicit clearing removes them.
	edit := modelUpsertReq{ID: "img/test", Backend: "primary", Upstream: "image"}
	if err := applyModelRequest(got, edit, true); err != nil {
		t.Fatal(err)
	}
	if got.ModelMeta["img/test"].PricingImage == nil {
		t.Fatal("omitted price cleared unexpectedly")
	}
	edit.ClearImagePrice = true
	edit.ImagePrices = &ImagePricing{}
	if err := applyModelRequest(got, edit, true); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SaveConfig(got); err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.LoadOrImportConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ModelMeta["img/test"].PricingImage != nil || len(loaded.ModelMeta["img/test"].ImagePrices.Variants) != 0 {
		t.Fatal("cleared price returned on reload")
	}
}

func TestMediaDurableAnalyticsAndEndpointCacheIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	price := &MediaPriceSnapshot{Image: mediaRate(.04), Size: "1024x1024", Quality: "high", ImagePrices: &ImagePricing{Variants: []ImagePriceVariant{{Size: "1024x1024", Quality: "high", Price: "0.04"}}}}
	for i, endpoint := range []string{"", "chat", "embeddings", "images/generations", "images/edits"} {
		r := analyticsRecord(7200001+int64(i), endpoint)
		r.Endpoint = endpoint
		if endpoint == "images/generations" {
			r.UsageKnown = false
			r.Images = 2
			r.ImagesKnown = true
			r.Cost = .08
			r.CostSource = "calculated"
			r.PriceSnapshot = price
		}
		if endpoint == "images/edits" {
			r.OK = false
			r.Status = 502
			r.UsageKnown = false
			r.CostKnown = false
		}
		if !s.QueueRequest(r) {
			t.Fatal("record dropped")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, endpoint := range []string{"", "chat", "embeddings", "images/generations", "images/edits", "unknown"} {
		f := RequestFilter{Endpoint: endpoint, Limit: 1}
		page, err := s.Requests(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.Analytics(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(1)
		if endpoint == "" {
			want = 5
		}
		if page.Total != want || a.Summary != page.Summary {
			t.Fatalf("endpoint=%q page=%+v analytics=%+v", endpoint, page.Summary, a.Summary)
		}
		if endpoint == "" || endpoint == "images/generations" {
			if page.Summary.Images != 2 {
				t.Fatalf("lost images: %+v", page.Summary)
			}
		}
		if endpoint == "images/generations" {
			r := page.Requests[0]
			if r.UsageKnown || !r.CostKnown || r.Cost != .08 || !reflect.DeepEqual(r.PriceSnapshot, price) || r.CostSource != "calculated" {
				t.Fatalf("accounting changed across restart: %+v", r)
			}
			for _, groups := range [][]UsageGroup{a.Models, a.Providers, a.Clients, a.Endpoints} {
				if len(groups) != 1 || groups[0].Images != 2 || groups[0].Cost != .08 {
					t.Fatalf("dimensional accounting missing: %+v", groups)
				}
			}
		}
		if endpoint == "images/edits" && page.Summary.UnknownImages != 1 {
			t.Fatal("failed images reported as known zero")
		}
		if endpoint == "unknown" && page.Requests[0].Endpoint != "" {
			t.Fatal("historical endpoint relabeled")
		}
	}
	if _, err := requestFilter(url.Values{"endpoint": {"not-an-endpoint"}}); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
	if f, err := requestFilter(url.Values{"endpoint": {"images/edits"}}); err != nil || f.Endpoint != "images/edits" {
		t.Fatalf("filter=%+v err=%v", f, err)
	}
}

func TestMediaPriceSnapshotSurvivesLiveUpdate(t *testing.T) {
	accepted, release := make(chan struct{}), make(chan struct{})
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(accepted)
		<-release
		io.WriteString(w, `{"data":[{"url":"a"}]}`)
	}, func(cfg *Config) { cfg.ModelMeta = map[string]*ModelMeta{"test": {PricingImage: mediaRate(.1)}} })
	done := make(chan struct{})
	go func() {
		defer close(done)
		mediaCall(s, "/v1/images/generations", `{"model":"test","size":"1024x1024","quality":"high"}`, "application/json")
	}()
	<-accepted
	cfg, err := cloneConfig(s.runtime().cfg)
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	cfg.ModelMeta["test"].PricingImage = mediaRate(.2)
	s.rt.Store(newRuntimeState(cfg, s.runtime().backends))
	close(release)
	<-done
	r := runtimeRecord(t, s)
	if r.Cost != .1 || r.PriceSnapshot == nil || *r.PriceSnapshot.Image != .1 || r.PriceSnapshot.Size != "1024x1024" || r.PriceSnapshot.Quality != "high" {
		t.Fatalf("request repriced mid-flight: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if !json.Valid(encoded) {
		t.Fatal("snapshot is not durable JSON")
	}
}

func TestMediaFallbackUsesTargetPricing(t *testing.T) {
	s := mediaGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var v struct{ Model string }
		json.Unmarshal(b, &v)
		if v.Model == "upstream-test" {
			w.WriteHeader(429)
			return
		}
		io.WriteString(w, `{"data":[{"url":"a"}]}`)
	}, func(cfg *Config) {
		p := *cfg.Providers["primary"]
		cfg.Providers["fallback"] = &p
		cfg.ModelMap["fallback-alias"] = "fallback:other"
		cfg.ModelMeta = map[string]*ModelMeta{"test": {PricingImage: mediaRate(.1)}, "fallback-alias": {PricingImage: mediaRate(.2), Operations: []string{"images"}}}
		cfg.ModelRoutes = map[string][]RouteTarget{"test": {{Backend: "fallback", Upstream: "other"}}}
	})
	mediaCall(s, "/v1/images/generations", `{"model":"test"}`, "application/json")
	r := runtimeRecord(t, s)
	if !r.OK || r.Backend != "fallback" || r.Cost != .2 || *r.PriceSnapshot.Image != .2 {
		t.Fatalf("wrong fallback accounting: %+v", r)
	}
}

func TestMediaPublicCatalogPrices(t *testing.T) {
	cfg := runtimeConfig("https://upstream.example/v1")
	cfg.applyDefaults()
	for _, req := range []modelUpsertReq{
		{ID: "img/free", Backend: "primary", Upstream: "free", PricingImage: mediaRate(0), Operations: []string{"images"}},
		{ID: "img/unknown", Backend: "primary", Upstream: "unknown", Operations: []string{"images"}},
		{ID: "emb/paid", Backend: "primary", Upstream: "embedding", PricingInput: mediaRate(2), Operations: []string{"embeddings"}},
	} {
		if err := applyModelRequest(cfg, req, false); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleModels(w, httptest.NewRequest("GET", "/v1/models", nil))
	var models struct {
		Data []modelInfo `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	for _, model := range models.Data {
		if model.ID == "img/free" && model.Type != "image" || model.ID == "emb/paid" && model.Type != "embedding" {
			t.Fatalf("wrong media kind: %+v", model)
		}
		if model.ID == "img/free" && model.Pricing.Image != "0" {
			t.Fatal("free image price omitted")
		}
		if model.ID == "img/unknown" && (model.PricingKnown || model.Pricing.Image != "") {
			t.Fatal("unknown image price advertised as free")
		}
	}
	w = httptest.NewRecorder()
	s.handlePricing(w, httptest.NewRequest("GET", "/v1/pricing", nil))
	var prices struct {
		Data   map[string]*pricingPer1M `json:"data"`
		Images map[string]struct {
			PerImage *float64 `json:"per_image"`
		} `json:"images"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &prices); err != nil {
		t.Fatal(err)
	}
	if prices.Data["img/unknown"] != nil {
		t.Fatal("unknown token prices advertised as zero")
	}
	if p := prices.Data["emb/paid"]; p == nil || p.Input != 2 {
		t.Fatal("input-only embedding price missing")
	}
	if p := prices.Images["img/free"].PerImage; p == nil || *p != 0 {
		t.Fatal("free image price missing")
	}
	var raw struct {
		Data map[string]map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, hasOutput := raw.Data["emb/paid"]["output"]; hasOutput {
		t.Fatal("input-only embedding price fabricated an output rate")
	}
}

func TestMediaAnalyticsMigrationPreservesLegacyHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.QueueRequest(analyticsRecord(7200001, "legacy")) {
		t.Fatal("record dropped")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.QueryRow(`SELECT record FROM requests`).Scan(&before); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, col := range []string{"endpoint", "images", "images_known"} {
		if _, err := db.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var after string
	if err := s.db.QueryRow(`SELECT record FROM requests`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("migration rewrote historical metadata")
	}
	page, err := s.Requests(context.Background(), RequestFilter{Endpoint: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Requests[0].ID != "legacy" || page.Requests[0].Endpoint != "" {
		t.Fatalf("history lost or reclassified: %+v", page)
	}
}
