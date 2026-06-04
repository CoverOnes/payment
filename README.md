# payment

Handles the payments and escrow domain for the CoverOnes platform — creating escrow transactions, releasing funds on completion, and processing refunds, with a full audit trail.

## What it does

- Creates escrow-style transactions to hold funds between a payer and a payee
- Releases escrowed funds to the payee when work is approved
- Processes refunds back to the payer when a transaction is disputed or cancelled
- Maintains an immutable audit log for every state transition
- Enforces a strict KYC Tier 3 gate on all money-moving operations
- Publishes domain events via Redis for downstream notification and reconciliation

## Where it sits

payment is the financial authority in the platform. It sits behind the API gateway and is reachable only through it. Tier 3 verification is required before any funds can be moved.

```
browser → gateway → payment
```

## API (high level)

All routes live under `/v1`. Full request/response contract: [`conventions/http-api.md`](../conventions/http-api.md).

| Group | Endpoints | Min tier |
|-------|-----------|----------|
| Health | `GET /healthz`, `GET /readyz` | public |
| Transactions (read) | `GET /transactions/:id`, `GET /me/transactions` | identity |
| Transactions (write) | `POST /transactions`, `POST /transactions/:id/release`, `POST /transactions/:id/refund` | 3 |

## Tech

| Item | Detail |
|------|--------|
| Language | Go 1.25 |
| Framework | Gin |
| Database | PostgreSQL (pgx v5) |
| Cache / events | Redis (optional; falls back gracefully) |
| Migrations | golang-migrate, embedded SQL |

## Project structure

```
cmd/server/      — entrypoint; wires config, pool, services, router
internal/
  config/        — env-based config loading
  domain/        — core types (Transaction, AuditEntry)
  service/       — business logic and state transitions
  store/postgres — SQL queries, audit store, and transaction manager
  handler/       — HTTP handlers and router
  platform/      — shared middleware, health, logger
  events/        — Redis publisher and noop fallback
migrations/      — versioned SQL migration files
```

## Run locally

```sh
cd ../dev-stack && docker compose up -d
```

See [`dev-stack/README.md`](../dev-stack/README.md) for full setup instructions.

## Environment variables

| Variable | Purpose |
|----------|---------|
| `PAYMENT_PORT` | HTTP listen port (default 8084) |
| `PAYMENT_POSTGRES_DSN` | PostgreSQL connection string |
| `PAYMENT_DB_SCHEMA` | Postgres schema for multi-tenant DB sharing |
| `PAYMENT_DB_MAX_CONNS` | Max DB connection pool size |
| `PAYMENT_DB_MIN_CONNS` | Min DB connection pool size |
| `PAYMENT_REDIS_URL` | Redis URL (optional) |
| `PAYMENT_GATEWAY_HMAC_SECRET` | Shared secret for verifying gateway-origin requests |
| `PAYMENT_LOG_LEVEL` | Log level (debug / info / warn / error) |
