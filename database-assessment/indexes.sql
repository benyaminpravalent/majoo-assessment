-- =========================================================================
-- Social media platform — index design
--
-- Assessment section 3, task 3: "Design indexes for common queries".
--
-- Apply after schema.sql:
--   psql "$DATABASE_URL" -f database-assessment/indexes.sql
--
-- Every index below is documented with the same five things the brief asks for:
--
--   QUERY       the statement it exists to serve, by section in queries.sql
--   ORDERING    why the columns are in this order
--   SELECTIVITY how much of the table a lookup is expected to touch
--   SHAPE       partial, covering, unique — and why
--   COST        what it costs on write and on disk
--
-- An index with no named query is an index nobody can justify removing later,
-- so there are none of those here.
--
-- Note on CONCURRENTLY: these run inside no transaction block on purpose in
-- production, where CREATE INDEX CONCURRENTLY avoids taking a write lock on a
-- live table. In this file they are plain CREATE INDEX so the script can be run
-- in one go against an empty database. The production migration order is noted
-- at the bottom.
-- =========================================================================

-- ==========================================================================
-- 1. Users — profile lookup
-- ==========================================================================

-- Handle and email uniqueness are already enforced by constraints in
-- schema.sql, and those constraints create the indexes that serve
--     SELECT ... FROM users WHERE handle = $1
-- (queries.sql §1). Because handle and email are CITEXT, those lookups are
-- case-insensitive without a functional index. No extra index is needed, and
-- adding one would be pure write overhead.
--
--   QUERY        §1 profile lookup by handle; login by email
--   ORDERING     single column
--   SELECTIVITY  1 row — perfect
--   SHAPE        unique, via the constraint
--   COST         already paid; unavoidable for correctness

-- Fuzzy search over handles, for the "find people" box.
--
--   QUERY        SELECT ... WHERE handle ILIKE '%ben%'
--   ORDERING     n/a — GIN over trigrams
--   SELECTIVITY  varies; trigram GIN is what makes a leading-wildcard LIKE
--                usable at all, since a B-tree cannot serve one
--   SHAPE        GIN with the trigram operator class
--   COST         Noticeably more expensive to update than a B-tree and several
--                times larger. Justified only because substring search over
--                handles is a product requirement; if it were not, this index
--                should not exist.
CREATE INDEX users_handle_trgm_idx
    ON users USING GIN (handle gin_trgm_ops);

-- Active users only, for admin listings and analytics.
--
--   QUERY        listings that exclude deactivated accounts
--   ORDERING     newest first
--   SELECTIVITY  ~99% of rows in a healthy system, so this is NOT a filtering
--                index — it exists to provide the sort order cheaply
--   SHAPE        partial on deactivated_at IS NULL
--   COST         Small. Reconsider it if admin listings turn out to be rare;
--                it is the most speculative index in this file.
CREATE INDEX users_active_created_idx
    ON users (created_at DESC)
    WHERE deactivated_at IS NULL;

-- ==========================================================================
-- 2. Follow graph
-- ==========================================================================

-- "Who does this user follow?" and "does A follow B?" are both served by the
-- follows primary key (follower_id, followee_id) created in schema.sql.
--
--   QUERY        §2 following list; the follow check inside feed queries
--   ORDERING     follower_id leads because it is the equality predicate
--   SELECTIVITY  one user's following set — hundreds to thousands of rows
--   SHAPE        unique, via the primary key
--   COST         already paid

-- The reverse direction: "who follows this user?" The primary key cannot serve
-- it, because followee_id is not the leading column.
--
--   QUERY        §3 followers list; fan-out on post creation, which reads the
--                entire follower set and is the highest-volume use of this index
--   ORDERING     (followee_id, created_at DESC, follower_id DESC) — equality
--                first, then the sort key, then the keyset tiebreaker.
--                The third column is not decoration. §3 orders by
--                (created_at DESC, follower_id DESC) so that pagination has a
--                total order; without follower_id in the index, EXPLAIN shows an
--                Incremental Sort above the scan. That was observed on the
--                seeded database and is why this column is here.
--   SELECTIVITY  one user's followers: a handful for most, millions for a few
--   SHAPE        plain composite B-tree
--   COST         One more index to maintain per follow, slightly wider for the
--                extra column. Unavoidable: without it, fan-out becomes a
--                sequential scan of the whole edge table.
CREATE INDEX follows_followee_created_idx
    ON follows (followee_id, created_at DESC, follower_id DESC);

