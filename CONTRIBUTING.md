# Contributing to verloc

Thanks for your interest in contributing to **verloc**. Whether you are fixing a bug, improving latency on the hot path, refining the embedded dashboard, or clarifying documentation, we appreciate your help.

## Repository Layout

```text
VERSION                  Canonical SemVer version (MAJOR.MINOR.PATCH)
cmd/verloc/              CLI entry point
internal/server/         Go HTTP gateway, routing, auth, management, analytics, embedded assets
dashboard/               React + TypeScript operations dashboard (builds to internal/server/static)
scripts/                 Build, Docker entrypoint, and catalog scripts
install.sh               One-command installer, updater, and service manager (Linux/macOS)
```

## Development Requirements

- **Go 1.25+** (CGO is disabled; no C compiler required)
- **Node.js 22.12+** (or 24) and **npm** (only required when editing `dashboard/`)

## Local Setup & Verification

```sh
# Install locked dashboard dependencies
npm --prefix dashboard ci

# Build embedded dashboard + Go binary
make build

# Run Go static checks and full test suite
go vet ./...
go test -race ./... -count=1

# Run dashboard lint, unit tests, and production embed build
npm --prefix dashboard run lint
npm --prefix dashboard test
npm --prefix dashboard run build

# Run catalog regression tests
node --test scripts/sync-model-details.test.mjs
```

> **Note:** `internal/server/static/` is committed so Go-only builds (`CGO_ENABLED=0 go build ./cmd/verloc`) work without Node.js. If your PR changes anything under `dashboard/`, rebuild and commit the updated embedded assets via `npm --prefix dashboard run build`.

## Pull Request Guidelines

1. **Keep PRs focused:** One logical change per PR makes review faster and release notes cleaner.
2. **Preserve backwards compatibility:** Gateway routes, `config.json`, SQLite schemas (`control.db`, `verloc.db`), and upgrade paths must preserve existing user data.
3. **Add tests:** Include unit or regression tests for new behavior and bug fixes.
4. **Use clear commit/PR titles:** Conventional prefixes (`feat:`, `fix:`, `perf:`, `docs:`, `chore:`, `security:`) are encouraged.
5. **Security issues:** Never open a public issue or PR for a security vulnerability — report it privately via [GitHub Security Advisories](https://github.com/getverloc/verloc/security/advisories/new).

## Code of Conduct

All contributors are expected to uphold our [Code of Conduct](https://github.com/getverloc/.github/blob/main/CODE_OF_CONDUCT.md).
