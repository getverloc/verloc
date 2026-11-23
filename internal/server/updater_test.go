package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemVerComparisonAndChannels(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		newer   bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.1.0", true},
		{"v1.0.0", "v2.0.0", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.2.0", "v1.1.9", false},
		{"1.0.0", "v1.0.1", true},
		{"v1.0.0-dev", "v1.0.0", true},
		{"v1.0.0-rc.2", "v1.0.0-rc.10", true},
		{"v1.0.0-rc.10", "v1.0.0", true},
		{"v1.0.0", "v1.0.0-rc.1", false},
		{"dev", "v1.0.0", true},
		{"f6a6e93", "v1.0.0", true},
		{"v1.0.0", "not-a-semver", false},
	}
	for _, tc := range cases {
		if got := isNewerVersion(tc.current, tc.latest); got != tc.newer {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.newer)
		}
	}

	if got := versionChannel("v1.0.0"); got != "stable" {
		t.Fatalf("versionChannel(v1.0.0) = %q, want stable", got)
	}
	if got := versionChannel("v1.1.0-rc.1"); got != "prerelease" {
		t.Fatalf("versionChannel(v1.1.0-rc.1) = %q, want prerelease", got)
	}
	if got := versionChannel("v1.0.0-dev"); got != "dev" {
		t.Fatalf("versionChannel(v1.0.0-dev) = %q, want dev", got)
	}
	if got := versionChannel("f6a6e93"); got != "dev" {
		t.Fatalf("versionChannel(f6a6e93) = %q, want dev", got)
	}
}

func TestValidateUpdateConfigAndDownloadURL(t *testing.T) {
	if _, err := validateUpdateConfig(&UpdateConfig{Repo: "getverloc/verloc", CheckIntervalHours: 6}); err != nil {
		t.Fatalf("valid update config rejected: %v", err)
	}
	// Legacy personal-account repo migrates seamlessly to the organization repo.
	migrated, err := validateUpdateConfig(&UpdateConfig{Repo: "kunalkcube/verloc", CheckIntervalHours: 6})
	if err != nil {
		t.Fatalf("legacy repo rejected: %v", err)
	}
	if migrated.Repo != "getverloc/verloc" {
		t.Fatalf("legacy repo migrated to %q, want getverloc/verloc", migrated.Repo)
	}
	for _, bad := range []UpdateConfig{
		{Repo: "../evil/repo", CheckIntervalHours: 6},
		{Repo: "https://evil.example/repo", CheckIntervalHours: 6},
		{Repo: "onlyone", CheckIntervalHours: 6},
		{Repo: "getverloc/verloc", CheckIntervalHours: 999},
	} {
		if _, err := validateUpdateConfig(&bad); err == nil {
			t.Errorf("expected invalid UpdateConfig %+v to fail", bad)
		}
	}

	if err := validateDownloadURL("https://github.com/getverloc/verloc/releases/download/v1.0.1/checksums.txt", false); err != nil {
		t.Fatalf("github.com rejected: %v", err)
	}
	if err := validateDownloadURL("https://objects.githubusercontent.com/github-production-release-asset/123", false); err != nil {
		t.Fatalf("objects.githubusercontent.com rejected: %v", err)
	}
	if err := validateDownloadURL("http://github.com/getverloc/verloc/releases/download/v1.0.1/checksums.txt", false); err == nil {
		t.Fatalf("expected HTTP download URL to be rejected")
	}
	if err := validateDownloadURL("https://evil.example/verloc.tar.gz", false); err == nil {
		t.Fatalf("expected non-GitHub host to be rejected")
	}
}

