# Seat Reservation Service

High-concurrency seat reservation API that guarantees no double-sells, enforces per-user limits, and provides idempotent reservations — even under 20,000+ concurrent requests.

**Tech stack**: Go, PostgreSQL, Prometheus metrics, Docker

## Quick Start

### Docker Compose (recommended)

```bash
docker compose up --build
```

Service runs at `http://localhost:8080`. PostgreSQL starts automatically with schema applied.

### Manual

1. Start PostgreSQL and create a `seatreservation` database
2. Apply the schema: `psql -d seatreservation -f init.sql`
3. Set env: `export DATABASE_URL=postgres://user:pass@localhost:5432/seatreservation?sslmode=disable`
4. Run: `go run .`

## API Reference

### Authentication

All endpoints (except health and `/auth/token`) require a Bearer token.

**Generate a token** (test utility):
```bash
curl -X POST http://localhost:8080/auth/token \
  -H "Content-Type: application/json" \
  -d '{"user_id": "alice", "role": "admin"}'
```

### Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | `/auth/token` | None | Generate auth token (testing) |
| POST | `/shows` | Admin | Create a show |
| GET | `/shows/{id}` | User | Get show with seat status |
| POST | `/shows/{id}/reserve` | User | Reserve seat(s) |
| POST | `/reservations/{id}/cancel` | User (owner) | Cancel a reservation |
| GET | `/health/live` | None | Liveness check |
| GET | `/health/ready` | None | Readiness check (verifies DB) |
| GET | `/metrics` | None | Prometheus metrics |

### Create Show

```bash
curl -X POST http://localhost:8080/shows \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "friday-night", "seats": ["A1","A2","A3"], "price_paise": 25000}'
```

### Reserve Seats

```bash
curl -X POST http://localhost:8080/shows/$SHOW_ID/reserve \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"seats": ["A1"], "idempotency_key": "unique-key-123"}'
```

**Behaviour**:
- **All-or-nothing**: if requesting multiple seats and any is taken, the entire request is declined (409)
- **Idempotency**: same key + same seats = returns original reservation; same key + different seats = 409
- **Per-user limit**: default 4 seats per user per show, enforced under concurrency

### Cancel Reservation

```bash
curl -X POST http://localhost:8080/reservations/$RES_ID/cancel \
  -H "Authorization: Bearer $USER_TOKEN"
```

Only the owner (token's user) can cancel their own reservation. Released seats become immediately re-bookable.

## Burst Test

Run the on-sale stampede simulation:

```bash
# Against local
./burst.sh http://localhost:8080

# Against deployed URL
./burst.sh https://your-app.onrender.com
```

This fires ~3000+ concurrent requests including:
- 500 users fighting over 5 hot seats
- Idempotent retries (same key, same seats)
- Idempotent conflicts (same key, different seats)
- Per-user limit stress tests

Prints outcome distribution and reconciliation check.

## Observability

### Metrics (Prometheus)

`GET /metrics` exposes:
- `reservations_confirmed_total` — counter
- `reservations_declined_total{reason}` — counter by decline reason
- `seats_available{show_id}` — gauge
- `seats_confirmed{show_id}` — gauge
- `http_request_duration_seconds` — histogram
- `http_requests_total` — counter
- `idempotent_replays_total` — counter

### Health Checks

- `GET /health/live` — always returns 200 if process is running
- `GET /health/ready` — returns 200 only if database is reachable, 503 otherwise

### Structured Logs

JSON-structured logs with request ID, user ID, method, path, status, and duration for every request.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/seatreservation?sslmode=disable` | PostgreSQL connection string |
| `PORT` | `8080` | HTTP server port |

## Architecture

See [WRITEUP.md](WRITEUP.md) for detailed design decisions.
