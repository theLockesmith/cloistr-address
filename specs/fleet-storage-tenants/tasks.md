# Fleet Storage Tenants — Tasks

## Status: IMPLEMENTED

All tasks complete. Tests pass, project compiles.

### Done

- [x] Migration 010: three tables (tenants, members, quotas) and updated quota function
  - Evidence: `db/migrations/010_up.sql` (157 lines), creates `tenants`, `tenant_members`, `tenant_quotas`, replaces `effective_quota()`
- [x] Storage layer: CRUD for tenants, members, quotas with user-row auto-creation
  - Evidence: `internal/storage/tenant.go` (146 lines), unit tests in `internal/storage/tenant_test.go` (5 tests, all pass)
- [x] API handlers: owner-gated member management, admin tenant/quota creation
  - Evidence: `internal/api/tenant.go` (212 lines), unit tests in `internal/api/tenant_test.go` (3 test groups, all pass)
- [x] Route wiring in handler and admin router
  - Evidence: `internal/api/handler.go` (3 authenticated routes added), `internal/api/admin.go` (2 admin routes added)
- [x] CLI commands for tenant creation and quota setting
  - Evidence: `cmd/cloistr-admin/main.go` (cmdTenant function, 28 lines added)
- [x] Unit tests: storage layer (5 tests), API layer (3 test groups)
  - Evidence: `go test ./internal/storage/ ./internal/api/ -run Tenant` all pass
- [x] Integration tests: quota pooling and fallback after removal
  - Evidence: `internal/storage/postgres_tenant_test.go` (2 tests, skip when TEST_DATABASE_URL unset, run in CI)
- [x] Full test suite passes with no regressions
  - Evidence: `GOWORK=off go test ./... -count=1` all packages pass

### Remaining (not in this PR)

- [ ] Orchestrator creates the arbiter-fleet tenant and adds the 71 role pubkeys via the owner API
- [ ] Set the actual quota limit for the arbiter-fleet tenant
