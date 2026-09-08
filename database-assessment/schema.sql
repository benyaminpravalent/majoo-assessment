-- =========================================================================
-- Social media platform — normalised schema
--
-- Assessment section 3, task 1: "Create normalized database schema" and
-- "Define relationships and constraints".
--
-- Target: PostgreSQL 16.
-- Apply with:  psql "$DATABASE_URL" -f database-assessment/schema.sql
-- Then:        psql "$DATABASE_URL" -f database-assessment/indexes.sql
--
-- The design rationale — why these tables, why these constraints, and which
-- edge cases each one closes — is in docs/social-media-database-design.md.
-- This file is the executable artefact; the reasoning lives there, with only
-- short notes here where a reader of the SQL alone would be puzzled.
--
-- Two conventions apply throughout:
--
--   * Every rule the application enforces also exists here as a constraint.
--     An application bug can then produce a failed transaction instead of a
--     corrupt row.
--   * Deletion behaviour is chosen per relationship rather than defaulting to
--     CASCADE. Some rows must vanish with their parent; others must survive it.
-- =========================================================================

BEGIN;

-- citext gives case-insensitive uniqueness for handles without a functional
-- index, which keeps the "find user by handle" plan trivially simple.
CREATE EXTENSION IF NOT EXISTS citext;
-- pg_trgm powers fuzzy handle and display-name search.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- --------------------------------------------------------------------------
-- Shared helpers
-- --------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- Enumerations are native types rather than TEXT + CHECK. They are compact
-- (4 bytes), self-documenting in \d output, and adding a value is a cheap
-- ALTER TYPE. The cost is that removing a value is awkward — acceptable, since
-- these sets change rarely.
CREATE TYPE reaction_kind    AS ENUM ('like', 'love', 'haha', 'wow', 'sad', 'angry');
CREATE TYPE media_kind       AS ENUM ('image', 'video', 'gif', 'audio');
CREATE TYPE post_visibility  AS ENUM ('public', 'followers', 'private');
CREATE TYPE notification_kind AS ENUM (
    'follow', 'post_reaction', 'comment', 'comment_reply',
    'mention', 'message', 'follow_request'
);
CREATE TYPE conversation_kind AS ENUM ('direct', 'group');

-- ==========================================================================
-- 1. Users and profiles
-- ==========================================================================

CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle        CITEXT      NOT NULL,
    email         CITEXT      NOT NULL,
    password_hash TEXT        NOT NULL,

    -- Denormalised counters. Maintained by triggers below, inside the same
    -- transaction as the row they count, so they cannot silently drift. They
    -- exist because "1.2M followers" appears on every profile view and a
    -- COUNT(*) over a 1.2M-row slice is not a per-request operation.
    follower_count  INTEGER   NOT NULL DEFAULT 0,
    following_count INTEGER   NOT NULL DEFAULT 0,
    post_count      INTEGER   NOT NULL DEFAULT 0,

    is_private    BOOLEAN     NOT NULL DEFAULT FALSE,
    is_verified   BOOLEAN     NOT NULL DEFAULT FALSE,

    -- Soft delete. A hard DELETE would cascade away every post and message the
    -- user ever wrote, including the halves of conversations other people still
    -- need to read. Deactivation hides them; a separate erasure job handles a
    -- genuine right-to-be-forgotten request.
    deactivated_at TIMESTAMPTZ,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_handle_unique UNIQUE (handle),
    CONSTRAINT users_email_unique  UNIQUE (email),
    CONSTRAINT users_handle_format CHECK (handle ~ '^[A-Za-z0-9_]{3,30}$'),
    CONSTRAINT users_email_format  CHECK (email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
    CONSTRAINT users_counts_non_negative CHECK (
        follower_count >= 0 AND following_count >= 0 AND post_count >= 0
    )
);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Profile fields live in their own table, one row per user.
--
-- The split is deliberate: users is read on nearly every request (auth,
-- authorship, counters), while bio and avatar are read only on a profile view.
-- Keeping the wide, rarely-read columns out of users keeps more of the hot
-- table in cache per page.
CREATE TABLE user_profiles (
    user_id      UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    display_name TEXT        NOT NULL DEFAULT '',
    bio          TEXT        NOT NULL DEFAULT '',
    avatar_url   TEXT,
    header_url   TEXT,
    location     TEXT,
    website_url  TEXT,
    birth_date   DATE,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT user_profiles_display_name_length CHECK (char_length(display_name) <= 80),
    CONSTRAINT user_profiles_bio_length          CHECK (char_length(bio) <= 500),
    CONSTRAINT user_profiles_location_length     CHECK (location IS NULL OR char_length(location) <= 100),
    -- A birth date in the future is always wrong; so is one implying an age
    -- over 130. Both are cheap to reject and expensive to discover later.
    CONSTRAINT user_profiles_birth_date_sane CHECK (
        birth_date IS NULL
        OR (birth_date <= CURRENT_DATE AND birth_date > CURRENT_DATE - INTERVAL '130 years')
    )
);

