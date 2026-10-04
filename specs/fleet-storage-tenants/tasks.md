# Fleet Storage Tenants — Tasks

## Status: IMPLEMENTED

All tasks complete. Tests pass, project compiles.

### Done

- [x] Migration 010: three tables (tenants, members, quotas) and updated quota function (merge !30, !31; verified 2026-10-04 in the production catalog: tenants, tenant_members, tenant_quotas present, cloistr holds SELECT/INSERT/UPDATE/DELETE on all three)
- [x] Storage layer: CRUD for tenants, members, quotas with user-row auto-creation (merge !30, !35, !36, !37; verified by 6 sqlmock unit tests in internal/storage/tenant_test.go, pass)
- [x] API handlers: owner-gated member management, admin tenant/quota creation (merge !30; verified by 3 test groups in internal/api/tenant_test.go, pass)
- [x] Route wiring in handler and admin router (merge !30; verified in production 2026-10-04: NIP-98 owner add/list/remove return 200, a non-owner gets 403)
- [x] CLI commands for tenant creation and quota setting (merge !30; see cmd/cloistr-admin: tenant create, tenant quota)
- [x] Integration tests on Postgres 17: pooling, exact fallback after removal, tenant without a quota row, fresh owner key (merge !35; tested 2026-10-04: 16 storage tests pass, CI runs them on every push)
- [x] CreateTenant works against production constraints: owner users row ensured, display_name set (merge !35, !36; verified 2026-10-04 by a throwaway-tenant run in production, cleaned up afterwards)
- [x] CI test schema matches production (merge !35, !36; verified 2026-10-04: column-by-column diff of all 12 tables against the production catalog shows no differences)
- [x] Empty member list returns [] (merge !37; tested by TestListTenantMembers_EmptyEncodesAsArray, fails on the old code)
- [x] Full test suite passes with no regressions (verified 2026-10-04: GOWORK=off go test ./internal/... all packages pass; CI green on !37)

### Remaining (owned by cloistr-orchestrator)

- [ ] Orchestrator creates the arbiter-fleet tenant (admin create needs the platform-admin key; first API-level run of that endpoint) and adds the 71 role pubkeys via the owner API
- [ ] Set the actual quota limit for the arbiter-fleet tenant
