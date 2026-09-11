# Wealthfolio Connect Self-hosted

> A self-hosted companion server for the
> [Wealthfolio](https://github.com/wealthfolio/wealthfolio) **web edition**
> (the self-hosted Docker image that ships out of `apps/server`),
> for users who can't or won't use a hosted sync service.

> **Scope.** This server targets the **web edition** of Wealthfolio — the
> Docker image you self-host on your own box, configured via
> `CONNECT_AUTH_URL` / `CONNECT_AUTH_PUBLISHABLE_KEY` / `CONNECT_API_URL`.
> It is **not** intended for the Tauri desktop app, which talks to the same
> HTTP contract but seeds its session through the local OS keyring.

## 👉 Please use the official Wealthfolio Connect if you can

The [Wealthfolio](https://wealthfolio.app) web edition is built and
maintained by [@afadil](https://github.com/afadil) and contributors, who
have given the entire app away for free under AGPL-3.0. The
**Wealthfolio Connect** hosted service is how that work gets paid for.

If the official Connect fits your needs — **please subscribe to it**.
It's the right thing to do, it keeps the upstream project healthy, and
whatever you pay there is almost certainly less than the value
Wealthfolio gives you back. Open source survives on people choosing to
pay when they don't strictly have to.

This project exists for the (small) set of users for whom hosted Connect
is genuinely not an option:

- **Strict data-residency or compliance rules** that forbid sending
  broker credentials or holdings off-prem.
- **Regions / payment methods** the official Connect doesn't serve.
- **Hobbyist self-hosters** who run their own infra for everything as a
  matter of principle.

If none of those describe you, close this tab and go to
[wealthfolio.app](https://wealthfolio.app). 🙏

## What this is

A single Go binary that speaks the same HTTP contract the Wealthfolio
web edition already uses to talk to its sync backend, so your self-hosted
Wealthfolio web instance can point at a server you control instead of the
hosted Connect. Data is pulled directly from each broker by this binary —
no third-party data aggregator sits between you and the exchange:

- **Futu Securities** — TCP/protobuf to a local **Futu OpenD** daemon (`hurisheng/go-futu-api`)
- **Interactive Brokers** — socket protocol to a local **IB Gateway / TWS** (`scmhub/ibapi`)
- **Binance Spot** — REST API (`adshao/go-binance/v2`)
- **OKX CEX** — signed v5 REST API (HMAC-SHA256)
- **OKX Web3 / DEX** — signed v6 OnchainOS API for on-chain wallet balances + history
- **Bitget Spot** — signed v2 REST API
- **Hyperliquid** — public `/info` endpoint, wallet-address only (read-only)
- **TON** — TON Center API v3 for native + Jetton balances and transfer history

All data is normalised into the Wealthfolio API shape and persisted in
PostgreSQL on your own infrastructure.

## Relationship with upstream Wealthfolio

This is an **independent, unaffiliated** project — not endorsed by,
sponsored by or supported by the Wealthfolio team. The HTTP contract this
server implements is part of the upstream open-source codebase, so this
is interoperation against a *published* protocol, not reverse
engineering. No upstream code is copied or linked into this binary.

"Wealthfolio" and "Wealthfolio Connect" are trademarks of their
respective owners and are used here solely to describe compatibility.

---

## Architecture

```
┌──────────────┐                ┌──────────────────────────────┐
│ Wealthfolio  │  HTTPS / JWT   │  wealthfolio-connect-open    │
│ Web edition  │ ──────────────▶│  (this repo, single Go bin)  │
│ (self-host)  │                │                              │
└──────────────┘                │                              │
                                │  ┌────────────────────────────┐  │
                                │  │ internal/interfaces/ (HTTP)│  │
                                │  ├────────────────────────────┤  │
                                │  │ internal/application/      │  │
                                │  ├────────────────────────────┤  │
                                │  │ internal/domain/ (entities)│  │
                                │  ├────────────────────────────┤  │
                                │  │ internal/infrastructure/   │  │
                                │  │  ├── persistence (PG)      │  │
                                │  │  ├── clients/              │  │
                                │  │  │   ├ futu, ibkr          │  │
                                │  │  │   ├ binance, okx        │  │
                                │  │  │   ├ bitget, hyperliquid │  │
                                │  │  │   └ cexcommon (shared)  │  │
                                │  │  └── auth (JWT)            │  │
                                │  └────────────────────────────┘  │
                                └────────┬─────────┬───────────┘
                                         │         │
                            ┌────────────▼──┐  ┌───▼────────────┐
                            │ PostgreSQL    │  │ Upstream       │
                            │ (single store)│  │ brokers/chains │
                            └───────────────┘  └────────────────┘
```

The architecture follows **Domain-Driven Design** (see [AGENTS.md](./AGENTS.md))
and uses **uber-go/fx** for dependency injection.

---

## Prerequisites

- **Go 1.22+**
- **PostgreSQL 14+**
- **Docker** (for containerised deployment)
- **Futu OpenD** running locally — only needed if you enable the Futu integration ([download](https://www.futunn.com/en/download/openAPI))
- **IB Gateway** or **TWS** running locally — only needed if you enable the IBKR integration
- (Optional) **mockgen** for regenerating gomock mocks: `go install go.uber.org/mock/mockgen@latest`
- (Optional) **golangci-lint** for linting: see [installation guide](https://golangci-lint.run/usage/install/).

---

## Quick Start

```bash
# 1. Clone
git clone https://github.com/your-org/wealthfolio-connect-open.git
cd wealthfolio-connect-open

# 2. Configure environment
export DATABASE_URL="postgres://user:pass@localhost:5432/wealthfolio?sslmode=disable"
export JWT_SECRET="change-me-to-a-long-random-string"
export CONNECT_AUTH_PUBLISHABLE_KEY="your-publishable-key"

# 3. Run (migrations execute automatically on startup)
go run ./cmd/server

# 4. Health check
curl http://localhost:8080/healthz
```

### Point the Wealthfolio web edition at this server

The Wealthfolio web container reads its Connect endpoints from environment
variables at runtime (the frontend pulls them through `get_connect_config`).
Set the following on the **Wealthfolio web** container — not on this one —
and restart it:

```bash
CONNECT_AUTH_URL=https://connect.your-domain.example         # base URL of THIS server
CONNECT_AUTH_PUBLISHABLE_KEY=your-publishable-key            # must match the value below
CONNECT_API_URL=https://connect.your-domain.example          # usually the same host
# Optional: only needed if you wire the OAuth-style callback flow.
# CONNECT_OAUTH_CALLBACK_URL=https://wealthfolio.your-domain.example/auth/callback
```

Then, in the Wealthfolio web UI, sign in to Connect with any email that is on
this server's `ALLOWED_EMAILS` allow-list and any 6-digit OTP (or `STATIC_OTP`
if configured). The session JWT and refresh token are persisted by the
Wealthfolio backend and reused across restarts — there is no separate "seed"
step, the OS-keyring dance only applies to the Tauri desktop build.

---

## Environment Variables

### Required

| Name                            | Description                                                                  |
| ------------------------------- | ---------------------------------------------------------------------------- |
| `DATABASE_URL`                  | PostgreSQL connection string (pgx format).                                   |
| `JWT_SECRET`                    | HS256 signing secret for access tokens.                                      |
| `CONNECT_AUTH_PUBLISHABLE_KEY`  | Expected `apikey` header on `/auth/v1/*`.                                    |
| `ALLOWED_EMAILS`                | Comma-separated email allow-list for the synthetic OTP login. See below.    |

### Optional — System

| Name                    | Default                   | Description                                                                                  |
| ----------------------- | ------------------------- | -------------------------------------------------------------------------------------------- |
| `SERVER_PORT`           | `8080`                    | HTTP listen port.                                                                            |
| `LOG_LEVEL`             | `info`                    | `debug` / `info` / `warn` / `error`.                                                         |
| `CORS_ORIGINS`          | `*`                       | Comma-separated list of allowed origins.                                                     |
| `SYNC_INTERVAL_MINUTES` | `240`                     | Periodic sync interval.                                                                      |
| `STATIC_TOKEN_MODE`     | `false`                   | If `true`, always returns the same JWT.                                                      |
| `TOKEN_TTL_SECONDS`     | `3600`                    | Access token lifetime.                                                                       |
| `STATIC_OTP`            | —                         | Optional fixed OTP code accepted by `/auth/v1/verify` in addition to any 6-digit numeric code. |

Every broker integration is **opt-in** — leave its credentials empty and the
corresponding client is silently skipped at startup. You only need to supply
the variables for the brokers you actually want to sync.

### Futu Securities (TCP to local OpenD)

Futu OpenD must already be running on the same host (or reachable network)
as this server. The server connects via TCP and signs trade requests with
your OpenD trading password.

| Name                 | Default      | Description                                                                |
| -------------------- | ------------ | -------------------------------------------------------------------------- |
| `FUTU_HOST`          | `127.0.0.1`  | OpenD host. Empty disables Futu.                                           |
| `FUTU_PORT`          | `11111`      | OpenD TCP port.                                                            |
| `FUTU_TRADE_PASSWORD`| —            | The trading password ("交易密码 / 交易密码 MD5") configured in OpenD.       |
| `FUTU_CONNECTION_ID` | `wealthfolio`| Logical connection identifier surfaced in the snapshot.                    |

### Interactive Brokers (socket to local IB Gateway / TWS)

Run **IB Gateway** (recommended) or **TWS** locally with API socket access
enabled. Allow this server's IP in the gateway's *Trusted IPs* list.

| Name              | Default     | Description                                                            |
| ----------------- | ----------- | ---------------------------------------------------------------------- |
| `IBKR_HOST`       | `127.0.0.1` | IB Gateway / TWS host. Empty disables IBKR.                            |
| `IBKR_PORT`       | `4001`      | `4001` for live IB Gateway, `4002` paper, `7496` TWS, `7497` TWS paper.|
| `IBKR_CLIENT_ID`  | `1`         | Any unique integer — must not clash with other API clients.            |
| `IBKR_ACCOUNT_ID` | —           | Optional account filter (e.g. `U1234567`); empty pulls every account.  |

> **Operational caveats.** IB Gateway is the weakest link in any IBKR
> integration: sessions die after ~24h of uptime, the daily reset window
> forces re-login, and Two-Factor Authentication can ask for a phone tap at
> any reconnect. The current client treats every `Fetch()` as a fresh
> attempt and surfaces the underlying error — it does not auto-restart the
> gateway. Recommended deployment hardening:
>
> - Run IB Gateway under a process supervisor (`systemd`, `supervisord`,
>   or `ibc` from [IbcAlpha/IBC](https://github.com/IbcAlpha/IBC)) that
>   restarts the daemon nightly before the daily reset.
> - Use IBC + a dedicated *paper* + *live* user pair when possible; paper
>   sessions do not enforce 2FA.
> - Monitor `/healthz` and the sync logs for repeated `IBKR_*` errors;
>   alert when consecutive failures exceed `SYNC_INTERVAL_MINUTES`.
> - If 2FA prompts become disruptive, evaluate IBKR's *Read-only login*
>   option, which suppresses the second factor for market-data + portfolio
>   queries.

### Binance Spot (REST)

Create a **read-only** API key (Spot account permissions are sufficient).

| Name                    | Description                                                                                                  |
| ----------------------- | ------------------------------------------------------------------------------------------------------------ |
| `BINANCE_API_KEY`       | Binance API key.                                                                                             |
| `BINANCE_API_SECRET`    | Binance secret.                                                                                              |
| `BINANCE_TRADE_SYMBOLS` | Optional comma-separated exact Spot pairs (e.g. `BTCUSDT,ETHBTC`). Empty uses held `*USDT` pairs plus pairs with saved trades. |

Spot history uses `GET /api/v3/myTrades` in small bounded batches (at most
20 history requests per sync, rotating fairly across pairs). Trade cursors
are derived from persisted activities, so a failed write is retried instead
of skipped. All requests share a local 600 request-weight/minute guard
(10% of Binance's 6000 limit) with backoff on `Retry-After`/`-1003`
responses, so one sync can no longer exhaust the IP budget.

### OKX CEX (signed v5 REST)

Create a **read-only** API key on OKX (no trading / withdrawal permissions
required).

| Name             | Description           |
| ---------------- | --------------------- |
| `OKX_API_KEY`    | OKX API key.          |
| `OKX_API_SECRET` | OKX API secret.       |
| `OKX_PASSPHRASE` | OKX API passphrase.   |

### OKX Web3 / DEX (signed v6 REST + wallet list)

The Web3 client uses a separate set of OKX credentials with **DEX API**
permissions enabled, and aggregates balances across the wallets you list.
Transfer quantities round to cents so in/out legs net cleanly; transfers
always carry an explicit `mapping_metadata.flow.is_external` flag (`true`
for counterparties outside the configured wallets).

| Name                  | Description                                                                                              |
| --------------------- | -------------------------------------------------------------------------------------------------------- |
| `OKX_WEB3_API_KEY`    | OKX Web3 API key (DEX-enabled).                                                                          |
| `OKX_WEB3_API_SECRET` | OKX Web3 API secret.                                                                                     |
| `OKX_WEB3_PASSPHRASE` | OKX Web3 passphrase.                                                                                     |
| `DEFI_WALLETS`        | JSON array of wallets. Example: `[{"address":"0xabc...","chains":["1","56","42161"],"label":"main"}]`. |

`chains` are OKX chain indexes — see [OKX docs](https://www.okx.com/web3/build/docs/waas/dex-supported-chains)
(e.g. `1` = Ethereum, `56` = BSC, `42161` = Arbitrum, `137` = Polygon, `10` = Optimism, `8453` = Base).
Omit `chains` for EVM wallets to auto-discover funded chains (cached 15 minutes).

Transaction history uses `/api/v6/dex/post-transaction/transactions-by-address`
with `limit=20` on every request — the live API rejects larger limits for
multi-chain queries (`81001`), despite what the docs suggest. History
failures leave balances intact and only mark transaction sync incomplete.

### Bitget Spot (signed v2 REST)

| Name                | Description              |
| ------------------- | ------------------------ |
| `BITGET_API_KEY`    | Bitget API key.          |
| `BITGET_API_SECRET` | Bitget API secret.       |
| `BITGET_PASSPHRASE` | Bitget API passphrase.   |

### Hyperliquid (public `/info`, wallet-only)

No API key — Hyperliquid's `/info` endpoint is public. Just point it at the
wallet whose perpetuals + spot balances you want tracked.

| Name                 | Description                                  |
| -------------------- | -------------------------------------------- |
| `HYPERLIQUID_WALLET` | EVM-style wallet address (`0x…`). Read-only. |

### TON (TON Center API v3, wallet list)

Tracks native TON plus all Jetton balances and the full transfer history
for a few explicitly configured wallets. Balances come from `/accountStates`
and `/jetton/wallets` (with symbol/decimals from `/jetton/masters`); history
comes from the decoded `/actions` API (`limit=1000`, offset pages until a
short page), grouped by `trace_id`. Only successful actions classify:
`jetton_swap` → swap, `stake_deposit` → staking (amount, tokens minted,
asset), every other token movement → `TRANSFER_IN`/`TRANSFER_OUT`.
Derived legs take precedence over primitive legs in the same trace, so swaps
and stakes never double count. Swap/stake pairs emit the SELL leg first with
the BUY leg one second later, keeping causal order in date-sorted views.
Multi-leg traces and single wallet outflows are verified against raw message
trees (`/traces?tx_hash=…&include_actions=true` plus
`/actions?tx_hash=…&include_transactions=true`, capped per wallet). Vault
deposits pair by causality, never by amount or timestamp: the vault echoes
the deposit `query_id` in its mint message (strongest link), otherwise a
mint credited to the user's jetton wallet (or attributed via
`response_address`) from protocol intake pairs structurally, and burns pair
with the redeemed transfer the same way. Pairs emit USD-routed
`STAKE_SELL`/`STAKE_BUY` conversions (`$TOKEN_IN → USD → $TOKEN_OUT`);
async receipts settling in another trace stay separate. Outflows proven to
enter a protocol are labeled as such; complete trees missing a claimed
protocol party downgrade interpreted legs to primitives, and verification
failures keep interpreted legs with the valued basis fallback.
`429`/indexer timeouts are retried with backoff honoring `Retry-After`.

Swaps and stakes are routed through USD on both legs: stablecoin sides convert
1:1, other sides use TonAPI's chart history at the transaction time (wide
window per token, cached; TON/GRAM and tickers like USDT directly, other
Jettons by master address), with CoinGecko history as fallback (stablecoins
and TON checked first there too). Pair prices carry a tiny role bias (sell up,
buy down, exact $1 legs exempt) so the amounts the app derives as quantity ×
unit_price can never leave a negative cash remainder. TonAPI retention reaches roughly 180 days,
CoinGecko 365 — older legs stay unvalued rather than invented, and throttled
lookups fail fast into per-token/per-day negative caches instead of stalling
the sync. Every token movement is a `TRANSFER_IN`/`TRANSFER_OUT` leg with
units and value rounded to cents so in/out legs net cleanly (swap/stake
`SELL`/`BUY` legs keep exact chain values); legs always carry an explicit `mapping_metadata.flow.is_external` flag
(`true` when the counterparty is none of the configured wallets, `false`
for transfers between tracked wallets so the app validates the pair),
swaps and stakes add `SELL`/`BUY` legs with the buy leg one second after the
sell leg so date-sorted views keep causal order. Single legs carry cost basis
(Price/Amount) when their token has a feed. Vault shares minted without any
transfer action additionally emit a synthetic `BUY` attributed from the
master-bound deposits — but only when history shows no inbound leg of that
token at all. Everything stays marked for review.

| Name               | Description                                                                                                     |
| ------------------ | --------------------------------------------------------------------------------------------------------------- |
| `TONCENTER_API_KEY` | TON Center API key, sent as `X-API-Key` (server-side only). Empty falls back to the 1 RPS anonymous quota. |
| `TON_WALLETS`       | Comma-separated TON addresses in any form (e.g. `UQ…` or `0:…`). Case is preserved — TON addresses are case-sensitive. |
| `REDIS_ADDR`        | Optional `host:port` for short-lived market data (latest prices ~1h TTL, today's candles). Empty disables Redis; durable price history always uses Postgres. |

---

## API Endpoints

| Method | Path                                                          | Description                                              |
| ------ | ------------------------------------------------------------- | -------------------------------------------------------- |
| POST   | `/auth/v1/otp`                                                | Request a magic-link OTP. No-op (no email sent).         |
| POST   | `/auth/v1/verify`                                             | Exchange `{email, token}` for an access + refresh token. |
| POST   | `/auth/v1/token?grant_type=refresh_token`                     | Exchange refresh token for JWT.                          |
| POST   | `/auth/v1/logout`                                             | Best-effort session invalidation.                        |
| GET    | `/auth/v1/user`                                               | Supabase-shaped current user.                            |
| GET    | `/api/v1/user/me`                                             | Current user + subscription info.                        |
| GET    | `/api/v1/subscription/plans`                                  | Available subscription plans. **No auth required.**      |
| GET    | `/api/v1/sync/brokerage/connections`                          | All broker connections.                                  |
| GET    | `/api/v1/sync/brokerage/accounts`                             | All broker accounts.                                     |
| PATCH  | `/api/v1/sync/brokerage/accounts/{id}`                        | Toggle `sync_enabled` for one account.                   |
| GET    | `/api/v1/sync/brokerage/accounts/{id}/activities`             | Paginated activities.                                    |
| GET    | `/api/v1/sync/brokerage/accounts/{id}/holdings`               | Latest holdings snapshot.                                |
| POST   | `/api/v1/connect/session`                                     | Seed/refresh the local sync session (see Quick Start).   |
| GET    | `/healthz`                                                    | Liveness + DB ping.                                      |
| GET    | `/readyz`                                                     | Readiness (post-migration).                              |

See [API.md](./API.md) for full request/response schemas.

---

## Authentication

This server impersonates the subset of Supabase Auth that Wealthfolio's
`wealthfolio-connect` feature talks to. There is **no real email provider,
no real OTP storage, and no per-user database**: gating is done by the
`apikey` header (`CONNECT_AUTH_PUBLISHABLE_KEY`) plus the `ALLOWED_EMAILS`
allow-list. The server is expected to be reachable only over a trusted
network (VPN, reverse proxy, IP allow-list, ...).

### Login flow

```
┌─ Wealthfolio web frontend ────────────────┐
│ 1. POST /auth/v1/otp   { email }          │ ── apikey-gated
│    server: email ∈ ALLOWED_EMAILS?        │     → 403 if not
│    server: 200, no email ever sent        │
│                                           │
│ 2. user types any 6-digit code (or the    │
│    configured STATIC_OTP)                 │
│                                           │
│ 3. POST /auth/v1/verify { email, token }  │ ── apikey-gated
│    server: validates code shape           │     → 400 otp_invalid
│    server: validates email allow-list     │     → 403 if not
│    server: signs JWT (sub = sha256(email) │
│            in UUID format) + refresh tk   │
│    → returns Supabase-shaped session JSON │
│                                           │
│ 4. session is persisted by the Wealthfolio│
│    web backend (encrypted via             │
│    WF_SECRET_KEY) and reused across       │
│    restarts.                              │
└───────────────────────────────────────────┘
```

### OTP policy

`/auth/v1/verify` accepts a token if **either** of the following holds:

- the token matches `^[0-9]{6}$` (any 6-digit numeric code), **or**
- `STATIC_OTP` is set and the token equals that value (constant-time
  comparison).

There is intentionally no per-email OTP storage, no expiry, no rate limit
on this endpoint — the apikey + allow-list are the gates that matter. If
you need stronger guarantees, put the server behind a reverse proxy that
rate-limits `/auth/v1/*`.

### Subject derivation

The JWT `sub` claim is `sha256(lowercased email)` reformatted as a UUID
v4 string. This keeps the raw email out of access logs / downstream
services while remaining stable across token refreshes for the same
address, and parses cleanly as a UUID — which `supabase-js` requires.

---

## Development

```bash
# Run tests with race detector and coverage
go test ./... -race -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1

# Lint
golangci-lint run

# Vet
go vet ./...

# Regenerate mocks
go generate ./...
```

Coverage threshold is **≥ 90%** — CI will fail below that.

---

## Docker

```bash
# Build the multi-stage image
docker build -t wealthfolio-connect-open:latest .

# Run
docker run --rm -p 8080:8080 \
  -e DATABASE_URL="postgres://..." \
  -e JWT_SECRET="..." \
  -e CONNECT_AUTH_PUBLISHABLE_KEY="..." \
  wealthfolio-connect-open:latest
```

The container exposes `SERVER_PORT` (default `8080`) and provides
`/healthz` + `/readyz` for Kubernetes probes.

---

## CI/CD

GitHub Actions workflow at [`.github/workflows/ci.yml`](./.github/workflows/ci.yml):

- Runs on every PR and push to `main`.
- Steps: `go vet` → `golangci-lint` → `go test -race -coverprofile` → coverage gate (≥ 90%).
- A PostgreSQL service container is provisioned for integration tests.
- On `main`, the multi-stage Docker image is built and pushed using the
  `REGISTRY_URL` / `REGISTRY_USERNAME` / `REGISTRY_PASSWORD` repo secrets.

---

## Project Structure

```
wealthfolio-connect-open/
├── cmd/
│   └── server/                 # main.go: fx.New() composition root
├── internal/                   # All non-main code (Go internal visibility)
│   ├── domain/                 # Pure business model
│   │   ├── auth/
│   │   ├── brokerage/
│   │   ├── sync/
│   │   └── repository/         # Repository interfaces
│   ├── application/            # Use cases / orchestration
│   │   ├── auth/
│   │   ├── brokerage/
│   │   └── sync/
│   ├── infrastructure/         # Adapters
│   │   ├── config/
│   │   ├── database/
│   │   ├── persistence/        # PG repositories
│   │   ├── auth/               # JWT signing
│   │   ├── logging/
│   │   └── clients/
│   │       ├── futu/           # TCP → local OpenD
│   │       ├── ibkr/           # socket → local IB Gateway / TWS
│   │       ├── binance/        # Spot REST
│   │       ├── okx/            # CEX (signed v5) + Web3/DEX (signed v6)
│   │       ├── bitget/         # Spot REST (signed v2)
│   │       ├── hyperliquid/    # public /info
│   │       └── cexcommon/      # shared snapshot translation
│   └── interfaces/
│       └── http/
│           ├── handlers/
│           └── middleware/
├── deploy/                     # Kubernetes manifests
├── .github/workflows/          # CI/CD
├── AGENTS.md                   # Conventions for AI agents and contributors
├── API.md                      # Full HTTP API reference
├── Dockerfile
└── README.md
```

---

## License

**AGPL-3.0-or-later** — same licence the upstream Wealthfolio project uses.
See [LICENSE](./LICENSE).
