# CLAUDE.md - cloistr-address

**Unified address management for the @cloistr.xyz namespace**

## Service Information

- **Company:** Coldforge (Cloistr product line)
- **Type:** Backend Service (Go)
- **Purpose:** NIP-05 verification, Lightning Address, address namespace management
- **Status:** Development

**Parent Context:** See [cloistr CLAUDE.md](../../arbiter/cloistr/CLAUDE.md)

## What This Service Does

`cloistr-address` is the authoritative service for the `@cloistr.xyz` namespace. When a user purchases an address like `alice@cloistr.xyz`, they get:

1. **NIP-05 Verification** - Nostr identity verification via `/.well-known/nostr.json`
2. **Lightning Address** - Receive payments via `/.well-known/lnurlp/*`
3. **Email Integration** - Future integration with cloistr-email

## Architecture

```
cloistr-address
├── NIP-05 Handler      → /.well-known/nostr.json
├── Lightning Proxy     → /.well-known/lnurlp/*
├── Management API      → /api/v1/addresses/*
├── Purchase API        → /api/v1/purchase/*
└── Shared Database     → PostgreSQL (cloistr)
```

## Project Structure

```
cloistr-address/
├── cmd/address/main.go      # Entry point
├── internal/
│   ├── api/                 # HTTP handlers
│   │   ├── handler.go       # Router setup
│   │   ├── nip05.go         # NIP-05 endpoint
│   │   ├── lnurlp.go        # Lightning Address
│   │   ├── management.go    # Address CRUD
│   │   └── purchase.go      # Payment flow
│   ├── config/              # Configuration
│   ├── storage/             # PostgreSQL
│   ├── lightning/           # LND/proxy (future)
│   └── metrics/             # Prometheus
├── db/migrations/           # SQL migrations
├── Dockerfile
└── .gitlab-ci.yml
```

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `SERVER_ADDRESS` | `:8080` | HTTP listen address |
| `DB_HOST` | `localhost` | PostgreSQL host |
| `DB_PORT` | `5432` | PostgreSQL port |
| `DB_USER` | `cloistr` | PostgreSQL user |
| `DB_PASSWORD` | (required) | PostgreSQL password |
| `DB_NAME` | `cloistr` | PostgreSQL database |
| `DB_SSLMODE` | `require` | PostgreSQL SSL mode |
| `DOMAIN` | `cloistr.xyz` | Address domain |
| `DEFAULT_RELAYS` | `wss://relay.cloistr.xyz` | Default relay URLs |
| `LND_REST_HOST` | (optional) | LND REST API host |

## Database Tables

Uses unified platform schema (`cloistr` database):
- `addresses` - Username ↔ pubkey mapping
- `address_relays` - Per-user relay hints
- `address_lightning` - Lightning configuration (NEW)
- `username_tiers` - Pricing by length
- `payments` - Payment tracking

## API Endpoints

### Public (No Auth)

| Endpoint | Description |
|----------|-------------|
| `GET /.well-known/nostr.json?name=X` | NIP-05 verification |
| `GET /.well-known/lnurlp/:username` | Lightning Address config |
| `GET /.well-known/lnurlp/:username/callback` | Invoice generation |
| `GET /api/v1/addresses/check/:username` | Check availability |
| `GET /health` | Health check |
| `GET /metrics` | Prometheus metrics |

### Authenticated (NIP-98)

| Endpoint | Description |
|----------|-------------|
| `GET /api/v1/addresses/me` | Get my address |
| `PUT /api/v1/addresses/lightning` | Update Lightning config |
| `POST /api/v1/purchase/quote` | Get purchase quote |
| `POST /api/v1/purchase/invoice` | Create payment invoice |

## Lightning Address Modes

1. **Proxy** (implemented) - Forward to user's existing Lightning Address
2. **NWC** (future) - Nostr Wallet Connect for direct invoice generation
3. **Disabled** - Lightning Address not active

## Development

```bash
# Run locally
export DB_PASSWORD="your_password"
export DB_HOST="postgres-rw.db.coldforge.xyz"
go run ./cmd/address

# Build
go build -o cloistr-address ./cmd/address

# Test NIP-05
curl "http://localhost:8080/.well-known/nostr.json?name=alice"

# Test Lightning Address
curl "http://localhost:8080/.well-known/lnurlp/alice"
```

## Deployment

- **Registry:** `registry.aegis-hq.xyz/coldforge/cloistr-address`
- **Namespace:** `cloistr`
- **Atlas Role:** `cloistr-address`

## Implementation Status

### Phase 1: Core Service (Current)
- [x] Project scaffolding
- [x] NIP-05 endpoint
- [x] Lightning Address proxy
- [x] Availability check API
- [ ] Atlas deployment role
- [ ] Production deployment

### Phase 2: Payments
- [ ] LND REST integration
- [ ] Purchase flow
- [ ] NIP-98 authentication

### Phase 3: Integration
- [ ] cloistr-email integration
- [ ] Relay bundle credits
- [ ] Address transfers

### Phase 4: NWC
- [ ] Nostr Wallet Connect
- [ ] Coldforge hosted Lightning

---

**Last Updated:** 2026-04-07
