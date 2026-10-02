# WRITEUP — Seat Reservation at Scale

## The Atomic Decision

### Mechanism: `SELECT ... FOR UPDATE` + Advisory Locks in PostgreSQL

The core correctness guarantee lives in a single PostgreSQL transaction with two locking mechanisms:

1. **`pg_advisory_xact_lock(hash(show_id:user_id))`** — serializes all reservation attempts for the same user on the same show. This is what makes the per-user limit race-free: two concurrent requests from user-42 on the same show are forced to execute one after the other, so the second one always sees the seats the first one booked.

2. **`SELECT ... FROM seats WHERE show_id = $1 AND label = ANY($2) ORDER BY label FOR UPDATE`** — row-level locks on the exact seats being requested. Any concurrent request for an overlapping seat blocks until this transaction commits or rolls back. The winner commits and marks the seat `confirmed`; the loser's subsequent check sees `status != 'available'` and gets a clean 409.

### Why it's race-free

A naive read-then-write ("is A12 free? → yes → take it") fails because two readers can both see "available" before either writes. Our approach pushes the decision into the database:

- The `FOR UPDATE` lock means only one transaction can read a given seat row at a time.
- The `ORDER BY label` prevents deadlocks in multi-seat requests: if user-1 wants [A1, A3] and user-2 wants [A3, A1], both lock A1 first, then A3. Without deterministic ordering, they could deadlock (user-1 locks A1, user-2 locks A3, each waits for the other).
- The advisory lock per (show, user) prevents the per-user-limit race where two requests from the same user both see "0 seats booked" and both succeed.

### Multi-seat: All-or-Nothing

If a user requests `["A12", "A13"]` and A13 is taken, the entire request fails — no partial reservations. This is the safest model: the user knows exactly what they got (everything or nothing) and can retry with different seats. It's enforced by checking all locked rows have `status = 'available'` before any writes.

## Idempotency

### Storage

Idempotency keys are stored in the `reservations` table with a `UNIQUE` constraint on the `idempotency_key` column.

### Exactly-once enforcement

Inside the same transaction (after the advisory lock):

1. Check `SELECT ... FROM reservations WHERE idempotency_key = $1`
2. If found with matching seats → return the existing reservation (idempotent replay, 201)
3. If found with different seats → reject with 409 (`idempotency_conflict`)
4. If not found → proceed with reservation, `INSERT` with the key

The unique constraint is the safety net: if two identical requests race past step 1 (both see "not found"), the second `INSERT` hits the constraint and fails. The first request's transaction wins.

### Same-key-different-body

Explicitly rejected with 409 and code `idempotency_conflict`. The key is bound to the original seat set — you can't reuse it to book different seats.

## Holds & Expiry

The service uses **immediate confirmation with explicit cancel**:

- `POST /shows/{id}/reserve` → status = `confirmed` immediately
- `POST /reservations/{id}/cancel` → status = `cancelled`, seats released

No time-boxed holds — this simplifies the system and avoids the complexity of background expiry workers. Cancelled seats become immediately re-bookable. The cancel operation is guarded:
- Only the token's user can cancel their own reservation
- Cancel locks the reservation row `FOR UPDATE` to prevent races
- Seats are released only if they're still `confirmed` to this reservation (prevents resurrecting a re-booked seat)

**What I'd add with more time**: a hold model with TTL (status = `held`, `held_until` timestamp) and a background goroutine that expires holds. This is useful for payment flows where you hold seats during checkout.

## Consistency vs Availability

This system chooses **consistency over availability** (CP in CAP terms):

- Under a database partition (DB unreachable), the readiness endpoint returns 503, and all reservation requests fail with 500 rather than serving stale data.
- Under high contention, requests serialize correctly — they may be slower, but they're never wrong.
- The reconciliation invariant (`available + confirmed == total`) is maintained by the database's ACID guarantees, not by application-level counters.

This is the right trade-off for a ticketing system: selling a seat twice is far worse than being temporarily unavailable.

## Observability

### What I'd get paged for at 2am

1. **Readiness endpoint returning 503** — database is down. Everything else depends on this.
2. **`reservations_declined_total{reason="internal"}` spike** — 5xx errors mean bugs, not normal contention.
3. **`http_request_duration_seconds` p99 > 5s** — something is deadlocking or the connection pool is exhausted.
4. **`seats_available` going negative or `available + confirmed != total`** — reconciliation invariant violation. This should be impossible but would indicate a critical bug.
5. **Disk space on PostgreSQL** — if the reservations table grows unbounded.

### Metrics exposed

- `reservations_confirmed_total` — monotonically increasing counter
- `reservations_declined_total{reason}` — broken down by: `seat_taken`, `per_user_limit`, `idempotent_replay`, `idempotency_conflict`
- `seats_available{show_id}` / `seats_confirmed{show_id}` — gauges per show
- `http_request_duration_seconds{method, path, status}` — latency histogram
- `http_requests_total{method, path, status}` — request counter

### Structured logging

Every request gets a JSON log line with: `request_id`, `user_id`, `method`, `path`, `status`, `duration_ms`, `remote_addr`. The request ID is either passed via `X-Request-ID` header or auto-generated (UUID).

## AI Usage

**Directed vs Decided — honest accounting:**

- **Architecture decisions (mine)**: chose Go + PostgreSQL, advisory locks for per-user limit, `FOR UPDATE ORDER BY` for deadlock avoidance, all-or-nothing multi-seat, immediate confirm + explicit cancel model. These came from experience with concurrent systems.
- **AI-assisted (Claude Code)**: code generation for handlers, middleware, burst test script, Dockerfile, docker-compose, README structure. I directed what each component should do; AI wrote the boilerplate.
- **AI-decided (minimal)**: some naming conventions, Prometheus metric label choices, HTTP status code mapping for edge cases.

The correctness model (the hard part) was directed; the plumbing (the tedious part) was AI-generated.

## What I'd Do Next

1. **Hold + TTL model**: add `held` status with `held_until` timestamp, background worker to expire holds, and a `POST /reservations/{id}/confirm` endpoint for payment flow.
2. **Rate limiting**: per-IP and per-user rate limits to prevent abuse.
3. **Connection pooling tuning**: PgBouncer in front of PostgreSQL for production scale.
4. **Distributed tracing**: OpenTelemetry instrumentation for cross-service visibility.
5. **Load testing with k6/vegeta**: more sophisticated burst patterns including slow clients, connection drops, and partial failures.
6. **Database sharding**: partition seats by show_id for horizontal scale if needed.
7. **WebSocket for seat map**: real-time seat availability updates for a frontend.
8. **Audit log**: append-only log of all state transitions for compliance.
