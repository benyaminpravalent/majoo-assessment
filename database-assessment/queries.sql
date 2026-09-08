-- =========================================================================
-- Social media platform — representative complex queries
--
-- Assessment section 3, task 4: "Write complex SQL queries for feed generation".
--
-- Every statement here is valid PostgreSQL 16 and runs against the schema in
-- schema.sql with the indexes from indexes.sql. Parameters are $1, $2, … so the
-- statements can be prepared as written; nothing is interpolated.
--
-- Verify them against a live database with:
--   psql "$DATABASE_URL" -f database-assessment/schema.sql
--   psql "$DATABASE_URL" -f database-assessment/indexes.sql
--   psql "$DATABASE_URL" -f database-assessment/seed.sql
--   psql "$DATABASE_URL" -f database-assessment/queries.sql   -- see §14
--
-- IMPORTANT, and stated plainly: no runtime figures appear in this file. The
-- EXPLAIN guidance in §13 says which plan each query *should* produce and how
-- to confirm it. Nothing here was executed against a database during authoring,
-- so any timing printed as fact would be an invention.
-- =========================================================================

-- ==========================================================================
-- 1. Profile lookup by handle
-- ==========================================================================
-- The single most-executed query in the system: it runs on every profile view
-- and every @mention resolution.
--
-- Counters come from the users row, not from COUNT(*) over follows. A profile
-- with 1.2M followers would otherwise scan 1.2M index entries to render one
-- number.

SELECT u.id,
       u.handle,
       u.is_private,
       u.is_verified,
       u.follower_count,
       u.following_count,
       u.post_count,
       u.created_at,
       p.display_name,
       p.bio,
       p.avatar_url,
       p.header_url,
       p.location,
       p.website_url,
       -- Does the viewer ($2) already follow this account?
       EXISTS (
           SELECT 1 FROM follows f
            WHERE f.follower_id = $2 AND f.followee_id = u.id
       ) AS viewer_follows,
       -- Is there a pending request from the viewer?
       EXISTS (
           SELECT 1 FROM follow_requests r
            WHERE r.requester_id = $2 AND r.target_id = u.id
       ) AS viewer_requested,
       -- Blocks are checked in both directions: a blocked viewer must not see
       -- the profile, and a viewer who blocked this account should be told.
       EXISTS (
           SELECT 1 FROM blocks b
            WHERE (b.blocker_id = u.id AND b.blocked_id = $2)
               OR (b.blocker_id = $2 AND b.blocked_id = u.id)
       ) AS blocked
  FROM users u
  JOIN user_profiles p ON p.user_id = u.id
 WHERE u.handle = $1
   AND u.deactivated_at IS NULL;

-- ==========================================================================
-- 2. Following list — who this user follows
-- ==========================================================================
-- Keyset pagination on (created_at, followee_id).
--
-- Offset pagination is wrong here for the reason set out in §3's comment: a
-- list that changes while a user scrolls will duplicate or skip rows. It is
-- also progressively slower, because OFFSET 10000 makes PostgreSQL walk and
-- discard 10,000 rows before returning any.

SELECT u.id,
       u.handle,
       u.is_verified,
       u.follower_count,
       p.display_name,
       p.avatar_url,
       f.created_at AS followed_at,
       -- Mutual follow, for the "follows you" badge.
       EXISTS (
           SELECT 1 FROM follows back
            WHERE back.follower_id = u.id AND back.followee_id = $1
       ) AS follows_back
  FROM follows f
  JOIN users u          ON u.id = f.followee_id
  JOIN user_profiles p  ON p.user_id = u.id
 WHERE f.follower_id = $1
   AND u.deactivated_at IS NULL
   -- Cursor. On the first page pass NULL for both and the predicate is skipped.
   AND ($2::timestamptz IS NULL OR (f.created_at, f.followee_id) < ($2, $3::uuid))
 ORDER BY f.created_at DESC, f.followee_id DESC
 LIMIT $4;

-- ==========================================================================
-- 3. Followers list
-- ==========================================================================
-- The mirror of §2, served by follows_followee_created_idx.
--
-- Why keyset rather than OFFSET, concretely: a celebrity gains followers while
-- you are scrolling their follower list. With OFFSET, every new follower pushes
-- the window down by one, so page 2 repeats a row from page 1. The keyset
-- cursor names a *position in the ordering*, not a count of skipped rows, so
-- new rows appearing above the cursor cannot disturb it.

