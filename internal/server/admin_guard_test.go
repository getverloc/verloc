package server

import (
	"strings"
	"testing"
)

func TestProviderPersistRejectsPrivateUpstream(t *testing.T) {
	cfg := &Config{Providers: map[string]*ProviderConfig{}}
	err := applyProviderRequest(cfg, providerUpsertReq{Name: "lan", BaseURL: "http://127.0.0.1:11434/v1", NoAuth: true})
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("private upstream persisted without opt-in: %v", err)
	}
	if err := applyProviderRequest(cfg, providerUpsertReq{Name: "lan", BaseURL: "http://127.0.0.1:11434/v1", NoAuth: true, AllowPrivateUpstreams: true}); err != nil {
		t.Fatalf("private upstream with explicit opt-in rejected: %v", err)
	}
}

// B6/D16: a %ACCOUNT% placeholder must not defeat the probe guard.
func TestProbeGuardHandlesAccountPlaceholder(t *testing.T) {
	_, err := (&Server{}).resolveProbeConn(t.Context(), probeReq{
		BaseURL: "https://203.0.113.10/client/v4/accounts/%ACCOUNT%/ai/v1", NoAuth: true,
	})
	if err != nil {
		t.Fatalf("public placeholder URL refused: %v", err)
	}
}

// D7: a non-finite tick cost is clamped instead of poisoning lifetime sums.
