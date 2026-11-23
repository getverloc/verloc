# Changelog

All notable changes to **verloc** are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

## [1.0.1] - 2026-11-23

### Added
- Multi-arch (`linux/amd64`, `linux/arm64`) container image publishing to `ghcr.io/getverloc/verloc` on every SemVer release tag.
- SLSA build provenance attestations (`actions/attest-build-provenance`) for both release archives and GHCR container images.
- Docker image build verification job in CI alongside Go race tests and dashboard checks.
- Organization-wide governance, issue forms, PR template, `CODEOWNERS`, categorized release notes, CodeQL scanning, secret scanning with push protection, and Dependabot automation.

### Changed
- Migrated canonical repository, installer, and auto-updater defaults from `kunalkcube/verloc` to `getverloc/verloc` with seamless normalization for existing `v1.0.0` configurations.
- Updated `modernc.org/sqlite` to `v1.62.0` (Go `1.26+`), `alpine` runtime base image to `3.24`, and rebuilt embedded dashboard dependencies (React `19.3.1`, Recharts `3.11`, Lucide `1.77`, Vite `8.3.8`).
- Configured CI and release workflows to read the Go version directly from `go.mod`.

## [1.0.0] - 2026-11-23

### Added
- Canonical SemVer versioning (`VERSION`), build metadata injection, and in-app version visibility across the embedded operations dashboard.
- ETag-cached GitHub Releases update checker and checksum-verified, zero-data-loss in-place self-updater with rollback backup, graceful request draining, and PID-preserving restart.
- One-command installer (`install.sh`) with user/system service management (`systemd` / `launchd`), multi-stage non-root `Dockerfile`, and loopback-safe `docker-compose.yml`.
- Dial-time SSRF guard with DNS pinning and per-provider private-upstream opt-in, global speculative hedging concurrency caps (`max_hedges` up to 2), accurate streaming client-disconnect classification (`499`), cancellable rollup backfill shutdown, Anthropic SSE gzip bypass, and sanitized upstream 4xx error forwarding.

[1.0.1]: https://github.com/getverloc/verloc/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/getverloc/verloc/releases/tag/v1.0.0