func makeTestReleaseArchive(t *testing.T, binaryContent []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "verloc",
		Mode: 0o755,
		Size: int64(len(binaryContent)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(binaryContent); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	return archive, hex.EncodeToString(sum[:])
}

func TestUpdaterCheckAndSelfUpdatePreservesUserData(t *testing.T) {
	prevVersion, prevCommit, prevBuild := Version, Commit, BuildTime
	Version, Commit, BuildTime = "v1.0.0", "abc1234", "2026-11-23T09:00:00Z"
	t.Cleanup(func() { Version, Commit, BuildTime = prevVersion, prevCommit, prevBuild })

	dir := t.TempDir()
	exePath := filepath.Join(dir, "verloc")
	oldBinary := []byte("#!/bin/sh\necho 'verloc v1.0.0 (commit abc1234)'\n")
	if err := os.WriteFile(exePath, oldBinary, 0o755); err != nil {
		t.Fatalf("write old binary: %v", err)
	}

	// Simulate user data living right beside the binary (same layout as kcube).
	userData := map[string][]byte{
		filepath.Join(dir, "verloc.db"):                  []byte("sqlite-analytics-user-data"),
		filepath.Join(dir, "config.json"):                []byte(`{"listen":":8787"}`),
		filepath.Join(dir, "verloc-data", "control.db"):  []byte("encrypted-control-data"),
		filepath.Join(dir, "verloc-data", "master.key"):  bytes.Repeat([]byte{0x42}, 32),
		filepath.Join(dir, "verloc-data", "setup.token"): []byte("secret-setup-token"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "verloc-data"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range userData {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("seed user file %s: %v", path, err)
		}
	}

	newBinary := []byte("#!/bin/sh\necho 'verloc v1.1.0 (commit def5678, built 2026-11-24T00:00:00Z)'\n")
	assetName := fmt.Sprintf("verloc_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archiveBytes, archiveSHA := makeTestReleaseArchive(t, newBinary)

	var releaseHits atomic.Int32
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/getverloc/verloc/releases/latest":
			releaseHits.Add(1)
			if r.Header.Get("If-None-Match") == `"etag-v1.1.0"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-v1.1.0"`)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(githubRelease{
				TagName:     "v1.1.0",
				Name:        "v1.1.0",
				Body:        "## What's Changed\n- Smooth auto-updates",
				HTMLURL:     "https://github.com/getverloc/verloc/releases/tag/v1.1.0",
				PublishedAt: "2026-11-24T00:00:00Z",
				Assets: []githubAsset{
					{Name: assetName, Size: int64(len(archiveBytes)), BrowserDownloadURL: ts.URL + "/dl/" + assetName},
					{Name: "checksums.txt", Size: 96, BrowserDownloadURL: ts.URL + "/dl/checksums.txt"},
				},
			})
		case "/dl/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", archiveSHA, assetName)
		case "/dl/" + assetName:
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	srv, err := NewServer(&Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctrl, _, _ := controlTestStore(t)
	srv.control = ctrl

	restarted := make(chan string, 1)
	u := srv.updater
	u.apiBaseURL = ts.URL
	u.allowInsecureURL = true
	u.metaClient = newUpdaterHTTPClient(5*time.Second, true)
	u.downloadClient = newUpdaterHTTPClient(10*time.Second, true)
	u.resolveExe = func() (string, error) { return exePath, nil }
	u.restartDelay = 10 * time.Millisecond
	u.restartHook = func(p string) { restarted <- p }
	u.refreshCapability()

	// First check populates latest release.
	st, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !st.UpdateAvailable || !st.CanSelfUpdate || st.Latest == nil || st.Latest.Version != "v1.1.0" || !st.Latest.AssetReady {
		t.Fatalf("unexpected status after Check: %+v", st)
	}

	// Second check sends ETag and receives 304 without losing cached release metadata.
	st2, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if releaseHits.Load() != 2 || !st2.UpdateAvailable || st2.Latest == nil || st2.Latest.Version != "v1.1.0" {
		t.Fatalf("ETag check failed: hits=%d status=%+v", releaseHits.Load(), st2)
	}

	// Apply update.
	applySt, code, err := u.StartApply()
	if err != nil || code != http.StatusAccepted {
		t.Fatalf("StartApply: code=%d err=%v st=%+v", code, err, applySt)
	}

	select {
	case gotExe := <-restarted:
		if gotExe != exePath {
			t.Fatalf("restart exe = %q, want %q", gotExe, exePath)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for self-update restart; status=%+v", u.Status())
	}

	// Verify binary was atomically replaced and backup preserves previous binary.
	gotBin, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read replaced binary: %v", err)
	}
	if !bytes.Equal(gotBin, newBinary) {
		t.Fatalf("replaced binary mismatch")
	}

	finalStatus := u.Status()
	if finalStatus.State != "restarting" || finalStatus.BackupPath == "" {
		t.Fatalf("unexpected final status: %+v", finalStatus)
	}
	bakBin, err := os.ReadFile(finalStatus.BackupPath)
	if err != nil {
		t.Fatalf("read backup binary: %v", err)
	}
	if !bytes.Equal(bakBin, oldBinary) {
		t.Fatalf("backup binary does not match previous binary")
	}

	// Verify every user data file is 100% untouched.
	for path, want := range userData {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("user data file missing %s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("user data file modified %s", path)
		}
	}

	// Verify audit event was recorded.
	events, err := ctrl.AuditEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	foundAudit := false
	for _, ev := range events {
		if ev.Action == "system.updated" && strings.Contains(ev.Subject, "v1.0.0 -> v1.1.0") {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Fatalf("expected system.updated audit event, got %+v", events)
	}
}

func TestUpdaterRejectsBadChecksumAndBadBinary(t *testing.T) {
	prevVersion := Version
	Version = "v1.0.0"
	t.Cleanup(func() { Version = prevVersion })

	for _, tc := range []struct {
		name        string
		tamperSHA   bool
		badBinary   bool
		wantErrPart string
	}{
		{name: "checksum mismatch", tamperSHA: true, wantErrPart: "checksum mismatch"},
		{name: "smoke test failure", badBinary: true, wantErrPart: "failed validation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			exePath := filepath.Join(dir, "verloc")
			orig := []byte("#!/bin/sh\necho 'verloc v1.0.0'\n")
			if err := os.WriteFile(exePath, orig, 0o755); err != nil {
				t.Fatal(err)
			}

			bin := []byte("#!/bin/sh\necho 'verloc v1.1.0'\n")
			if tc.badBinary {
				bin = []byte("#!/bin/sh\nexit 1\n")
			}
			assetName := fmt.Sprintf("verloc_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
			archive, sha := makeTestReleaseArchive(t, bin)
			if tc.tamperSHA {
				sha = strings.Repeat("0", 64)
			}

			var ts *httptest.Server
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/getverloc/verloc/releases/latest":
					_ = json.NewEncoder(w).Encode(githubRelease{
						TagName: "v1.1.0",
						Assets: []githubAsset{
							{Name: assetName, Size: int64(len(archive)), BrowserDownloadURL: ts.URL + "/dl/" + assetName},
							{Name: "checksums.txt", Size: 96, BrowserDownloadURL: ts.URL + "/dl/checksums.txt"},
						},
					})
				case "/dl/checksums.txt":
					_, _ = fmt.Fprintf(w, "%s  %s\n", sha, assetName)
				case "/dl/" + assetName:
					_, _ = w.Write(archive)
				default:
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()

			srv, err := NewServer(&Config{})
			if err != nil {
				t.Fatal(err)
			}
			u := srv.updater
			u.apiBaseURL = ts.URL
			u.allowInsecureURL = true
			u.metaClient = newUpdaterHTTPClient(5*time.Second, true)
			u.downloadClient = newUpdaterHTTPClient(10*time.Second, true)
			u.resolveExe = func() (string, error) { return exePath, nil }
			u.restartHook = func(string) { t.Errorf("restartHook should never fire on %s", tc.name) }
			u.refreshCapability()

			if _, err := u.Check(context.Background()); err != nil {
				t.Fatalf("Check: %v", err)
			}
			if _, code, err := u.StartApply(); err != nil || code != http.StatusAccepted {
				t.Fatalf("StartApply: code=%d err=%v", code, err)
			}

			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				st := u.Status()
				if st.State == "error" {
					if !strings.Contains(st.LastError, tc.wantErrPart) {
						t.Fatalf("error = %q, want substring %q", st.LastError, tc.wantErrPart)
					}
					got, err := os.ReadFile(exePath)
					if err != nil || !bytes.Equal(got, orig) {
						t.Fatalf("original executable was modified on failed update")
					}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatalf("timed out waiting for error state; status=%+v", u.Status())
		})
	}
}

func TestUpdateSettingsPersistenceAndEndpoints(t *testing.T) {
	store, dbPath, keyPath := controlTestStore(t)
	seed := controlTestConfig()
	autoCheck := true
	seed.Update = &UpdateConfig{
		AutoCheck:          &autoCheck,
		AutoUpdate:         true,
		CheckIntervalHours: 12,
		IncludePrereleases: true,
		Repo:               "getverloc/verloc",
	}
	if _, err := store.LoadOrImportConfig(seed); err != nil {
		t.Fatalf("LoadOrImportConfig: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenControlStore(dbPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Update == nil || !loaded.Update.AutoUpdate || loaded.Update.CheckIntervalHours != 12 || !loaded.Update.IncludePrereleases {
		t.Fatalf("update settings did not persist: %+v", loaded.Update)
	}

	// Unauthenticated callers must be rejected on update endpoints.
	srv, err := NewServer(loaded)
	if err != nil {
		t.Fatal(err)
	}
	srv.control = reopened
	sec, err := NewSecurity(reopened, SecurityOptions{SetupToken: strings.Repeat("a", 43)})
	if err != nil {
		t.Fatal(err)
	}
	srv.security = sec
	h := srv.Handler()
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/dash/api/admin/update"},
		{http.MethodPost, "/dash/api/admin/update/check"},
		{http.MethodPost, "/dash/api/admin/update/apply"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(ep.method, ep.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without session = %d, want 401", ep.method, ep.path, rec.Code)
		}
	}
}
