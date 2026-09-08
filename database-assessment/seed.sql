-- =========================================================================
-- Social media platform — generated seed data
--
-- Apply after schema.sql and indexes.sql:
--   psql "$DATABASE_URL" -f database-assessment/seed.sql
--
-- Purpose: give the queries in queries.sql something to run against, so their
-- syntax can be verified and their EXPLAIN plans inspected on data with a
-- realistic *shape* — a skewed follow graph, a few celebrities, threads of
-- varying depth, conversations with unread tails.
--
-- The volumes are small (hundreds of users, thousands of posts) so this runs in
-- seconds. That is deliberate: this file exists to prove the SQL is correct and
-- to show which index each query chooses, not to benchmark. Plan *shape* is
-- stable across volumes; plan *cost* is not, and any timing taken here would
-- say nothing about production.
--
-- To inspect plans at a realistic scale, raise the constants in §1 — 50k users
-- and 500k posts take a few minutes on a laptop — and re-run ANALYZE.
--
-- Idempotent: it truncates before inserting, so it can be re-run.
-- =========================================================================

BEGIN;

TRUNCATE
    feed_entries, celebrity_accounts,
    notification_counters, notifications,
    message_media, messages, conversation_participants, conversations,
    post_reaction_counts, comment_reactions, post_reactions,
    comments, post_media, posts,
    blocks, follow_requests, follows,
    user_profiles, users
RESTART IDENTITY CASCADE;

-- --------------------------------------------------------------------------
-- 1. Volume knobs
-- --------------------------------------------------------------------------
CREATE TEMP TABLE seed_config (
    user_count           INT,
    celebrity_count      INT,
    posts_per_user       INT,
    max_follows_per_user INT,
    conversation_count   INT
) ON COMMIT DROP;

INSERT INTO seed_config VALUES (
    300,   -- users
    5,     -- of whom this many are celebrities
    8,     -- posts each
    25,    -- upper bound on follows per user
    120    -- conversations
);

-- Deterministic randomness: the same seed produces the same graph every run,
-- so a plan that changes between runs indicates a real change, not noise.
SELECT setseed(0.42);

-- --------------------------------------------------------------------------
-- 2. Users and profiles
-- --------------------------------------------------------------------------
INSERT INTO users (id, handle, email, password_hash, is_private, is_verified, created_at)
SELECT
    -- Deterministic UUIDs derived from the index, so seeded rows can be
    -- referenced from a script without a lookup.
    ('00000000-0000-4000-8000-' || lpad(i::text, 12, '0'))::uuid,
    'user' || i,
    'user' || i || '@example.com',
    -- A real bcrypt hash shape. Not a usable credential.
    '$2a$10$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRS',
    -- Every eleventh account is private, so visibility rules get exercised.
    (i % 11 = 0),
    (i <= (SELECT celebrity_count FROM seed_config)),
    now() - (random() * INTERVAL '365 days')
  FROM generate_series(1, (SELECT user_count FROM seed_config)) AS i;

INSERT INTO user_profiles (user_id, display_name, bio, avatar_url, location)
SELECT u.id,
       'User ' || substring(u.handle FROM 5),
       'Bio for ' || u.handle || '. Generated seed data.',
       'avatars/' || u.handle || '.jpg',
       (ARRAY['Jakarta', 'Bandung', 'Surabaya', 'Medan', 'Yogyakarta'])[1 + (random() * 4)::int]
  FROM users u;

-- --------------------------------------------------------------------------
-- 3. Follow graph — deliberately skewed
-- --------------------------------------------------------------------------
-- Two populations, because a uniform random graph would hide exactly the
-- problem the hybrid feed exists to solve.

-- 3a. Everyone follows every celebrity. This is what makes fan-out-on-write
-- untenable for those accounts and is the reason for the read-time merge.
INSERT INTO follows (follower_id, followee_id, created_at)
SELECT f.id, c.id, now() - (random() * INTERVAL '300 days')
  FROM users f
 CROSS JOIN LATERAL (
        SELECT id FROM users
         ORDER BY handle
         LIMIT (SELECT celebrity_count FROM seed_config)
 ) c
 WHERE f.id <> c.id
ON CONFLICT DO NOTHING;

