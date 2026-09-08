# Requirement Traceability

Every requirement from *Test Cases & Assessment Framework (Take-Home)* mapped to
where it is satisfied and how it was checked.

**Status key**

| Symbol | Meaning |
|---|---|
| ✅ | Implemented **and executed** on this machine |
| 📝 | Delivered as a written artefact (sections 2 and 3 are design deliverables) |
| ⚠️ | Implemented but **not executed here** — see [Unverified](#unverified-items) |

---

## Section 1 — Technical Coding Assessment

The brief says to choose **one** test case. **Test Case 2: REST API Development**
is implemented. Test Case 1 is not implemented as a second application, but the
concurrency skills it targets are demonstrated where they do real work — see
[Test Case 1 skills](#test-case-1-skills-demonstrated-without-a-second-application).

### Scenario requirements

| Requirement | Where | Verified by | Status |
|---|---|---|---|
| User authentication | `internal/auth`, `internal/middleware/auth.go` | `auth/token_test.go`, `middleware/auth_test.go` | ✅ |
| User authorization | `internal/domain` `Actor.CanModify`, applied in every service | `domain_test.go`, ownership tests in all three features | ✅ |
| CRUD for posts | `internal/post/{repository,service,handler}.go` | 27 service + 19 handler + 15 repository tests | ✅ |
| CRUD for comments | `internal/comment/{repository,service,handler}.go` | 19 service + 14 handler + 12 repository tests | ✅ |
| Input validation | `internal/platform/validation`, DTO tags | `validation_test.go`, per-handler validation tests | ✅ |
| Error responses | `internal/platform/apierr`, `httpx.WriteError` | `apierr_test.go`, `response_test.go` | ✅ |
| Database integration | `internal/platform/postgres`, pgx pool | `postgres_test.go`, `pgxmock` in every repository test | ✅ |
| Database transactions | 4 boundaries via `postgres.InTx` | commit **and** rollback asserted for all four | ✅ |

### Expected skills

| Skill | Demonstrated by |
|---|---|
| HTTP handlers | 23 routes across three feature packages, each decode → validate → service → render |
| Middleware | 8 middlewares in `internal/middleware`, order documented at the assembly point |
| Database operations | Hand-written SQL: window-function pagination, full-text search, recursive CTE, partial unique indexes, composite foreign keys |
| JSON handling | Request/response DTOs separate from domain models; strict decoding rejects unknown fields and trailing content |
| Security considerations | See the [security table](../README.md#security) — 17 concerns, each with a named mechanism |

---

### Functional features (from the execution brief)

| Feature | Where | Status |
|---|---|---|
| User registration | `POST /api/v1/auth/register` | ✅ |
| User login | `POST /api/v1/auth/login` | ✅ |
| Secure password hashing | bcrypt, cost ≥ 10 enforced at start-up ([ADR-004](architecture-decisions.md#adr-004-bcrypt-rather-than-argon2id)) | ✅ |
| JWT authentication | HS256, `iss`/`aud`/`exp` required, algorithm pinned on verify | ✅ |
| Authorization + ownership | `Actor.CanModify`, one rule, applied in services | ✅ |
| Roles (user, admin) | `domain.Role`, `CHECK` constraint, admin override on every write path | ✅ |
| Post CRUD | 5 endpoints | ✅ |
| Comment CRUD | 5 endpoints | ✅ |
| Pagination and filtering | `page`, `limit`, `author_id`, `status`, `q` | ✅ |
| Input validation | Struct tags → field-level errors with JSON names | ✅ |
| Consistent JSON responses | `{data, pagination, request_id}` on every success | ✅ |
| Structured error responses | `{error:{code, message, fields}, request_id}` on every failure | ✅ |
| PostgreSQL persistence | pgx v5 with a configured pool | ✅ |
| Transactions where atomicity is required | comment create/delete, post delete, token rotation | ✅ |
| Graceful shutdown | SIGTERM → drain HTTP → drain events → stop janitors → close pool | ✅ |
| Health and readiness | `/healthz` (no dependency) and `/readyz` (checks the DB) — deliberately different | ✅ |

### Business rules

| Rule | Where | Test |
|---|---|---|
| Only authenticated users create content | `Authenticator.Require` on the write group | `TestCreatePostRequiresAuthentication`, `TestCreateCommentRequiresAuthentication` |
| Users modify only their own content, admins any | `Actor.CanModify` | `TestUpdatePostByAnotherUserIsForbidden`, `TestDeletePostByAdminIsAllowed`, and comment equivalents |
| Comments must reference a valid post | Counter `UPDATE ... WHERE deleted_at IS NULL` runs first and proves existence under lock | `TestCreateRollsBackWhenThePostIsGone`, `TestCreateHandlesAPostDeletedMidFlight` |
| Deleted resources return appropriate errors | Soft delete + partial indexes | `TestDeleteTwiceReturns404`, `TestGetPostReturns404ForUnknownID` |
| Duplicate identity handled safely | Unique constraints translated by name to 409 | `TestRegisterRejectsDuplicateEmail`, `...DifferingOnlyByCase`, `...DuplicateUsername` |
| Invalid/expired/missing tokens → correct status | One 401 for all cases | 9 token tests + 6 middleware tests |

### API quality

| Requirement | Where | Status |
|---|---|---|
| Correct methods and status codes | 200/201/204/400/401/403/404/409/413/415/422/429/500/504 all used deliberately | ✅ |
| Versioned routes | `/api/v1` | ✅ |
| Request/response DTOs | `dto.go` in each feature package | ✅ |
| HTTP models separate from domain | `domain.User` has `PasswordHash`; no response DTO has a field for it | ✅ |
| Pagination metadata | `total_items`, `total_pages`, `has_next`, `has_prev` | ✅ |
| Validation errors identify fields | JSON field names, every failure at once | ✅ |
| Request/correlation ID | `X-Request-Id`, sanitised if client-supplied, in every log line and response | ✅ |
| Centralised error handling | `httpx.WriteError` is the only place a status is chosen | ✅ |
| No leaking of internal errors | Cause in an unexported field, never serialised | ✅ |
| Body size and server timeouts | 1 MiB cap; all five server timeouts set and cross-validated | ✅ |

### Database requirements

| Requirement | Where | Status |
|---|---|---|
| UUID primary keys | All four tables, with the rationale in the migration header | ✅ |
| Foreign keys | 6, each with a deliberate delete behaviour | ✅ |
| Unique constraints | email, username, slug (partial, live rows only), refresh-token digest | ✅ |
| Check constraints | 16 across the schema | ✅ |
| Created/updated timestamps | Every table; `updated_at` maintained by trigger | ✅ |
| Indexes matching common queries | 6 purpose-built, each with a comment naming its query | ✅ |
| Safe delete behaviour | Soft delete for content, cascade for ownership, `SET NULL` where a row must outlive its parent | ✅ |
| Transaction boundaries | 4, all tested for commit and rollback | ✅ |
| Migration rollback | `.down.sql` for every migration; `migrate down [n]` | ✅ up and down both applied and verified |
| Prevent invalid state at the DB level | draft/published_at consistency, non-negative counters, composite FK forcing a reply onto its parent's post | ✅ |

### Security requirements

Full table in the [README](../README.md#security). Every item in the brief is
covered:

| Requirement | Status |
|---|---|
| Secure password hashing | ✅ |
| JWT validation, expiry, signing, claims | ✅ |
| Authentication vs authorization | ✅ separate middleware and service concerns |
| Ownership enforcement | ✅ |
| SQL injection prevention | ✅ parameterised throughout; asserted with a hostile search term |
| Input validation | ✅ |
| Secrets via environment | ✅ `.env.example`, no secret committed |
| CORS configuration | ✅ exact-origin, never reflected, `*` refused in production |
| Secure logging and redaction | ✅ enforced in the slog handler — a test found and fixed a real gap |
| Safe error responses | ✅ |
| Rate limiting | ✅ implemented, with its single-process scope documented |
| Dependency and container security | ✅ 8 pinned direct deps; distroless, non-root, read-only rootfs |

### Reliability and observability

| Requirement | Where | Status |
|---|---|---|
| Graceful shutdown | `server.Run`, ordered drain | ✅ |
| Context propagation | Every layer; deadline reaches pgx | ✅ |
| Connection pool configuration | Max/min conns, lifetime, idle, jitter | ✅ |
| HTTP server timeouts | All five, cross-validated at start-up | ✅ |
| Structured logs | slog JSON, one line per request | ✅ |
| Request IDs | ✅ | ✅ |
| Health and readiness | ✅ | ✅ |
| Approach to metrics and tracing | Documented in the README; not wired — stated as a limitation | 📝 |
| Explicit dependency-failure handling | Driver errors classified by SQLSTATE; readiness reports the reason | ✅ |

### Testing

| Requirement | Evidence | Status |
|---|---|---|
| Unit tests for business logic | 344 test functions, 618 cases | ✅ |
| HTTP handler tests | 51 across three features, through the real router | ✅ |
| Repository/database tests | `pgxmock` for SQL and transactions; a build-tagged suite for a real DB | ✅ / ⚠️ |
| Authentication and authorization tests | 23 token + middleware tests, plus ownership tests per feature | ✅ |
| Validation and error-path tests | Throughout | ✅ |
| Transaction rollback tests | All four boundaries, both directions | ✅ |
| Race-detector compatibility | Concurrency tests written for it | ⚠️ no C toolchain here |
| Invalid credentials | `TestLoginRejectsWrongPassword` | ✅ |
| Missing/malformed authentication | 5 cases in `TestRequireRejectsMissingOrMalformedHeader` | ✅ |
| Unauthorized ownership access | Post and comment, read and write | ✅ |
| Duplicate registration | Email, username, and case-only differences | ✅ |
| Invalid input | Per-field, per-endpoint | ✅ |
| Missing resources | 404 paths throughout | ✅ |
| Database conflicts | Unique, foreign-key and check violations classified by constraint name | ✅ |
| Pagination edge cases | 9 cases | ✅ |
| **A real coverage report, not an invented number** | **71.5%**, per-package breakdown in the README | ✅ |

### Required check runs

| Check | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | ✅ no output |
| `go vet` | `go vet ./...` and `-tags=integration` | ✅ clean |
| Unit and integration tests | `go test ./...` | ✅ all pass / ⚠️ integration unrun |
| Race detector | `go test -race ./...` | ⚠️ needs cgo |
| Coverage | `go tool cover -func` | ✅ 71.5% |
| Linter | `make lint` | ⚠️ golangci-lint not installed; the target says so and continues |
| Docker build | `docker build .` | ⚠️ Docker not installed |
| Migration verification | The SQL: `node scripts/verify-sql.mjs` ✅. The Go runner against a live server: ⚠️ |
| Basic API smoke test | `scripts/smoke.sh` | ⚠️ needs a running server |

### API documentation

| Requirement | Where | Status |
|---|---|---|
| Complete OpenAPI 3 spec | `api/openapi.yaml`, 3.0.3 | ✅ |
| Authentication documented | `bearerAuth` scheme + 5 auth operations | ✅ |
| Users, posts, comments | All 23 operations | ✅ |
| Health endpoints | `/healthz`, `/readyz` | ✅ |
| Request/response schemas | 20 component schemas | ✅ |
| Pagination | `Pagination` schema + shared parameters | ✅ |
| Error responses | 10 reusable responses, codes enumerated | ✅ |
| Security schemes | ✅ | ✅ |
| Example requests and responses | On every non-trivial operation | ✅ |
| **Spec agrees with the implementation** | **A contract test enforces it in both directions** | ✅ |

---

### Test Case 1 skills, demonstrated without a second application

The brief says to choose one test case, so Test Case 1 (Concurrent Data
Processing) is not implemented. Its four expected skills appear where they do
real work:

| Test Case 1 skill | Where it appears here |
|---|---|
| Goroutines and channels | `internal/platform/events` — bounded worker pool over a buffered channel, with drain-on-shutdown; `internal/server` — signal handling and ordered shutdown |
| Error handling | One error type across every boundary; driver errors classified by SQLSTATE; unclassified errors default to 500 |
| Memory management | Bounded queue with drop-on-full; rate-limiter bucket eviction (an unbounded map is a slow leak); `MaxBytesReader`; a page-size cap |
| Code organization | Feature-first packages, consumer-defined interfaces, one-way dependencies |

Concurrency here is also *tested* as concurrency: the rate limiter under 20
goroutines with an injected clock, and the event bus with 16 publishers racing
a shutdown.

---

## Section 2 — System Architecture Assessment

All in [`docs/ecommerce-architecture.md`](ecommerce-architecture.md).

| Requirement | Section | Status |
|---|---|---|
| 10k+ concurrent users | §2.2 capacity, with the derivation shown | 📝 |
| Multiple payment gateways | §6, adapter pattern + routing by method/health/cost | 📝 |
| Real-time inventory | §7, two-layer admission filter | 📝 |
| Order processing and tracking | §5 flow, §9.5 state machine | 📝 |
| Search and recommendations | §9.6, §9.7 | 📝 |
| **High-level architecture diagram** | §3 context, §4 services | 📝 |
| **Technology stack recommendations** | §13, with rejections | 📝 |
| **Database schema design** | §10.1 ownership, §10.2 entities, §10.3 partitioning | 📝 |
| **API specification outline** | §15 | 📝 |
| **Scalability and reliability** | §11 | 📝 |

| Assessment criterion | Where |
|---|---|
| System decomposition and service boundaries | §4.1, including which components start inside a modular monolith and the pressure that extracts each |
| Data flow and communication patterns | §9 — sync vs async, outbox, idempotency, ordering, retries, DLQ |
| Performance and scalability planning | §11 — scaling, load balancing, backpressure, rate limits, breakers, bulkheads |
| Error handling and fault tolerance | §8 saga + **the six named failure scenarios, each answered with a mechanism** |
| Security and compliance | §12 — including PCI SAQ A scope reduction as the central decision |

Also required by the execution brief:

| Item | Section |
|---|---|
| Seven architecture views | §3, §4, §5, §6, §7, §8, §14 |
| Transactional outbox | §9.2 |
| Idempotency | §9.3, three mechanisms for three problems |
| Event ordering, duplicates, retries, DLQ | §9.4 |
| Saga / compensation | §8 |
| Overselling prevention | §7.1 |
| Webhook verification | §6, four mandatory steps |
| Order state machine | §9.5 |
| RPO / RTO / SLOs | §2.3 |
| Key trade-offs | §16 |
| Alternatives considered | §17 |
| Phased evolution plan | §18 |
| Explicit assumptions | §2.1, labelled A1–A6 |
| **Diagrams render** | **14 parsed by Mermaid 11.17.2 — `node scripts/check-mermaid.mjs docs/*.md`** ✅ |

---

## Section 3 — Database Design & Optimization

Design in [`docs/social-media-database-design.md`](social-media-database-design.md);
executable artefacts in [`database-assessment/`](../database-assessment/).

| Requirement | Where | Status |
|---|---|---|
| User profiles | `users` + `user_profiles` | ✅ executed |
| Followers/following | `follows`, `follow_requests`, `blocks` | ✅ |
| Posts with multimedia | `posts` + `post_media` | ✅ |
| Comments and reactions | `comments` (materialised path), `post_reactions`, `comment_reactions` | ✅ |
| Private messaging | `conversations`, `conversation_participants`, `messages` | ✅ |
| Activity feeds and notifications | `feed_entries`, `notifications`, `notification_counters` | ✅ |

| Task | Where | Status |
|---|---|---|
| Normalised schema | `schema.sql`, 19 tables | ✅ applies cleanly to PostgreSQL 18.3 |
| Relationships and constraints | 30+ constraints; delete behaviour chosen per relationship | ✅ **11 invalid writes confirmed rejected** |
| Indexes for common queries | `indexes.sql`, 50 indexes, each with query/ordering/selectivity/shape/cost | ✅ |
| Complex SQL for feed generation | `queries.sql` §7, hybrid fan-out | ✅ **executed, plan inspected** |
| Caching strategy | Design doc §6 | 📝 |

| Assessment criterion | Evidence |
|---|---|
| Normalisation and data integrity | 3NF with deliberate, documented denormalisation; **7 counters reconciled with zero drift** |
| Performance optimisation | Index design verified by EXPLAIN; three plan defects found and fixed |
| Scalability considerations | §7 — pooling, replicas, partitioning, archival, sharding, in cost order |
| Query efficiency | Keyset pagination throughout; window-function totals; materialised path instead of recursion |

Required queries, all executed:

| Query | Section | Status |
|---|---|---|
| Cursor-based home feed | §7 | ✅ |
| Followers / following lists | §2, §3 | ✅ |
| Reaction counts by type | §6a, §6b, §6c | ✅ |
| Comments with reply structure | §5, §5b | ✅ |
| Conversation list with latest message | §8, §8b | ✅ |
| Unread message count | §10 | ✅ |
| Notification retrieval and marking read | §11, §12 | ✅ |
| Keyset vs offset explained | Design doc §5 | 📝 |
| EXPLAIN guidance | `queries.sql` §13, **with observed plans** | ✅ |

Feed and cache strategy:

| Item | Section |
|---|---|
| Fan-out on write vs read vs hybrid | §6.1, §6.2 with the threshold arithmetic |
| Normal vs celebrity accounts | §6.2 |
| Feed cache structure | §6.4 |
| Cursor handling | §6.3 |
| Cache invalidation | §6.5 |
| Ranking | §6.6 |
| Eventual consistency | §6.8, in product terms |
| Redis, cache-aside | §6.4 |
| Notification counters | §6.7 |
| Failure and rebuild | §6.9 |
| Future scaling | §7 |

---

## General deliverables

| Deliverable | Where | Status |
|---|---|---|
| Complete source code with Git history | 12 commits, each one logical change | ✅ |
| Database schema and migration files | `migrations/` (section 1), `database-assessment/` (section 3) | ✅ |
| API documentation (Swagger/OpenAPI) | `api/openapi.yaml`, served at `/docs` | ✅ |
| Docker configuration | `Dockerfile` (distroless, non-root), `docker-compose.yml` | ⚠️ written, not built here |
| README — setup and running | [Quick start](../README.md#quick-start), [Local development](../README.md#local-development) | ✅ |
| README — architecture explanation | [Architecture](../README.md#architecture) | ✅ |
| README — technology justification | [Technology choices](../README.md#technology-choices) + 12 ADRs | ✅ |
| README — limitations and improvements | [Known limitations](../README.md#known-limitations), [Future improvements](../README.md#future-improvements) | ✅ |
| README — test coverage report | [Tests and coverage](../README.md#tests-and-coverage), **71.5% measured** | ✅ |
| Live demo (optional) | Not deployed | — |
| AI usage disclosure | [`docs/ai-usage.md`](ai-usage.md) | ✅ |

---

## Unverified items

Repeated here so they are impossible to miss. Each is written and reviewed but
**not executed**, because this machine has no Docker, no PostgreSQL and no C
toolchain.

| Item | Command to verify |
|---|---|
| Docker image build | `make docker-build` |
| Compose stack and health checks | `make up` |
| The Go migration runner against a live server (the SQL itself is verified) | `make migrate-up && make migrate-status` |
| Integration test suite | `make up && make test-integration` |
| End-to-end smoke test | `make up && ./scripts/smoke.sh` |
| Race detector | `make test-race` |
| golangci-lint | `make lint` after installing it |

Everything else in this document was executed, and the commands are in the
README so any claim can be re-checked.