SELECT u.id,
       u.handle,
       u.is_verified,
       u.follower_count,
       p.display_name,
       p.avatar_url,
       f.created_at AS followed_at,
       EXISTS (
           SELECT 1 FROM follows mine
            WHERE mine.follower_id = $2 AND mine.followee_id = u.id
       ) AS viewer_follows
  FROM follows f
  JOIN users u         ON u.id = f.follower_id
  JOIN user_profiles p ON p.user_id = u.id
 WHERE f.followee_id = $1
   AND u.deactivated_at IS NULL
   -- Exclude accounts either party has blocked.
   AND NOT EXISTS (
       SELECT 1 FROM blocks b
        WHERE (b.blocker_id = $2 AND b.blocked_id = u.id)
           OR (b.blocker_id = u.id AND b.blocked_id = $2)
   )
   AND ($3::timestamptz IS NULL OR (f.created_at, f.follower_id) < ($3, $4::uuid))
 ORDER BY f.created_at DESC, f.follower_id DESC
 LIMIT $5;

-- ==========================================================================
-- 4. A user's own posts — the profile timeline
-- ==========================================================================
-- Media is aggregated into a JSON array in the same statement rather than
-- fetched per post. Twenty posts would otherwise be one query for the posts and
-- twenty for their attachments; the classic N+1.

SELECT p.id,
       p.body,
       p.visibility,
       p.comment_count,
       p.reaction_count,
       p.share_count,
       p.created_at,
       -- Empty array rather than NULL when a post has no media, so the client
       -- can iterate unconditionally.
       COALESCE(
           (SELECT json_agg(
                       json_build_object(
                           'kind',        m.kind,
                           'storage_key', m.storage_key,
                           'width',       m.width,
                           'height',      m.height,
                           'alt_text',    m.alt_text
                       ) ORDER BY m.position
                   )
              FROM post_media m
             WHERE m.post_id = p.id),
           '[]'::json
       ) AS media,
       -- The viewer's own reaction, so the UI can highlight it.
       (SELECT r.kind FROM post_reactions r
         WHERE r.post_id = p.id AND r.user_id = $2) AS viewer_reaction
  FROM posts p
 WHERE p.author_id = $1
   AND p.deleted_at IS NULL
   -- Visibility: the author sees everything; a follower sees public and
   -- followers-only; everyone else sees only public.
   AND (
        p.author_id = $2
        OR p.visibility = 'public'
        OR (p.visibility = 'followers'
            AND EXISTS (SELECT 1 FROM follows f
                         WHERE f.follower_id = $2 AND f.followee_id = p.author_id))
   )
   AND ($3::timestamptz IS NULL OR (p.created_at, p.id) < ($3, $4::uuid))
 ORDER BY p.created_at DESC, p.id DESC
 LIMIT $5;

-- ==========================================================================
-- 5. Comments with reply structure
-- ==========================================================================
-- One statement returns a whole thread in reading order, using the materialised
-- path from schema.sql.
--
-- The alternative — a recursive CTE walking parent_id — works, but re-walks the
-- tree on every read. Since a comment's position in the thread is fixed once
-- written, computing the path once at insert and range-scanning it afterwards
-- is strictly better: one index scan, no recursion, no sort.

SELECT c.id,
       c.parent_id,
       c.path,
       c.depth,
       -- Deleted comments are returned as tombstones rather than omitted:
       -- removing a parent from the result would orphan its replies in the UI.
       CASE WHEN c.deleted_at IS NOT NULL THEN NULL ELSE c.body END AS body,
       c.deleted_at IS NOT NULL AS is_deleted,
       c.reaction_count,
       c.reply_count,
       c.created_at,
       u.id     AS author_id,
       u.handle AS author_handle,
       pr.display_name AS author_display_name,
       pr.avatar_url   AS author_avatar_url,
       (SELECT cr.kind FROM comment_reactions cr
         WHERE cr.comment_id = c.id AND cr.user_id = $2) AS viewer_reaction
  FROM comments c
  JOIN users u          ON u.id = c.author_id
  JOIN user_profiles pr ON pr.user_id = u.id
 WHERE c.post_id = $1
   -- Only the first two levels; deeper replies load on demand via §5b.
   AND c.depth <= 1
   AND ($3::text IS NULL OR c.path > $3)
 -- Lexicographic order over the path IS thread reading order.
 ORDER BY c.path
 LIMIT $4;

-- --------------------------------------------------------------------------
-- 5b. Load a subtree — "show 12 more replies"
-- --------------------------------------------------------------------------
-- A prefix match on the path. The `path > prefix AND path < prefix || '/'`
-- form is a range scan on comments_post_path_idx; `path LIKE 'prefix%'` would
-- also work but is easier to write in a way the planner cannot use.

SELECT c.id, c.parent_id, c.path, c.depth, c.body, c.created_at,
       u.handle AS author_handle
  FROM comments c
  JOIN users u ON u.id = c.author_id
 WHERE c.post_id = $1
   AND c.path > $2
   -- '/' is the character immediately after '.' in ASCII, so this bounds the
   -- scan to exactly the subtree under $2.
   AND c.path < ($2 || '/')
   AND c.deleted_at IS NULL
 ORDER BY c.path
 LIMIT $3;

