# Randevum (Go)

REST API for booking appointments at small businesses such as barbers, salons and clinics. Owners publish their services and weekly working hours; customers see free time slots and book them.

This is a Go version of [randevum-api](https://github.com/suderdagi/randevum-api), which I first wrote in Node.js and TypeScript. I rewrote it to learn Go on a problem I already understood, so I could focus on how Go does things differently rather than on the domain.

**Stack:** Go 1.26, standard library `net/http` router, PostgreSQL 16 with [pgx](https://github.com/jackc/pgx), JWT, bcrypt, Docker, GitHub Actions

## Features

- **Accounts:** customers and business owners; access tokens (15 min) and refresh tokens with rotation and reuse detection
- **Businesses:** create, search by city, category and name with pagination, set weekly working hours, add services
- **Availability:** free slots for a service on a day, from working hours and existing bookings, correct across daylight saving changes
- **Booking without double bookings**, even when requests arrive at the same moment
- **Appointment lifecycle:** `pending → confirmed → completed`, or `cancelled`; customers can only cancel before the business's deadline
- Consistent JSON errors, request logging, graceful shutdown, health check

## Quick start

```bash
docker compose up --build
curl localhost:4000/health
```

Without Docker (needs PostgreSQL):

```bash
cp .env.example .env    # then edit DATABASE_URL and JWT_SECRET
export $(cat .env | xargs)
go run ./cmd/api        # migrations run on startup
```

## Example

```bash
# Register as a customer
curl -X POST localhost:4000/api/v1/auth/register \
  -d '{"email":"sude@example.com","password":"password123","fullName":"Sude","role":"customer"}'

# Free slots for service 1 of business 1
curl 'localhost:4000/api/v1/businesses/1/availability?serviceId=1&date=2026-12-01'
# {"date":"2026-12-01","timezone":"Europe/Istanbul","durationMinutes":30,"slots":["2026-12-01T09:00:00+03:00", ...]}

# Book one
curl -X POST localhost:4000/api/v1/appointments -H "Authorization: Bearer $TOKEN" \
  -d '{"serviceId":1,"startsAt":"2026-12-01T09:00:00+03:00"}'

# The same slot again: 409 {"error":{"code":"SLOT_TAKEN", ...}}
```

## Endpoints

| Method | Path | Who |
| --- | --- | --- |
| POST | `/api/v1/auth/register`, `/login`, `/refresh`, `/logout` | everyone |
| GET | `/api/v1/auth/me` | logged in |
| GET | `/api/v1/businesses?city=&category=&q=&page=&limit=` | everyone |
| GET | `/api/v1/businesses/{id}` | everyone |
| GET | `/api/v1/businesses/{id}/availability?serviceId=&date=` | everyone |
| POST | `/api/v1/businesses` | owner |
| PUT | `/api/v1/businesses/{id}/working-hours` | owner of the business |
| POST | `/api/v1/businesses/{id}/services` | owner of the business |
| POST | `/api/v1/appointments` | customer |
| GET | `/api/v1/appointments/me?status=&upcoming=` | logged in |
| PATCH | `/api/v1/appointments/{id}/status` | customer or owner (different rules) |

Errors always have the same shape:

```json
{ "error": { "code": "VALIDATION_ERROR", "message": "Invalid email address", "field": "email" } }
```

## Project structure

```
cmd/api/              # main: config, database, HTTP server, graceful shutdown
internal/
├── app/              # wires routes and middleware; integration tests live here
├── auth/             # accounts, JWT and refresh tokens, Require middleware
├── business/         # businesses, services, working hours, search
├── booking/          # slot calculation, status rules, appointments
│   ├── availability.go   # pure functions, unit tested
│   └── transitions.go    # who can change which status
├── httpx/            # JSON helpers, typed errors, PostgreSQL error mapping
├── db/               # connection pool, embedded SQL migrations
└── config/
```

## Design decisions

**No double bookings, enforced by the database.** Checking "is this slot free?" and then inserting has a race: two requests can both see the slot as free. Instead the `appointments` table has an exclusion constraint, so PostgreSQL itself rejects an active appointment that overlaps another one of the same business:

```sql
EXCLUDE USING gist (business_id WITH =, tstzrange(starts_at, ends_at) WITH &&)
  WHERE (status IN ('pending', 'confirmed'))
```

`httpx` maps that error (`23P01`) to `409 SLOT_TAKEN`. A test sends eight bookings for the same slot at once and checks that exactly one succeeds.

**Errors as return values.** Handlers have the signature `func(w, r) error`. A small adapter writes the error: an `*httpx.Error` becomes its status and code, known PostgreSQL errors become `409`, and anything else is logged and returned as a generic `500` so internal details never reach the client. This keeps every handler free of repeated error-writing code.

**Standard library router.** Go 1.22 added methods and path parameters to `http.ServeMux` (`"GET /api/v1/businesses/{id}"`), which is everything this API needs, so there is no router dependency.

**Raw SQL with pgx.** Range overlaps, window functions for the total count and `FOR UPDATE` row locks are clearer in SQL than through an ORM. Every value is a query parameter; search terms are escaped so `%` and `_` match literally.

**Time zones.** Times are stored as `timestamptz`. Working hours are local clock times, turned into instants with `time.Date(..., location)`, so a business that opens at 09:00 opens at 09:00 local time even on the day clocks change. The binary embeds the time zone database (`time/tzdata`), so this also works in the minimal container image.

**Refresh token rotation.** Refresh tokens are random, stored only as SHA-256 hashes and usable once. If a used token comes back it was probably stolen, so all of the user's sessions are revoked. The revocation is committed before the `401` is returned; an early version returned the error inside the transaction, which rolled the revocation back, and the test caught it.

**Testable time.** Handlers get the current time from a `Now func() time.Time` field. Tests run at a fixed moment, so slot lists and cancellation deadlines are deterministic.

**Status rules in one table.** Allowed transitions are a map in `transitions.go`, and status changes lock the row (`SELECT ... FOR UPDATE`) so two changes to one appointment can't interleave.

## Tests

```bash
go test ./...                                         # unit tests
TEST_DATABASE_URL=postgres://... go test -race -p 1 ./...   # plus integration tests
```

- **Unit:** slot generation (overlaps, past times, closed days, a daylight saving change in Berlin), valid start times, status transitions
- **Integration:** the real router against PostgreSQL: registration, login, refresh token rotation and reuse detection, roles and ownership, search, the full booking flow, cancellation deadline, concurrent bookings of the same slot, JSON error format

CI runs `gofmt`, `go vet`, the tests with the race detector against a PostgreSQL service, and builds the Docker image.
