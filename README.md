# Majoo Software Architect Engineer — Take-Home Assessment

**Candidate:** Benyamin Pravalent Siregar
**Role:** Software Architect Engineer, Majoo Indonesia

One repository, three deliverables.

| # | Assessment section | Deliverable | Where |
|---|---|---|---|
| 1 | Technical Coding — **Test Case 2: REST API Development** | A production-minded blog API in Go | `cmd/`, `internal/`, `migrations/`, `api/` |
| 2 | System Architecture | E-commerce platform design | [`docs/ecommerce-architecture.md`](docs/ecommerce-architecture.md) |
| 3 | Database Design & Optimization | Social media schema, indexes and queries | [`docs/social-media-database-design.md`](docs/social-media-database-design.md) + [`database-assessment/`](database-assessment/) |

The brief says to choose **one** coding test case, so only Test Case 2 is
implemented as an application. Concurrency — Test Case 1's subject — appears
where it earns its place: graceful shutdown, a bounded event worker pool with
backpressure, and a concurrency-safe rate limiter. Not as goroutines added to
make a point.

---

## Table of contents

- [Verified results](#verified-results) — what was actually run, and what was not
- [Quick start](#quick-start)
- [Prerequisites](#prerequisites)
- [Configuration](#configuration)
- [Local development](#local-development)
- [Migrations](#migrations)
- [Tests and coverage](#tests-and-coverage)
- [API documentation](#api-documentation)
- [Architecture](#architecture)
- [Project structure](#project-structure)
- [Technology choices](#technology-choices)
- [Trade-offs](#trade-offs)
- [Security](#security)
- [Known limitations](#known-limitations)
- [Future improvements](#future-improvements)
- [AI usage](#ai-usage)
- [Troubleshooting](#troubleshooting)
- [Reviewer walkthrough](#reviewer-walkthrough)

---

## Verified results

Everything in this table was executed. Nothing is estimated.

| Check | Command | Result |
|---|---|---|
| Build | `go build ./...` | ✅ clean |
| Vet | `go vet ./...` and `go vet -tags=integration ./...` | ✅ no findings |
| Format | `gofmt -l .` | ✅ no output |
| Unit + HTTP tests | `go test ./...` | ✅ **379 test functions, 538 cases with subtests, all passing** |
| Coverage | `go test -covermode=atomic -coverprofile=...` | ✅ **80.4% of statements** ([breakdown](#tests-and-coverage)) |
| Race detector | `make test-race` | ✅ **clean across all 16 packages**, with and without a live database |
| OpenAPI ↔ router contract | `go test ./tests/` | ✅ **23 routes matched in both directions** |
| **Docker image build** | `make docker-build` | ✅ **multi-stage build succeeds; distroless runtime** |
| **`docker compose up`** | `make up` | ✅ **Postgres healthy → migrations exit 0 → API healthy**, ordering enforced by conditions |
| **Health probes** | `curl /healthz`, `/readyz` | ✅ **200 each; `/readyz` reports the database check** |
| **The Go migration runner, live** | `make migrate-up` / `migrate-down` / `migrate-status` | ✅ **up, down and re-up against PostgreSQL 16.15; `down` leaves only `schema_migrations`** |
| **Integration suite** | `make test-integration` | ✅ **12 test functions, 25 cases, 0 skipped** |
| **End-to-end smoke test** | `make smoke` | ✅ **all 46 checks passed** against the Compose stack |
| Mermaid diagrams | `node scripts/check-mermaid.mjs docs/*.md` | ✅ **14 diagrams parsed by Mermaid 11.17.2** |
| **Blog API migration** | `node scripts/verify-sql.mjs` | ✅ **up and down apply; 10 constraints reject invalid rows; slug reuse, composite FK, full-text search and the `updated_at` trigger all confirmed** |
| Social-media SQL | `node scripts/verify-sql.mjs` | ✅ **22 statements executed, 7 counters reconciled, 11 invalid writes rejected** |

The last two ran against a genuine PostgreSQL engine — PostgreSQL 18.3 compiled
to WebAssembly via PGlite, which is the real Postgres source including its
planner and constraint machinery. That run found
[four real defects in section 3](docs/social-media-database-design.md#8-defects-found-by-actually-running-this),
all fixed.

### The linter

`make lint` reports **four findings, all of which are deliberately left
standing**. A linter is an advisor, not an authority:

| Finding | Where | Why it stands |
|---|---|---|
| `errcheck` ×2 | `internal/platform/httpx/request_test.go` | `requireCode` returns `*apierr.Error` so a caller *may* inspect it further; it already asserts internally. Because that type satisfies `error`, errcheck misreads an ignored convenience return as an unhandled error. False positive. |
| `gosimple` S1016 ×2 | `internal/post/service.go`, `internal/user/handler.go` | Suggests type-converting `registerRequest`→`RegisterInput` and `ListInput`→`ListFilter`. Accepting it would couple the HTTP DTO to the domain input: the structs would have to stay field-identical forever, and a field added to the wire model would silently reach the domain. The explicit mapping *is* the separation this API is supposed to have. |

Note that the widely-installed golangci-lint v1.x before v1.64 cannot analyse a
Go 1.24 module at all — it reports every package as a `typecheck` failure
("could not load export data: unsupported version: 2"). If you see that, the
linter is too old; it is not a finding about this code.

### What was *not* verified, and why

Being straight about this matters more than a longer list of green ticks.
Everything in the first table above has now been executed. Two things have not:

| Not verified | Reason | How to verify it |
|---|---|---|
| **Behaviour under real concurrent load** | No load-generation was run. The race detector proves the absence of *data races*, which is not the same as proving throughput or the rate limiter's behaviour under contention | A load tool such as `k6` or `vegeta` against `make up` |
| **Deployment to a cluster** | No Kubernetes manifests are included; the deployment discussion in the architecture document is a design, not a running system | Out of scope for this assessment |

The verification environment: macOS 26.5.2 on arm64, Docker Desktop 20.10.17
with Compose v2.7.0, PostgreSQL 16.15 in the Compose stack, Go 1.24.3, and
golangci-lint v1.64.8.

---

## Quick start

The fastest path from clone to a working API:

```bash
git clone <this-repository>
cd majoo-assessment
cp .env.example .env          # defaults work as-is for local Docker

docker compose up --build     # or: make up
```

That starts PostgreSQL, runs the migrations to completion, then starts the API.
The ordering is enforced by health and completion conditions, not a sleep, so it
either works or fails loudly.

Then:

```bash
curl http://localhost:8080/healthz
open http://localhost:8080/docs        # interactive API reference
./scripts/smoke.sh                     # the full flow, with assertions
```

Without Docker, see [Local development](#local-development).

---

## Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.24.3+ | Building and testing |
| Docker + Compose v2 | any recent | The quick start |
| PostgreSQL | 16+ | Running without Docker |
| `curl`, `jq` | any | `scripts/smoke.sh` |
| Node.js | 20+ | The Mermaid and SQL verification scripts (optional) |
| `make` | any | Convenience only — every target is a plain command, listed below |

`go.mod` pins the language version to 1.24.3 and every dependency to a version
compatible with it, so the build does not silently pull a newer toolchain.

---

## Configuration

All configuration comes from the environment. `.env.example` documents every
variable with its default and its purpose.

**The binaries do not read `.env` themselves.** There is no dotenv dependency,
deliberately: nothing can silently pick up a stray file, and what the process
sees is exactly what its supervisor gave it. Compose passes every value
explicitly, so the [Quick start](#quick-start) needs no setup. To run from
source, load the file into your shell first:

```bash
set -a; . ./.env; set +a       # or export the two required variables by hand
```

Without that, `go run ./cmd/api` and `go run ./cmd/migrate` stop immediately
with `DATABASE_URL: required` — which is the intended behaviour, not a bug.

Configuration is loaded once at start-up, validated **as a whole**, and passed
explicitly down the call graph. There are no mutable globals. Validation
reports *every* problem at once, so a broken deployment takes one fix-and-restart
cycle rather than one per mistake:

```
fatal: load configuration: DATABASE_URL: required
JWT_SECRET: required, at least 32 bytes
BCRYPT_COST: 2 out of accepted range 10..31
LOG_LEVEL: "verbose" is not one of debug|info|warn|error
```

The settings worth knowing about:

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_URL` | — | **Required.** libpq-style URL |
| `JWT_SECRET` | — | **Required, ≥ 32 bytes.** Generate: `openssl rand -base64 48` |
| `JWT_ACCESS_TTL` | `15m` | Also the worst-case revocation delay — see [ADR-006](docs/architecture-decisions.md#adr-006-stateless-access-tokens-with-stateful-rotating-refresh-tokens) |
| `JWT_REFRESH_TTL` | `720h` | Must exceed the access TTL; validated |
| `BCRYPT_COST` | `12` | Rejected below 10. Compose uses 10 for local speed |
| `HTTP_HANDLER_TIMEOUT` | `10s` | Validated to be less than `HTTP_WRITE_TIMEOUT`, or a deadline could never render a response |
| `RATE_LIMIT_*` | 20 rps / 40 burst | Stricter budget on `/auth/*` |
| `TRUST_PROXY_HEADERS` | `false` | **Leave false unless genuinely behind a proxy that overwrites `X-Forwarded-For`** — otherwise anyone can bypass per-IP limits by inventing an address |
| `CORS_ALLOWED_ORIGINS` | — | Exact origins. `*` is refused when `APP_ENV=production` |

Never commit a real `.env`; `.gitignore` excludes it.

---

## Local development

Without Docker, with a PostgreSQL server available:

```bash
createdb blog
export DATABASE_URL="postgres://blog:blog@localhost:5432/blog?sslmode=disable"
export JWT_SECRET="$(openssl rand -base64 48)"

go run ./cmd/migrate up      # or: make migrate-up
go run ./cmd/api             # or: make run
```

Common commands, with and without `make`:

| Task | `make` | Plain command |
|---|---|---|
| Build both binaries | `make build` | `go build -o bin/ ./cmd/...` |
| Run the API | `make run` | `go run ./cmd/api` |
| Format | `make fmt` | `gofmt -w .` |
| Check formatting | `make fmt-check` | `gofmt -l .` |
| Vet | `make vet` | `go vet ./...` |
| Unit tests | `make test` | `go test -count=1 ./...` |
| Coverage | `make coverage` | `go test -covermode=atomic -coverprofile=coverage.out ./...` |
| Everything checkable offline | `make verify` | — |

`make help` lists every target.

---

## Migrations

Migrations are plain SQL in [`migrations/`](migrations/), embedded into the
binary with `go:embed`. There is no volume to mount and no way to deploy an
image whose code and schema disagree.

These need `DATABASE_URL` and `JWT_SECRET` in the environment — see
[Configuration](#configuration). Inside Compose they already are. `JWT_SECRET`
is required even though the migrator never issues a token, because
configuration is validated as one unit; the alternative is a second, partial
config path that nothing else exercises.

```bash
go run ./cmd/migrate up          # apply everything pending
go run ./cmd/migrate down 1      # revert the newest
go run ./cmd/migrate status      # what is applied, what is pending
```

```
VERSION  NAME  STATE    APPLIED AT
0001     init  applied  2026-09-08T14:31:02Z

1 applied, 0 pending
```

The runner ([ADR-005](docs/architecture-decisions.md#adr-005-a-small-in-house-migration-runner))
is about 200 lines and does four things:

- **Ordered up and down**, one transaction per migration — PostgreSQL has
  transactional DDL, so a failing statement leaves the database on the previous
  version rather than half-migrated.
- **SHA-256 checksums.** Editing a migration that has already been applied is
  refused rather than silently diverging. Line endings are normalised, so a CRLF
  checkout on Windows does not look like an edit.
- **A session advisory lock**, so two replicas starting together cannot apply
  the same migration twice.
- **Migration and bookkeeping in the same transaction**, so the recorded version
  can never disagree with the schema.

In Compose, migrations run as a **separate one-shot service**, and the API waits
for it to exit successfully. That keeps schema change an explicit, observable
deployment step — the shape a Kubernetes init container or Job would take.

### What the schema enforces

Every rule the application applies also exists as a database constraint, so an
application bug produces a failed transaction rather than a corrupt row:

- roles and post statuses as `CHECK` constraints;
- a published post always has `published_at`, a draft never does;
- `comment_count >= 0`, so a counter mistake fails the transaction;
- a reply must live on the same post as its parent — a composite foreign key
  `(parent_id, post_id) → (id, post_id)`, which holds even for writes that never
  pass through this service;
- slugs unique among *live* posts only, via a partial unique index, so a deleted
  post releases its slug;
- refresh-token digests exactly 32 bytes wide.

---

## Tests and coverage

```bash
go test -count=1 ./...                                          # unit + HTTP
go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
go tool cover -html=coverage.out -o coverage.html                # browsable
```

### Verified result: 80.4% of statements

```
internal/domain                   100.0%
internal/health                   100.0%
internal/platform/apierr           97.2%
internal/platform/logging          96.4%
internal/middleware                96.1%
internal/config                    94.7%
internal/platform/httpx            93.9%
internal/server                    93.4%
internal/platform/events           92.6%
internal/platform/validation       91.1%
internal/post                      89.7%
internal/auth                      89.5%
internal/user                      86.3%
internal/comment                   84.0%
internal/platform/postgres         40.1%   ← DB-dependent; see below
cmd/api, cmd/migrate                0.0%   ← process entry points
------------------------------------------
total                              80.4%
```

**Where the uncovered code is, honestly.** Two numbers stay low, and neither is
an oversight:

- `internal/platform/postgres` (40.1%) — the covered part is everything that can
  be tested without a server: error classification, migration file parsing,
  checksums, and the `InTx` transaction boundary (commit, rollback, rollback on
  panic, and rollback after the caller's context is already cancelled). The
  remainder is the migration runner's advisory lock and its `schema_migrations`
  bookkeeping, which need a real connection because the lock is session-scoped;
  the `integration` suite and `make migrate-up`/`migrate-down` exercise it.
- `cmd/*` (0.0%) — `main`, signal handling and flag parsing.

`internal/server` was 0.0% and is now 93.4%. That change is worth a note,
because the old entry claimed the package was covered indirectly by
`tests/openapi_contract_test.go`. That was true of the *router*, but coverage is
attributed per package under test, so nothing there counted — and more to the
point, the contract test never exercised the **lifecycle**: `Run`, graceful
shutdown, the listener-failure path, or the audit handler. Graceful shutdown is
something the brief asks for by name, and it had no direct test at all. It does
now, including an assertion that the port is actually released.

### What the tests actually check

Coverage counts statements; these check behaviour:

| Area | Examples |
|---|---|
| **Token security** | `alg=none` rejected; foreign signing key rejected; wrong issuer and audience rejected; missing `exp` rejected; unknown role rejected — and **all failures return an identical message**, asserted explicitly |
| **Account enumeration** | "no such account" and "wrong password" compared on code, status *and* message |
| **Refresh-token theft** | Replaying a rotated token is rejected **and revokes every session for the account** |
| **Transactions** | `pgxmock` verifies begin, statement order, and commit or rollback for all four transactional paths — including that a failed revoke never inserts a replacement |
| **Ownership** | Owner, other user and admin, across posts and comments; drafts return 404 rather than 403 to non-owners, on read *and* write |
| **Validation** | Every field reported at once; JSON names not Go names; unknown fields rejected |
| **Pagination edges** | `page=0`, negative, non-numeric, over-maximum, page past the end, empty page as `[]` not `null` |
| **Concurrency** | Rate limiter under 20 goroutines with a fake clock; event bus with 16 concurrent publishers racing a shutdown |
| **Lifecycle** | `Run` returns cleanly when its context is cancelled **and the port is released**; a listener that cannot bind still releases the background goroutines; `InTx` rolls back on error, on panic, and when the caller's context is already dead |
| **Redaction** | Credential-shaped log keys replaced — this test **found a real gap**, `X-Api-Key` was not matched, and the fix is in `internal/platform/logging` |
| **Contract** | Every one of the 23 routes exists in `api/openapi.yaml`, and every documented operation exists in the router |

### Integration tests

Behind the `integration` build tag and gated on `TEST_DATABASE_URL`, so
`go test ./...` stays fast and hermetic:

```bash
docker compose up -d postgres
make test-integration
# or:
TEST_DATABASE_URL="postgres://blog:blog@localhost:5432/blog_test?sslmode=disable" \
  go test -tags=integration -count=1 -v ./tests/integration/...
```

They cover what a mock structurally cannot: that the SQL is valid against the
real schema, that 13 constraints actually reject what they claim to, that the
comment counter stays correct under **20 concurrent writers**, and that exactly
one of **8 racing refresh-token rotations** wins.

> Verified: **12 test functions, 25 cases, 0 skipped**, against PostgreSQL 16.15
> from the Compose stack. This suite is what caught the refresh-token rotation
> defect described in
> [ai-usage.md §5.2](docs/ai-usage.md#52-a-500-on-every-token-refresh--a-foreign-key-ordering-bug) —
> a foreign key a mock cannot enforce.

### Race detector

```bash
make test-race     # CGO_ENABLED=1 go test -race -count=1 ./...
```

> Verified clean across all 16 packages, both with and without a live database
> reachable on `localhost:5432`. If cgo is unavailable in your environment, see
> [Troubleshooting](#troubleshooting) for a Docker one-liner.

---

## API documentation

| Format | Location |
|---|---|
| OpenAPI 3.0.3 | [`api/openapi.yaml`](api/openapi.yaml) |
| Served by the running API | `GET /openapi.yaml` |
| Interactive reference | `GET /docs` |

The spec is **embedded in the binary**, so a running service always serves the
contract it was built from.

More usefully, `tests/openapi_contract_test.go` walks the routes Chi actually
registers and compares them with the document **in both directions**: an
endpoint added without documentation fails the build, and so does a documented
endpoint that does not exist. It currently checks 23 operations. Documentation
drift is a failing test here, not a discovery.

### Endpoints

```
GET    /                                    service metadata
GET    /healthz                             liveness  — touches no dependency
GET    /readyz                              readiness — checks the database
GET    /openapi.yaml                        the contract
GET    /docs                                interactive reference

POST   /api/v1/auth/register                create an account
POST   /api/v1/auth/login                   exchange credentials for tokens
POST   /api/v1/auth/refresh                 rotate a refresh token
POST   /api/v1/auth/logout                  revoke one session
POST   /api/v1/auth/logout-all              revoke every session          [auth]

GET    /api/v1/users/me                     own profile                   [auth]
PATCH  /api/v1/users/me                     update own profile            [auth]
GET    /api/v1/users/{userID}               public profile

GET    /api/v1/posts                        list; filter, search, paginate
POST   /api/v1/posts                        create                        [auth]
GET    /api/v1/posts/{postID}               read
PATCH  /api/v1/posts/{postID}               update            [auth, owner/admin]
DELETE /api/v1/posts/{postID}               delete            [auth, owner/admin]

GET    /api/v1/posts/{postID}/comments      list a thread
POST   /api/v1/posts/{postID}/comments      comment                       [auth]
GET    /api/v1/comments/{commentID}         read
PATCH  /api/v1/comments/{commentID}         edit              [auth, owner/admin]
DELETE /api/v1/comments/{commentID}         delete            [auth, owner/admin]
```

### Response shape

Success:

```json
{ "data": { }, "pagination": { }, "request_id": "…" }
```

Failure:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "one or more fields are invalid",
    "fields": [{ "field": "email", "message": "must be a valid email address" }]
  },
  "request_id": "…"
}
```

`error.code` is stable and machine-readable; `error.message` is for humans.
Clients should branch on the code.

---

## Architecture

### Request path

```
Request
  ↓ RequestID            correlation ID, sanitised if client-supplied
  ↓ Logger               one structured line per request, with the ID
  ↓ Recoverer            panic → 500, logged with request context
  ↓ SecurityHeaders      nosniff, DENY, no-referrer, no-store when authenticated
  ↓ CORS                 exact-origin allowlist, never reflected
  ↓ RateLimit            token bucket, keyed by user or client address
  ↓ Timeout              context deadline propagated into pgx
  ↓ BodyLimit            MaxBytesReader
  ↓ Auth                 Require or Optional, per route group
  ↓ Handler              decode → validate → service → render
      ↓ Service          business rules and every authorisation decision
          ↓ Repository   SQL, row scanning, transaction boundaries
              ↓ PostgreSQL
```

The order is documented where the stack is assembled
(`internal/server/router.go`) rather than left implicit. Two orderings matter:
`Recoverer` sits *inside* `Logger` so a panic is logged with request context,
and `RateLimit` sits *before* `Timeout` and `BodyLimit` so shed load costs as
little work as possible.

### Layering

Feature packages (`user`, `post`, `comment`) each own their repository, service,
DTOs and handler. Dependencies point one way — handler → service → repository —
and the only cross-feature dependency is comment → post, for visibility checks.

Interfaces are declared by the **consumer**, not exported next to the
implementation. `post.Service` declares the six repository methods it needs; a
test double is six methods, not a whole repository.

### Errors

One type (`internal/platform/apierr`) crosses every boundary. It carries a
client-safe message and a stable code in fields *separate* from the wrapped
cause, and exactly one function — `httpx.WriteError` — turns it into a response.

That structure is what makes leaking internal detail hard rather than merely
discouraged: the driver error is in an unexported field that is never
serialised. A test asserts a `pq: password authentication failed for user "blog"
at 10.0.0.7` never reaches the body while remaining in the log.

An error nobody classified becomes a 500, because the safe default for an
unrecognised failure is "server's fault, say nothing".

### Concurrency, where it earns its place

Test Case 1 was not implemented — the brief says choose one. Concurrency appears
in four places where it does real work:

1. **Graceful shutdown.** `signal.NotifyContext` on SIGTERM, then: stop
   accepting connections and drain in-flight requests, drain the event bus with
   the remaining budget, stop the limiters' janitors. The pool closes last,
   because draining may still need it.
2. **A bounded event worker pool** (`internal/platform/events`) — fixed workers,
   fixed queue, non-blocking publish that **drops and counts** when full, panic
   isolation per handler, drain on shutdown. The backpressure choice is
   deliberate: delaying a user's HTTP response to make room for an audit log is
   the wrong trade.
3. **A concurrency-safe rate limiter** with lazy refill and an eviction janitor,
   tested under 20 concurrent goroutines with an injected clock.
4. **Context propagation** everywhere, so a client disconnect or a deadline
   aborts the in-flight query instead of leaving it running.

---

## Project structure

```
.
├── cmd/
│   ├── api/                  server entry point + the distroless self-probe
│   └── migrate/              migration CLI
├── internal/
│   ├── config/               load once, validate as a whole, no globals
│   ├── domain/               entities and the one authorisation rule
│   ├── auth/                 bcrypt, JWT, opaque refresh tokens
│   ├── user/                 accounts and sessions   ┐
│   ├── post/                 posts                   ├ feature packages
│   ├── comment/              comments                ┘
│   ├── health/               liveness and readiness
│   ├── middleware/           the cross-cutting stack
│   ├── server/               wiring and process lifecycle
│   └── platform/
│       ├── apierr/           the one error type
│       ├── httpx/            envelopes, decoding, pagination
│       ├── logging/          slog with enforced redaction
│       ├── postgres/         pool, tx helper, migration runner
│       ├── validation/       struct tags → field errors
│       └── events/           bounded async worker pool
├── migrations/               versioned SQL, embedded
├── api/                      OpenAPI 3, embedded
├── tests/
│   ├── openapi_contract_test.go   routes ↔ spec, both directions
│   └── integration/               build-tagged, needs a database
├── database-assessment/      section 3: schema, indexes, queries, seed
├── docs/                     ADRs, sections 2 and 3, traceability, AI usage
└── scripts/                  smoke test, SQL and Mermaid verifiers
```

---

## Technology choices

| Choice | Why | Rejected |
|---|---|---|
| **Go 1.24** | Brief requirement; good fit for a concurrent HTTP service | — |
| **Chi over `net/http`** | Middleware is plain `func(http.Handler) http.Handler` — testable, portable, no framework context type in every signature | Gin, Echo, Fiber ([ADR-002](docs/architecture-decisions.md#adr-002-nethttp-with-chi-rather-than-a-framework)) |
| **pgx + hand-written SQL** | The interesting queries (`COUNT(*) OVER ()`, `websearch_to_tsquery`, a recursive CTE) are not what an ORM makes easier | GORM, sqlc ([ADR-003](docs/architecture-decisions.md#adr-003-pgx-with-hand-written-sql-rather-than-an-orm)) |
| **bcrypt** | One parameter to get right; self-describing cost; already a dependency | Argon2id ([ADR-004](docs/architecture-decisions.md#adr-004-bcrypt-rather-than-argon2id)) — stronger, easier to misconfigure |
| **JWT + rotating opaque refresh** | No database read on the hot path, and sessions stay revocable | Stateless-only, session-only ([ADR-006](docs/architecture-decisions.md#adr-006-stateless-access-tokens-with-stateful-rotating-refresh-tokens)) |
| **`log/slog`** | Standard library; structured; redaction enforced in the handler | zap, zerolog |
| **`validator/v10`** | Struct tags for the mechanical part; messages and JSON field names written here | Hand-rolled |
| **In-house migrator** | ~200 lines a reviewer can read in full; files stay plain SQL | golang-migrate ([ADR-005](docs/architecture-decisions.md#adr-005-a-small-in-house-migration-runner)) |
| **Distroless container** | ~2 MB, no shell, no package manager, non-root | Alpine — a shell is a foothold |

Full reasoning for each: [`docs/architecture-decisions.md`](docs/architecture-decisions.md).

---

## Trade-offs

| Decision | Cost, stated |
|---|---|
| Stateless access tokens | **The access TTL is the worst-case revocation delay** — 15 minutes by default |
| In-process rate limiting | **Behind N replicas the effective limit is N×**, and a restart resets it |
| In-process event bus | **At-most-once. Events are lost if the process is killed** |
| Offset pagination | `OFFSET 100000` walks 100,000 rows; concurrent inserts shift pages |
| Denormalised `comment_count` | Every comment write is a transaction, and concurrent comments serialise on the post row |
| Transactions inside repositories | A service cannot compose two repositories into one transaction |
| 404 for invisible drafts | A legitimate user cannot tell "does not exist" from "not yours" |
| bcrypt over Argon2id | Weaker against a GPU attacker |
| Feature-first packages | Nothing but review stops a feature importing another's internals |

Each of these is a decision, not an oversight, and each has an ADR.

---

## Security

| Concern | Implementation |
|---|---|
| Password storage | bcrypt, cost ≥ 10 enforced at start-up; 72-byte limit surfaced as a field error, not a 500 |
| Token forgery | HS256 **pinned on verification**; `iss`, `aud` and `exp` all required. Tests cover `alg=none`, a foreign key, wrong issuer, wrong audience, missing `exp` |
| Account enumeration | Login returns one identical response for both failures, **and** burns a dummy bcrypt comparison so timing does not leak either |
| Refresh-token theft | Rotation on every use; the revoke is guarded by `revoked_at IS NULL`, so exactly one party wins. Replay revokes every session for the account |
| Privilege escalation | No request body can set a role. Promotion is out-of-band (`scripts/promote_admin.sql`) |
| Ownership | One rule, `domain.Actor.CanModify`, applied in services. Handlers never decide |
| Existence disclosure | Drafts return 404 to non-owners on read, update, delete **and** comment |
| SQL injection | Every value is a `$n` placeholder. A test asserts `'; DROP TABLE posts; --` as a *search term* binds as a parameter, and an integration test confirms the table survives |
| Input validation | Struct tags plus DB constraints; unknown JSON fields and trailing content rejected |
| Secrets | Environment only; `JWT_SECRET` ≥ 32 bytes enforced at start-up; `.env` git-ignored |
| CORS | Exact-origin allowlist, **never** reflected; `*` refused in production. Tests cover suffix, port and scheme near-misses |
| Log leakage | Redaction enforced in the slog handler by key, with separators stripped so `api_key`, `apiKey` and `X-Api-Key` all match. **A test found and fixed a real gap here** |
| Error leakage | Client message and internal cause are separate fields; only the former is serialised |
| Rate limiting | Token bucket, stricter on `/auth/*`; `Retry-After` on rejection |
| Request smuggling / DoS | Every server timeout set explicitly, `ReadHeaderTimeout` against Slowloris, 1 MiB body cap, 100-row page cap |
| Proxy header spoofing | `X-Forwarded-For` trusted **only** when explicitly enabled |
| Container | Distroless, non-root (65532), read-only root filesystem, `no-new-privileges` |
| Dependency surface | 8 direct dependencies, all pinned; `go mod verify` in `make tidy` |

### Deliberately out of scope

Named rather than left as gaps: email verification, password reset, MFA,
account lockout after repeated failures, audit log persistence (currently
structured logs), and a token deny-list for instant revocation.

---

## Known limitations

1. **Access tokens cannot be revoked before expiry** — 15 minutes by default.
   A Redis deny-list keyed by `jti` would close it, at one cache read per
   request.
2. **Rate limiting is per-process.** Effective limit scales with replica count.
3. **Events are at-most-once and in-process.** Lost if the process is killed.
   The outbox upgrade path is specified in [ADR-007](docs/architecture-decisions.md#adr-007-an-in-process-event-bus-with-the-outbox-as-the-stated-upgrade-path).
4. **No slug history.** Renaming a post breaks existing links; doing it properly
   needs a redirect table.
5. **Non-ASCII titles fall back to a random slug.** Transliteration has no
   correct language-independent answer, and half-right is worse than a stable
   fallback.
6. **A post's author cannot moderate comments on their own post** — only the
   comment's author and admins can. Defensible, but not what most blogs do.
7. **Offset pagination.** See [Trade-offs](#trade-offs).
8. **No metrics endpoint.** Structured logs and health checks only; the
   Prometheus/OTel approach is described but not wired.
9. **Soft-deleted rows are never purged.** A retention job is needed.
10. **Docker, migrations, integration tests, the smoke script and the race
    detector are unrun here** — see [Verified results](#verified-results).

---

## Future improvements

**Would do next, in order:**

1. Load testing — the one claim class still unmeasured; the race detector proves
   the absence of data races, not throughput.
2. Prometheus metrics and OpenTelemetry tracing — the code is structured for it;
   the request ID is already the correlation point.
3. Email verification and password reset.
4. Redis-backed rate limiting and a token deny-list.
5. Transactional outbox, replacing the in-process bus.
6. CI: build, vet, lint, test, race, integration against a service container,
   Trivy image scan, and the OpenAPI contract test as a required check.

**Larger, if the product justified it:** full-text ranking, cursor pagination
for high-volume paths, an admin moderation API, per-post view counters, and
media uploads to object storage.

---

## AI usage

The brief encourages AI assistance and assesses discernment and ownership.

This submission was built with **Claude (Opus 5)** in an agentic terminal
workflow. The full disclosure — what it drafted, what was reviewed, what was
corrected or rejected, and what evidence establishes correctness — is in
[`docs/ai-usage.md`](docs/ai-usage.md).

The short version: AI wrote most of the first-draft code, tests and prose;
every design decision was reviewed and several were changed; and the claims in
this README are backed by commands that were actually run. Five real defects
were caught by executing the work rather than by reading it, and each is
documented at the place it was found — including the `X-Api-Key` redaction gap
and the four SQL defects in section 3.

Git history carries a `Co-Authored-By: Claude` trailer on each commit. Say the
word if you would prefer it removed before submission.

---

## Troubleshooting

**`JWT_SECRET: required, at least 32 bytes`**
Set it: `export JWT_SECRET="$(openssl rand -base64 48)"`. Short secrets weaken
the HMAC, so the process refuses to start rather than running insecurely.

**`connect to database: dial tcp ... connection refused`**
PostgreSQL is not reachable. With Compose: `docker compose ps` and
`docker compose logs postgres`. Standalone: check `DATABASE_URL`.

**`HTTP_HANDLER_TIMEOUT (30s): must be less than HTTP_WRITE_TIMEOUT (20s)`**
Deliberate. If the write timeout fires first, the connection closes before any
error can be rendered.

**`go test` fails with `missing go.sum entry`**
`go mod tidy`.

**`-race requires cgo` / `gcc not found`**
No C toolchain. Either install one, or run it in a container:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.24.3 go test -race ./...
```

**`go: downloading go1.26.0`**
Something raised the `go` directive. It is pinned to 1.24.3; prefix commands
with `GOTOOLCHAIN=local` to make an accidental bump a visible error.

**Compose: `dependency failed to start: container ... exited (1)`**
The migrator failed. `docker compose logs migrate` has the SQL error.

**`/docs` renders blank**
Swagger UI loads from a CDN. Offline, read `api/openapi.yaml` directly or fetch
`GET /openapi.yaml`.

**`scripts/smoke.sh: jq: command not found`**
Install `jq`, or read the individual `curl` commands in
[Reviewer walkthrough](#reviewer-walkthrough).

---

## Reviewer walkthrough

### Five minutes

```bash
docker compose up --build -d
curl -s localhost:8080/healthz | jq
./scripts/smoke.sh
```

The smoke script asserts a status code at every step — registration, duplicate
rejection, login, wrong password, post creation, draft invisibility, ownership
enforcement, comment counters, refresh rotation, replay rejection, cascade on
delete. Any deviation fails the script.

### Fifteen minutes, by hand

```bash
API=http://localhost:8080/api/v1

# Register and log in
curl -s -X POST $API/auth/register -H 'Content-Type: application/json' -d '{
  "email":"alice@example.com","username":"alice",
  "display_name":"Alice","password":"a-good-enough-password"}' | jq

TOKEN=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"a-good-enough-password"}' \
  | jq -r .data.access_token)

# Create a post and read it back
POST_ID=$(curl -s -X POST $API/posts -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Hello Majoo","content":"First post.","status":"published"}' \
  | jq -r .data.id)

curl -s $API/posts/$POST_ID | jq

# Comment on it, and watch the counter move in the same transaction
curl -s -X POST $API/posts/$POST_ID/comments -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"content":"Nice."}' | jq
curl -s $API/posts/$POST_ID | jq .data.comment_count      # → 1

# Ownership: register a second user and try to edit Alice's post
curl -s -X POST $API/auth/register -H 'Content-Type: application/json' -d '{
  "email":"mallory@example.com","username":"mallory",
  "display_name":"Mallory","password":"a-good-enough-password"}' > /dev/null
M=$(curl -s -X POST $API/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"mallory@example.com","password":"a-good-enough-password"}' \
  | jq -r .data.access_token)

curl -s -X PATCH $API/posts/$POST_ID -H "Authorization: Bearer $M" \
  -H 'Content-Type: application/json' -d '{"title":"Hijacked"}' | jq   # → 403

# Validation reports every bad field at once
curl -s -X POST $API/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"nope","username":"x","display_name":"","password":"short"}' | jq
```

### Where to look in the code

| To see | Read |
|---|---|
| The most interesting transaction | [`internal/comment/repository.go`](internal/comment/repository.go) — why the counter UPDATE runs *before* the insert |
| Refresh-token theft detection | [`internal/user/service.go`](internal/user/service.go) `Refresh` |
| Why drafts 404 | [`internal/post/service.go`](internal/post/service.go) `authorize`, `canView` |
| Injection safety | [`internal/post/repository.go`](internal/post/repository.go) `List` |
| Concurrency | [`internal/platform/events/events.go`](internal/platform/events/events.go), [`internal/middleware/ratelimit.go`](internal/middleware/ratelimit.go) |
| Shutdown ordering | [`internal/server/server.go`](internal/server/server.go) `Run` |
| Constraints as design | [`migrations/0001_init.up.sql`](migrations/0001_init.up.sql) |
| Documentation that cannot drift | [`tests/openapi_contract_test.go`](tests/openapi_contract_test.go) |

### The other two sections

- **Architecture** — [`docs/ecommerce-architecture.md`](docs/ecommerce-architecture.md).
  Start at §7 (inventory) and §8 (the saga and the six named failure scenarios);
  that is where the engineering judgement is.
- **Database** — [`docs/social-media-database-design.md`](docs/social-media-database-design.md).
  Start at §8, the four defects found by executing it. Reproduce with
  `node scripts/verify-sql.mjs`.
- **Traceability** — [`docs/requirement-traceability.md`](docs/requirement-traceability.md)
  maps every line of the brief to where it is satisfied.
