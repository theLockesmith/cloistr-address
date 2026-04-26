# CLAUDE.md - cloistr-me

**Nostr identity service for the @cloistr.xyz namespace**

## Service Information

- **Company:** Coldforge (Cloistr product line)
- **Type:** Backend Service (Go)
- **Purpose:** NIP-05 verification, Lightning Address, identity management
- **Status:** Production
- **Primary Domain:** https://me.cloistr.xyz

**Parent Context:** See [cloistr CLAUDE.md](../../arbiter/cloistr/CLAUDE.md)

## What This Service Does

`cloistr-me` is the identity service for the Cloistr platform. When a user purchases an address like `alice@cloistr.xyz`, they get:

1. **NIP-05 Verification** - Nostr identity verification via `/.well-known/nostr.json`
2. **Lightning Address** - Receive payments via `/.well-known/lnurlp/*`
3. **Email Integration** - Future integration with cloistr-email

## Architecture

```
cloistr-me
├── NIP-05 Handler      → /.well-known/nostr.json
├── Lightning Proxy     → /.well-known/lnurlp/*
├── Management API      → /api/v1/addresses/*
├── Purchase API        → /api/v1/purchase/*
├── BTCPay Webhook      → /api/v1/payment/webhook
└── Shared Database     → PostgreSQL (cloistr)
```

## Project Structure

```
cloistr-me/
├── cmd/address/main.go      # Entry point
├── internal/
│   ├── api/                 # HTTP handlers
│   │   ├── handler.go       # Router setup
│   │   ├── nip05.go         # NIP-05 endpoint
│   │   ├── lnurlp.go        # Lightning Address (proxy, NWC, hosted modes)
│   │   ├── management.go    # Address CRUD
│   │   ├── purchase.go      # Payment flow
│   │   ├── webhook.go       # BTCPay webhook
│   │   └── internal.go      # Internal service-to-service API
│   ├── auth/                # NIP-98 authentication
│   ├── btcpay/              # BTCPay Server client
│   ├── config/              # Configuration
│   ├── crypto/              # AES-256-GCM encryption for secrets at rest
│   ├── nwc/                 # NIP-47 Nostr Wallet Connect client
│   ├── storage/             # PostgreSQL
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
| `BTCPAY_URL` | (required) | BTCPay Server URL |
| `BTCPAY_API_KEY` | (required) | BTCPay API key |
| `BTCPAY_STORE_ID` | (required) | BTCPay store ID |
| `BTCPAY_WEBHOOK_SECRET` | (required) | BTCPay webhook secret |
| `INTERNAL_API_SECRET` | (optional) | Shared secret for internal service calls |
| `NWC_ENCRYPTION_KEY` | (optional) | 32-byte hex key for encrypting NWC secrets at rest |

## Database Tables

Uses unified platform schema (`cloistr` database):
- `addresses` - Username ↔ pubkey mapping
- `address_relays` - Per-user relay hints
- `address_lightning` - Lightning configuration
- `username_tiers` - Pricing by length
- `payments` - Payment tracking
- `pubkey_credits` - Withdrawable credits per pubkey

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
| `POST /api/v1/addresses/transfer` | Transfer address to another pubkey |
| `POST /api/v1/purchase/quote` | Get purchase quote |
| `POST /api/v1/purchase/invoice` | Create payment invoice |
| `GET /api/v1/credits` | Get credit balance |
| `POST /api/v1/credits/withdraw` | Withdraw credits to Lightning address |

### Internal (Service-to-Service)

| Endpoint | Description |
|----------|-------------|
| `POST /api/v1/webhook/payment` | BTCPay payment webhook |
| `POST /internal/v1/credits/grant` | Grant credits (relay bundle, promo, etc.) |
| `GET /internal/v1/addresses/verify` | Verify pubkey owns username (for cloistr-email) |

## Lightning Address Modes

1. **Proxy** - Forward to user's existing Lightning Address
2. **NWC** - Nostr Wallet Connect (NIP-47) for direct invoice generation via user's wallet
3. **Hosted** - Coldforge-operated Lightning wallet (coming soon)
4. **Disabled** - Lightning Address not active

## Development

```bash
# Run locally
export DB_PASSWORD="your_password"
export DB_HOST="postgres-rw.db.coldforge.xyz"
go run ./cmd/address

# Build
go build -o cloistr-me ./cmd/address

# Test NIP-05
curl "http://localhost:8080/.well-known/nostr.json?name=alice"

# Test Lightning Address
curl "http://localhost:8080/.well-known/lnurlp/alice"
```

## Deployment

- **Registry:** `registry.aegis-hq.xyz/coldforge/cloistr-me`
- **Namespace:** `cloistr`
- **Atlas Role:** `cloistr-me`
- **Domain:** `me.cloistr.xyz` (also accessible via `cloistr.xyz`)

## Implementation Status

### Phase 1: Core Service ✓
- [x] Project scaffolding
- [x] NIP-05 endpoint
- [x] Lightning Address proxy
- [x] Availability check API
- [x] Atlas deployment role
- [x] Production deployment

### Phase 2: Payments ✓
- [x] BTCPay Server integration
- [x] Purchase flow with race-based claiming
- [x] NIP-98 authentication
- [x] Withdrawable credits system

### Phase 3: Integration ✓
- [x] cloistr-email integration (internal API: GET /internal/v1/addresses/verify)
- [x] Relay bundle credits (internal API: POST /internal/v1/credits/grant)
- [x] Address transfers (POST /api/v1/addresses/transfer with 7-day cooldown)

### Phase 4: NWC ✓
- [x] Nostr Wallet Connect (NIP-47 with NIP-44/NIP-04 encryption)
- [x] NWC secrets encrypted at rest (AES-256-GCM)
- [x] Coldforge hosted Lightning (stubbed - returns "coming soon")

---

**Last Updated:** 2026-04-25