CREATE TRIGGER user_profiles_set_updated_at
    BEFORE UPDATE ON user_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ==========================================================================
-- 2. Follow graph
-- ==========================================================================

-- follows is a directed edge: follower_id follows followee_id.
--
-- Two edge cases from the brief are closed here rather than in application
-- code, because the graph is written from several paths — the API, an import
-- job, an admin tool — and only the database sees all of them:
--
--   * duplicate follows: impossible, the primary key is the pair;
--   * self-follow: impossible, follows_not_self rejects it.
CREATE TABLE follows (
    follower_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    followee_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The composite primary key IS the uniqueness rule; no surrogate id is
    -- needed, and its index is the one "does A follow B?" uses.
    CONSTRAINT follows_pkey PRIMARY KEY (follower_id, followee_id),
    CONSTRAINT follows_not_self CHECK (follower_id <> followee_id)
);

-- Pending requests to follow a private account. Kept separate from follows so
-- that "is following" never has to mean "is following, and approved" — a
-- predicate that would be easy to forget in one query out of twenty.
CREATE TABLE follow_requests (
    requester_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    target_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT follow_requests_pkey PRIMARY KEY (requester_id, target_id),
    CONSTRAINT follow_requests_not_self CHECK (requester_id <> target_id)
);

CREATE TABLE blocks (
    blocker_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    blocked_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT blocks_pkey PRIMARY KEY (blocker_id, blocked_id),
    CONSTRAINT blocks_not_self CHECK (blocker_id <> blocked_id)
);

-- ==========================================================================
-- 3. Posts and media
-- ==========================================================================

CREATE TABLE posts (
    id         UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id  UUID            NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    body       TEXT            NOT NULL DEFAULT '',
    visibility post_visibility NOT NULL DEFAULT 'public',

    -- Self-reference for shares and quote-posts. ON DELETE SET NULL, not
    -- CASCADE: deleting an original must not silently delete everyone else's
    -- commentary on it. The share survives as an orphan the UI renders as
    -- "this post is no longer available".
    shared_post_id UUID REFERENCES posts (id) ON DELETE SET NULL,

    -- Reply-to for threaded posts, distinct from comments: this is a top-level
    -- post that happens to reply, the way a thread works.
    reply_to_post_id UUID REFERENCES posts (id) ON DELETE SET NULL,

    comment_count  INTEGER NOT NULL DEFAULT 0,
    reaction_count INTEGER NOT NULL DEFAULT 0,
    share_count    INTEGER NOT NULL DEFAULT 0,

    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    search_vector TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', coalesce(body, ''))) STORED,

    CONSTRAINT posts_body_length CHECK (char_length(body) <= 5000),
    CONSTRAINT posts_counts_non_negative CHECK (
        comment_count >= 0 AND reaction_count >= 0 AND share_count >= 0
    ),
    CONSTRAINT posts_not_self_share CHECK (shared_post_id IS NULL OR shared_post_id <> id),
    CONSTRAINT posts_not_self_reply CHECK (reply_to_post_id IS NULL OR reply_to_post_id <> id),
    -- A post must say something: text, an attachment, or a share of something
    -- else. An entirely empty post is a client bug, and this catches it.
    CONSTRAINT posts_not_empty CHECK (
        char_length(btrim(body)) > 0 OR shared_post_id IS NOT NULL
    )
);

