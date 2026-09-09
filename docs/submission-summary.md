# Submission Summary

**Benyamin Pravalent Siregar** — Software Architect Engineer, Majoo Indonesia

A one-page orientation for a reviewer. Everything here is expanded elsewhere;
the links go straight to it.

---

## What was built

| Section | Deliverable | Size |
|---|---|---|
| 1 — Coding (**Test Case 2**) | A production-minded blog REST API in Go | ~6,500 lines of Go, ~8,500 of tests, 23 endpoints, 4 tables |
| 2 — Architecture | E-commerce platform design | [1 document](ecommerce-architecture.md), 12 diagrams |
| 3 — Database | Social media schema, indexes, queries | [1 document](social-media-database-design.md) + 4 executable SQL files, 19 tables, 50 indexes |

The brief says to choose one coding test case, so Test Case 1 is not implemented
as a second application. Its skills appear where they do real work — see the
[traceability note](requirement-traceability.md#test-case-1-skills-demonstrated-without-a-second-application).

---

## Results that were measured, not estimated

| Check | Result |
|---|---|
| `go build ./...` · `go vet ./...` · `gofmt -l .` | clean |
| `go test ./...` | **379 test functions, 538 cases, all passing** |
| Coverage | **80.4% of statements** — 84–100% across the packages holding logic |
| Race detector | **clean across all 16 packages** |
| OpenAPI ↔ router contract test | **23 routes matched in both directions** |
| Docker build and `docker compose up` | **image builds; Postgres → migrations → API all healthy** |
| Live migration runner | **up, down and re-up against PostgreSQL 16.15** |
| Integration suite | **12 test functions, 25 cases, 0 skipped** |
| End-to-end smoke test | **all 46 checks passed** |
| Mermaid diagrams | **14 parsed by Mermaid 11.17.2 itself** |
| Blog API migration | **up and down apply; 10 constraints reject invalid rows** |
| Social-media SQL | **22 statements executed, 7 counters reconciled, 11 invalid writes rejected** |

The last two ran against PostgreSQL 18.3 (compiled to WebAssembly via PGlite —
the genuine Postgres source, planner included); the Compose stack runs
PostgreSQL 16.15.

Everything above has been executed. What has **not** been measured: behaviour
under real concurrent load, and any actual deployment — the README's second
table says so.

---

## Eight defects found by running the work rather than reading it

This is the part worth the reviewer's attention, because it is the difference
between "looks right" and "is right".

| # | Defect | Found by |
|---|---|---|
| 1 | Log redaction matched `api_key` but **not `X-Api-Key`** — a real credential leak | A unit test logging several credential-shaped keys |
| 2 | A missing keyset tiebreaker in `follows_followee_created_idx` | `EXPLAIN` showing an `Incremental Sort` |
| 3 | A partial index whose predicate **contradicted its own query**, so the plan was a `Seq Scan` removing 7,292 rows to return four | `EXPLAIN` on the seeded database |
| 4 | A parameter CTE that hid `LIMIT` from the planner, costing the feed query its early-terminating index scan | `EXPLAIN` |
| 5 | A **double-decremented notification counter** — the query updated a badge a trigger already maintains | Reconciling every counter after running every statement |
| 6 | **`docker compose up` failed outright.** Two scripts were committed without the executable bit (developed on Windows), so Postgres's init script died, the container restarted, and initialisation was silently skipped — no `blog_test` database, and `api` never started | Running `make up` on a machine with Docker |
| 7 | **A 500 on every token refresh.** `Rotate` set `replaced_by` before inserting the row it references, violating a non-deferrable self-referencing foreign key. Refresh rotation — and the theft detection built on it — was entirely broken | `make smoke`, 4 of 46 checks failing |
| 8 | **A test that passed for the wrong reason.** The readiness probe test relied on nothing listening on port 5432, so it would fail for anyone running `make up && make test`, or on any CI runner with a Postgres service | Running the suite with the stack up |

Defects 2–5 are documented at
[social-media-database-design.md §8](social-media-database-design.md#8-defects-found-by-actually-running-this);
defect 1 at [ai-usage.md §4.3](ai-usage.md#43-found-by-a-test-a-real-gap-in-log-redaction);
defects 6–8 at [ai-usage.md §5](ai-usage.md#5-defects-found-by-running-the-previously-unverified-checks).

Defect 7 is the one worth discussing in an interview. The unit tests did not
merely miss it — `pgxmock` does not enforce foreign keys, and the test asserted
the two statements in their original, wrong order. The test *encoded* the bug and
would have rejected the correct implementation. It is a compact argument for why
a mock-only repository suite is not sufficient evidence.

Both verification harnesses are committed — [`scripts/verify-sql.mjs`](../scripts/verify-sql.mjs)
and [`scripts/check-mermaid.mjs`](../scripts/check-mermaid.mjs) — so every claim
above can be re-run.

---

## Key architectural decisions

Twelve ADRs in [`architecture-decisions.md`](architecture-decisions.md). The six
most consequential:

1. **Modular monolith, feature-first packages.** Three microservices for three
   tables would be all cost. Dependencies point one way; interfaces are declared
   by the consumer.
2. **Stateless access tokens, stateful rotating refresh tokens.** No database
   read on the hot path, sessions still revocable. Rotation makes theft
   *detectable*: replaying a rotated token revokes every session for the
   account. The cost — a 15-minute worst-case revocation delay — is stated
   everywhere it matters.
3. **Transaction boundaries in repositories**, next to the SQL. Four of them,
   each tested for commit *and* rollback.
4. **Invariants in the database, not only in code.** A published post always has
   `published_at`; `comment_count >= 0`; a reply is forced onto its parent's
   post by a composite foreign key. All verified by attempting the invalid write.
5. **404, not 403, for invisible drafts** — consistently across read, update,
   delete and comment, so no endpoint becomes an existence oracle.
6. **Verify rather than assert.** The OpenAPI contract test, the Mermaid parser
   and the PGlite harness exist because a claim nobody can re-run is not much
   better than no claim.

---

## Known limitations

The [full list is in the README](../README.md#known-limitations). The four that
would matter first in production:

1. Access tokens cannot be revoked before expiry (15 min).
2. Rate limiting is per-process, so the effective limit scales with replicas.
3. Events are at-most-once and in-process — lost if the process is killed. The
   outbox upgrade path is specified.
4. No metrics endpoint; structured logs and health checks only.

---

## Reading order

**Twenty minutes:**

1. [`README.md`](../README.md) — verified results, then the two tables of
   trade-offs and limitations.
2. [`internal/comment/repository.go`](../internal/comment/repository.go) — the
   most interesting transaction, and why statement order matters.
3. [`docs/ecommerce-architecture.md`](ecommerce-architecture.md) §7 and §8 —
   inventory correctness and the six named failure scenarios.
4. [`docs/social-media-database-design.md`](social-media-database-design.md) §8 —
   the four defects.

**Running it:**

```bash
docker compose up --build -d && ./scripts/smoke.sh
```

---

## Repository

```
cmd/            two binaries: the API and the migration CLI
internal/       config, domain, auth, user, post, comment, health,
                middleware, server, platform/*
migrations/     versioned SQL, embedded in the binary
api/            OpenAPI 3, embedded and served at /docs
tests/          the OpenAPI contract test; build-tagged integration suite
database-assessment/   section 3: schema, indexes, queries, seed
docs/           ADRs, sections 2 and 3, traceability, AI usage
scripts/        smoke test, SQL verifier, Mermaid verifier
```

Every commit is one logical change, and the message says why rather than what.
AI usage is disclosed in full in [`ai-usage.md`](ai-usage.md), and each commit
carries a `Co-Authored-By: Claude` trailer — kept deliberately, so the git
history agrees with that disclosure instead of quietly contradicting it.