-- 3b. Ordinary follows: a random neighbourhood per user.
INSERT INTO follows (follower_id, followee_id, created_at)
SELECT f.id, t.id, now() - (random() * INTERVAL '300 days')
  FROM users f
 CROSS JOIN LATERAL (
        SELECT id FROM users
         WHERE id <> f.id
         ORDER BY random()
         LIMIT (1 + (random() * (SELECT max_follows_per_user FROM seed_config))::int)
 ) t
ON CONFLICT DO NOTHING;

-- Designate the celebrities. The threshold in production would be a follower
-- count; here it is the first N accounts, which the follow graph above matches.
INSERT INTO celebrity_accounts (user_id, follower_count)
SELECT u.id, u.follower_count
  FROM users u
 ORDER BY u.follower_count DESC
 LIMIT (SELECT celebrity_count FROM seed_config);

-- A few blocks, so the block-filter branches are not dead code in EXPLAIN.
INSERT INTO blocks (blocker_id, blocked_id)
SELECT a.id, b.id
  FROM users a
 CROSS JOIN LATERAL (SELECT id FROM users WHERE id <> a.id ORDER BY random() LIMIT 1) b
 WHERE random() < 0.05
ON CONFLICT DO NOTHING;

-- Pending follow requests against the private accounts.
INSERT INTO follow_requests (requester_id, target_id)
SELECT r.id, t.id
  FROM users t
 CROSS JOIN LATERAL (SELECT id FROM users WHERE id <> t.id ORDER BY random() LIMIT 3) r
 WHERE t.is_private
ON CONFLICT DO NOTHING;

-- --------------------------------------------------------------------------
-- 4. Posts and media
-- --------------------------------------------------------------------------
INSERT INTO posts (id, author_id, body, visibility, created_at)
SELECT
    ('10000000-0000-4000-8000-' || lpad((row_number() OVER ())::text, 12, '0'))::uuid,
    u.id,
    'Post ' || n || ' by ' || u.handle || '. '
      || (ARRAY[
           'Notes on database indexing and query plans.',
           'A short thread about concurrency in Go.',
           'Thoughts on Kubernetes rollouts and readiness probes.',
           'Why keyset pagination beats offset at scale.',
           'On caching, invalidation and the two hard problems.'
         ])[1 + (random() * 4)::int],
    -- Mostly public, some followers-only, a few private.
    (CASE WHEN random() < 0.85 THEN 'public'
          WHEN random() < 0.95 THEN 'followers'
          ELSE 'private' END)::post_visibility,
    now() - (random() * INTERVAL '180 days')
  FROM users u
 CROSS JOIN generate_series(1, (SELECT posts_per_user FROM seed_config)) AS n;

-- Celebrities post more, which is what makes the read-time merge branch of the
-- feed query return a meaningful number of rows.
INSERT INTO posts (author_id, body, visibility, created_at)
SELECT ca.user_id,
       'Celebrity post ' || n || '. Higher volume, very wide audience.',
       'public',
       now() - (random() * INTERVAL '30 days')
  FROM celebrity_accounts ca
 CROSS JOIN generate_series(1, 40) AS n;

-- Attachments on roughly a third of posts, some with several images so the
-- ordered multi-media path is exercised.
INSERT INTO post_media (post_id, position, kind, storage_key, mime_type, byte_size, width, height, alt_text)
SELECT p.id,
       m.pos,
       'image'::media_kind,
       'media/' || p.id || '/' || m.pos || '.jpg',
       'image/jpeg',
       200000 + (random() * 3000000)::bigint,
       1200, 900,
       'Image ' || (m.pos + 1) || ' attached to a seeded post'
  FROM posts p
 CROSS JOIN LATERAL generate_series(0, (random() * 3)::int) AS m(pos)
 WHERE random() < 0.35;

-- Shares, exercising the self-referencing foreign key and the flattened
-- shared_post projection in the feed query.
INSERT INTO posts (author_id, body, visibility, shared_post_id, created_at)
SELECT u.id,
       'Sharing this — worth reading.',
       'public',
       src.id,
       now() - (random() * INTERVAL '20 days')
  FROM users u
 CROSS JOIN LATERAL (
        SELECT id FROM posts
         WHERE visibility = 'public' AND shared_post_id IS NULL
         ORDER BY random() LIMIT 1
 ) src
 WHERE random() < 0.15;

