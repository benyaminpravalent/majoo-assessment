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
| `go test ./...` | **344 test functions, 618 cases, all passing** |
| Coverage | **71.5% of statements** — 84–100% across the packages holding logic |
| OpenAPI ↔ router contract test | **23 routes matched in both directions** |
| Mermaid diagrams | **14 parsed by Mermaid 11.17.2 itself** |
| Blog API migration | **up and down apply; 10 constraints reject invalid rows** |
| Social-media SQL | **22 statements executed, 7 counters reconciled, 11 invalid writes rejected** |

The last two ran against PostgreSQL 18.3 (compiled to WebAssembly via PGlite —
the genuine Postgres source, planner included).

**Not verified here, and the README says so in its second table:** the Docker
build, Compose startup, the Go migration runner against a live server, the
integration suite, the smoke script, and the race detector. This machine has no
Docker, no PostgreSQL and no C toolchain. Each has a one-line command to check
it.

---

## Five defects found by running the work rather than reading it

This is the part worth the reviewer's attention, because it is the difference
between "looks right" and "is right".

| # | Defect | Found by |
|---|---|---|
| 1 | Log redaction matched `api_key` but **not `X-Api-Key`** — a real credential leak | A unit test logging several credential-shaped keys |
| 2 | A missing keyset tiebreaker in `follows_followee_created_idx` | `EXPLAIN` showing an `Incremental Sort` |
| 3 | A partial index whose predicate **contradicted its own query**, so the plan was a `Seq Scan` removing 7,292 rows to return four | `EXPLAIN` on the seeded database |
| 4 | A parameter CTE that hid `LIMIT` from the planner, costing the feed query its early-terminating index scan | `EXPLAIN` |
| 5 | A **double-decremented notification counter** — the query updated a badge a trigger already maintains | Reconciling every counter after running every statement |

Defects 2–5 are documented at
[social-media-database-design.md §8](social-media-database-design.md#8-defects-found-by-actually-running-this);
defect 1 at [ai-usage.md §4.3](ai-usage.md#43-found-by-a-test-a-real-gap-in-log-redaction).

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

16 commits, each one logical change. AI usage is disclosed in full in
[`ai-usage.md`](ai-usage.md), and every commit carries a `Co-Authored-By: Claude`
trailer — say the word if you would prefer it removed before submission.
