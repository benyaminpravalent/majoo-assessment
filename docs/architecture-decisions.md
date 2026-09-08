# Architecture Decision Records

Decisions taken while building the blog API (assessment section 1), recorded
with the alternative that was rejected and the cost of the choice. Several are
referenced by name from the source code, so a reader who wonders why a thing is
the way it is can find the reasoning without archaeology.

Format: context, decision, consequences — the short form, because a 40-page ADR
nobody reads is worse than a paragraph somebody does.

| # | Decision | Status |
|---|---|---|
| [ADR-001](#adr-001-modular-monolith-organised-by-feature) | Modular monolith organised by feature | Accepted |
| [ADR-002](#adr-002-nethttp-with-chi-rather-than-a-framework) | `net/http` with Chi rather than a framework | Accepted |
| [ADR-003](#adr-003-pgx-with-hand-written-sql-rather-than-an-orm) | pgx with hand-written SQL rather than an ORM | Accepted |
| [ADR-004](#adr-004-bcrypt-rather-than-argon2id) | bcrypt rather than Argon2id | Accepted |
| [ADR-005](#adr-005-a-small-in-house-migration-runner) | A small in-house migration runner | Accepted |
| [ADR-006](#adr-006-stateless-access-tokens-with-stateful-rotating-refresh-tokens) | Stateless access tokens, stateful rotating refresh tokens | Accepted |
| [ADR-007](#adr-007-an-in-process-event-bus-with-the-outbox-as-the-stated-upgrade-path) | An in-process event bus | Accepted, with a stated upgrade path |
| [ADR-008](#adr-008-in-process-rate-limiting) | In-process rate limiting | Accepted, with a stated upgrade path |
| [ADR-009](#adr-009-offset-pagination-for-this-api) | Offset pagination for this API | Accepted |
| [ADR-010](#adr-010-transaction-boundaries-live-in-repositories) | Transaction boundaries live in repositories | Accepted |
| [ADR-011](#adr-011-invisible-drafts-return-404-not-403) | Invisible drafts return 404, not 403 | Accepted |
| [ADR-012](#adr-012-a-denormalised-comment-counter) | A denormalised comment counter | Accepted |

---

## ADR-001: Modular monolith organised by feature

**Context.** The brief asks for a blog API with users, posts and comments. The
default instinct in 2026 is microservices; the default instinct before that was
layered packages (`handlers/`, `services/`, `repositories/`).

**Decision.** One deployable binary, with packages organised by *feature*
(`internal/user`, `internal/post`, `internal/comment`), each owning its own
repository, service, DTOs and handler. Shared plumbing lives under
`internal/platform`.

**Why not microservices.** Three services for three tables would mean three
deploy pipelines, three databases, and a distributed transaction across the
comment counter — all cost, no benefit, at a scale where a single Postgres
instance is not remotely stressed. The e-commerce design in
`docs/ecommerce-architecture.md` §4.1 argues the same way at much larger scale.

**Why not layer-first packages.** With `handlers/`, `services/`, `repositories/`,
a change to "how a post works" touches three directories and every package
imports every other. Feature-first means the change is contained, and the
dependency direction is visible: handler → service → repository, never back.

**Consequences.**
- Adding a feature means adding one directory, not editing four.
- Cross-feature dependencies are explicit and few — only comment → post.
- Module boundaries need discipline to stay real; nothing but review prevents
  `post` from importing `comment`'s internals. In a larger codebase this would
  be worth enforcing with an import-lint rule.

---

## ADR-002: `net/http` with Chi rather than a framework

**Context.** Gin, Echo and Fiber are the popular choices; the standard library
plus a router is the conservative one.

**Decision.** `net/http` handlers with `github.com/go-chi/chi/v5` for routing.

**Why.** Chi's `Handler` *is* `http.Handler`. Every piece of middleware here is
a plain `func(http.Handler) http.Handler`, testable with `httptest` and usable
outside this project. A framework's context type is a lock-in that shows up in
every function signature — and Go 1.22's `http.ServeMux` still lacks route
groups and per-group middleware, which this router uses heavily.

**Consequences.**
- No framework-specific idioms to learn or to unlearn.
- Slightly more code: binding, validation and rendering are written explicitly
  in `internal/platform/httpx` rather than provided.
- That explicitness is what makes the strict decoding in `httpx.DecodeJSON`
  possible — rejecting unknown fields and trailing content is a deliberate
  choice a framework would have made for us, probably differently.

---

## ADR-003: pgx with hand-written SQL rather than an ORM

**Context.** GORM is the default in Go; sqlc generates code from SQL; pgx plus
hand-written queries is the explicit option.

**Decision.** `pgx/v5` with SQL written by hand in the repositories.

**Why not GORM.** The queries in this project are not generic. The post listing
uses `COUNT(*) OVER ()` to get a page and its total in one statement;
`websearch_to_tsquery` drives search; comment deletion uses a recursive CTE. An
ORM makes the easy 80% shorter and the interesting 20% a fight, and it hides the
statement actually being sent — which is the thing you most need to see when a
query is slow.

**Why not sqlc.** Genuinely attractive: type-safe, generated from real SQL,
verified against the schema at build time. Rejected here for one reason — the
dynamic filter construction in `post.Repository.List` (optional author, status
and search predicates) does not fit generated static queries, and mixing sqlc
with hand-written SQL gives two idioms in one package. For a project without
dynamic filtering, sqlc would be the better choice.

**Consequences.**
- Every SQL statement is visible, reviewable and directly explainable.
- Repositories are longer and scanning is manual.
- No compile-time check that a query matches the schema — which is exactly the
  gap the build-tagged integration tests in `tests/integration` exist to close.

---

## ADR-004: bcrypt rather than Argon2id

*Referenced from `internal/auth/password.go`.*

**Context.** Argon2id is the current recommendation from OWASP and won the
Password Hashing Competition. bcrypt is older and, on paper, weaker against
GPU-parallel attack.

**Decision.** bcrypt, with the cost factor configurable and validated to be at
least 10.

**Why.** Three practical reasons, none of them "bcrypt is better":

1. **One parameter to get right.** Argon2id needs memory, iterations and
   parallelism tuned to the deployment. Tuned badly — which is the common
   outcome — it can be *weaker* than bcrypt at a sensible cost. bcrypt has one
   knob and a well-understood default.
2. **Self-describing output.** The cost is encoded in the hash, so raising it
   later re-hashes users on their next successful login without invalidating
   anything.
3. **It is in `golang.org/x/crypto`,** already a dependency, and its 72-byte
   input limit is handled explicitly in the DTO rather than surfacing as a 500.

**Consequences.**
- Weaker than a well-tuned Argon2id against a GPU attacker.
- The migration path is straightforward and worth stating: on login, if the
  stored hash is bcrypt, verify with bcrypt and re-hash with Argon2id. Both
  algorithms coexist during the transition because the hash prefix identifies
  which is which.
- For a service whose primary business is holding credentials, Argon2id would be
  the right call. For a blog API, this is a defensible trade.

---

## ADR-005: A small in-house migration runner

*Referenced from `internal/platform/postgres/migrate.go`.*

**Context.** golang-migrate, goose and Atlas are the established tools.

**Decision.** A ~200-line runner in `internal/platform/postgres/migrate.go`,
driving plain `.sql` files embedded in the binary.

**Why.** The runner does exactly four things this project needs: ordered
up/down, SHA-256 checksums that refuse silently edited history, a session
advisory lock so two replicas starting together cannot race, and one transaction
per migration. golang-migrate does all of that too, plus source drivers for S3,
GitHub and a dozen databases — none of which apply.

The deciding factor is not size, it is **auditability**. Schema changes are the
riskiest thing this service does, and a reviewer can read this runner in full in
five minutes.

**Consequences.**
- One fewer dependency, and one fewer binary to install in CI.
- The migration files stay plain SQL, so `psql -f` or golang-migrate can apply
  them too — the runner is not a lock-in.
- Missing features that would matter elsewhere: no `force` to escape a dirty
  state, no multi-statement `-- +goose` annotations, no drift detection against
  a live schema.
- The checksum normalises line endings, so a CRLF checkout on Windows does not
  look like an edited migration. That is not hypothetical for this repository.

---

## ADR-006: Stateless access tokens with stateful, rotating refresh tokens

*Referenced from `internal/auth/token.go`.*

**Context.** Sessions can be stateless JWTs (fast, unrevocable) or database-
backed (revocable, one read per request).

**Decision.** Both, split by role. A short-lived HS256 JWT for access; a
long-lived opaque random token for refresh, stored only as a SHA-256 digest and
rotated on every use.

**Why.**
- The hot path — every authenticated request — needs no database read.
- Sessions remain revocable, because the refresh token is a row that can be
  marked revoked.
- **Rotation makes theft detectable.** The revoke is guarded by
  `AND revoked_at IS NULL`, so exactly one party can use a given token. When the
  loser presents an already-rotated token, that is the signature of a stolen
  token being replayed, and the service revokes every session for the account.

**Why SHA-256 and not bcrypt for the refresh token.** The token is 256 bits of
uniform randomness. There is no dictionary to attack, so bcrypt's slowness would
buy nothing and make every refresh expensive.

**Consequences.**
- An access token stays valid until it expires: **the access TTL is the
  worst-case revocation delay**, 15 minutes by default. This is the real cost,
  and it is stated in the README and the OpenAPI description rather than
  glossed over.
- A deny-list in Redis keyed by `jti` would close that window, at the cost of a
  cache read per request. Not worth it here; it would be for an admin API.
- HS256, not RS256: there is one issuer and one verifier. Asymmetric signing
  earns its complexity when a third party must verify without being able to
  mint, which is not the case here.

---

## ADR-007: An in-process event bus, with the outbox as the stated upgrade path

*Referenced from `internal/platform/events/events.go` and `internal/server/server.go`.*

**Context.** Several actions have follow-up work the caller should not wait for:
audit logging, and later cache warming or notifications.

**Decision.** A bounded in-process worker pool: fixed workers, fixed queue
depth, non-blocking publish that drops and counts when full, drain on shutdown.

**Why not a bare `go func()`.** It is unbounded — an attacker can create
goroutines as fast as they can send requests — and the work is lost on shutdown.

**Why not Kafka now.** A broker for audit logging in a blog API is
infrastructure without a problem. The e-commerce design uses one because
*there* the consequences of losing an event are real.

**Consequences, stated plainly.** This is **at-most-once, in-process delivery.
Events are lost if the process is killed.** For audit logging that is
acceptable, and the package documentation says so rather than implying
durability it does not have.

**The upgrade path**, which the e-commerce design assumes: write the event to an
`outbox` table inside the same transaction as the business change, and have a
relay publish it. That removes the gap between committing and publishing
entirely, and turns a broker outage into a delay rather than a data-loss event.
The handler signature here is already the right shape for it.

The backpressure choice is deliberate and worth defending: **a full queue drops
the event rather than blocking the HTTP handler.** Delaying a user's response to
make room for an audit log is the wrong trade, and the drop is counted and
logged so the condition is visible rather than silent.

---

## ADR-008: In-process rate limiting

*Referenced from `internal/middleware/ratelimit.go` and from
`docs/ecommerce-architecture.md` §11.5.*

**Context.** Rate limiting can be per-process (simple, approximate) or shared
through Redis (exact across a cluster, one more dependency).

**Decision.** A per-process token bucket with lazy refill and a janitor
goroutine that evicts idle buckets.

**Why.** It needs no extra infrastructure and it addresses the abuse it targets:
credential stuffing and runaway client loops. Keying by authenticated user when
one is present, and by client address otherwise, means an office behind one NAT
address does not throttle itself.

**Why hand-written rather than `golang.org/x/time/rate`.** That package would do
the bucket arithmetic, but it has no eviction — its `Limiter` is per key and the
map still has to be managed. Since the map and its janitor are the actual work,
adding a dependency for a third of the job is not a good trade.

**Consequences.**
- **Behind N replicas the effective limit is N times the configured rate, and a
  restart resets every bucket.** Both are stated in the package documentation.
- The eviction janitor is not optional: without it the map grows once per
  distinct client address and never shrinks, which is a slow memory leak on a
  public API.
- The cluster-wide version is the same algorithm with the bucket in a Redis Lua
  script — one round trip, atomic. That is what the e-commerce design specifies.

---

## ADR-009: Offset pagination for this API

**Context.** Offset pagination is simple and supports jump-to-page. Keyset
pagination is stable under concurrent writes and has constant cost at any depth.

**Decision.** Offset pagination, with `page`, `limit` and full metadata
(`total_items`, `total_pages`, `has_next`, `has_prev`), bounded at 100 per page.

**Why.** A blog's lists are bounded — tens of thousands of posts, not billions
of feed rows — and a reviewer or a UI benefits from a total count and the
ability to jump to page 5. Neither is possible with a keyset cursor.

**Consequences.**
- `OFFSET 100000` walks and discards 100,000 rows. Acceptable at this size,
  unacceptable at feed scale.
- A concurrent insert can shift rows between pages.
- **The contrast is deliberate**: the social-media design in this same
  submission uses keyset pagination throughout, and
  `docs/social-media-database-design.md` §5 explains exactly why offset is wrong
  there. Choosing differently in the two designs is the point — the answer
  depends on data volume and interface, not on fashion.
- `internal/platform/httpx/pagination.go` carries this reasoning as a comment,
  so the next person to touch it knows it was a decision.

---

## ADR-010: Transaction boundaries live in repositories

**Context.** Transactions can be owned by the service layer (via a Unit of Work
or a `TxManager` passed down) or by the repository method that needs them.

**Decision.** A repository method that requires atomicity opens its own
transaction. `postgres.InTx` is the helper; `comment.Repository.Create`,
`comment.Repository.Delete`, `post.Repository.Delete` and
`user.SessionRepository.Rotate` are the four places that use it.

**Why.** The alternative means threading a transaction handle through every
service signature and inventing an abstraction — a `TxManager`, a `UnitOfWork` —
whose only purpose is to be passed around. Keeping the boundary next to the SQL
puts it where a reader looking at the statements can see it.

**Consequences.**
- A service cannot compose two repositories into one transaction. Nothing in
  this application needs to: the comment counter lives in the comment
  repository, and refresh-token rotation lives in the session repository. If
  that changed, this decision would need revisiting, and it is written down so
  that revisit is informed.
- Each transactional method is self-contained and testable — `pgxmock` verifies
  begin, statement order, and commit or rollback for every one of them.
- The rollback in `postgres.InTx` uses a background context deliberately: if the
  request context is already cancelled — which is exactly when a rollback
  matters most — a rollback issued on it would itself fail and leak the
  transaction until the connection is recycled.

---

## ADR-011: Invisible drafts return 404, not 403

**Context.** A non-owner requests an unpublished draft. `403 Forbidden` is the
literally accurate answer.

**Decision.** Return `404 Not Found`, with the same body a genuinely missing
post produces.

**Why.** `403` confirms that a post with that ID exists. For unpublished
content, existence is exactly the thing that must not be disclosed — an attacker
enumerating IDs learns which drafts are real, and when a competitor's
announcement is being prepared.

The rule is applied consistently, which is what makes it work:

| Operation | Non-owner, published post | Non-owner, draft |
|---|---|---|
| Read | 200 | **404** |
| Update | 403 | **404** |
| Delete | 403 | **404** |
| Comment | 201 | **404** |

If any one of those returned 403 for a draft, the whole scheme would leak
through that endpoint.

**Consequences.**
- Slightly confusing for a legitimate user who mistypes a URL — they cannot tell
  "does not exist" from "not yours". That is the intended trade.
- Published posts still return 403 on write, because their existence is already
  public and 403 is the more useful answer.
- Enforced in one place per resource — `post.Service.authorize` and
  `post.Service.canView` — rather than repeated in handlers, and covered by
  explicit tests.

---

## ADR-012: A denormalised comment counter

**Context.** `posts.comment_count` duplicates `SELECT count(*) FROM comments
WHERE post_id = ...`.

**Decision.** Store the counter on the post, and maintain it in the same
transaction as the comment write.

**Why.** A list of 20 posts would otherwise run 20 counting subqueries, on the
API's hottest read path.

**Why not a trigger** — which is what the social-media schema in this submission
*does* use. Here the counter update doubles as the existence check: the
`UPDATE ... WHERE id = $1 AND deleted_at IS NULL` runs *first*, takes the post's
row lock, and its zero-rows-affected result is what proves the post was deleted
concurrently. A trigger would fire after the insert, too late to make that
check. Two different answers to the same question, each right in its context.

**Consequences.**
- Every comment write is a transaction. That is the point, and it is tested for
  both commit and rollback.
- The counter can drift if a future write path bypasses the repository. Two
  defences: `CHECK (comment_count >= 0)` turns any arithmetic mistake into a
  failed transaction rather than silent corruption, and the deletion path
  decrements by the number of rows actually removed rather than by one.
- Concurrent comments on the same post serialise on that row lock. At blog scale
  that is irrelevant; the social-media document §6.7 describes what to do when
  it stops being.