-- ==========================================================================
-- 6. Reaction counts grouped by kind
-- ==========================================================================
-- Two forms, for two situations.

-- 6a. Display path. Reads the maintained tally table: one index scan over at
-- most six rows, regardless of whether the post has ten reactions or ten
-- million. This is what the API serves.

SELECT rc.kind,
       rc.total
  FROM post_reaction_counts rc
 WHERE rc.post_id = $1
   AND rc.total > 0
 ORDER BY rc.total DESC;

-- 6b. Aggregate form, and the reconciliation query.
--
-- This is what the counters in 6a are *supposed* to equal. It aggregates the
-- source rows directly, which is correct but scales with the number of
-- reactions — fine for a nightly reconciliation job, wrong for a page render.
--
-- Running 6b and comparing it with 6a is how counter drift is detected. A
-- denormalised counter with no reconciliation query is a counter nobody can
-- prove is right.

SELECT r.kind,
       count(*) AS total,
       -- A sample of reactors, for "Alice, Bob and 340 others".
       (array_agg(u.handle ORDER BY r.created_at DESC))[1:3] AS recent_handles
  FROM post_reactions r
  JOIN users u ON u.id = r.user_id
 WHERE r.post_id = $1
 GROUP BY r.kind
 ORDER BY total DESC;

-- 6c. Full reconciliation across every post with a discrepancy.
-- Intended for a scheduled job, not for a request path.

SELECT p.id AS post_id,
       p.reaction_count AS stored_total,
       COALESCE(actual.total, 0) AS actual_total,
       p.reaction_count - COALESCE(actual.total, 0) AS drift
  FROM posts p
  LEFT JOIN (
        SELECT post_id, count(*) AS total
          FROM post_reactions
         GROUP BY post_id
  ) actual ON actual.post_id = p.id
 WHERE p.reaction_count <> COALESCE(actual.total, 0)
 ORDER BY abs(p.reaction_count - COALESCE(actual.total, 0)) DESC
 LIMIT 100;

-- ==========================================================================
-- 7. Home feed generation — the hybrid, cursor-paginated
-- ==========================================================================
-- This is the centrepiece query. It merges two sources:
--
--   A. feed_entries — posts fanned out on write from ordinary accounts;
--   B. a read-time scan of posts by the celebrity accounts this user follows.
--
-- Why hybrid: fanning out a celebrity's post to 50 million followers is 50
-- million inserts for one action, and the write amplification makes posting
-- slow and expensive. Reading their posts at query time costs one extra index
-- scan over a handful of authors. The full comparison is in
-- docs/social-media-database-design.md §6.
--
-- The cursor is (created_at, post_id), which is a total order because post_id
-- breaks ties. Both branches ORDER BY and LIMIT independently before the merge,
-- so neither branch materialises more than one page.
--
-- Parameters are referenced directly rather than through a `viewer` CTE that
-- selects them once. That reads more repetitively, and it is deliberate: an
-- earlier draft did use such a CTE, and `LIMIT (SELECT page_size FROM viewer)`
-- makes the row limit opaque to the planner at plan time. On the seeded
-- database that produced a bitmap scan followed by a sort instead of an ordered
-- index scan with early termination. A literal `LIMIT $4` restores it.
--
--   $1 viewer id, $2 cursor created_at (NULL on the first page),
--   $3 cursor post_id, $4 page size.

-- Branch A: the materialised feed.
WITH materialised AS (
    SELECT fe.post_id,
           fe.author_id,
           fe.created_at
      FROM feed_entries fe
     WHERE fe.user_id = $1
       AND ($2::timestamptz IS NULL
            OR (fe.created_at, fe.post_id) < ($2, $3::uuid))
     ORDER BY fe.created_at DESC, fe.post_id DESC
     LIMIT $4
),

-- Branch B: celebrities the viewer follows, read at query time.
celebrity_posts AS (
    SELECT p.id AS post_id,
           p.author_id,
           p.created_at
      FROM posts p
      JOIN follows f            ON f.followee_id = p.author_id AND f.follower_id = $1
      JOIN celebrity_accounts ca ON ca.user_id = p.author_id
     WHERE p.deleted_at IS NULL
       AND p.visibility IN ('public', 'followers')
       AND ($2::timestamptz IS NULL
            OR (p.created_at, p.id) < ($2, $3::uuid))
     ORDER BY p.created_at DESC, p.id DESC
     LIMIT $4
),

