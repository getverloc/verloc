## Summary

<!-- Briefly describe what this PR changes and why. -->

## Related Issues

<!-- Closes #123 -->

## Verification

- [ ] `go vet ./...`
- [ ] `go test -race ./... -count=1`
- [ ] If `dashboard/` changed: `npm --prefix dashboard run lint`, `npm --prefix dashboard test`, and `npm --prefix dashboard run build` (with rebuilt `internal/server/static/` committed)
- [ ] Existing user data (`control.db`, `verloc.db`, `master.key`, `config.json`) remains backwards-compatible

## Screenshots (if UI changed)

<!-- Attach before/after screenshots for dashboard changes. -->
