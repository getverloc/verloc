package server

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	defaultUpdateRepo          = "getverloc/verloc"
	legacyUpdateRepo           = "kunalkcube/verloc"
	defaultUpdateIntervalHours = 6
	maxChecksumBytes           = 1 << 20   // 1 MiB
	maxReleaseArchiveBytes     = 200 << 20 // 200 MiB
	maxExtractedBinaryBytes    = 180 << 20 // 180 MiB
	maxReleaseNotesChars       = 16_000
	updateRestartDelay         = 600 * time.Millisecond
)

var validUpdateRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// allowedDownloadHosts constrains release asset and checksum downloads to
// official GitHub endpoints so a compromised or unexpected redirect cannot
// pivot the updater to arbitrary servers.
var allowedDownloadHosts = map[string]bool{
	"api.github.com":                        true,
	"github.com":                            true,
	"objects.githubusercontent.com":         true,
	"release-assets.githubusercontent.com":  true,
	"github-releases.githubusercontent.com": true,
	"codeload.github.com":                   true,
}

// UpdateConfig controls release checking and automatic self-updates.
type UpdateConfig struct {
	AutoCheck          *bool  `json:"auto_check,omitempty"`
	AutoUpdate         bool   `json:"auto_update,omitempty"`
	CheckIntervalHours int    `json:"check_interval_hours,omitempty"`
	IncludePrereleases bool   `json:"include_prereleases,omitempty"`
	Repo               string `json:"repo,omitempty"`
}

func boolPtr(v bool) *bool { return &v }

func normalizeUpdateRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.EqualFold(repo, legacyUpdateRepo) {
		return defaultUpdateRepo
	}
	return repo
}

func effectiveUpdateConfig(cfg *UpdateConfig) UpdateConfig {
	out := UpdateConfig{
		AutoCheck:          boolPtr(true),
		CheckIntervalHours: defaultUpdateIntervalHours,
		Repo:               normalizeUpdateRepo(os.Getenv("VERLOC_UPDATE_REPO")),
	}
	if cfg == nil {
		return out
	}
	if cfg.AutoCheck != nil {
		out.AutoCheck = boolPtr(*cfg.AutoCheck)
	}
	out.AutoUpdate = cfg.AutoUpdate
	if cfg.CheckIntervalHours > 0 {
		out.CheckIntervalHours = cfg.CheckIntervalHours
	}
	out.IncludePrereleases = cfg.IncludePrereleases
	if strings.TrimSpace(cfg.Repo) != "" {
		out.Repo = normalizeUpdateRepo(cfg.Repo)
	}
	return out
}

func validateUpdateConfig(cfg *UpdateConfig) (*UpdateConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	eff := effectiveUpdateConfig(cfg)
	if cfg.CheckIntervalHours < 0 || cfg.CheckIntervalHours > 168 {
		return nil, fmt.Errorf("update check_interval_hours must be between 1 and 168")
	}
	if !validUpdateRepoRe.MatchString(eff.Repo) || strings.Contains(eff.Repo, "..") {
		return nil, fmt.Errorf("update repo must be in owner/name format")
	}
	return &eff, nil
}