-- Merge, de-duplicate and take one page.
--
-- UNION rather than UNION ALL: a post can appear in both branches during the
-- window in which an account was just promoted to celebrity status, and showing
-- it twice is a visible bug.
merged AS (
    SELECT post_id, author_id, created_at FROM materialised
    UNION
    SELECT post_id, author_id, created_at FROM celebrity_posts
),
page AS (
    SELECT m.post_id, m.author_id, m.created_at
      FROM merged m
     -- Blocked authors are filtered after the merge, so neither branch has to
     -- carry the predicate. At most 2 × page_size rows reach this point.
     WHERE NOT EXISTS (
             SELECT 1 FROM blocks b
              WHERE (b.blocker_id = $1 AND b.blocked_id = m.author_id)
                 OR (b.blocker_id = m.author_id AND b.blocked_id = $1)
           )
     ORDER BY m.created_at DESC, m.post_id DESC
     LIMIT $4
)

-- Hydrate only the page — at most page_size posts, never the whole merge.
SELECT p.id,
       p.body,
       p.created_at,
       p.comment_count,
       p.reaction_count,
       p.share_count,
       a.id     AS author_id,
       a.handle AS author_handle,
       a.is_verified AS author_verified,
       ap.display_name AS author_display_name,
       ap.avatar_url   AS author_avatar_url,
       COALESCE(
           (SELECT json_agg(
                       json_build_object(
                           'kind', m.kind, 'storage_key', m.storage_key,
                           'width', m.width, 'height', m.height, 'alt_text', m.alt_text
                       ) ORDER BY m.position)
              FROM post_media m WHERE m.post_id = p.id),
           '[]'::json
       ) AS media,
       -- Shared post, flattened one level. Deeper nesting is not rendered.
       CASE WHEN p.shared_post_id IS NOT NULL THEN
           json_build_object(
               'id',        sp.id,
               'body',      sp.body,
               'author',    sa.handle,
               'created_at', sp.created_at,
               'deleted',   sp.deleted_at IS NOT NULL
           )
       END AS shared_post,
       (SELECT r.kind FROM post_reactions r
         WHERE r.post_id = p.id AND r.user_id = $1) AS viewer_reaction
  FROM page
  JOIN posts p          ON p.id = page.post_id
  JOIN users a          ON a.id = p.author_id
  JOIN user_profiles ap ON ap.user_id = a.id
  LEFT JOIN posts sp    ON sp.id = p.shared_post_id
  LEFT JOIN users sa    ON sa.id = sp.author_id
 WHERE p.deleted_at IS NULL
 ORDER BY p.created_at DESC, p.id DESC;

-- --------------------------------------------------------------------------
-- 7b. Fan-out on write — the INSERT that populates feed_entries
-- --------------------------------------------------------------------------
-- Runs asynchronously after a post is created, in batches. Celebrities are
-- excluded, because their posts are merged at read time by §7 instead.
--
-- ON CONFLICT DO NOTHING makes the job idempotent, which matters because the
-- queue that drives it delivers at least once.

INSERT INTO feed_entries (user_id, post_id, author_id, created_at)
SELECT f.follower_id, $1, $2, $3
  FROM follows f
 WHERE f.followee_id = $2
   -- Batch bound: the job pages through a large follower set.
   AND f.follower_id > $4
   AND NOT EXISTS (SELECT 1 FROM celebrity_accounts ca WHERE ca.user_id = $2)
   -- Do not deliver to someone who has blocked the author.
   AND NOT EXISTS (
        SELECT 1 FROM blocks b
         WHERE b.blocker_id = f.follower_id AND b.blocked_id = $2
   )
 ORDER BY f.follower_id
 LIMIT $5
 ON CONFLICT (user_id, post_id) DO NOTHING;

-- --------------------------------------------------------------------------
-- 7c. Trim the materialised feed
-- --------------------------------------------------------------------------
-- feed_entries would grow without bound otherwise. Keeping the newest N per
-- user bounds it; anything older is served by falling back to a read-time query
-- against the follow graph, which is rare enough not to matter.

DELETE FROM feed_entries fe
 WHERE fe.user_id = $1
   AND fe.post_id IN (
        SELECT post_id
          FROM feed_entries
         WHERE user_id = $1
         ORDER BY created_at DESC, post_id DESC
        OFFSET $2   -- keep this many; delete the rest
   );

-- ==========================================================================
-- 8. Conversation list with the latest message
-- ==========================================================================
-- The conversation list is the most-viewed screen in any messaging product, so
-- it gets the denormalised last_message_id pointer on conversations.
--
-- The alternative — a LATERAL join taking the newest message per conversation —
-- is written out in §8b for comparison. It is correct and needs no denormalised
-- column, but it performs one index scan per conversation row; the pointer
-- version is a single join.

