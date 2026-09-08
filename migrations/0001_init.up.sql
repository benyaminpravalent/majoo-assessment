-- 0001_init.up.sql
--
-- Initial schema for the blog API.
--
-- Design principles applied throughout this file:
--
--   * Invalid state is rejected by the database, not only by the application.
--     Every rule the API enforces (roles, statuses, lengths, formats, the
--     draft/published_at pairing, non-negative counters) also exists here as a
--     CHECK or a constraint, so a bug in a handler — or a manual UPDATE during
--     an incident — cannot persist a row the domain considers impossible.
--   * UUID primary keys. The API is expected to sit behind a load balancer with
--     more than one writer, and UUIDs let the application generate an ID before
--     the INSERT (useful for logging and for building the outbox pattern later)
--     without a shared sequence. The cost versus bigserial is wider keys and
--     random insert order; at blog scale that is not the bottleneck.
--   * Soft deletes on posts and comments (deleted_at), because a blog needs
--     moderation and undo. Users are hard-deleted with ON DELETE CASCADE, since
--     account erasure must actually erase.
--   * updated_at is maintained by a trigger rather than by each UPDATE
--     statement, so it cannot drift when a new query forgets the column.

-- --------------------------------------------------------------------------
-- Shared helpers
-- --------------------------------------------------------------------------

-- Keeps updated_at honest regardless of which statement performed the write.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- --------------------------------------------------------------------------
-- users
-- --------------------------------------------------------------------------

CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT        NOT NULL,
    username      TEXT        NOT NULL,
    display_name  TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    role          TEXT        NOT NULL DEFAULT 'user',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_email_unique    UNIQUE (email),
    CONSTRAINT users_username_unique UNIQUE (username),

    -- Email and username are stored already folded to lower case, so a plain
    -- UNIQUE constraint gives case-insensitive identity without needing the
    -- citext extension or a functional index. The application normalises on the
    -- way in; these CHECKs make sure nothing else can bypass that.
    CONSTRAINT users_email_lowercase    CHECK (email = lower(email)),
    CONSTRAINT users_username_lowercase CHECK (username = lower(username)),

    CONSTRAINT users_email_length CHECK (char_length(email) BETWEEN 3 AND 254),
    CONSTRAINT users_email_format CHECK (email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),

    -- Mirrors the "username" rule in internal/platform/validation.
    CONSTRAINT users_username_format CHECK (username ~ '^[a-z0-9_-]{3,30}$'),

    CONSTRAINT users_display_name_length CHECK (char_length(btrim(display_name)) BETWEEN 1 AND 80),

    CONSTRAINT users_role_valid CHECK (role IN ('user', 'admin')),

    -- A blank hash would let an empty password compare successfully against a
    -- future buggy comparison. Refuse it outright.
    CONSTRAINT users_password_hash_present CHECK (char_length(password_hash) >= 20)
);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON COLUMN users.password_hash IS 'bcrypt hash. Never selected into any API response type.';

-- --------------------------------------------------------------------------
-- posts
-- --------------------------------------------------------------------------

CREATE TABLE posts (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title         TEXT        NOT NULL,
    slug          TEXT        NOT NULL,
    content       TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'draft',
    comment_count INTEGER     NOT NULL DEFAULT 0,
    published_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ,

    -- Full-text search over title (weighted A) and body (weighted B). Computing
    -- it as a STORED generated column means the index can never disagree with
    -- the row, which a trigger-maintained column can after a bulk UPDATE.
    search_vector TSVECTOR GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(content, '')), 'B')
    ) STORED,

    CONSTRAINT posts_title_length   CHECK (char_length(btrim(title)) BETWEEN 3 AND 200),
    CONSTRAINT posts_content_length CHECK (char_length(btrim(content)) BETWEEN 1 AND 50000),
    CONSTRAINT posts_slug_format    CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) BETWEEN 3 AND 220),
    CONSTRAINT posts_status_valid   CHECK (status IN ('draft', 'published')),

    -- comment_count is denormalised for the list endpoint and adjusted inside
    -- the same transaction as the comment write. If that arithmetic is ever
    -- wrong, this constraint turns a silent data bug into a failed transaction.
    CONSTRAINT posts_comment_count_non_negative CHECK (comment_count >= 0),

    -- A published post always has a publication time; a draft never does.
    -- Without this the two columns can disagree and every reader has to guess.
    CONSTRAINT posts_published_at_consistent CHECK (
        (status = 'published' AND published_at IS NOT NULL) OR
        (status = 'draft'     AND published_at IS NULL)
    )
);