CREATE TRIGGER posts_set_updated_at
    BEFORE UPDATE ON posts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One row per attachment: a post has many media items, in a defined order.
--
-- Storing them as an array column on posts would make "the third image" hard to
-- address, per-item metadata impossible, and any future per-item moderation a
-- rewrite.
CREATE TABLE post_media (
    id            UUID       PRIMARY KEY DEFAULT gen_random_uuid(),
    post_id       UUID       NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    position      SMALLINT   NOT NULL,
    kind          media_kind NOT NULL,

    -- Only the object-storage key is stored. A full URL would bake the CDN
    -- hostname into every row and make moving buckets a data migration.
    storage_key   TEXT       NOT NULL,
    thumbnail_key TEXT,
    mime_type     TEXT       NOT NULL,
    byte_size     BIGINT     NOT NULL,
    width         INTEGER,
    height        INTEGER,
    duration_ms   INTEGER,
    alt_text      TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Ordering is data, not insertion luck. The unique constraint makes two
    -- items at the same position impossible.
    CONSTRAINT post_media_position_unique UNIQUE (post_id, position),
    CONSTRAINT post_media_position_range  CHECK (position BETWEEN 0 AND 9),
    CONSTRAINT post_media_size_positive   CHECK (byte_size > 0),
    CONSTRAINT post_media_alt_text_length CHECK (alt_text IS NULL OR char_length(alt_text) <= 1000),
    -- Dimensions belong to visual media; duration belongs to timed media.
    CONSTRAINT post_media_dimensions CHECK (
        (kind IN ('image', 'gif') AND width IS NOT NULL AND height IS NOT NULL)
        OR kind IN ('video', 'audio')
    ),
    CONSTRAINT post_media_duration CHECK (
        (kind IN ('video', 'audio') AND duration_ms IS NOT NULL AND duration_ms > 0)
        OR kind IN ('image', 'gif')
    )
);

-- ==========================================================================
-- 4. Comments
-- ==========================================================================

-- Comments use an adjacency list (parent_id) plus a materialised path.
--
-- Adjacency alone makes "fetch this whole thread in order" a recursive CTE.
-- The path column makes it one indexed range scan and one ORDER BY, at the cost
-- of a column that must be maintained on insert. Since comments are written
-- once and read many times, that is the right side of the trade.
--
-- Path format: dot-separated zero-padded sibling positions, e.g.
--   '0001'            a top-level comment
--   '0001.0003'       the third reply to it
--   '0001.0003.0002'  the second reply to that
-- Lexicographic order over this string is exactly thread reading order.
CREATE TABLE comments (
    id        UUID NOT NULL DEFAULT gen_random_uuid(),
    post_id   UUID NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    author_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id UUID,

    body  TEXT     NOT NULL,
    path  TEXT     NOT NULL,
    depth SMALLINT NOT NULL DEFAULT 0,

    reaction_count INTEGER NOT NULL DEFAULT 0,
    reply_count    INTEGER NOT NULL DEFAULT 0,

    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT comments_pkey PRIMARY KEY (id),

    CONSTRAINT comments_body_length  CHECK (char_length(btrim(body)) BETWEEN 1 AND 2000),
    -- Depth is capped so a reply chain cannot grow without bound, which would
    -- make the path column and the UI both unmanageable.
    CONSTRAINT comments_depth_range  CHECK (depth BETWEEN 0 AND 5),
    CONSTRAINT comments_not_self_parent CHECK (parent_id IS NULL OR parent_id <> id),
    CONSTRAINT comments_counts_non_negative CHECK (reaction_count >= 0 AND reply_count >= 0),
    -- A top-level comment has depth 0 and no parent; a reply has both.
    CONSTRAINT comments_depth_matches_parent CHECK (
        (parent_id IS NULL AND depth = 0) OR (parent_id IS NOT NULL AND depth > 0)
    ),

    -- Needed by the composite foreign key below.
    CONSTRAINT comments_id_post_unique UNIQUE (id, post_id),

    -- A reply must live on the same post as its parent. Expressing this as a
    -- composite foreign key means it holds for every writer, not only for code
    -- that remembers to check.
    CONSTRAINT comments_parent_same_post
        FOREIGN KEY (parent_id, post_id)
        REFERENCES comments (id, post_id)
        ON DELETE CASCADE
);