-- Same shape for the forward direction, so the *following* list can also be
-- ordered by recency without a sort. The primary key already covers
-- (follower_id, ...) for lookups, but its second column is followee_id in
-- ascending order, not created_at, so ordering by recency needs this index.
--
--   QUERY        §2 following list, ordered newest first
--   ORDERING     equality, sort key, keyset tiebreaker — as above
--   SELECTIVITY  one user's following set
--   SHAPE        plain composite
--   COST         a second index on the same table; accepted because both
--                directions are user-facing screens
CREATE INDEX follows_follower_created_idx
    ON follows (follower_id, created_at DESC, followee_id DESC);

-- Pending follow requests, for the recipient's approval screen.
--
--   QUERY        SELECT ... FROM follow_requests WHERE target_id = $1
--   ORDERING     target_id first — the equality predicate
--   SELECTIVITY  very high; most users have none pending
--   SHAPE        plain composite
--   COST         negligible; the table stays small
CREATE INDEX follow_requests_target_idx
    ON follow_requests (target_id, created_at DESC);

-- Block checks run on nearly every read path that shows one user's content to
-- another, so both directions need to be cheap.
--
--   QUERY        "has B blocked A?" during feed and profile rendering
--   ORDERING     blocked_id leads; the primary key already covers blocker_id
--   SELECTIVITY  near-perfect
--   SHAPE        plain
--   COST         negligible
CREATE INDEX blocks_blocked_idx
    ON blocks (blocked_id);

-- ==========================================================================
-- 3. Posts
-- ==========================================================================

