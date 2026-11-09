package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
)

// TestCatalogOverlaySurvivesAppendReallocation guards the SyncFromConfig
// ordering bug: appending non-seed ids reallocates c.all, which invalidated the
// *modelInfo pointers held in c.byID, so any embedded-id overlay processed after
// a reallocation wrote into the stale backing array and was silently dropped
// (map iteration order decided the outcome). The overlay must win for embedded
// ids regardless of how many new ids are appended.
func TestCatalogOverlaySurvivesAppendReallocation(t *testing.T) {
	defer catalog.SyncFromConfig(&Config{})
	cfg := &Config{
		ModelMap:  map[string]string{"hy/qwen3.8-max": "hyper:qwen3.8-max"},
		ModelMeta: map[string]*ModelMeta{"hy/qwen3.8-max": {Name: "Overlaid", Description: "meta-desc", ContextWindow: 1234, MaxTokens: 5678, Tags: []string{"meta-tag"}}},
	}
	// Enough non-seed ids to force at least one append reallocation.
	for i := range 200 {
		id := fmt.Sprintf("zz/overlay-%d", i)
		cfg.ModelMap[id] = "zz:upstream"
		cfg.ModelMeta[id] = &ModelMeta{Name: id}
	}
	// Map order is random; repeat so a stale-pointer write cannot pass by luck.
	for run := range 50 {
		catalog.SyncFromConfig(cfg)
		m := catalog.Lookup("hy/qwen3.8-max")
		if m == nil {
			t.Fatalf("run %d: embedded id missing from catalog", run)
		}
		if m.Name != "Overlaid" || m.Description != "meta-desc" || m.ContextWindow != 1234 || m.MaxTokens != 5678 || len(m.Tags) != 1 || m.Tags[0] != "meta-tag" {
			t.Fatalf("run %d: embedded overlay lost: name=%q desc=%q ctx=%d max=%d tags=%v",
				run, m.Name, m.Description, m.ContextWindow, m.MaxTokens, m.Tags)
		}
	}
}

func TestCatalogCapabilityOverrides(t *testing.T) {
	yes, no := true, false
	text := map[string]any{"input": []any{"text"}, "output": []any{"text"}}
	vision := map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}}
	for _, tc := range []struct {
		name string
		seed modelInfo
		meta *ModelMeta
		want modelInfo
	}{
		{
			name: "omitted inherits seed",
			seed: modelInfo{Reasoning: &yes, Attachment: &yes, Temperature: &no, Modalities: vision},
			meta: &ModelMeta{},
			want: modelInfo{Reasoning: &yes, Attachment: &yes, Temperature: &no, Modalities: vision},
		},
		{
			name: "false overrides true",
			seed: modelInfo{Reasoning: &yes, Attachment: &yes, Temperature: &yes, Modalities: vision},
			meta: &ModelMeta{Reasoning: &no, Attachment: &no, Temperature: &no, Modalities: text},
			want: modelInfo{Reasoning: &no, Attachment: &no, Temperature: &no, Modalities: text},
		},
		{
			name: "true overrides false",
			seed: modelInfo{Reasoning: &no, Attachment: &no, Temperature: &no, Modalities: text},
			meta: &ModelMeta{Reasoning: &yes, Attachment: &yes, Temperature: &yes, Modalities: vision},
			want: modelInfo{Reasoning: &yes, Attachment: &yes, Temperature: &yes, Modalities: vision},
		},
		{name: "unknown stays unknown", meta: &ModelMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.seed.ID = "seed"
			c := &modelCatalog{embedded: []modelInfo{tc.seed}}
			c.SyncFromConfig(&Config{ModelMap: map[string]string{"seed": "mock:upstream"}, ModelMeta: map[string]*ModelMeta{"seed": tc.meta}})
			got := c.Lookup("seed")
			if !reflect.DeepEqual(got.Reasoning, tc.want.Reasoning) ||
				!reflect.DeepEqual(got.Attachment, tc.want.Attachment) ||
				!reflect.DeepEqual(got.Temperature, tc.want.Temperature) ||
				!reflect.DeepEqual(got.Modalities, tc.want.Modalities) {
				t.Fatalf("capability precedence: got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAdminModelCapabilitiesSurviveEditsAndReload(t *testing.T) {
	srv, path := testAdminServer(t)
	for _, body := range []string{
		`{"id":"mock/capabilities","backend":"mock","upstream":"capabilities","reasoning":true,"attachment":true,"temperature":false,"modalities":{"input":["text","image"],"output":["text"]}}`,
		`{"id":"mock/capabilities","backend":"mock","upstream":"capabilities","reasoning":false,"attachment":false,"temperature":true,"modalities":{"input":["text"],"output":["text"]}}`,
		`{"id":"mock/capabilities","backend":"mock","upstream":"capabilities","name":"Edited without capabilities"}`,
	} {
		req := httptest.NewRequest(http.MethodPut, "/dash/api/admin/models/mock/capabilities", bytes.NewBufferString(body))
		authorizeAdminTest(t, srv, req)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("model edit: %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	srv.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var listed struct {
		Data []modelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var got *modelInfo
	for i := range listed.Data {
		if listed.Data[i].ID == "mock/capabilities" {
			got = &listed.Data[i]
		}
	}
	text := map[string]any{"input": []any{"text"}, "output": []any{"text"}}
	if got == nil || got.Reasoning == nil || *got.Reasoning || got.Attachment == nil ||
		*got.Attachment || got.Temperature == nil || !*got.Temperature || !reflect.DeepEqual(got.Modalities, text) {
		t.Fatalf("ordinary edit erased explicit capabilities: %+v", got)
	}
	reopened, err := OpenControlStore(path, filepath.Join(filepath.Dir(path), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	disk, err := reopened.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	meta := disk.ModelMeta["mock/capabilities"]
	if meta == nil || meta.Reasoning == nil || *meta.Reasoning || meta.Attachment == nil ||
		*meta.Attachment || meta.Temperature == nil || !*meta.Temperature || !reflect.DeepEqual(meta.Modalities, text) {
		t.Fatalf("persisted capability overrides lost on reload: %+v", meta)
	}
}
