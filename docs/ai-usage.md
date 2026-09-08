# AI Usage Disclosure

The brief encourages AI tooling and assesses three things: efficiency gained,
critical thinking about what the tool suggests, and ownership of the result.
This is the honest account.

---

## 1. What was used

| | |
|---|---|
| Tool | **Claude (Opus 5)**, agentic terminal workflow |
| Mode | Multi-turn; the model read and wrote files, ran commands, and iterated on failures |
| Human input | An initial brief plus the assessment PDF; direction and review throughout |
| Elapsed | One working session |

The workflow was not "generate code, paste, submit". It was: plan against the
brief, implement a slice, **run it**, fix what broke, commit, repeat. Almost
everything valuable in this submission came from the *run it* step.

---

## 2. What AI contributed, and what that means

| Area | AI's contribution | What that leaves |
|---|---|---|
| Boilerplate — DTOs, handler scaffolding, table-driven test skeletons | Nearly all | Genuine speed-up; low risk; mechanically checkable |
| Repository SQL | Drafted, then revised | Every statement read line by line. The window-function pagination, the recursive CTE and the composite foreign key were deliberate choices, not accepted defaults |
| Architecture and package layout | Drafted from a stated intent | The intent — feature-first, consumer-defined interfaces, transaction boundaries in repositories — was the direction given; AI turned it into files |
| Security decisions | Drafted, then tightened | Several were changed. See §4 |
| Migrations and constraints | Drafted | Each constraint was justified against a specific failure it prevents; several were added that AI did not propose |
| Tests | Drafted at volume | Volume is where AI is strongest. What was steered was *which* behaviours to test — the negative and security cases, not more happy paths |
| Documentation | Drafted | Restructured repeatedly for a reader who has 20 minutes |
| Verification tooling | Drafted on request | The idea of executing the SQL and the diagrams rather than asserting them was the direction; it changed the outcome materially |

**A fair summary:** AI wrote most of the characters. The decisions that make
this submission what it is — verify rather than assert, state costs plainly,
put invariants in the database, do not implement a second test case just to look
thorough — were the human contribution, and each is defensible in an interview.

---

## 3. How the output was verified

This is the part that matters, because AI-generated code is confidently wrong in
ways that read well.

### Executed, not assumed

| Claim | How it was established |
|---|---|
| It compiles | `go build ./...` after every meaningful change |
| No vet findings | `go vet ./...` **and** `go vet -tags=integration ./...` |
| Formatted | `gofmt -l .` — must produce no output |
| Tests pass | `go test -count=1 ./...` — 344 functions, 481 cases |
| Coverage is 71.5% | `go tool cover -func`, read off the tool. Not estimated, not rounded up |
| The OpenAPI spec matches the code | A contract test walks Chi's routes and compares both directions — 23 operations |
| The Mermaid diagrams render | Parsed by Mermaid 11.17.2 itself via jsdom, not by eye |
| The section-3 SQL is valid | Executed against real PostgreSQL 18.3 (PGlite/WASM): schema, indexes, seed, and all 22 queries with bound parameters |
| The section-3 constraints work | 11 deliberately invalid writes attempted; all 11 rejected |
| The section-3 counters are correct | 7 trigger-maintained counters reconciled against their source rows — zero drift |

### Previously not claimed — and what happened when they were run

For most of this project's life the Docker build, Compose startup, migrations
against a live server, the integration suite, the smoke script and the race
detector were **not run**: the machine it was developed on had no Docker, no
PostgreSQL and no C toolchain. They were written, they compiled, and the README
said plainly that they were unverified.