-- A user's own posts, newest first — the profile timeline.
--
--   QUERY        §4 user posts
--   ORDERING     (author_id, created_at DESC, id DESC). author_id is the
--                equality predicate and must lead. created_at supplies the
--                sort. id breaks ties, which is what makes keyset pagination
--                exact: without a total order, two posts sharing a timestamp
--                can be returned twice or skipped at a page boundary.
--   SELECTIVITY  one author's posts
--   SHAPE        partial on deleted_at IS NULL. Deleted posts are never listed,
--                so excluding them keeps the index smaller and lets the planner
--                use it without re-checking the predicate.
--   COST         one index write per post; the partial predicate means a
--                soft-deleted post is removed from the index on delete
CREATE INDEX posts_author_created_idx
    ON posts (author_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

-- Public posts, newest first. Serves the discovery timeline and the read-time
-- merge of celebrity posts into a home feed.
--
--   QUERY        §7 fan-out-on-read merge; discovery
--   ORDERING     created_at DESC, id DESC — the keyset cursor
--   SELECTIVITY  the whole table, but only ever the newest page of it
--   SHAPE        partial on visibility = 'public' AND deleted_at IS NULL,
--                which is precisely the predicate every such query carries
--   COST         moderate; the partial predicate excludes private and deleted
--                posts entirely
CREATE INDEX posts_public_created_idx
    ON posts (created_at DESC, id DESC)
    WHERE deleted_at IS NULL AND visibility = 'public';

-- Full-text search over post bodies.
--
--   QUERY        SELECT ... WHERE search_vector @@ websearch_to_tsquery($1)
--   ORDERING     n/a
--   SELECTIVITY  varies by term
--   SHAPE        GIN over the STORED generated column, so the index can never
--                disagree with the row — a trigger-maintained tsvector can,
--                after a bulk UPDATE that forgets to fire it
--   COST         GIN is slower to update than B-tree and larger on disk. Posts
--                are read far more often than written, so this is the right way
--                round.
CREATE INDEX posts_search_idx
    ON posts USING GIN (search_vector);

-- Replies and shares of a given post.
--
--   QUERY        "show the thread"; "who shared this?"
--   ORDERING     single column each
--   SELECTIVITY  high; most posts are neither shared nor replied to
--   SHAPE        partial on IS NOT NULL — the overwhelming majority of posts
--                have NULL here, and indexing those entries would roughly
--                double the index for no benefit
--   COST         small
CREATE INDEX posts_reply_to_idx
    ON posts (reply_to_post_id, created_at DESC)
    WHERE reply_to_post_id IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX posts_shared_from_idx
    ON posts (shared_post_id)
    WHERE shared_post_id IS NOT NULL AND deleted_at IS NULL;

-- Media belonging to a post, in display order.
--
--   QUERY        hydrating a post's attachments
--   ORDERING     already provided by the post_media_position_unique constraint
--                on (post_id, position), which is exactly the lookup and the
--                sort. No additional index is needed — noted here so a future
--                reader does not add a redundant one.

-- ==========================================================================
-- 4. Comments
-- ==========================================================================

-- A post's comment thread, in reading order.
--
--   QUERY        §5 comments with reply structure
--   ORDERING     (post_id, path). post_id is the equality predicate; path is a
--                materialised sibling path whose lexicographic order IS thread
--                order, so this single index gives both the filter and the sort
--                with no sort node and no recursive CTE at read time.
--   SELECTIVITY  one post's comments
--   SHAPE        NOT partial — and this is the interesting part.
--
--                The obvious shape here is `WHERE deleted_at IS NULL`, matching
--                every other soft-deleted table in this schema. It is wrong for
--                this one. Query §5 deliberately returns deleted comments as
--                tombstones, because omitting a deleted parent would orphan its
--                replies in the rendered thread. A query with no
--                `deleted_at IS NULL` predicate cannot use an index that has
--                one, so the partial version left the planner with a sequential
--                scan of the whole comments table — confirmed on the seeded
--                database, which showed "Seq Scan on comments, Rows Removed by
--                Filter: 7292" for a thread of four.
--
--                A partial index is only a win when its predicate matches the
--                query's. Here it silently did the opposite.
--   COST         Larger than the partial version by the proportion of deleted
--                comments, and one index write per comment. The path column
--                must be computed on insert, which is the price of making reads
--                a single ordered range scan.
CREATE INDEX comments_post_path_idx
    ON comments (post_id, path);

-- Direct replies to a specific comment, for "load more replies".
--
--   QUERY        lazy-loading a subtree
--   ORDERING     parent_id, then chronological
--   SELECTIVITY  high
--   SHAPE        partial on parent_id IS NOT NULL — top-level comments are the
--                majority and would otherwise bloat the index
--   COST         small
CREATE INDEX comments_parent_created_idx
    ON comments (parent_id, created_at)
    WHERE parent_id IS NOT NULL AND deleted_at IS NULL;

-- A user's comment history, for their profile and for moderation.
--
--   QUERY        "comments by this user"
--   SELECTIVITY  one author's comments
--   SHAPE        partial
--   COST         small
CREATE INDEX comments_author_created_idx
    ON comments (author_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- ==========================================================================
-- 5. Reactions
-- ==========================================================================

-- "Which reaction did I leave on this post?" is served by the
-- post_reactions primary key (post_id, user_id) from schema.sql.
--
--   QUERY        §6 reaction counts; per-viewer reaction state
--   ORDERING     post_id leads, so "all reactions on this post" is a range scan
--                and "this user's reaction on this post" is a point lookup
--   SELECTIVITY  point lookup: 1 row
--   SHAPE        unique, via the primary key
--   COST         already paid

-- The reverse: "everything this user has reacted to", for their activity feed.
--
--   QUERY        user activity history
--   ORDERING     user_id first, then recency
--   SELECTIVITY  one user's reactions
--   SHAPE        plain composite
--   COST         a second index on a very high-volume table. This is the most
--                expensive optional index here; if the activity feed is dropped
--                from the product, drop this with it.
CREATE INDEX post_reactions_user_created_idx
    ON post_reactions (user_id, created_at DESC);

-- Grouped counts per kind are read directly from post_reaction_counts, whose
-- primary key (post_id, kind) is the whole access path. No GROUP BY over
-- post_reactions is ever needed for display — see queries.sql §6, which shows
-- both the aggregate query and the counter read, and explains when each is
-- appropriate.

CREATE INDEX comment_reactions_user_idx
    ON comment_reactions (user_id, created_at DESC);

-- ==========================================================================
-- 6. Conversations and messages
-- ==========================================================================

-- A user's conversation list, most recently active first.
--
--   QUERY        §8 conversation list with latest message
--   ORDERING     This index is on conversations, but the query filters by
--                participant. The join order is: participant rows for the user
--                (from the conversation_participants primary key), then
--                conversations by id. Sorting by last_message_at then needs
--                this index only when the user has many conversations.
--   SELECTIVITY  moderate
--   SHAPE        plain, DESC NULLS LAST so empty conversations sort last
--   COST         one write per message sent, since last_message_at changes
CREATE INDEX conversations_last_message_idx
    ON conversations (last_message_at DESC NULLS LAST);

-- "Which conversations is this user in?" — the driving side of the list query.
--
--   QUERY        §8
--   ORDERING     user_id leads; the primary key is (conversation_id, user_id)
--                and therefore cannot serve a user-first lookup
--   SELECTIVITY  one user's conversations
--   SHAPE        partial on left_at IS NULL — a user who left a group should
--                not see it in their list
--   COST         small
CREATE INDEX conversation_participants_user_idx
    ON conversation_participants (user_id)
    WHERE left_at IS NULL;

-- Messages within a conversation, in order.
--
--   QUERY        §9 message history with keyset pagination
--   ORDERING     Already provided by messages_seq_unique on
--                (conversation_id, seq) in schema.sql. seq is monotonic within
--                a conversation, so this one index gives the filter, the total
--                order and the keyset cursor. Ordering by created_at would need
--                a separate index and would still be ambiguous when two
--                messages share a timestamp.
--   SELECTIVITY  one conversation
--   SHAPE        unique, via the constraint
--   COST         already paid

-- Unread counting.
--
--   The unread count for a conversation is
--       (SELECT max(seq) FROM messages WHERE conversation_id = c) - last_read_seq
--   which needs no index beyond messages_seq_unique: max(seq) over a
--   conversation is a backward index scan returning one row.
--
--   This is why the read cursor is a sequence number rather than a
--   message_reads table. With a per-message read table, the same question would
--   be an anti-join over every message in the conversation.

-- Total unread across all conversations, for the app badge.
--
--   QUERY        §10 unread message count
--   ORDERING     user_id leads
--   SELECTIVITY  one user
--   SHAPE        partial on left_at IS NULL, covering last_read_seq via INCLUDE
--                so the count can be answered from the index alone
--   COST         small
CREATE INDEX conversation_participants_unread_idx
    ON conversation_participants (user_id, conversation_id)
    INCLUDE (last_read_seq)
    WHERE left_at IS NULL;

-- Message search within a conversation is deliberately not indexed here.
-- Adding a GIN index over message bodies would be expensive on a table with the
-- highest write rate in the system, and message search is better served by an
-- external index if the product needs it at all.

-- ==========================================================================
-- 7. Notifications
-- ==========================================================================

-- The notification list: one user's notifications, newest first.
--
--   QUERY        §11 notification retrieval
--   ORDERING     (recipient_id, created_at DESC, id DESC) — equality, sort,
--                tiebreaker, so the list paginates by keyset with no sort node
--   SELECTIVITY  one user's notifications
--   SHAPE        plain composite
--   COST         one write per notification
CREATE INDEX notifications_recipient_created_idx
    ON notifications (recipient_id, created_at DESC, id DESC);

-- Unread notifications only. This is a separate, much smaller index rather than
-- a filter on the one above.
--
--   QUERY        §11 with the unread filter; §12 mark-all-as-read
--   ORDERING     same as above
--   SELECTIVITY  Very high in practice: most notifications are read, so this
--                index holds a small fraction of the table and stays in cache.
--                A partial index whose predicate matches the query exactly is
--                the single most effective index shape available in PostgreSQL,
--                and this is the clearest example of it in the schema.
--   SHAPE        partial on read_at IS NULL
--   COST         Rows leave the index when marked read, which is a cheap
--                index-tuple deletion, and the index shrinks over time rather
--                than growing.
CREATE INDEX notifications_unread_idx
    ON notifications (recipient_id, created_at DESC)
    WHERE read_at IS NULL;

-- Aggregation lookup: find the existing notification to bump instead of
-- inserting a duplicate.
--
--   QUERY        the ON CONFLICT upsert that implements notification dedup
--   ORDERING     recipient_id, dedup_key — both are equality predicates
--   SELECTIVITY  point lookup
--   SHAPE        UNIQUE and partial. Unique is what makes ON CONFLICT work at
--                all. Partial on read_at IS NULL is the interesting part: once
--                a user has read "Alice liked your post", a new like should
--                create a *fresh* unread notification rather than resurrecting
--                the read one. Scoping uniqueness to unread rows expresses that
--                exactly.
--   COST         small
CREATE UNIQUE INDEX notifications_dedup_idx
    ON notifications (recipient_id, dedup_key)
    WHERE read_at IS NULL;

-- ==========================================================================
-- 8. Feed materialisation
-- ==========================================================================

-- The precomputed home feed, newest first.
--
--   QUERY        §7 hybrid feed, materialised half
--   ORDERING     (user_id, created_at DESC, post_id DESC). user_id is the
--                equality predicate; created_at is the cursor; post_id makes
--                the order total so keyset pagination cannot skip or repeat.
--   SELECTIVITY  one user's feed window
--   SHAPE        plain composite. Not partial: every row in this table is a
--                candidate for someone's feed.
--   COST         The heaviest write cost in the schema. A post by a user with
--                10,000 followers writes 10,000 rows here, each maintaining
--                this index. That cost is precisely why celebrities are
--                excluded from fan-out; see the design document, §6.
CREATE INDEX feed_entries_user_created_idx
    ON feed_entries (user_id, created_at DESC, post_id DESC);

-- Deleting a post must remove it from every materialised feed it reached.
--
--   QUERY        DELETE FROM feed_entries WHERE post_id = $1
--   SELECTIVITY  one post's fan-out — potentially many rows
--   SHAPE        plain
--   COST         Necessary. Without it, deleting a post is a sequential scan of
--                the largest table in the database.
CREATE INDEX feed_entries_post_idx
    ON feed_entries (post_id);

-- Unfollowing must remove that author's posts from the follower's feed.
--
--   QUERY        DELETE FROM feed_entries WHERE user_id = $1 AND author_id = $2
--   ORDERING     both are equality predicates; user_id is more selective first
--   SHAPE        plain composite
--   COST         a third index on the largest table. Justified because the
--                alternative is leaving unfollowed authors in the feed, which
--                is a visible bug.
CREATE INDEX feed_entries_user_author_idx
    ON feed_entries (user_id, author_id);

-- ==========================================================================
-- 9. Notes on rollout and maintenance
-- ==========================================================================
--
-- Building these in production:
--
--   CREATE INDEX CONCURRENTLY ... ;
--
-- CONCURRENTLY avoids an exclusive lock on a live table. It cannot run inside a
-- transaction block, takes roughly twice as long, and can leave an INVALID
-- index if it fails — so the migration that creates it must check
-- pg_index.indisvalid afterwards and drop-and-retry on failure.
--
-- Keeping them honest:
--
--   -- Indexes nobody uses. Drop them: they cost writes and buy nothing.
--   SELECT relname, indexrelname, idx_scan, pg_size_pretty(pg_relation_size(indexrelid))
--     FROM pg_stat_user_indexes
--    WHERE idx_scan < 50
--    ORDER BY pg_relation_size(indexrelid) DESC;
--
--   -- Tables being sequentially scanned despite having indexes.
--   SELECT relname, seq_scan, idx_scan, n_live_tup
--     FROM pg_stat_user_tables
--    WHERE seq_scan > idx_scan AND n_live_tup > 10000
--    ORDER BY seq_scan DESC;
--
--   -- Index bloat, which develops on heavily-updated tables and is fixed with
--   -- REINDEX CONCURRENTLY.
--   SELECT indexrelname, pg_size_pretty(pg_relation_size(indexrelid))
--     FROM pg_stat_user_indexes
--    ORDER BY pg_relation_size(indexrelid) DESC
--    LIMIT 20;
--
-- The general rule this file follows: an index is added when a named query
-- needs it, and removed when that query goes away. Indexes accumulated "just in
-- case" are paid for on every write, forever.