// ReleaseInfo describes a published GitHub release.
type ReleaseInfo struct {
	Version     string `json:"version"`
	Name        string `json:"name,omitempty"`
	Notes       string `json:"notes,omitempty"`
	URL         string `json:"url,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Prerelease  bool   `json:"prerelease,omitempty"`
	AssetName   string `json:"asset_name,omitempty"`
	AssetSize   int64  `json:"asset_size,omitempty"`
	AssetReady  bool   `json:"asset_ready"`
}

// UpdateStatus is the full update state returned by /dash/api/admin/update.
type UpdateStatus struct {
	Current            BuildInfo    `json:"current"`
	Policy             UpdateConfig `json:"policy"`
	State              string       `json:"state"` // idle | checking | downloading | verifying | installing | restarting | error
	ProgressPct        int          `json:"progress_pct,omitempty"`
	Message            string       `json:"message,omitempty"`
	LastError          string       `json:"last_error,omitempty"`
	LastCheckedAt      int64        `json:"last_checked_at,omitempty"`
	UpdateAvailable    bool         `json:"update_available"`
	Latest             *ReleaseInfo `json:"latest,omitempty"`
	CanSelfUpdate      bool         `json:"can_self_update"`
	CannotUpdateReason string       `json:"cannot_update_reason,omitempty"`
	UpdateCommand      string       `json:"update_command,omitempty"`
	ExecutablePath     string       `json:"executable_path,omitempty"`
	BackupPath         string       `json:"backup_path,omitempty"`
}

// UpdateSummary is a compact view embedded inside /dash/api/state.
type UpdateSummary struct {
	UpdateAvailable bool   `json:"update_available"`
	LatestVersion   string `json:"latest_version,omitempty"`
	State           string `json:"state"`
	ProgressPct     int    `json:"progress_pct,omitempty"`
	AutoUpdate      bool   `json:"auto_update"`
	CanSelfUpdate   bool   `json:"can_self_update"`
	LastCheckedAt   int64  `json:"last_checked_at,omitempty"`
}

// Updater checks GitHub Releases for newer versions and performs verified,
// atomic, zero-data-loss binary self-updates.
type Updater struct {
	srv *Server

	mu                sync.Mutex
	state             string
	progressPct       int
	message           string
	lastError         string
	lastCheckedAt     int64
	latest            *ReleaseInfo
	latestAssetURL    string
	latestChecksumURL string
	etag              string
	etagKey           string
	canSelfUpdate     bool
	cannotReason      string
	updateCommand     string
	exePath           string
	backupPath        string
	failedAutoVersion string

	// Overridable seams for deterministic tests.
	apiBaseURL       string
	allowInsecureURL bool
	metaClient       *http.Client
	downloadClient   *http.Client
	resolveExe       func() (string, error)
	restartHook      func(exePath string)
	restartDelay     time.Duration
	smokeTestFn      func(ctx context.Context, binPath string) error

	loopMu     sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	wakeCh     chan struct{}
	applyCtx   context.Context
	applyStop  context.CancelFunc
	applyWG    sync.WaitGroup
	stopping   bool
}

func NewUpdater(srv *Server) *Updater {
	applyCtx, applyStop := context.WithCancel(context.Background())
	u := &Updater{
		srv:            srv,
		state:          "idle",
		apiBaseURL:     "https://api.github.com",
		metaClient:     newUpdaterHTTPClient(15*time.Second, false),
		downloadClient: newUpdaterHTTPClient(5*time.Minute, false),
		resolveExe:     resolveCurrentExecutable,
		restartDelay:   updateRestartDelay,
		smokeTestFn:    smokeTestBinary,
		wakeCh:         make(chan struct{}, 1),
		applyCtx:       applyCtx,
		applyStop:      applyStop,
	}
	u.refreshCapability()
	return u
}

func newUpdaterHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext:           safeDialContext(dialer, allowPrivate),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !allowPrivate {
				if err := validateDownloadURL(req.URL.String(), false); err != nil {
					return err
				}
			}
			// Never leak GitHub auth headers to redirected asset storage hosts.
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

func validateDownloadURL(raw string, allowInsecure bool) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid release URL")
	}
	if allowInsecure {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
		}
		return nil
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("release downloads require HTTPS")
	}
	host := strings.ToLower(parsed.Hostname())
	if !allowedDownloadHosts[host] {
		return fmt.Errorf("disallowed release download host %q", host)
	}
	return nil
}

func resolveCurrentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return filepath.Clean(exe), nil
	}
	return filepath.Clean(resolved), nil
}

func runningInContainer() bool {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("VERLOC_IN_DOCKER"))); v == "1" || v == "true" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	return false
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".verloc-write-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func (u *Updater) policy() UpdateConfig {
	if u.srv == nil {
		return effectiveUpdateConfig(nil)
	}
	rt := u.srv.runtime()
	if rt == nil || rt.cfg == nil {
		return effectiveUpdateConfig(nil)
	}
	return effectiveUpdateConfig(rt.cfg.Update)
}

func (u *Updater) refreshCapability() {
	policy := u.policy()
	repo := policy.Repo
	defaultCmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/master/install.sh | sh -s -- --service", repo)
	systemCmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/master/install.sh | sudo sh -s -- --system", repo)

	can := true
	reason := ""
	cmd := defaultCmd
	exePath := ""

	if v := strings.ToLower(strings.TrimSpace(os.Getenv("VERLOC_DISABLE_SELF_UPDATE"))); v == "1" || v == "true" {
		can = false
		reason = "Self-update is disabled via VERLOC_DISABLE_SELF_UPDATE."
	} else if runningInContainer() {
		can = false
		reason = "Running inside a container — update the container image instead."
		cmd = "docker compose pull && docker compose up -d"
	} else if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		can = false
		reason = fmt.Sprintf("In-place self-update is not supported on %s.", runtime.GOOS)
	} else {
		exe, err := u.resolveExe()
		if err != nil || exe == "" {
			can = false
			reason = "Cannot locate the running verloc executable."
		} else {
			exePath = exe
			dir := filepath.Dir(exe)
			if !dirWritable(dir) {
				can = false
				reason = fmt.Sprintf("Binary directory %s is not writable by the current process user.", dir)
				if strings.HasPrefix(dir, "/usr/") || strings.HasPrefix(dir, "/opt/") {
					cmd = systemCmd
				}
			}
		}
	}

	u.mu.Lock()
	u.canSelfUpdate = can
	u.cannotReason = reason
	u.updateCommand = cmd
	u.exePath = exePath
	u.mu.Unlock()
}

// Start launches the periodic background release checker and auto-updater.
func (u *Updater) Start(ctx context.Context) {
	u.loopMu.Lock()
	if u.cancel != nil || u.stopping {
		u.loopMu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	u.cancel = cancel
	u.done = make(chan struct{})
	u.loopMu.Unlock()

	go u.runLoop(loopCtx)
}

// Stop stops the background loop and waits for any in-flight check/update work.
func (u *Updater) Stop() {
	u.loopMu.Lock()
	u.stopping = true
	cancel := u.cancel
	done := u.done
	u.loopMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if u.applyStop != nil {
		u.applyStop()
	}
	if done != nil {
		<-done
	}
	u.applyWG.Wait()
}

// ConfigChanged wakes the background loop after update settings change.
func (u *Updater) ConfigChanged() {
	u.refreshCapability()
	select {
	case u.wakeCh <- struct{}{}:
	default:
	}
}

func (u *Updater) runLoop(ctx context.Context) {
	defer close(u.done)

	// Short startup delay so gateway boot and initial health checks stay fast.
	initial := time.NewTimer(15 * time.Second)
	select {
	case <-ctx.Done():
		initial.Stop()
		return
	case <-initial.C:
	case <-u.wakeCh:
		initial.Stop()
	}

	for {
		policy := u.policy()
		if policy.AutoCheck == nil || *policy.AutoCheck {
			st, err := u.Check(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("[update] check failed: %v", err)
			}
			if err == nil && policy.AutoUpdate && st.UpdateAvailable && st.CanSelfUpdate && st.Latest != nil && st.Latest.AssetReady {
				u.mu.Lock()
				skip := u.failedAutoVersion == st.Latest.Version
				u.mu.Unlock()
				if !skip {
					log.Printf("[update] auto-updating from %s to %s", st.Current.Version, st.Latest.Version)
					if _, _, applyErr := u.StartApply(); applyErr != nil {
						log.Printf("[update] auto-update could not start: %v", applyErr)
					}
				}
			}
		}

		interval := time.Duration(max(1, policy.CheckIntervalHours)) * time.Hour
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-u.wakeCh:
			timer.Stop()
		}
	}
}

// Status returns a consistent snapshot of the updater's current state.
func (u *Updater) Status() UpdateStatus {
	policy := u.policy()
	cur := CurrentBuildInfo()
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.statusLocked(cur, policy)
}

func (u *Updater) statusLocked(cur BuildInfo, policy UpdateConfig) UpdateStatus {
	var latestCopy *ReleaseInfo
	updateAvail := false
	if u.latest != nil {
		cp := *u.latest
		latestCopy = &cp
		updateAvail = isNewerVersion(cur.Version, cp.Version)
	}
	return UpdateStatus{
		Current:            cur,
		Policy:             policy,
		State:              u.state,
		ProgressPct:        u.progressPct,
		Message:            u.message,
		LastError:          u.lastError,
		LastCheckedAt:      u.lastCheckedAt,
		UpdateAvailable:    updateAvail,
		Latest:             latestCopy,
		CanSelfUpdate:      u.canSelfUpdate,
		CannotUpdateReason: u.cannotReason,
		UpdateCommand:      u.updateCommand,
		ExecutablePath:     u.exePath,
		BackupPath:         u.backupPath,
	}
}

// Summary returns a lightweight update summary for /dash/api/state without I/O.
func (u *Updater) Summary() UpdateSummary {
	policy := u.policy()
	cur := CurrentBuildInfo()
	u.mu.Lock()
	defer u.mu.Unlock()
	sum := UpdateSummary{
		State:         u.state,
		ProgressPct:   u.progressPct,
		AutoUpdate:    policy.AutoUpdate,
		CanSelfUpdate: u.canSelfUpdate,
		LastCheckedAt: u.lastCheckedAt,
	}
	if u.latest != nil {
		sum.LatestVersion = u.latest.Version
		sum.UpdateAvailable = isNewerVersion(cur.Version, u.latest.Version)
	}
	return sum
}

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	HTMLURL     string        `json:"html_url"`
	PublishedAt string        `json:"published_at"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (u *Updater) isBusyLocked() bool {
	switch u.state {
	case "downloading", "verifying", "installing", "restarting":
		return true
	}
	return false
}

// Check queries GitHub Releases for the latest matching release.
func (u *Updater) Check(ctx context.Context) (UpdateStatus, error) {
	u.refreshCapability()
	policy := u.policy()
	cur := CurrentBuildInfo()

	u.mu.Lock()
	if u.isBusyLocked() {
		st := u.statusLocked(cur, policy)
		u.mu.Unlock()
		return st, nil
	}
	prevState := u.state
	u.state = "checking"
	u.message = "Checking for new releases…"
	u.lastError = ""
	endpoint := u.releaseEndpoint(policy)
	etag := ""
	if u.etagKey == endpoint {
		etag = u.etag
	}
	u.mu.Unlock()

	rel, newETag, notModified, err := u.fetchLatestRelease(ctx, endpoint, etag, policy.IncludePrereleases)
	now := time.Now().UnixMilli()

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.isBusyLocked() {
		return u.statusLocked(cur, policy), nil
	}
	if err != nil {
		u.state = "error"
		if prevState == "idle" && u.latest != nil {
			u.state = "idle"
		}
		u.lastError = err.Error()
		u.message = ""
		u.lastCheckedAt = now
		return u.statusLocked(cur, policy), err
	}

	u.lastCheckedAt = now
	u.state = "idle"
	u.lastError = ""
	u.message = ""
	if newETag != "" {
		u.etag = newETag
		u.etagKey = endpoint
	}
	if notModified {
		return u.statusLocked(cur, policy), nil
	}
	if rel == nil {
		u.latest = nil
		u.latestAssetURL = ""
		u.latestChecksumURL = ""
		u.message = "No published releases found yet."
		return u.statusLocked(cur, policy), nil
	}

	wantAsset := fmt.Sprintf("verloc_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var assetURL, checksumURL string
	var assetSize int64
	for _, a := range rel.Assets {
		if a.Name == wantAsset {
			assetURL = a.BrowserDownloadURL
			assetSize = a.Size
		}
		if a.Name == "checksums.txt" {
			checksumURL = a.BrowserDownloadURL
		}
	}
	notes := strings.TrimSpace(rel.Body)
	if len(notes) > maxReleaseNotesChars {
		notes = notes[:maxReleaseNotesChars] + "\n…"
	}
	u.latest = &ReleaseInfo{
		Version:     normalizeVersionTag(rel.TagName),
		Name:        strings.TrimSpace(rel.Name),
		Notes:       notes,
		URL:         rel.HTMLURL,
		PublishedAt: rel.PublishedAt,
		Prerelease:  rel.Prerelease,
		AssetName:   wantAsset,
		AssetSize:   assetSize,
		AssetReady:  assetURL != "" && checksumURL != "",
	}
	u.latestAssetURL = assetURL
	u.latestChecksumURL = checksumURL
	return u.statusLocked(cur, policy), nil
}

func (u *Updater) releaseEndpoint(policy UpdateConfig) string {
	base := strings.TrimRight(u.apiBaseURL, "/")
	if policy.IncludePrereleases {
		return fmt.Sprintf("%s/repos/%s/releases?per_page=10", base, policy.Repo)
	}
	return fmt.Sprintf("%s/repos/%s/releases/latest", base, policy.Repo)
}

func (u *Updater) fetchLatestRelease(ctx context.Context, endpoint, etag string, includePrereleases bool) (*githubRelease, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", fmt.Sprintf("verloc/%s (%s/%s)", CurrentBuildInfo().Version, runtime.GOOS, runtime.GOARCH))
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := u.metaClient.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("release check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header.Get("ETag"), true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", false, fmt.Errorf("GitHub API rate limit reached (HTTP %d); try again later", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("GitHub Releases returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, "", false, fmt.Errorf("read release metadata: %w", err)
	}

	newETag := resp.Header.Get("ETag")
	if !includePrereleases {
		var rel githubRelease
		if err := json.Unmarshal(body, &rel); err != nil {
			return nil, "", false, fmt.Errorf("parse release metadata: %w", err)
		}
		if rel.Draft || rel.TagName == "" {
			return nil, newETag, false, nil
		}
		return &rel, newETag, false, nil
	}

	var list []githubRelease
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, "", false, fmt.Errorf("parse releases list: %w", err)
	}
	var best *githubRelease
	for i := range list {
		r := &list[i]
		if r.Draft || r.TagName == "" {
			continue
		}
		if _, ok := parseSemVer(r.TagName); !ok {
			continue
		}
		if best == nil || isNewerVersion(best.TagName, r.TagName) {
			best = r
		}
	}
	return best, newETag, false, nil
}

// StartApply validates prerequisites and kicks off an asynchronous verified self-update.
// Returns the updated status snapshot, an HTTP status code, and any synchronous validation error.
func (u *Updater) StartApply() (UpdateStatus, int, error) {
	u.refreshCapability()
	policy := u.policy()
	cur := CurrentBuildInfo()

	u.mu.Lock()
	if u.isBusyLocked() {
		st := u.statusLocked(cur, policy)
		u.mu.Unlock()
		return st, http.StatusConflict, errors.New("an update is already in progress")
	}
	if !u.canSelfUpdate {
		reason := u.cannotReason
		st := u.statusLocked(cur, policy)
		u.mu.Unlock()
		return st, http.StatusBadRequest, errors.New(reason)
	}
	if u.latest == nil || !isNewerVersion(cur.Version, u.latest.Version) {
		st := u.statusLocked(cur, policy)
		u.mu.Unlock()
		return st, http.StatusBadRequest, errors.New("verloc is already running the latest version")
	}
	if !u.latest.AssetReady || u.latestAssetURL == "" || u.latestChecksumURL == "" {
		st := u.statusLocked(cur, policy)
		u.mu.Unlock()
		return st, http.StatusBadRequest, fmt.Errorf("release %s does not publish a verified %s/%s binary yet", u.latest.Version, runtime.GOOS, runtime.GOARCH)
	}

	target := *u.latest
	assetURL := u.latestAssetURL
	checksumURL := u.latestChecksumURL
	exePath := u.exePath

	u.state = "downloading"
	u.progressPct = 0
	u.lastError = ""
	u.message = fmt.Sprintf("Downloading %s…", target.Version)
	u.failedAutoVersion = ""
	st := u.statusLocked(cur, policy)
	u.mu.Unlock()

	u.applyWG.Add(1)
	go func() {
		defer u.applyWG.Done()
		u.applyUpdate(cur.Version, target, assetURL, checksumURL, exePath)
	}()

	return st, http.StatusAccepted, nil
}

func (u *Updater) setProgress(state string, pct int, msg string) {
	u.mu.Lock()
	u.state = state
	u.progressPct = pct
	u.message = msg
	u.mu.Unlock()
}

func (u *Updater) failApply(targetVersion string, err error) {
	log.Printf("[update] failed to install %s: %v", targetVersion, err)
	u.mu.Lock()
	u.state = "error"
	u.progressPct = 0
	u.message = ""
	u.lastError = err.Error()
	u.failedAutoVersion = targetVersion
	u.mu.Unlock()
}

func (u *Updater) applyUpdate(fromVersion string, target ReleaseInfo, assetURL, checksumURL, exePath string) {
	parent := u.applyCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()

	if err := validateDownloadURL(checksumURL, u.allowInsecureURL); err != nil {
		u.failApply(target.Version, err)
		return
	}
	if err := validateDownloadURL(assetURL, u.allowInsecureURL); err != nil {
		u.failApply(target.Version, err)
		return
	}

	u.setProgress("downloading", 5, fmt.Sprintf("Fetching checksums for %s…", target.Version))
	expectedSHA, err := u.fetchExpectedChecksum(ctx, checksumURL, target.AssetName)
	if err != nil {
		u.failApply(target.Version, err)
		return
	}

	dir := filepath.Dir(exePath)
	archiveFile, err := os.CreateTemp(dir, ".verloc-archive-*.tar.gz")
	if err != nil {
		u.failApply(target.Version, fmt.Errorf("create staging archive: %w", err))
		return
	}
	archivePath := archiveFile.Name()
	defer os.Remove(archivePath)

	u.setProgress("downloading", 10, fmt.Sprintf("Downloading %s…", target.AssetName))
	actualSHA, dlErr := u.downloadArchiveWithSHA(ctx, assetURL, archiveFile, target.AssetSize)
	closeErr := archiveFile.Close()
	if dlErr != nil {
		u.failApply(target.Version, dlErr)
		return
	}
	if closeErr != nil {
		u.failApply(target.Version, closeErr)
		return
	}

	u.setProgress("verifying", 80, "Verifying SHA-256 checksum…")
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(actualSHA)), []byte(strings.ToLower(expectedSHA))) != 1 {
		u.failApply(target.Version, fmt.Errorf("SHA-256 checksum mismatch for %s", target.AssetName))
		return
	}

	u.setProgress("verifying", 85, "Extracting and validating binary…")
	stagedPath, err := extractBinaryFromTarGz(archivePath, dir)
	if err != nil {
		u.failApply(target.Version, err)
		return
	}
	cleanupStaged := true
	defer func() {
		if cleanupStaged {
			_ = os.Remove(stagedPath)
		}
	}()

	if err := u.smokeTestFn(ctx, stagedPath); err != nil {
		u.failApply(target.Version, fmt.Errorf("downloaded binary failed validation: %w", err))
		return
	}

	u.setProgress("installing", 92, "Backing up previous binary and installing update…")
	// Preserve executable mode if readable, defaulting to 0755.
	mode := os.FileMode(0o755)
	if info, statErr := os.Stat(exePath); statErr == nil {
		mode = info.Mode().Perm() | 0o100
	}
	if err := os.Chmod(stagedPath, mode); err != nil {
		u.failApply(target.Version, fmt.Errorf("chmod staged binary: %w", err))
		return
	}

	// Keep a rollback copy of the previous executable; never touch user data files.
	backupPath := ""
	if bp, berr := backupExecutable(exePath, fromVersion); berr == nil {
		backupPath = bp
	} else {
		log.Printf("[update] warning: could not back up previous binary: %v", berr)
	}

	if err := os.Rename(stagedPath, exePath); err != nil {
		u.failApply(target.Version, fmt.Errorf("replace executable: %w", err))
		return
	}
	cleanupStaged = false
	syncParentDir(dir)

	if u.srv != nil && u.srv.control != nil && u.srv.control.db != nil {
		subject := fmt.Sprintf("%s -> %s", fromVersion, target.Version)
		_, _ = u.srv.control.db.ExecContext(ctx, "INSERT INTO audit_events(ts,action,subject) VALUES(?,?,?)", time.Now().UTC().UnixMilli(), "system.updated", subject)
	}

	u.mu.Lock()
	u.backupPath = backupPath
	u.state = "restarting"
	u.progressPct = 100
	u.message = fmt.Sprintf("Installed %s — restarting gateway…", target.Version)
	u.mu.Unlock()

	log.Printf("[update] installed %s at %s (backup=%s); restarting gateway", target.Version, exePath, backupPath)
	if u.restartDelay > 0 {
		time.Sleep(u.restartDelay)
	}
	if u.restartHook != nil {
		u.restartHook(exePath)
		return
	}
	if u.srv != nil {
		u.srv.requestRestart(exePath)
	}
}

func (u *Updater) fetchExpectedChecksum(ctx context.Context, checksumURL, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("verloc/%s (%s/%s)", CurrentBuildInfo().Version, runtime.GOOS, runtime.GOARCH))
	resp, err := u.downloadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download checksums.txt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksums.txt: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumBytes))
	if err != nil {
		return "", fmt.Errorf("read checksums.txt: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		digest := strings.ToLower(strings.TrimSpace(fields[0]))
		name := strings.TrimPrefix(strings.TrimSpace(fields[1]), "*")
		if name == assetName {
			if len(digest) != 64 {
				return "", fmt.Errorf("malformed SHA-256 checksum for %s", assetName)
			}
			if _, err := hex.DecodeString(digest); err != nil {
				return "", fmt.Errorf("malformed SHA-256 checksum for %s", assetName)
			}
			return digest, nil
		}
	}
	return "", fmt.Errorf("checksum for %s not found in checksums.txt", assetName)
}