CREATE TRIGGER posts_set_updated_at
    BEFORE UPDATE ON posts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Slugs must be unique among live posts only. A partial unique index lets a
-- deleted post release its slug, which a table-level UNIQUE cannot express.
CREATE UNIQUE INDEX posts_slug_active_unique
    ON posts (slug)
    WHERE deleted_at IS NULL;

-- Public listing: published posts, newest first. id is the tiebreaker so the
-- ordering is total and a page boundary cannot duplicate or skip a row when two
-- posts share a created_at. Partial on the exact predicate the query uses, which
-- keeps the index small and lets the planner use it as an ordered scan.
CREATE INDEX posts_public_feed_idx
    ON posts (created_at DESC, id DESC)
    WHERE deleted_at IS NULL AND status = 'published';

-- "Posts by this author", used both by the public author page and by an author
-- browsing their own drafts, hence no status predicate here.
CREATE INDEX posts_author_created_idx
    ON posts (author_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

-- Full-text search. GIN is the right choice for tsvector: slower to update than
-- GiST but far faster to query, and posts are read far more often than written.
CREATE INDEX posts_search_idx
    ON posts USING GIN (search_vector);

-- --------------------------------------------------------------------------
-- comments
-- --------------------------------------------------------------------------

CREATE TABLE comments (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    post_id    UUID        NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    author_id  UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id  UUID,
    content    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,

    CONSTRAINT comments_content_length CHECK (char_length(btrim(content)) BETWEEN 1 AND 5000),
    CONSTRAINT comments_not_self_parent CHECK (parent_id IS NULL OR parent_id <> id),

    -- Composite key needed by the self-referencing foreign key below.
    CONSTRAINT comments_id_post_unique UNIQUE (id, post_id),

    -- A reply must live on the same post as its parent. Expressing this as a
    -- composite foreign key rather than an application check means it holds even
    -- for writes that never pass through this service.
    CONSTRAINT comments_parent_same_post
        FOREIGN KEY (parent_id, post_id)
        REFERENCES comments (id, post_id)
        ON DELETE CASCADE
);

CREATE TRIGGER comments_set_updated_at
    BEFORE UPDATE ON comments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Thread listing for one post, oldest first (reading order). Composite on
-- (post_id, created_at, id): post_id is the equality predicate and must lead,
-- created_at supplies the ordering, id makes it total.
CREATE INDEX comments_post_created_idx
    ON comments (post_id, created_at, id)
    WHERE deleted_at IS NULL;

-- Supports "comments written by this user" and moderation views.
CREATE INDEX comments_author_created_idx
    ON comments (author_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- Supports assembling reply trees. Partial on parent_id IS NOT NULL because
-- top-level comments are the majority and would otherwise bloat the index.
CREATE INDEX comments_parent_idx
    ON comments (parent_id)
    WHERE deleted_at IS NULL AND parent_id IS NOT NULL;

-- --------------------------------------------------------------------------
-- refresh_tokens
-- --------------------------------------------------------------------------
--
-- Access tokens are short-lived, stateless JWTs; refresh tokens are opaque,
-- long-lived and stored here so they can be revoked. Only the SHA-256 digest is
-- persisted: a dump of this table does not let an attacker mint access tokens.
-- Rotation is recorded via replaced_by, which makes reuse of an already-rotated
-- token detectable (the classic refresh-token-theft signal).

CREATE TABLE refresh_tokens (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  BYTEA       NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    replaced_by UUID        REFERENCES refresh_tokens (id) ON DELETE SET NULL,
    user_agent  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT refresh_tokens_hash_unique UNIQUE (token_hash),
    CONSTRAINT refresh_tokens_hash_is_sha256 CHECK (octet_length(token_hash) = 32),
    CONSTRAINT refresh_tokens_expires_after_creation CHECK (expires_at > created_at),
    CONSTRAINT refresh_tokens_user_agent_length CHECK (user_agent IS NULL OR char_length(user_agent) <= 512)
);

-- "All live sessions for this user", used by logout-everywhere and by the
-- revocation check. Partial: revoked rows are only ever read by audit queries.
CREATE INDEX refresh_tokens_active_by_user_idx
    ON refresh_tokens (user_id)
    WHERE revoked_at IS NULL;

-- Supports the periodic purge of expired rows.
CREATE INDEX refresh_tokens_expires_at_idx
    ON refresh_tokens (expires_at);