-- --------------------------------------------------------------------------
-- 5. Comments — with real thread structure
-- --------------------------------------------------------------------------
-- Top-level comments. The path is the zero-padded sibling position, which is
-- what makes ORDER BY path equal to thread reading order.
INSERT INTO comments (post_id, author_id, parent_id, body, path, depth, created_at)
SELECT p.id,
       a.id,
       NULL,
       'Top-level comment ' || n || ' on this post.',
       lpad(n::text, 4, '0'),
       0,
       p.created_at + (random() * INTERVAL '2 days')
  FROM posts p
 CROSS JOIN generate_series(1, 3) AS n
 CROSS JOIN LATERAL (SELECT id FROM users ORDER BY random() LIMIT 1) a
 WHERE random() < 0.4
   AND p.deleted_at IS NULL;

-- Replies, one level down: path becomes 'parent.NNNN'.
INSERT INTO comments (post_id, author_id, parent_id, body, path, depth, created_at)
SELECT c.post_id,
       a.id,
       c.id,
       'Reply ' || n || ' to comment ' || c.path || '.',
       c.path || '.' || lpad(n::text, 4, '0'),
       c.depth + 1,
       c.created_at + (random() * INTERVAL '1 day')
  FROM comments c
 CROSS JOIN generate_series(1, 2) AS n
 CROSS JOIN LATERAL (SELECT id FROM users ORDER BY random() LIMIT 1) a
 WHERE c.depth = 0
   AND random() < 0.5;

-- A third level, so the depth cap and the subtree query in queries.sql §5b have
-- something to find.
INSERT INTO comments (post_id, author_id, parent_id, body, path, depth, created_at)
SELECT c.post_id,
       a.id,
       c.id,
       'Nested reply to ' || c.path || '.',
       c.path || '.0001',
       c.depth + 1,
       c.created_at + (random() * INTERVAL '12 hours')
  FROM comments c
 CROSS JOIN LATERAL (SELECT id FROM users ORDER BY random() LIMIT 1) a
 WHERE c.depth = 1
   AND random() < 0.3;

-- A few soft-deleted comments, so the tombstone branch of the thread query is
-- exercised rather than assumed.
UPDATE comments
   SET deleted_at = now()
 WHERE id IN (SELECT id FROM comments ORDER BY random() LIMIT 20);

-- --------------------------------------------------------------------------
-- 6. Reactions
-- --------------------------------------------------------------------------
-- Skewed toward 'like', as real reaction distributions are.
INSERT INTO post_reactions (post_id, user_id, kind, created_at)
SELECT p.id,
       u.id,
       (CASE WHEN random() < 0.70 THEN 'like'
             WHEN random() < 0.85 THEN 'love'
             WHEN random() < 0.92 THEN 'haha'
             WHEN random() < 0.96 THEN 'wow'
             WHEN random() < 0.98 THEN 'sad'
             ELSE 'angry' END)::reaction_kind,
       p.created_at + (random() * INTERVAL '5 days')
  FROM posts p
 CROSS JOIN LATERAL (
        SELECT id FROM users ORDER BY random() LIMIT (random() * 12)::int
 ) u
 WHERE p.deleted_at IS NULL
ON CONFLICT DO NOTHING;

INSERT INTO comment_reactions (comment_id, user_id, kind, created_at)
SELECT c.id, u.id, 'like'::reaction_kind, c.created_at + (random() * INTERVAL '2 days')
  FROM comments c
 CROSS JOIN LATERAL (SELECT id FROM users ORDER BY random() LIMIT (random() * 3)::int) u
 WHERE c.deleted_at IS NULL
ON CONFLICT DO NOTHING;

-- --------------------------------------------------------------------------
-- 7. Conversations and messages
-- --------------------------------------------------------------------------
INSERT INTO conversations (id, kind, created_by, created_at)
SELECT ('20000000-0000-4000-8000-' || lpad(i::text, 12, '0'))::uuid,
       (CASE WHEN i % 10 = 0 THEN 'group' ELSE 'direct' END)::conversation_kind,
       (SELECT id FROM users ORDER BY random() LIMIT 1),
       now() - (random() * INTERVAL '90 days')
  FROM generate_series(1, (SELECT conversation_count FROM seed_config)) AS i;