func (u *Updater) downloadArchiveWithSHA(ctx context.Context, assetURL string, dst *os.File, expectedSize int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("verloc/%s (%s/%s)", CurrentBuildInfo().Version, runtime.GOOS, runtime.GOARCH))
	resp, err := u.downloadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download release archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download release archive: HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total <= 0 {
		total = expectedSize
	}
	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, maxReleaseArchiveBytes+1)
	buf := make([]byte, 64<<10)
	var written int64
	lastPct := 10
	for {
		n, rerr := limited.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxReleaseArchiveBytes {
				return "", fmt.Errorf("release archive exceeds %d MiB limit", maxReleaseArchiveBytes>>20)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return "", fmt.Errorf("write release archive: %w", werr)
			}
			_, _ = hasher.Write(buf[:n])
			if total > 0 {
				pct := 10 + int((written*68)/total)
				if pct > 78 {
					pct = 78
				}
				if pct > lastPct {
					lastPct = pct
					u.mu.Lock()
					if u.state == "downloading" {
						u.progressPct = pct
					}
					u.mu.Unlock()
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("read release archive: %w", rerr)
		}
	}
	if err := dst.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func extractBinaryFromTarGz(archivePath, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("open release archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("decompress release archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read release archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		if filepath.Base(clean) != "verloc" {
			continue
		}
		if hdr.Size <= 0 || hdr.Size > maxExtractedBinaryBytes {
			return "", fmt.Errorf("unexpected verloc binary size (%d bytes)", hdr.Size)
		}
		out, err := os.CreateTemp(destDir, ".verloc.update-*")
		if err != nil {
			return "", fmt.Errorf("create staged binary: %w", err)
		}
		outPath := out.Name()
		n, copyErr := io.Copy(out, io.LimitReader(tr, maxExtractedBinaryBytes+1))
		if copyErr == nil && n > maxExtractedBinaryBytes {
			copyErr = errors.New("extracted binary exceeded size limit")
		}
		syncErr := out.Sync()
		closeErr := out.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			_ = os.Remove(outPath)
			return "", fmt.Errorf("extract binary: %w", err)
		}
		if err := os.Chmod(outPath, 0o755); err != nil {
			_ = os.Remove(outPath)
			return "", err
		}
		return outPath, nil
	}
	return "", errors.New("release archive did not contain a verloc binary")
}

func smokeTestBinary(ctx context.Context, binPath string) error {
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(tctx, binPath, "-version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("-version failed: %w", err)
	}
	if !strings.Contains(string(out), "verloc") {
		return fmt.Errorf("unexpected -version output")
	}
	return nil
}

func sanitizeVersionForFilename(v string) string {
	var b strings.Builder
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "prev"
	}
	return b.String()
}

func backupExecutable(exePath, currentVersion string) (string, error) {
	in, err := os.Open(exePath)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	tag := sanitizeVersionForFilename(currentVersion)
	backupPath := fmt.Sprintf("%s.bak.%s-%s", exePath, tag, time.Now().UTC().Format("20060102T150405Z"))
	out, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(backupPath)
		return "", err
	}
	return backupPath, nil
}

func syncParentDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// ---- Admin HTTP handlers ----

func (s *Server) ensureUpdater() *Updater {
	if s.updater == nil {
		s.updater = NewUpdater(s)
	}
	return s.updater
}

func (s *Server) handleAdminUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.ensureUpdater().Status())
}

func (s *Server) handleAdminUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u := s.ensureUpdater()
	st, err := u.Check(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, st)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAdminUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u := s.ensureUpdater()
	st, code, err := u.StartApply()
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, code, st)
}
