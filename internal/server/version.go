package server

import (
	"runtime"
	"strconv"
	"strings"
)

// BuildInfo describes the running verloc binary for the dashboard and updater.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Channel   string `json:"channel"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// CurrentBuildInfo returns structured metadata for the currently running binary.
func CurrentBuildInfo() BuildInfo {
	v := strings.TrimSpace(Version)
	if v == "" {
		v = "dev"
	}
	c := strings.TrimSpace(Commit)
	if c == "" {
		c = "none"
	}
	bt := strings.TrimSpace(BuildTime)
	if bt == "" {
		bt = "unknown"
	}
	return BuildInfo{
		Version:   v,
		Commit:    c,
		BuildTime: bt,
		Channel:   versionChannel(v),
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// SemVer represents a Semantic Versioning 2.0.0 version (MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD]).
type SemVer struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease []string
	Build      string
}

// parseSemVer parses a SemVer 2.0.0 string with an optional leading "v".
func parseSemVer(raw string) (SemVer, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return SemVer{}, false
	}
	var build string
	if i := strings.IndexByte(s, '+'); i >= 0 {
		build = s[i+1:]
		s = s[:i]
	}
	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		preStr := s[i+1:]
		s = s[:i]
		if preStr == "" {
			return SemVer{}, false
		}
		pre = strings.Split(preStr, ".")
		for _, id := range pre {
			if id == "" {
				return SemVer{}, false
			}
			for _, ch := range id {
				if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '-') {
					return SemVer{}, false
				}
			}
		}
	}
	core := strings.Split(s, ".")
	if len(core) != 3 {
		return SemVer{}, false
	}
	nums := make([]int, 3)
	for i, part := range core {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return SemVer{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return SemVer{}, false
		}
		nums[i] = n
	}
	return SemVer{
		Major:      nums[0],
		Minor:      nums[1],
		Patch:      nums[2],
		Prerelease: pre,
		Build:      build,
	}, true
}

// compareSemVer compares two parsed SemVer values per SemVer 2.0.0 §11.
// Returns -1 if a < b, 0 if a == b, and +1 if a > b.
func compareSemVer(a, b SemVer) int {
	if a.Major != b.Major {
		return cmpInt(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpInt(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpInt(a.Patch, b.Patch)
	}
	// A normal version has higher precedence than a pre-release version.
	if len(a.Prerelease) == 0 && len(b.Prerelease) == 0 {
		return 0
	}
	if len(a.Prerelease) == 0 {
		return 1
	}
	if len(b.Prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(a.Prerelease) && i < len(b.Prerelease); i++ {
		ai, bi := a.Prerelease[i], b.Prerelease[i]
		if ai == bi {
			continue
		}
		an, aNum := parseNonNegInt(ai)
		bn, bNum := parseNonNegInt(bi)
		switch {
		case aNum && bNum:
			return cmpInt(an, bn)
		case aNum && !bNum:
			return -1
		case !aNum && bNum:
			return 1
		default:
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return cmpInt(len(a.Prerelease), len(b.Prerelease))
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func parseNonNegInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// versionChannel classifies a version string into "stable", "prerelease", or "dev".
func versionChannel(v string) string {
	sv, ok := parseSemVer(v)
	if !ok {
		return "dev"
	}
	if len(sv.Prerelease) == 0 {
		return "stable"
	}
	for _, id := range sv.Prerelease {
		low := strings.ToLower(id)
		if low == "dev" || low == "dirty" || strings.HasPrefix(low, "dev") {
			return "dev"
		}
	}
	return "prerelease"
}

// normalizeVersionTag returns a canonical v-prefixed tag for valid SemVer strings.
func normalizeVersionTag(v string) string {
	s := strings.TrimSpace(v)
	if _, ok := parseSemVer(s); ok {
		if !strings.HasPrefix(s, "v") {
			return "v" + s
		}
		return s
	}
	return s
}

// isNewerVersion reports whether latest is strictly newer than current.
// Untagged dev/commit builds always treat a valid SemVer release as newer so
// operators can cleanly upgrade onto official releases from the dashboard.
func isNewerVersion(current, latest string) bool {
	latSV, latOK := parseSemVer(latest)
	if !latOK {
		return false
	}
	curSV, curOK := parseSemVer(current)
	if !curOK {
		return strings.TrimSpace(current) != strings.TrimSpace(latest)
	}
	return compareSemVer(curSV, latSV) < 0
}