CREATE TRIGGER comments_set_updated_at
    BEFORE UPDATE ON comments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ==========================================================================
-- 5. Reactions
-- ==========================================================================

-- One row per (user, target). Changing from 'like' to 'love' is an UPDATE of
-- kind, not a second row — which is exactly what the primary key enforces.
--
-- Posts and comments have separate reaction tables rather than one polymorphic
-- table with a (target_type, target_id) pair. A polymorphic key cannot have a
-- foreign key, so nothing would stop a reaction pointing at a post that no
-- longer exists. Two tables cost a little duplication and buy referential
-- integrity, which is the better trade.
CREATE TABLE post_reactions (
    post_id    UUID          NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    user_id    UUID          NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       reaction_kind NOT NULL,
    created_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ   NOT NULL DEFAULT now(),

    -- Duplicate reactions are impossible; a reaction change is an UPDATE.
    CONSTRAINT post_reactions_pkey PRIMARY KEY (post_id, user_id)
);

CREATE TRIGGER post_reactions_set_updated_at
    BEFORE UPDATE ON post_reactions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE comment_reactions (
    comment_id UUID          NOT NULL REFERENCES comments (id) ON DELETE CASCADE,
    user_id    UUID          NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       reaction_kind NOT NULL,
    created_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ   NOT NULL DEFAULT now(),

    CONSTRAINT comment_reactions_pkey PRIMARY KEY (comment_id, user_id)
);

CREATE TRIGGER comment_reactions_set_updated_at
    BEFORE UPDATE ON comment_reactions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Per-kind tallies, so "12k likes, 340 loves" needs no GROUP BY over millions
-- of reaction rows. Maintained by trigger alongside posts.reaction_count.
CREATE TABLE post_reaction_counts (
    post_id UUID          NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    kind    reaction_kind NOT NULL,
    total   INTEGER       NOT NULL DEFAULT 0,

    CONSTRAINT post_reaction_counts_pkey PRIMARY KEY (post_id, kind),
    CONSTRAINT post_reaction_counts_non_negative CHECK (total >= 0)
);

-- ==========================================================================
-- 6. Private messaging
-- ==========================================================================

-- Conversations are modelled uniformly: a direct message is a conversation with
-- exactly two participants. Modelling DMs as their own table with sender_id and
-- recipient_id would mean writing every message query twice and rewriting both
-- the day group chat is added.
CREATE TABLE conversations (
    id         UUID              PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       conversation_kind NOT NULL DEFAULT 'direct',
    title      TEXT,
    created_by UUID              REFERENCES users (id) ON DELETE SET NULL,

    -- Denormalised pointer to the newest message. Without it, the conversation
    -- list — the most-viewed screen in any messaging UI — needs a correlated
    -- "latest message per conversation" subquery per row. See queries.sql §5.
    -- The circular dependency with messages is resolved by adding the foreign
    -- key after that table exists.
    last_message_id UUID,
    last_message_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT conversations_title_length CHECK (title IS NULL OR char_length(title) <= 100),
    -- A direct conversation is between two people and has no title; a group
    -- conversation may have one.
    CONSTRAINT conversations_direct_has_no_title CHECK (kind <> 'direct' OR title IS NULL)
);

CREATE TRIGGER conversations_set_updated_at
    BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Membership, plus each participant's own read cursor.
--
-- The read cursor lives here rather than in a per-message read table: one row
-- per participant instead of one row per participant per message. For a
-- 10,000-message group chat that is 20 rows instead of 200,000, and "unread
-- count" becomes a range count rather than an anti-join.
CREATE TABLE conversation_participants (
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    joined_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Leaving is recorded rather than deleting the row, so history stays
    -- attributable and a re-join does not lose the earlier membership.
    left_at     TIMESTAMPTZ,

    -- The read cursor: everything at or before this sequence has been read.
    last_read_seq  BIGINT      NOT NULL DEFAULT 0,
    last_read_at   TIMESTAMPTZ,

    is_muted    BOOLEAN     NOT NULL DEFAULT FALSE,
    is_admin    BOOLEAN     NOT NULL DEFAULT FALSE,

    CONSTRAINT conversation_participants_pkey PRIMARY KEY (conversation_id, user_id),
    CONSTRAINT conversation_participants_left_after_joined CHECK (left_at IS NULL OR left_at >= joined_at),
    CONSTRAINT conversation_participants_seq_non_negative CHECK (last_read_seq >= 0)
);

