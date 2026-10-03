# Fleet Storage Tenants — Design

## Status: IMPLEMENTED

## Problem

Fleet roles (71 arbiter agent pubkeys) each get an individual anonymous-tier storage quota of 100 MB. They need to share one pooled quota instead.

## Approach

Three new database tables let multiple pubkeys pool their quota under a named tenant. The quota-check function was updated: when a pubkey belongs to a tenant, it reads the limit from the tenant's quota row and sums usage across all members of that tenant, instead of checking the individual limit.

A pubkey that is not a tenant member behaves exactly as before. A removed member falls back to its own individual quota.

## Schema

Three tables in the shared cloistr database:

1. **tenants** — a named pool with an owner pubkey (primary key: id)
2. **tenant_members** — which pubkeys belong to which tenant (composite key: tenant_id + pubkey, indexed on pubkey for the hot path)
3. **tenant_quotas** — per-tenant quota limits by type (composite key: tenant_id + quota_type_id)

## API surface

### Owner API (NIP-98 authenticated, owner-gated)

- Add a member to a tenant
- Remove a member from a tenant
- List members of a tenant

Only the tenant's owner pubkey can call these.

### Admin API (platform admin)

- Create a tenant (id + owner pubkey)
- Set a tenant's quota limit (tenant id + quota type + limit)

### CLI

Two admin commands: create a tenant, and set a tenant's quota.

## Quota resolution path

1. Look up whether the checked pubkey belongs to a tenant.
2. If yes, look up the tenant's quota limit for the requested type.
3. If a tenant quota row exists, sum usage across all members and return the pooled result.
4. If no tenant quota row exists for that type, fall through to the individual path.
5. The individual path is unchanged from the previous migration.

## Security

- Member management is gated on the tenant owner's pubkey (NIP-98 auth).
- Admin endpoints require the platform admin secret.
- Adding a member also ensures the pubkey has a users row, so service access checks pass.
