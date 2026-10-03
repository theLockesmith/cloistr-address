# Fleet Storage Tenants

## Problem
Fleet roles (71 arbiter agent pubkeys) need blossom storage. Each gets an anonymous-tier quota individually (100 MB), which is inadequate. They should share one pooled quota under a single tenant.

## Requirements

1. **Tenant schema**: `tenants`, `tenant_members`, `tenant_quotas` tables in the shared `cloistr` database.
2. **Quota pooling**: `effective_quota()` must sum usage across all tenant members when a member is checked, and read the limit from `tenant_quotas` instead of individual limits.
3. **Owner API** (NIP-98 authenticated):
   - `POST /api/v1/tenants/:id/members` — add a member pubkey (creates users row for service access)
   - `DELETE /api/v1/tenants/:id/members/:pubkey` — remove a member
   - `GET /api/v1/tenants/:id/members` — list members
   - Only the tenant owner_pubkey can call these.
4. **Admin API** (platform admin):
   - `POST /admin/v1/tenants` — create a tenant
   - `POST /admin/v1/tenants/quota` — set tenant quota limit
5. **Fallback**: a removed member falls back to its own individual quota (anonymous/named tier).
6. **Refusal tests**: non-owner cannot add members; removed member's quota resolves individually.

## Design

- Migration 010 creates the three tables and updates `effective_quota()`.
- Storage layer: `internal/storage/tenant.go` with CRUD.
- API layer: `internal/api/tenant.go` with handlers, routes wired in handler.go.
- CLI: `cloistr-admin tenant create` and `cloistr-admin tenant quota`.

## Initial tenant

- ID: `arbiter-fleet`
- Owner: pubkey of the `arbiter-fleet` signer account (provided by cloistr-orchestrator)
- The orchestrator's kit adds the 71 role pubkeys via the owner API.