-- Messages carry a per-conversation monotonic sequence number in addition to
-- their timestamp.
--
-- Ordering by created_at alone is not safe: two messages can share a timestamp,
-- and clock adjustment can move one behind another. seq is assigned by the
-- database, is strictly increasing within a conversation, and makes both
-- ordering and the read cursor exact.
CREATE TABLE messages (
    id              UUID   NOT NULL DEFAULT gen_random_uuid(),
    conversation_id UUID   NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             BIGINT NOT NULL,

    -- The sender's account may be erased; the message must remain readable to
    -- the other participant, rendered as "deleted user".
    sender_id UUID REFERENCES users (id) ON DELETE SET NULL,

    body TEXT NOT NULL DEFAULT '',

    -- Client-generated idempotency key. A mobile client retrying a send over a
    -- flaky connection must not post the message twice.
    client_nonce UUID,

    edited_at  TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT messages_pkey PRIMARY KEY (id),
    CONSTRAINT messages_seq_unique UNIQUE (conversation_id, seq),
    CONSTRAINT messages_seq_positive CHECK (seq > 0),
    CONSTRAINT messages_body_length CHECK (char_length(body) <= 5000),
    CONSTRAINT messages_edited_after_created CHECK (edited_at IS NULL OR edited_at >= created_at)
);

-- One attachment per row, mirroring post_media.
CREATE TABLE message_media (
    id          UUID       PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id  UUID       NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    position    SMALLINT   NOT NULL DEFAULT 0,
    kind        media_kind NOT NULL,
    storage_key TEXT       NOT NULL,
    mime_type   TEXT       NOT NULL,
    byte_size   BIGINT     NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT message_media_position_unique UNIQUE (message_id, position),
    CONSTRAINT message_media_size_positive   CHECK (byte_size > 0)
);

-- The circular reference, added now that messages exists. ON DELETE SET NULL so
-- deleting the newest message does not remove the conversation.
ALTER TABLE conversations
    ADD CONSTRAINT conversations_last_message_fkey
    FOREIGN KEY (last_message_id) REFERENCES messages (id) ON DELETE SET NULL;

-- ==========================================================================
-- 7. Notifications
-- ==========================================================================

-- Notifications are aggregated, not one row per event.
--
-- "Alice and 340 others liked your post" is one row whose actor_count is 341,
-- not 341 rows the UI has to group at read time. The dedup_key is what makes
-- that possible, and it is what closes the "notification deduplication" edge
-- case from the brief: an ON CONFLICT upsert on that key either creates the
-- notification or bumps its counter.
CREATE TABLE notifications (
    id           UUID              NOT NULL DEFAULT gen_random_uuid(),
    recipient_id UUID              NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         notification_kind NOT NULL,

    -- The most recent actor, shown first in the rendered text.
    actor_id     UUID REFERENCES users (id) ON DELETE CASCADE,
    actor_count  INTEGER NOT NULL DEFAULT 1,

    -- The subject. Nullable because a 'follow' notification has no post.
    post_id      UUID REFERENCES posts (id) ON DELETE CASCADE,
    comment_id   UUID REFERENCES comments (id) ON DELETE CASCADE,

    -- Groups repeated events onto one row, e.g. 'post_reaction:{post_id}'.
    dedup_key    TEXT NOT NULL,

    read_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notifications_pkey PRIMARY KEY (id),
    CONSTRAINT notifications_actor_count_positive CHECK (actor_count >= 1),
    -- Nobody should be notified about their own action.
    CONSTRAINT notifications_not_self CHECK (actor_id IS NULL OR actor_id <> recipient_id)
);