It would have been easy to write "Docker build: ✅" instead. That is exactly the
kind of unverified claim this section exists to rule out — and the vindication
is [section 5](#5-defects-found-by-running-the-previously-unverified-checks):
when those checks were finally executed on a machine that could run them, three
of them failed, and one was a **500 on every token refresh**. Marking them green
would have shipped a broken authentication flow with a tick beside it.

---

## 4. Suggestions that were corrected, rejected or simplified

Concrete cases. Most were found by *running* something, not by reading it.

### 4.1 Rejected: an `ErrSlugTaken` sentinel that could never match

The first draft declared `var ErrSlugTaken = apierr.Conflict(...)` and the
service retried on `errors.Is(err, ErrSlugTaken)`. That never matches: the
`apierr` constructors return fresh values and `WithCause` returns a copy, so
`errors.Is` compares two different pointers and is always false. The retry loop
would have been dead code — and the failure mode is silent, a 409 to the user
instead of a retried slug.

Fixed by making the sentinel a plain `errors.New` wrapped *inside* the
client-facing error, so the chain matches. `internal/post/repository.go` carries
the reasoning.

### 4.2 Corrected: context errors reported as 500

Repositories wrap every driver error with `apierr.Internal`. A query aborted by
the request deadline therefore surfaced as `500`. It is not a server fault — the
client's deadline expired — and reporting it as one inflates the error rate and
pages the wrong person.

`apierr.Internal` now reclassifies `context.DeadlineExceeded` to 504 and
`context.Canceled` to 499. Covered by `TestContextErrorsAreReclassified`.

### 4.3 Found by a test: a real gap in log redaction

The redaction list matched `password`, `token`, `api_key` — and **not
`X-Api-Key`**, because the needle had an underscore and the header a hyphen. A
test that logged several credential-shaped keys caught it.

Fixed by stripping separators before matching, so one entry covers `api_key`,
`apiKey` and `X-Api-Key`. This is the clearest example in the submission of a
test earning its place: the code looked right, and it leaked.

### 4.4 Rejected: global mutable state to share a context key

To let the event bus read the request ID, an early draft added a package-level
`SetRequestIDKey(key any)` called at start-up. That is global mutable state — the
one thing the brief explicitly rules out — introduced to avoid an import that
was not actually circular. Replaced by importing `httpx` directly.

### 4.5 Simplified: a panic on `crypto/rand` failure

Slug generation had an error branch panicking if `crypto/rand.Read` failed. As
of Go 1.24 that function is documented never to return an error. The branch was
dead, and it violated "avoid panics for normal application failures" while
looking careful. Removed, with a comment explaining why there is no error check.

### 4.6 Found by running the SQL: four defects in section 3

None of these would have been caught by reading. All four are documented at
[`social-media-database-design.md` §8](social-media-database-design.md#8-defects-found-by-actually-running-this).

1. **A missing keyset tiebreaker** in `follows_followee_created_idx`. `EXPLAIN`
   showed an `Incremental Sort`. The same mistake had already been avoided in
   the posts index, which is exactly why it was worth catching where it slipped.
2. **A partial index that contradicted its query.** `comments_post_path_idx` was
   partial on `deleted_at IS NULL`, matching every other soft-deleted table —
   but the thread query deliberately returns deleted comments as tombstones, so
   it could not use the index at all. The plan was a `Seq Scan` removing 7,292
   rows to return four. This one is instructive: the index looked *more*
   carefully designed than the fix.
3. **A parameter CTE that hid `LIMIT` from the planner**, costing the feed query
   its early-terminating index scan.
4. **A double-decremented counter.** The "mark all notifications read" query
   updated a badge that a trigger already maintains. Caught only because the
   harness runs every statement and *then* reconciles every counter.

### 4.7 Corrected: an over-eager `Optional` auth middleware

The draft silently ignored an invalid token on optionally-authenticated routes.
That means an author whose session has expired sees the *public* view of their
own drafts — their posts appear to have vanished, with no error to explain it.
Changed to reject an invalid token even where authentication is optional, with
the reasoning in the code.

### 4.8 Simplified: a "clever" compile-time assertion

A `const _ = uint(auth.MaxPasswordBytes - 72)` was inserted to make a mismatch a
compile error. It only catches one direction, and it takes longer to understand
than the comment it replaced. Removed in favour of a plain comment.

---

## 5. Defects found by running the previously unverified checks

The checks in §3 that could not run on the original development machine were
later executed on one with Docker, PostgreSQL and a C toolchain. **Three of the
seven failed.** Each failure is the kind that only execution finds.

### 5.1 `docker compose up` failed outright — a missing executable bit

`make up` aborted with `container for service "postgres" is unhealthy`. The
cause was two files committed as mode `100644` instead of `100755`:
`scripts/init-test-db.sh` and `scripts/smoke.sh`. The project was developed on
Windows, where Git does not record the executable bit.

The failure chain is worth tracing, because the symptom is three steps from the
cause. Postgres's entrypoint tried to execute the init script, got
`bad interpreter: Permission denied`, and the container died. `restart:
unless-stopped` brought it back, and on the second start the entrypoint found a
populated data directory and **skipped initialisation entirely** — so
`blog_test` was never created, silently. The `api` and `migrate` containers
never started at all.

Three documented reviewer commands were broken by this: `make up`, `make smoke`
(the script was not executable either) and `make test-integration` (no
`blog_test` database). Fixed with `git update-index --chmod=+x`. Note that
`.gitattributes` was already correct and line endings were never the problem —
the LF normalisation worked; it just does not cover file modes.

### 5.2 A 500 on every token refresh — a foreign key ordering bug

`make smoke` failed 4 of 46 checks. The API returned `500 internal_error` on
`POST /api/v1/auth/refresh`; the log gave the real reason:

```
insert or update on table "refresh_tokens" violates foreign key
constraint "refresh_tokens_replaced_by_fkey" (SQLSTATE 23503)
```

`SessionRepository.Rotate` ran its `UPDATE ... SET replaced_by = $2` **before**
the `INSERT` that creates the row `$2` refers to. `replaced_by` is a
self-reference and the constraint is not deferrable, so Postgres rejected it
immediately. Refresh-token rotation — and with it the theft-detection story this
submission makes a point of — was completely broken.

The fix was to insert the replacement first, then point the presented token at
it. The concurrency guarantee is unchanged: the `revoked_at IS NULL` guard on
the `UPDATE` still means exactly one of two concurrent refreshes wins, and a
zero-row update still rolls the transaction back, discarding the insert.

**Why the tests missed it.** `pgxmock` does not enforce foreign keys, and
`TestSessionRepositoryRotateCommitsBothStatements` asserted the statements in
their original — wrong — order. The test did not merely fail to catch the bug;
it *encoded* it, and would have rejected the correct implementation. All three
rotation tests were updated, and the happy-path test now documents that the
ordering is load-bearing rather than incidental.

This is precisely the risk §6 names below: mock-verified SQL cannot prove
validity against a schema. The warning was written before the bug was found, and
the bug is exactly the shape the warning predicted.

### 5.3 A test that only passed because nothing was listening

With the stack running, `TestReadinessFailsWithoutADatabase` failed: it expected
`503 degraded` and got `200 ok`. The test built the router against
`127.0.0.1:5432` and relied on nothing being there — true on the original
machine, false the moment `make up` works.

So the test passed for the wrong reason and would fail for any reviewer running
`make up && make test`, and on any CI runner with a Postgres service container.
Fixed by pointing that one router at port 1, where nothing serves, which also
keeps the probe's "must be bounded" assertion honest. The full suite now passes
both with and without a live database.

### 5.4 A documentation error: an inflated test-case count

The docs claimed "618 cases". The measured figure is **481** — 344 top-level
functions plus 137 subtests. The 618 came from adding the 137 subtests to a
total that already included them. Corrected everywhere it appeared.

It is a small number on a page of larger ones, and that is the point: a document
whose whole argument is "measured, not estimated" cannot afford an arithmetic
slip in its own evidence table.

---

## 6. What remains a risk

Being specific is more useful than a disclaimer.

| Risk | Assessment |
|---|---|
| **Unrun artefacts** | *Resolved.* Docker, Compose, live migrations, the integration suite, the smoke script and the race detector have now all been executed. This was correctly identified as the most likely place for a defect: three of the seven failed. See [§5](#5-defects-found-by-running-the-previously-unverified-checks) |
| **Section 1's SQL is mock-verified only** | *Partly resolved, and the warning was right.* `pgxmock` proves the statements are issued with the right arguments and that transactions commit or roll back; it cannot prove the SQL is valid against the schema. Running the integration suite and the smoke script against a real PostgreSQL found exactly that class of defect — a foreign-key ordering bug in refresh-token rotation ([§5.2](#52-a-500-on-every-token-refresh--a-foreign-key-ordering-bug)). The mock had asserted the wrong order and would have rejected the fix. Residual risk: the integration suite covers the main paths, not every statement |
| **PGlite is not a production server** | PostgreSQL 18.3 compiled to WebAssembly is the genuine source, but single-connection and 32-bit. Plan *shape* and constraint behaviour transfer; concurrency behaviour under real load does not |
| **No load testing** | Every performance statement is reasoning about algorithms and index shapes, not measurement. The e-commerce capacity figures are labelled as estimates with their derivation shown, precisely so they are not mistaken for data |
| **The architecture documents are designs** | Sections 2 and 3 are written deliverables. Nothing in section 2 has been built |
| **Volume of generated code** | ~6,500 lines of Go and ~8,500 of tests. Every file was read, but a defect can hide in that much text. The tests are the mitigation, and the coverage number is honest about where they do not reach |

---

## 7. Ownership

I can explain and defend every decision in this submission: why comment creation
increments the counter *before* inserting, why drafts return 404 rather than
403, why bcrypt over Argon2id, why offset pagination in section 1 and keyset in
section 3, and why the event bus drops rather than blocks.

Where a decision has a cost, the cost is stated — in the code comment, the ADR,
or the README's trade-offs table. Where something was not verified, it says so.

The most useful thing AI did here was not writing code faster. It was making it
cheap enough to *execute* the verification — the Mermaid parser, the PGlite
harness, the OpenAPI contract test, and later the full Docker stack — that
assertions became checks. **Eight real defects came out of that**, and none of
them would have been found by review. The three in
[§5](#5-defects-found-by-running-the-previously-unverified-checks) are the most
pointed, because they sat behind checks this document had honestly marked as
unrun rather than quietly ticked.

---

## 8. Reproducing the verification

```bash
# Go: build, vet, format, test, coverage
go build ./... && go vet ./... && gofmt -l .
go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1        # → total: 71.5%

# OpenAPI ↔ router contract
go test -count=1 -v ./tests/

# Mermaid diagrams
npm install --no-save mermaid@11 jsdom
node scripts/check-mermaid.mjs docs/*.md          # → 14 parsed, 0 failures

# Section 3 SQL against real PostgreSQL
npm install --no-save @electric-sql/pglite
node scripts/verify-sql.mjs                       # → all checks passed

# The database-backed checks (needs Docker)
make docker-build                                 # → image builds
make up                                           # → postgres → migrate → api, all healthy
make test-integration                             # → 12 functions, 25 cases, 0 skipped
make smoke                                        # → all 46 checks passed
make test-race                                    # → clean, 15 packages

# The live migration runner, including rollback
make migrate-status && make migrate-down && make migrate-up
```

The two Node scripts are committed for exactly this reason: a verification claim
nobody else can re-run is not much better than no claim at all.
