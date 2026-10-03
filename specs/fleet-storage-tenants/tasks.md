# Fleet Storage Tenants — Tasks

## Status: IMPLEMENTED

All tasks complete. Tests pass, project compiles.

### Done

- [x] Migration 010: three tables (tenants, members, quotas) and updated quota function — verified in `db/migrations/010_up.sql`, creates tenants/tenant_members/tenant_quotas tables and replaces effective_quota()
- [x] Storage layer: CRUD for tenants, members, quotas with user-row auto-creation — verified in `internal/storage/tenant.go`, 5 unit tests pass in `internal/storage/tenant_test.go`
- [x] API handlers: owner-gated member management, admin tenant/quota creation — verified in `internal/api/tenant.go`, 3 test groups pass in `internal/api/tenant_test.go`
- [x] Route wiring in handler and admin router — verified: 3 authenticated routes in `internal/api/handler.go`, 2 admin routes in `internal/api/admin.go`
- [x] CLI commands for tenant creation and quota setting — verified: cmdTenant in `cmd/cloistr-admin/main.go`
- [x] Unit tests: storage layer (5 tests), API layer (3 test groups) — verified: `GOWORK=off go test ./internal/storage/ ./internal/api/ -run Tenant` all pass
- [x] Integration tests: quota pooling and fallback after removal — verified in `internal/storage/postgres_tenant_test.go` (2 tests, require TEST_DATABASE_URL)
- [x] Full test suite passes with no regressions — verified: `GOWORK=off go test ./... -count=1` all packages pass

### Remaining (not in this PR)

- [ ] Orchestrator creates the arbiter-fleet tenant and adds the 71 role pubkeys via the owner API
- [ ] Set the actual quota limit for the arbiter-fleet tenant