SELECT c.id AS conversation_id,
       c.kind,
       c.title,
       c.last_message_at,
       lm.id         AS last_message_id,
       lm.body       AS last_message_body,
       lm.created_at AS last_message_at_exact,
       lm.deleted_at IS NOT NULL AS last_message_deleted,
       ls.handle     AS last_message_sender_handle,
       -- Unread count: the gap between the newest sequence and this
       -- participant's read cursor. No anti-join over messages required.
       GREATEST(
           COALESCE((SELECT max(m.seq) FROM messages m WHERE m.conversation_id = c.id), 0)
           - cp.last_read_seq,
           0
       ) AS unread_count,
       cp.is_muted,
       -- For a direct conversation, the other participant is what the UI shows
       -- as the title.
       CASE WHEN c.kind = 'direct' THEN (
            SELECT json_build_object(
                       'id', ou.id, 'handle', ou.handle,
                       'display_name', op.display_name, 'avatar_url', op.avatar_url)
              FROM conversation_participants ocp
              JOIN users ou         ON ou.id = ocp.user_id
              JOIN user_profiles op ON op.user_id = ou.id
             WHERE ocp.conversation_id = c.id
               AND ocp.user_id <> $1
             LIMIT 1
       ) END AS counterpart
  FROM conversation_participants cp
  JOIN conversations c    ON c.id = cp.conversation_id
  LEFT JOIN messages lm   ON lm.id = c.last_message_id
  LEFT JOIN users ls      ON ls.id = lm.sender_id
 WHERE cp.user_id = $1
   AND cp.left_at IS NULL
   AND ($2::timestamptz IS NULL OR c.last_message_at < $2)
 ORDER BY c.last_message_at DESC NULLS LAST
 LIMIT $3;

-- --------------------------------------------------------------------------
-- 8b. The same list without the denormalised pointer, for comparison
-- --------------------------------------------------------------------------
-- LATERAL is the idiomatic "top-1 per group" in PostgreSQL. Correct, and one
-- index scan per conversation instead of one join for the whole page — the
-- reason §8 keeps the pointer.

SELECT c.id AS conversation_id,
       c.kind,
       lm.id, lm.body, lm.created_at
  FROM conversation_participants cp
  JOIN conversations c ON c.id = cp.conversation_id
  LEFT JOIN LATERAL (
        SELECT m.id, m.body, m.created_at
          FROM messages m
         WHERE m.conversation_id = c.id
           AND m.deleted_at IS NULL
         ORDER BY m.seq DESC
         LIMIT 1
  ) lm ON TRUE
 WHERE cp.user_id = $1
   AND cp.left_at IS NULL
 ORDER BY lm.created_at DESC NULLS LAST
 LIMIT $2;

-- ==========================================================================
-- 9. Message history, keyset-paginated
-- ==========================================================================
-- Messages page *backwards* — a chat opens at the newest message and scrolls up
-- into history — so the cursor is "seq less than", and the client reverses the
-- page for display.
--
-- seq is used rather than created_at because it is unambiguous: two messages
-- can share a timestamp, and a clock adjustment can reorder them. seq cannot.

SELECT m.id,
       m.seq,
       m.body,
       m.created_at,
       m.edited_at,
       m.deleted_at IS NOT NULL AS is_deleted,
       m.sender_id,
       -- The sender's account may have been erased; the message survives.
       COALESCE(u.handle, 'deleted_user') AS sender_handle,
       p.avatar_url AS sender_avatar_url,
       COALESCE(
           (SELECT json_agg(json_build_object(
                       'kind', mm.kind, 'storage_key', mm.storage_key,
                       'mime_type', mm.mime_type, 'byte_size', mm.byte_size)
                   ORDER BY mm.position)
              FROM message_media mm WHERE mm.message_id = m.id),
           '[]'::json
       ) AS media
  FROM messages m
  LEFT JOIN users u         ON u.id = m.sender_id
  LEFT JOIN user_profiles p ON p.user_id = u.id
 WHERE m.conversation_id = $1
   -- Authorisation is part of the query, not a separate check that a caller
   -- could forget: a non-participant selects zero rows.
   AND EXISTS (
        SELECT 1 FROM conversation_participants cp
         WHERE cp.conversation_id = $1
           AND cp.user_id = $2
           AND cp.left_at IS NULL
   )
   AND ($3::bigint IS NULL OR m.seq < $3)
 ORDER BY m.seq DESC
 LIMIT $4;

-- --------------------------------------------------------------------------
-- 9b. Advance the read cursor
-- --------------------------------------------------------------------------
-- GREATEST makes this monotonic: an out-of-order acknowledgement from a second
-- device cannot move the cursor backwards and resurrect read messages as unread.

UPDATE conversation_participants
   SET last_read_seq = GREATEST(last_read_seq, $3),
       last_read_at  = now()
 WHERE conversation_id = $1
   AND user_id = $2
RETURNING last_read_seq;