-- Direct conversations get exactly two participants; groups get three to six.
INSERT INTO conversation_participants (conversation_id, user_id, joined_at)
SELECT c.id, u.id, c.created_at
  FROM conversations c
 CROSS JOIN LATERAL (
        SELECT id FROM users
         ORDER BY random()
         LIMIT (CASE WHEN c.kind = 'direct' THEN 2 ELSE 3 + (random() * 3)::int END)
 ) u
ON CONFLICT DO NOTHING;

-- Messages. seq is assigned by the messages_seq trigger, so it is omitted here
-- — which also exercises that trigger.
INSERT INTO messages (conversation_id, sender_id, body, created_at)
SELECT c.id,
       p.user_id,
       'Message ' || n || ' in conversation.',
       c.created_at + (n * INTERVAL '7 minutes')
  FROM conversations c
 CROSS JOIN generate_series(1, 15) AS n
 CROSS JOIN LATERAL (
        SELECT user_id FROM conversation_participants
         WHERE conversation_id = c.id
         ORDER BY random() LIMIT 1
 ) p;

-- Read cursors partway through, so unread counts are non-zero and the
-- unread-count queries return something meaningful.
UPDATE conversation_participants cp
   SET last_read_seq = (random() * 15)::bigint,
       last_read_at  = now() - (random() * INTERVAL '3 days');

-- --------------------------------------------------------------------------
-- 8. Notifications
-- --------------------------------------------------------------------------
-- Aggregated, with the dedup_key populated exactly as the upsert in
-- queries.sql §11b would write it.
INSERT INTO notifications (recipient_id, kind, actor_id, post_id, actor_count, dedup_key, read_at, created_at)
SELECT p.author_id,
       'post_reaction'::notification_kind,
       r.user_id,
       p.id,
       1 + (random() * 20)::int,
       'post_reaction:' || p.id,
       -- Two thirds read, so the partial unread index has a small, realistic
       -- slice of the table.
       CASE WHEN random() < 0.66 THEN now() - (random() * INTERVAL '10 days') END,
       p.created_at + (random() * INTERVAL '6 days')
  FROM posts p
  JOIN LATERAL (
        SELECT user_id FROM post_reactions WHERE post_id = p.id LIMIT 1
  ) r ON TRUE
 WHERE random() < 0.5
   AND p.author_id <> r.user_id
ON CONFLICT DO NOTHING;

INSERT INTO notifications (recipient_id, kind, actor_id, actor_count, dedup_key, read_at, created_at)
SELECT f.followee_id,
       'follow'::notification_kind,
       f.follower_id,
       1,
       'follow:' || f.follower_id,
       CASE WHEN random() < 0.5 THEN now() - (random() * INTERVAL '5 days') END,
       f.created_at
  FROM follows f
 WHERE random() < 0.2
ON CONFLICT DO NOTHING;

-- --------------------------------------------------------------------------
-- 9. Materialised feed
-- --------------------------------------------------------------------------
-- Fan-out for non-celebrity authors only, exactly as queries.sql §7b does it.
-- The celebrity posts are deliberately absent here: the feed query merges them
-- in at read time, and this is what makes that branch observable in EXPLAIN.
INSERT INTO feed_entries (user_id, post_id, author_id, created_at)
SELECT f.follower_id, p.id, p.author_id, p.created_at
  FROM posts p
  JOIN follows f ON f.followee_id = p.author_id
 WHERE p.deleted_at IS NULL
   AND p.visibility IN ('public', 'followers')
   AND NOT EXISTS (SELECT 1 FROM celebrity_accounts ca WHERE ca.user_id = p.author_id)
   -- Only the recent window, mirroring the trim in queries.sql §7c.
   AND p.created_at > now() - INTERVAL '60 days'
ON CONFLICT DO NOTHING;

COMMIT;

-- --------------------------------------------------------------------------
-- 10. Statistics
-- --------------------------------------------------------------------------
-- Without this the planner has no statistics and will produce plans that bear
-- no relation to the ones it would choose in production. Any EXPLAIN taken
-- before this runs is misleading.
ANALYZE;
