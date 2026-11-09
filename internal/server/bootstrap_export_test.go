package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBootstrapExportPreservesRuntimeSecurity(t *testing.T) {
	dir := privateTestDir(t)
	seed := controlTestConfig()
	seed.Listen = "127.0.0.1:8123"
	seed.ManagementDB = filepath.Join(dir, "control.db")
	seed.MasterKeyFile = filepath.Join(dir, "master.key")
	seed.SetupTokenFile = filepath.Join(dir, "setup.token")
	seed.Dashboard.DBPath = filepath.Join(dir, "analytics.db")
	seed.SecureCookies = true
	seed.AllowedHosts = []string{"gateway.example:8443"}
	seed.TrustedProxies = []string{"10.20.0.0/16", "127.0.0.1"}
	first, err := openGateway(seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	cloned, err := cloneConfig(first.runtime().cfg)
	if err != nil {
		t.Fatal(err)
	}
	cloned.ModelMeta["stable/model-id"].Name = "Saved before export"
	if err := first.applyLive(cloned); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(dir, "exported", "bootstrap.json")
	if err := exportBootstrapFile(first.runtime().cfg, exportPath); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := loadBootstrap(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	assertBootstrap := func(got *Config) {
		t.Helper()
		if got.Listen != seed.Listen || got.ManagementDB != seed.ManagementDB || got.MasterKeyFile != seed.MasterKeyFile || got.SetupTokenFile != seed.SetupTokenFile || got.Dashboard.DBPath != seed.Dashboard.DBPath {
			t.Fatalf("bootstrap listener or paths changed after export: %#v", got)
		}
		if !got.SecureCookies || !reflect.DeepEqual(got.AllowedHosts, seed.AllowedHosts) || !reflect.DeepEqual(got.TrustedProxies, seed.TrustedProxies) {
			t.Fatalf("bootstrap security changed after export: secure=%v hosts=%v proxies=%v", got.SecureCookies, got.AllowedHosts, got.TrustedProxies)
		}
	}
	assertBootstrap(bootstrap)
	second, err := openGateway(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	assertBootstrap(second.runtime().cfg)
	if second.runtime().cfg.ModelMeta["stable/model-id"].Name != "Saved before export" {
		t.Fatal("exported bootstrap did not reopen the same management database")
	}

	badHost := httptest.NewRequest(http.MethodPost, "http://unlisted.example/dash/api/auth/login", strings.NewReader(`{}`))
	badHost.Header.Set("Origin", "https://unlisted.example")
	rejected := httptest.NewRecorder()
	second.Handler().ServeHTTP(rejected, badHost)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("unlisted host status = %d, want 403", rejected.Code)
	}
	allowed := httptest.NewRequest(http.MethodPost, "http://gateway.example:8443/dash/api/auth/login", nil)
	allowed.Header.Set("Origin", "https://gateway.example:8443")
	if !second.security.securityOriginOK(allowed) {
		t.Fatal("exported host allowlist rejected its configured host")
	}
	allowed.RemoteAddr = "10.20.3.4:1234"
	allowed.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := second.security.loginAddr(allowed); got != "198.51.100.7" {
		t.Fatalf("trusted proxy lost after export: login address = %q", got)
	}
	allowed.RemoteAddr = "192.0.2.8:1234"
	if got := second.security.loginAddr(allowed); got != "192.0.2.8" {
		t.Fatalf("untrusted proxy was accepted after export: login address = %q", got)
	}
	response := httptest.NewRecorder()
	second.security.respondSession(response, allowed, "test-session-token", "test-csrf-token")
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatal("exported secure_cookies did not protect the session cookie")
	}
}