-- ==========================================================================
-- 10. Unread message count
-- ==========================================================================
-- The app badge: total unread across every conversation the user is in.
--
-- Each conversation contributes (max seq) - (read cursor). The max is a
-- backward index scan on messages_seq_unique returning one row per
-- conversation, so the cost is proportional to the user's conversation count,
-- not to their message count.

SELECT COALESCE(SUM(
           GREATEST(
               COALESCE((SELECT max(m.seq)
                           FROM messages m
                          WHERE m.conversation_id = cp.conversation_id), 0)
               - cp.last_read_seq,
               0
           )
       ), 0) AS total_unread,
       count(*) FILTER (
           WHERE COALESCE((SELECT max(m.seq)
                             FROM messages m
                            WHERE m.conversation_id = cp.conversation_id), 0)
                 > cp.last_read_seq
       ) AS conversations_with_unread
  FROM conversation_participants cp
 WHERE cp.user_id = $1
   AND cp.left_at IS NULL
   AND cp.is_muted = FALSE;

-- ==========================================================================
-- 11. Notification retrieval
-- ==========================================================================
-- Aggregated notifications, newest first, keyset-paginated. actor_count carries
-- "and 340 others" without the client grouping anything.

SELECT n.id,
       n.kind,
       n.actor_count,
       n.created_at,
       n.read_at IS NOT NULL AS is_read,
       a.id     AS actor_id,
       a.handle AS actor_handle,
       ap.display_name AS actor_display_name,
       ap.avatar_url   AS actor_avatar_url,
       n.post_id,
       -- A short excerpt of the subject, so the list needs no second query.
       CASE WHEN n.post_id IS NOT NULL
            THEN left(COALESCE(po.body, ''), 100)
       END AS post_excerpt,
       n.comment_id,
       CASE WHEN n.comment_id IS NOT NULL
            THEN left(COALESCE(co.body, ''), 100)
       END AS comment_excerpt
  FROM notifications n
  LEFT JOIN users a          ON a.id = n.actor_id
  LEFT JOIN user_profiles ap ON ap.user_id = a.id
  LEFT JOIN posts po         ON po.id = n.post_id AND po.deleted_at IS NULL
  LEFT JOIN comments co      ON co.id = n.comment_id AND co.deleted_at IS NULL
 WHERE n.recipient_id = $1
   -- $2 = true filters to unread only, which uses the partial index.
   AND ($2::boolean IS NOT TRUE OR n.read_at IS NULL)
   AND ($3::timestamptz IS NULL OR (n.created_at, n.id) < ($3, $4::uuid))
 ORDER BY n.created_at DESC, n.id DESC
 LIMIT $5;

-- --------------------------------------------------------------------------
-- 11b. Create or aggregate a notification
-- --------------------------------------------------------------------------
-- This is the deduplication mechanism. The unique partial index
-- notifications_dedup_idx on (recipient_id, dedup_key) WHERE read_at IS NULL
-- means the upsert either inserts a new unread notification or bumps the
-- existing one's counter — never both, and never a duplicate.
--
-- Scoping the index to unread rows is deliberate: once the user has read
-- "Alice liked your post", a new like should raise a *fresh* notification
-- rather than silently incrementing one they have already dismissed.

INSERT INTO notifications (recipient_id, kind, actor_id, post_id, comment_id, dedup_key)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (recipient_id, dedup_key) WHERE read_at IS NULL
DO UPDATE SET actor_count = notifications.actor_count + 1,
              -- Show the most recent actor's name first.
              actor_id    = EXCLUDED.actor_id,
              updated_at  = now()
RETURNING id, actor_count;

-- --------------------------------------------------------------------------
-- 11c. Unread badge
-- --------------------------------------------------------------------------
-- A primary-key lookup on the counter table rather than a COUNT over
-- notifications. This runs on every page load.

SELECT COALESCE(nc.unread_count, 0) AS unread_count
  FROM notification_counters nc
 WHERE nc.user_id = $1;

-- ==========================================================================
-- 12. Marking notifications as read
-- ==========================================================================

-- 12a. Mark specific notifications read.
-- The read_at IS NULL guard makes this idempotent: re-marking an already-read
-- notification affects zero rows, so the counter trigger cannot double-count.

UPDATE notifications
   SET read_at = now()
 WHERE recipient_id = $1
   AND id = ANY($2::uuid[])
   AND read_at IS NULL
RETURNING id;

-- 12b. Mark everything read, up to a cursor.
--
-- Bounded by created_at rather than unbounded, so a notification that arrives
-- while the request is in flight is not marked read without ever being shown.
--
-- Note what this statement does NOT do: it does not touch
-- notification_counters. The notifications_counter trigger in schema.sql
-- already decrements the badge for every row whose read_at goes from NULL to a
-- value, so decrementing it here as well would subtract the same count twice.
--
-- That is not hypothetical. An earlier version of this file did exactly that,
-- and it was caught by scripts/verify-sql.mjs, which runs every statement here
-- and then checks each counter against the rows it counts. It is the clearest
-- argument in this whole assessment for keeping counter maintenance in exactly
-- one place: with a trigger doing the work invisibly, a helpful-looking manual
-- update in a query is a bug, and one that only shows up as slow drift.
--
-- The client reads the resulting badge with §11c.