CREATE TRIGGER notifications_set_updated_at
    BEFORE UPDATE ON notifications
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The unread badge is read on every page load. Recomputing it from
-- notifications each time is an unnecessary scan; this single row per user
-- makes it a primary-key lookup.
CREATE TABLE notification_counters (
    user_id      UUID    PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    unread_count INTEGER NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notification_counters_non_negative CHECK (unread_count >= 0)
);

-- ==========================================================================
-- 8. Feed materialisation (hybrid fan-out)
-- ==========================================================================

-- Precomputed home-feed entries for ordinary users. Celebrity posts are NOT
-- fanned out here; they are merged at read time. The rationale for the hybrid
-- is in docs/social-media-database-design.md §6.
--
-- This table is a cache with a database's durability: it can be rebuilt from
-- posts and follows at any time, which is what makes a fan-out bug recoverable.
CREATE TABLE feed_entries (
    user_id   UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    post_id   UUID        NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    author_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Copied from posts so the feed can be ordered and paginated without a join.
    created_at TIMESTAMPTZ NOT NULL,
    -- Optional ranking score for a non-chronological feed.
    score      REAL,

    CONSTRAINT feed_entries_pkey PRIMARY KEY (user_id, post_id)
);

-- Accounts above the fan-out threshold. Writing a post from one of these must
-- not enqueue millions of inserts; readers merge their posts in instead.
CREATE TABLE celebrity_accounts (
    user_id        UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    follower_count INTEGER     NOT NULL,
    designated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ==========================================================================
-- 9. Counter maintenance
-- ==========================================================================
--
-- Triggers keep the denormalised counters correct inside the same transaction
-- as the row they count. The alternative — updating them from application code
-- — means every new write path has to remember, and one that forgets produces
-- a drift nobody notices for months.
--
-- The cost is real and worth stating: a trigger is invisible at the call site,
-- and every counter UPDATE takes a row lock on the parent, which serialises
-- concurrent writers to the same hot post. §6.7 of the design document covers
-- moving the hottest counters to Redis with periodic reconciliation.

CREATE OR REPLACE FUNCTION follows_maintain_counts()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE users SET following_count = following_count + 1 WHERE id = NEW.follower_id;
        UPDATE users SET follower_count  = follower_count  + 1 WHERE id = NEW.followee_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE users SET following_count = following_count - 1 WHERE id = OLD.follower_id;
        UPDATE users SET follower_count  = follower_count  - 1 WHERE id = OLD.followee_id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER follows_counts
    AFTER INSERT OR DELETE ON follows
    FOR EACH ROW EXECUTE FUNCTION follows_maintain_counts();

CREATE OR REPLACE FUNCTION posts_maintain_author_count()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE users SET post_count = post_count + 1 WHERE id = NEW.author_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE users SET post_count = post_count - 1 WHERE id = OLD.author_id;
    ELSIF TG_OP = 'UPDATE' THEN
        -- Soft delete and undelete move the counter without a row appearing or
        -- disappearing, so UPDATE has to be handled too.
        IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
            UPDATE users SET post_count = post_count - 1 WHERE id = NEW.author_id;
        ELSIF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
            UPDATE users SET post_count = post_count + 1 WHERE id = NEW.author_id;
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER posts_author_count
    AFTER INSERT OR DELETE OR UPDATE OF deleted_at ON posts
    FOR EACH ROW EXECUTE FUNCTION posts_maintain_author_count();

CREATE OR REPLACE FUNCTION post_reactions_maintain_counts()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE posts SET reaction_count = reaction_count + 1 WHERE id = NEW.post_id;
        INSERT INTO post_reaction_counts (post_id, kind, total)
        VALUES (NEW.post_id, NEW.kind, 1)
        ON CONFLICT (post_id, kind) DO UPDATE SET total = post_reaction_counts.total + 1;

    ELSIF TG_OP = 'DELETE' THEN
        UPDATE posts SET reaction_count = reaction_count - 1 WHERE id = OLD.post_id;
        UPDATE post_reaction_counts SET total = total - 1
         WHERE post_id = OLD.post_id AND kind = OLD.kind;

    ELSIF TG_OP = 'UPDATE' AND OLD.kind <> NEW.kind THEN
        -- Changing 'like' to 'love' moves one tally to another; the total on
        -- posts does not change.
        UPDATE post_reaction_counts SET total = total - 1
         WHERE post_id = OLD.post_id AND kind = OLD.kind;
        INSERT INTO post_reaction_counts (post_id, kind, total)
        VALUES (NEW.post_id, NEW.kind, 1)
        ON CONFLICT (post_id, kind) DO UPDATE SET total = post_reaction_counts.total + 1;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER post_reactions_counts
    AFTER INSERT OR DELETE OR UPDATE OF kind ON post_reactions
    FOR EACH ROW EXECUTE FUNCTION post_reactions_maintain_counts();

CREATE OR REPLACE FUNCTION comments_maintain_counts()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE posts SET comment_count = comment_count + 1 WHERE id = NEW.post_id;
        IF NEW.parent_id IS NOT NULL THEN
            UPDATE comments SET reply_count = reply_count + 1 WHERE id = NEW.parent_id;
        END IF;

    ELSIF TG_OP = 'UPDATE' THEN
        IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
            UPDATE posts SET comment_count = comment_count - 1 WHERE id = NEW.post_id;
            IF NEW.parent_id IS NOT NULL THEN
                UPDATE comments SET reply_count = reply_count - 1 WHERE id = NEW.parent_id;
            END IF;
        ELSIF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
            UPDATE posts SET comment_count = comment_count + 1 WHERE id = NEW.post_id;
            IF NEW.parent_id IS NOT NULL THEN
                UPDATE comments SET reply_count = reply_count + 1 WHERE id = NEW.parent_id;
            END IF;
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER comments_counts
    AFTER INSERT OR UPDATE OF deleted_at ON comments
    FOR EACH ROW EXECUTE FUNCTION comments_maintain_counts();

-- Assign the per-conversation sequence number and keep the conversation's
-- pointer to its newest message current.
--
-- The sequence is derived under the conversation's row lock, which serialises
-- concurrent sends to the same conversation. That is exactly the behaviour
-- wanted: message order within a conversation must be unambiguous, and a
-- conversation is not a high-contention object.
CREATE OR REPLACE FUNCTION messages_assign_seq()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    next_seq BIGINT;
BEGIN
    SELECT coalesce(max(seq), 0) + 1 INTO next_seq
      FROM messages
     WHERE conversation_id = NEW.conversation_id;

    NEW.seq := next_seq;
    RETURN NEW;
END;
$$;

CREATE TRIGGER messages_seq
    BEFORE INSERT ON messages
    FOR EACH ROW
    WHEN (NEW.seq IS NULL OR NEW.seq = 0)
    EXECUTE FUNCTION messages_assign_seq();

CREATE OR REPLACE FUNCTION messages_touch_conversation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    UPDATE conversations
       SET last_message_id = NEW.id,
           last_message_at = NEW.created_at
     WHERE id = NEW.conversation_id;
    RETURN NULL;
END;
$$;

CREATE TRIGGER messages_touch_conversation_after_insert
    AFTER INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION messages_touch_conversation();

-- Keep the unread badge in step with the notifications table.
CREATE OR REPLACE FUNCTION notifications_maintain_counter()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.read_at IS NULL THEN
        INSERT INTO notification_counters (user_id, unread_count)
        VALUES (NEW.recipient_id, 1)
        ON CONFLICT (user_id) DO UPDATE
            SET unread_count = notification_counters.unread_count + 1,
                updated_at   = now();

    ELSIF TG_OP = 'UPDATE' THEN
        IF OLD.read_at IS NULL AND NEW.read_at IS NOT NULL THEN
            UPDATE notification_counters
               SET unread_count = greatest(unread_count - 1, 0), updated_at = now()
             WHERE user_id = NEW.recipient_id;
        ELSIF OLD.read_at IS NOT NULL AND NEW.read_at IS NULL THEN
            UPDATE notification_counters
               SET unread_count = unread_count + 1, updated_at = now()
             WHERE user_id = NEW.recipient_id;
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER notifications_counter
    AFTER INSERT OR UPDATE OF read_at ON notifications
    FOR EACH ROW EXECUTE FUNCTION notifications_maintain_counter();

COMMIT;