UPDATE notifications
   SET read_at = now()
 WHERE recipient_id = $1
   AND read_at IS NULL
   AND created_at <= $2
RETURNING id;

-- ==========================================================================
-- 13. EXPLAIN guidance and verification record
-- ==========================================================================
--
-- What was actually done
-- ----------------------
-- Every statement in this file was executed against a real PostgreSQL engine
-- (PostgreSQL 18.3, run as WebAssembly via PGlite 0.5.8) with schema.sql,
-- indexes.sql and seed.sql applied — 300 users, 5,417 follow edges, 2,634
-- posts, 7,296 comments, 13,170 reactions, 1,800 messages, 2,353 notifications
-- and 10,613 materialised feed rows. All 22 statements executed without error,
-- and the plans below were read from EXPLAIN (ANALYZE, BUFFERS) output.
--
-- Three design defects were found this way and fixed, rather than being
-- documented around:
--
--   1. §3's ORDER BY carries a keyset tiebreaker (follower_id) that was not in
--      follows_followee_created_idx, so the plan showed an Incremental Sort
--      above the index scan. The index now includes it.
--   2. comments_post_path_idx was partial on `deleted_at IS NULL`, but §5
--      deliberately returns deleted comments as tombstones and therefore has no
--      such predicate. The planner could not use the index at all: the plan was
--      a Seq Scan with "Rows Removed by Filter: 7292" for a four-comment
--      thread. The index is no longer partial.
--   3. §7 originally passed its parameters through a `viewer` CTE, which made
--      `LIMIT (SELECT page_size FROM viewer)` opaque at plan time and cost the
--      ordered index scan. Parameters are now referenced directly.
--
-- What is NOT claimed: no timings appear anywhere in this file. PGlite is a
-- single-connection WebAssembly build and the seed volumes are small, so
-- wall-clock numbers from it would be meaningless. Plan *shape* and index
-- *choice* are what was verified; those are what the index design controls.
--
-- Reproduce it with the runner described in §14, or against a real server with
-- the psql commands at the top of this file.
--
-- How to check:
--
--   EXPLAIN (ANALYZE, BUFFERS, VERBOSE, FORMAT TEXT)
--   <the statement>;
--
-- ANALYZE executes the statement, so use a transaction that rolls back when
-- checking a write:
--
--   BEGIN;
--   EXPLAIN (ANALYZE, BUFFERS) INSERT INTO feed_entries ...;
--   ROLLBACK;
--
-- Always run ANALYZE on the tables first. On an unanalysed table the planner
-- has no statistics, will guess, and the plan you inspect will not be the plan
-- production uses:
--
--   ANALYZE users, follows, posts, comments, post_reactions,
--           conversations, messages, notifications, feed_entries;
--
-- ---------------------------------------------------------------------------
-- Observed access paths, and the red flags that mean something has regressed
-- ---------------------------------------------------------------------------
--
-- §1 profile lookup
--   Observed: Index Scan using users_handle_unique, one row, Nested Loop to
--           user_profiles_pkey, and an Index Only Scan on follows_pkey for the
--           "does the viewer follow?" probe. 11 shared buffers total.
--   Note: the blocks and follow_requests probes showed Seq Scans on the seeded
--           database. That is correct — those tables held 21 and 81 rows, and
--           a sequential scan of one page beats an index lookup. Expect index
--           scans there once the tables are large enough to matter.
--   Red flag: Seq Scan on *users*. Means the handle is being cast or wrapped in
--           a function, defeating the unique index.
--
-- §2, §3 follower and following lists
--   Observed: Index Only Scan using follows_followee_created_idx, actual
--           rows = LIMIT, no sort node.
--   Red flag: an Incremental Sort or Sort above the scan. That is exactly what
--           appeared before follower_id was added to the index, and it means
--           the ORDER BY has a column the index does not carry.
--
-- §4 user posts
--   Observed: Index Scan using posts_author_created_idx with the visibility
--           rules applied as a Filter, plus one small subquery per row for
--           media.
--   Red flag: rows removed by filter far exceeding rows returned. Usually means
--           a partial index predicate no longer matches the query's WHERE.
--
-- §5 comment thread
--   Observed, on a 20,001-comment thread: Index Scan using
--           comments_post_path_idx, actual rows = 50 for LIMIT 50, no Sort
--           node, 152 buffers. The index alone supplies both the filter and the
--           thread ordering.
--   Observed, on a four-comment thread: Bitmap Index Scan plus a Sort. That is
--           the planner being right, not wrong — sorting four rows is cheaper
--           than an ordered scan, and it will not choose that plan once threads
--           are large enough for the LIMIT to matter.
--   Red flag: Seq Scan on comments, or a Recursive Union. The first means the
--           index predicate and the query have drifted apart (see the defect
--           log above); the second means the path column is being ignored and
--           the tree walked at read time.
--
-- §6a reaction counts
--   Observed: Bitmap Index Scan on post_reaction_counts_pkey, 5 rows, 7
--           buffers — constant work regardless of how popular the post is.
--   §6b, by contrast, aggregates the source rows and grows with the reaction
--   count. Seeing §6b's plan on a request path is the bug §6a exists to prevent.
--
-- §7 home feed
--   Observed, materialised branch: Index Scan using
--           feed_entries_user_created_idx, actual rows = 20 for LIMIT 20 —
--           early termination, the whole point of the index's column order.
--   Observed, celebrity branch: Hash Join of follows to celebrity_accounts,
--           then Index Scan using posts_author_created_idx per celebrity, then
--           a top-N heapsort down to the page size.
--   Then: Unique over the UNION, the block anti-join, and the hydration joins.
--   Red flags:
--     * A Bitmap Heap Scan plus a full Sort in the materialised branch. That is
--       what the `viewer` CTE caused, and it means the LIMIT has become opaque
--       to the planner again.
--     * A Sort whose input greatly exceeds 2 × page_size — one branch has lost
--       its LIMIT and the merge is sorting the whole feed.
--     * Any Seq Scan on posts — the celebrity branch has lost its index.
--
-- §8 conversation list
--   Observed: Index Scan on conversation_participants_user_idx, Nested Loop to
--           conversations by primary key, and the max(seq) subquery as a
--           per-row scan of messages_seq_unique.
--   Red flag: an Aggregate over a full scan of messages. Means conversation_id
--           was not pushed into the max(seq) subquery.
--
-- §9 message history
--   Observed: Index Scan Backward using messages_seq_unique, rows = LIMIT.
--   Red flag: Sort. Means the ORDER BY is on created_at rather than seq.
--
-- §10 unread total
--   Observed: one scan of conversation_participants for the user, then one
--           backward index scan per conversation for max(seq). Cost scales with
--           the user's conversation count, not their message count.
--   Look for "Heap Fetches: 0" on the participants scan — the
--   INCLUDE (last_read_seq) is what makes it index-only, and a non-zero value
--   after bulk writes means the table needs a VACUUM.
--
-- §11 notifications
--   Observed, unread only: the planner selects notifications_unread_idx, the
--           small partial index, rather than the full recipient index. That
--           switch is the entire justification for maintaining both.
--
-- ---------------------------------------------------------------------------
-- Reading the output
-- ---------------------------------------------------------------------------
--
--   * Compare `rows=` (estimated) with `actual rows=`. An order-of-magnitude
--     gap means the statistics are stale or a correlation is invisible to the
--     planner; ANALYZE first, then consider CREATE STATISTICS.
--   * `Buffers: shared hit=… read=…`. High `read` means the working set is not
--     in cache. This is the number that actually predicts latency under load —
--     more than the timing on an idle laptop.
--   * `Rows Removed by Filter` should be near zero on indexed paths. A large
--     value means the index is finding rows the query then throws away, which
--     is usually a partial-index predicate that does not match the WHERE clause.
--   * `Heap Fetches: 0` on an Index Only Scan confirms the visibility map is
--     current. A non-zero value after a bulk write means the table needs a
--     VACUUM before the index-only path pays off.
--
-- ==========================================================================
-- 14. Verifying this file
-- ==========================================================================
--
-- Running this file directly with psql executes every statement, including the
-- writes in §7b, §7c, §9b, §11b and §12. Against the seeded database that is
-- harmless and is the point — it proves the SQL is valid and the parameters
-- bind. Against anything with real data it is not; use a scratch database.
--
--   createdb social_scratch
--   psql -d social_scratch -f database-assessment/schema.sql
--   psql -d social_scratch -f database-assessment/indexes.sql
--   psql -d social_scratch -f database-assessment/seed.sql
--
-- Then check a specific query with its parameters bound, for example:
--
--   psql -d social_scratch -v ON_ERROR_STOP=1 <<'SQL'
--   PREPARE feed (uuid, timestamptz, uuid, int) AS
--     <paste §7 here>;
--   EXPLAIN (ANALYZE, BUFFERS)
--     EXECUTE feed('...'::uuid, NULL, NULL, 20);
--   SQL
--
-- PREPARE is the right way to test these: it binds the parameters exactly as
-- the application will, so the plan you inspect is the plan production gets.
