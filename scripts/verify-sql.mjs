// Verify every piece of SQL in this repository against a real PostgreSQL engine.
//
//   npm install --no-save @electric-sql/pglite
//   node scripts/verify-sql.mjs
//
// PGlite is PostgreSQL compiled to WebAssembly, so this runs the genuine query
// planner and the genuine constraint machinery without needing a server
// installed. Two parts:
//
//   Part 1 — the blog API's own migration (assessment section 1):
//     applies migrations/0001_init.up.sql, confirms nine documented constraints
//     reject invalid rows, confirms the partial unique index frees a slug on
//     soft delete, confirms the composite foreign key forces a reply onto its
//     parent's post, confirms full-text search and the updated_at trigger, then
//     applies the down migration and confirms it leaves nothing behind.
//
//   Part 2 — the social media assessment (section 3):
//     applies schema.sql, indexes.sql and seed.sql; executes every statement in
//     queries.sql with bound parameters; reconciles every trigger-maintained
//     counter against the rows it counts; confirms each documented edge case is
//     rejected; and prints the EXPLAIN plans claimed in queries.sql §13.
//
// What this does NOT do is produce timings, and it is not a substitute for
// running the Go migration runner against a live server. The seed volumes are
// small and PGlite is single-connection WebAssembly, so any wall-clock number
// here would say nothing about production. What is verified is that the SQL is
// valid, that the constraints behave as documented, and which index each query
// chooses.
//
// Exit code 0 means everything passed.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { PGlite } from '@electric-sql/pglite';
import { citext } from '@electric-sql/pglite/contrib/citext';
import { pg_trgm } from '@electric-sql/pglite/contrib/pg_trgm';

const here = path.dirname(fileURLToPath(import.meta.url));
const sqlDir = path.join(here, '..', 'database-assessment');
const migrationsDir = path.join(here, '..', 'migrations');
const read = (f) => fs.readFileSync(path.join(sqlDir, f), 'utf8');
const readMigration = (f) => fs.readFileSync(path.join(migrationsDir, f), 'utf8');

let failures = 0;
const ok = (msg) => console.log(`  ok    ${msg}`);
const fail = (msg, detail) => {
  failures += 1;
  console.error(`  FAIL  ${msg}`);
  if (detail) console.error(`        ${detail}`);
};

console.log(
  (await (await PGlite.create()).query('select version()')).rows[0].version);

// ===========================================================================
// Part 1 — the blog API's migration (assessment section 1)
// ===========================================================================
{
  console.log('\n== blog API migration: migrations/0001_init ==');
  const mig = await PGlite.create();

  try {
    await mig.exec(readMigration('0001_init.up.sql'));
    ok('0001_init.up.sql applies');
  } catch (err) {
    fail('0001_init.up.sql', err.message);
    process.exit(1);
  }

  const tables = (await mig.query(
    `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY 1`))
    .rows.map((r) => r.table_name);
  console.log(`        tables: ${tables.join(', ')}`);

  const owner = '11111111-1111-4111-8111-111111111111';
  await mig.query(
    `INSERT INTO users (id, email, username, display_name, password_hash)
     VALUES ($1, 'ben@example.com', 'ben', 'Ben', '$2a$10$0123456789012345678901')`, [owner]);

  const rejects = {
    'an email that is not folded to lower case': `INSERT INTO users (email, username, display_name, password_hash) VALUES ('Up@Example.com', 'up', 'U', '$2a$10$0123456789012345678901')`,
    'a username containing a space': `INSERT INTO users (email, username, display_name, password_hash) VALUES ('a@b.co', 'has space', 'U', '$2a$10$0123456789012345678901')`,
    'an unknown role': `INSERT INTO users (email, username, display_name, password_hash, role) VALUES ('c@b.co', 'cee', 'U', '$2a$10$0123456789012345678901', 'root')`,
    'a duplicate email': `INSERT INTO users (email, username, display_name, password_hash) VALUES ('ben@example.com', 'ben2', 'B', '$2a$10$0123456789012345678901')`,
    'a published post with no published_at': `INSERT INTO posts (author_id, title, slug, content, status) VALUES ('${owner}', 'A Title', 'a-title', 'body', 'published')`,
    'a draft that has a published_at': `INSERT INTO posts (author_id, title, slug, content, status, published_at) VALUES ('${owner}', 'A Title', 'a-title-b', 'body', 'draft', now())`,
    'a slug containing upper case': `INSERT INTO posts (author_id, title, slug, content, status) VALUES ('${owner}', 'A Title', 'Bad-Slug', 'body', 'draft')`,
    'a title below the minimum length': `INSERT INTO posts (author_id, title, slug, content, status) VALUES ('${owner}', 'ab', 'ab-title', 'body', 'draft')`,
    'a negative comment_count': `INSERT INTO posts (author_id, title, slug, content, status, comment_count) VALUES ('${owner}', 'A Title', 'a-title-c', 'body', 'draft', -1)`,
    'a refresh-token digest of the wrong width': `INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ('${owner}', '\\x0102'::bytea, now() + interval '1 day')`,
  };
  for (const [label, sql] of Object.entries(rejects)) {
    try {
      await mig.query(sql);
      fail(`${label} was NOT rejected`);
    } catch {
      ok(`${label} — rejected`);
    }
  }

  // The partial unique index must release a slug once the post is soft-deleted,
  // which a table-level UNIQUE could not express.
  const kept = '22222222-2222-4222-8222-222222222222';
  await mig.query(
    `INSERT INTO posts (id, author_id, title, slug, content, status)
     VALUES ($1, $2, 'Same Slug Post', 'same-slug', 'body', 'draft')`, [kept, owner]);
  try {
    await mig.query(
      `INSERT INTO posts (author_id, title, slug, content, status)
       VALUES ($1, 'Same Slug Post', 'same-slug', 'body', 'draft')`, [owner]);
    fail('a duplicate slug on a live post was NOT rejected');
  } catch {
    ok('a duplicate slug on a live post — rejected');
  }
  await mig.query(`UPDATE posts SET deleted_at = now() WHERE id = $1`, [kept]);
  await mig.query(
    `INSERT INTO posts (author_id, title, slug, content, status)
     VALUES ($1, 'Same Slug Post', 'same-slug', 'body', 'draft')`, [owner]);
  ok('the slug is reusable once the original is soft-deleted');

  // The composite foreign key must force a reply onto its parent's post.
  const postOne = (await mig.query(
    `INSERT INTO posts (author_id, title, slug, content, status)
     VALUES ($1, 'Post One', 'post-one', 'body', 'draft') RETURNING id`, [owner])).rows[0].id;
  const postTwo = (await mig.query(
    `INSERT INTO posts (author_id, title, slug, content, status)
     VALUES ($1, 'Post Two', 'post-two', 'body', 'draft') RETURNING id`, [owner])).rows[0].id;
  const root = (await mig.query(
    `INSERT INTO comments (post_id, author_id, content) VALUES ($1, $2, 'root') RETURNING id`,
    [postOne, owner])).rows[0].id;
  try {
    await mig.query(
      `INSERT INTO comments (post_id, author_id, parent_id, content) VALUES ($1, $2, $3, 'cross')`,
      [postTwo, owner, root]);
    fail('a reply on a different post than its parent was NOT rejected');
  } catch {
    ok('a reply on a different post than its parent — rejected');
  }
  await mig.query(
    `INSERT INTO comments (post_id, author_id, parent_id, content) VALUES ($1, $2, $3, 'reply')`,
    [postOne, owner, root]);
  ok('a reply on the same post is accepted');

  // Full-text search over the STORED generated column.
  await mig.query(
    `INSERT INTO posts (author_id, title, slug, content, status, published_at)
     VALUES ($1, 'Concurrency in Go', 'concurrency-in-go', 'Goroutines and channels', 'published', now())`,
    [owner]);
  const hits = await mig.query(
    `SELECT title FROM posts WHERE search_vector @@ websearch_to_tsquery('english', 'goroutines')`);
  if (hits.rows.length === 1) ok(`full-text search matches: ${hits.rows[0].title}`);
  else fail(`full-text search returned ${hits.rows.length} rows, expected 1`);

  // The updated_at trigger.
  const before = (await mig.query(`SELECT updated_at FROM users WHERE id = $1`, [owner])).rows[0].updated_at;
  await mig.query(`UPDATE users SET display_name = 'Ben S' WHERE id = $1`, [owner]);
  const after = (await mig.query(`SELECT updated_at FROM users WHERE id = $1`, [owner])).rows[0].updated_at;
  if (after > before) ok('the updated_at trigger fires on UPDATE');
  else fail('the updated_at trigger did not fire');

  // The down migration must fully reverse the up.
  await mig.exec(readMigration('0001_init.down.sql'));
  const remaining = Number((await mig.query(
    `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`)).rows[0].count);
  if (remaining === 0) ok('0001_init.down.sql leaves no tables behind');
  else fail(`0001_init.down.sql left ${remaining} table(s) behind`);

  await mig.close();
}

// ===========================================================================
// Part 2 — the social media assessment (section 3)
// ===========================================================================

const db = await PGlite.create({ extensions: { citext, pg_trgm } });

// ---------------------------------------------------------------------------
console.log('\n== applying schema, indexes and seed ==');
// ---------------------------------------------------------------------------
for (const file of ['schema.sql', 'indexes.sql', 'seed.sql']) {
  try {
    await db.exec(read(file));
    ok(file);
  } catch (err) {
    fail(file, err.message);
    process.exit(1);
  }
}

const counts = await db.query(`
  SELECT (SELECT count(*) FROM users) AS users,
         (SELECT count(*) FROM follows) AS follows,
         (SELECT count(*) FROM posts) AS posts,
         (SELECT count(*) FROM comments) AS comments,
         (SELECT count(*) FROM post_reactions) AS reactions,
         (SELECT count(*) FROM messages) AS messages,
         (SELECT count(*) FROM notifications) AS notifications,
         (SELECT count(*) FROM feed_entries) AS feed_entries`);
console.log('  seeded:', JSON.stringify(counts.rows[0]));

// ---------------------------------------------------------------------------
console.log('\n== executing every statement in queries.sql ==');
// ---------------------------------------------------------------------------

// Split on statements, ignoring whole-line comments. queries.sql contains no
// dollar-quoted bodies, so this is sufficient and keeps the harness small.
function statements(sql) {
  const out = [];
  let buf = [];
  for (const line of sql.split(/\r?\n/)) {
    if (/^\s*--/.test(line)) continue;
    buf.push(line);
    if (/;\s*$/.test(line)) {
      const s = buf.join('\n').trim();
      if (s && s !== ';') out.push(s);
      buf = [];
    }
  }
  return out;
}

const stmts = statements(read('queries.sql'));

const users = (await db.query(`SELECT id, handle FROM users ORDER BY handle LIMIT 2`)).rows;
const celebrity = (await db.query(`SELECT user_id FROM celebrity_accounts LIMIT 1`)).rows[0].user_id;
const post = (await db.query(`SELECT id, author_id FROM posts WHERE deleted_at IS NULL LIMIT 1`)).rows[0];
const conv = (await db.query(`SELECT conversation_id, user_id FROM conversation_participants LIMIT 1`)).rows[0];
const comment = (await db.query(`SELECT id, path, post_id FROM comments WHERE depth = 0 LIMIT 1`)).rows[0];
const notif = (await db.query(`SELECT id, recipient_id FROM notifications LIMIT 1`)).rows[0];
const viewer = users[0].id;
const other = users[1].id;
const now = new Date().toISOString();

// One entry per statement, in file order.
const params = [
  [users[0].handle, other],                                   // §1  profile by handle
  [viewer, null, null, 20],                                   // §2  following list
  [celebrity, viewer, null, null, 20],                        // §3  followers list
  [post.author_id, viewer, null, null, 20],                   // §4  user posts
  [comment.post_id, viewer, null, 50],                        // §5  comment thread
  [comment.post_id, comment.path, 20],                        // §5b subtree
  [post.id],                                                  // §6a reaction counts
  [post.id],                                                  // §6b reaction aggregate
  [],                                                         // §6c reconciliation
  [viewer, null, null, 20],                                   // §7  home feed
  [post.id, post.author_id, now, '00000000-0000-0000-0000-000000000000', 100], // §7b fan-out
  [viewer, 500],                                              // §7c feed trim
  [conv.user_id, null, 20],                                   // §8  conversation list
  [conv.user_id, 20],                                         // §8b lateral variant
  [conv.conversation_id, conv.user_id, null, 30],             // §9  message history
  [conv.conversation_id, conv.user_id, 5],                    // §9b advance read cursor
  [conv.user_id],                                             // §10 unread total
  [notif.recipient_id, false, null, null, 20],                // §11 notifications
  [notif.recipient_id, 'mention', other, post.id, null, 'mention:verify'], // §11b upsert
  [notif.recipient_id],                                       // §11c unread badge
  [notif.recipient_id, [notif.id]],                           // §12a mark read
  [notif.recipient_id, now],                                  // §12b mark all read
];

if (params.length !== stmts.length) {
  fail(`the parameter table has ${params.length} entries but queries.sql has ${stmts.length} statements`);
  process.exit(1);
}

for (let i = 0; i < stmts.length; i += 1) {
  const label = stmts[i].split('\n')[0].slice(0, 48).padEnd(48);
  try {
    const res = await db.query(stmts[i], params[i]);
    ok(`[${String(i).padStart(2)}] ${label} rows=${res.rows?.length ?? 0}`);
  } catch (err) {
    fail(`[${String(i).padStart(2)}] ${label}`, err.message);
  }
}

// ---------------------------------------------------------------------------
console.log('\n== trigger-maintained counters agree with their source rows ==');
// ---------------------------------------------------------------------------
const drifts = {
  'users.follower_count': `SELECT count(*) FROM users u WHERE u.follower_count <> (SELECT count(*) FROM follows f WHERE f.followee_id = u.id)`,
  'users.following_count': `SELECT count(*) FROM users u WHERE u.following_count <> (SELECT count(*) FROM follows f WHERE f.follower_id = u.id)`,
  'users.post_count': `SELECT count(*) FROM users u WHERE u.post_count <> (SELECT count(*) FROM posts p WHERE p.author_id = u.id AND p.deleted_at IS NULL)`,
  'posts.reaction_count': `SELECT count(*) FROM posts p WHERE p.reaction_count <> (SELECT count(*) FROM post_reactions r WHERE r.post_id = p.id)`,
  'posts.comment_count': `SELECT count(*) FROM posts p WHERE p.comment_count <> (SELECT count(*) FROM comments c WHERE c.post_id = p.id AND c.deleted_at IS NULL)`,
  'post_reaction_counts.total': `SELECT count(*) FROM post_reaction_counts rc WHERE rc.total <> (SELECT count(*) FROM post_reactions r WHERE r.post_id = rc.post_id AND r.kind = rc.kind)`,
  'notification_counters.unread_count': `SELECT count(*) FROM notification_counters nc WHERE nc.unread_count <> (SELECT count(*) FROM notifications n WHERE n.recipient_id = nc.user_id AND n.read_at IS NULL)`,
  'messages.seq has no gaps': `SELECT count(*) FROM (SELECT conversation_id, count(*) c, max(seq) m FROM messages GROUP BY 1) t WHERE c <> m`,
};

for (const [label, sql] of Object.entries(drifts)) {
  const n = Number((await db.query(sql)).rows[0].count);
  if (n === 0) ok(`${label} — no drift`);
  else fail(`${label} — ${n} row(s) disagree`);
}

// ---------------------------------------------------------------------------
console.log('\n== the database rejects each documented edge case ==');
// ---------------------------------------------------------------------------
const mustReject = {
  'a user following themselves': `INSERT INTO follows (follower_id, followee_id) SELECT id, id FROM users LIMIT 1`,
  'a duplicate follow': `INSERT INTO follows (follower_id, followee_id) SELECT follower_id, followee_id FROM follows LIMIT 1`,
  'a duplicate reaction': `INSERT INTO post_reactions (post_id, user_id, kind) SELECT post_id, user_id, 'love' FROM post_reactions LIMIT 1`,
  'a handle containing a space': `INSERT INTO users (handle, email, password_hash) VALUES ('has space', 'x@y.co', 'aaaaaaaaaaaaaaaaaaaaaaaa')`,
  'a comment deeper than the cap': `INSERT INTO comments (post_id, author_id, body, path, depth) SELECT id, author_id, 'x', '0001', 9 FROM posts LIMIT 1`,
  'a depth-0 comment with a parent': `INSERT INTO comments (post_id, author_id, parent_id, body, path, depth) SELECT c.post_id, c.author_id, c.id, 'x', '0002', 0 FROM comments c LIMIT 1`,
  'an entirely empty post': `INSERT INTO posts (author_id, body) SELECT id, '   ' FROM users LIMIT 1`,
  'notifying someone about their own action': `INSERT INTO notifications (recipient_id, kind, actor_id, dedup_key) SELECT id, 'follow', id, 'k' FROM users LIMIT 1`,
  'a media item beyond the position cap': `INSERT INTO post_media (post_id, position, kind, storage_key, mime_type, byte_size, width, height) SELECT id, 99, 'image', 'k', 'image/jpeg', 1, 1, 1 FROM posts LIMIT 1`,
  'an image with no dimensions': `INSERT INTO post_media (post_id, position, kind, storage_key, mime_type, byte_size) SELECT id, 8, 'image', 'k', 'image/jpeg', 1 FROM posts LIMIT 1`,
  'a refresh of a reaction onto a missing post': `INSERT INTO post_reactions (post_id, user_id, kind) SELECT gen_random_uuid(), id, 'like' FROM users LIMIT 1`,
};

for (const [label, sql] of Object.entries(mustReject)) {
  try {
    await db.query(sql);
    fail(`${label} was NOT rejected`);
  } catch {
    ok(`${label} — rejected`);
  }
}

// A reaction change must be an UPDATE, not a second row.
const r = (await db.query(`SELECT post_id, user_id FROM post_reactions LIMIT 1`)).rows[0];
const before = Number((await db.query(`SELECT count(*) FROM post_reactions WHERE post_id = $1`, [r.post_id])).rows[0].count);
await db.query(`UPDATE post_reactions SET kind = 'angry' WHERE post_id = $1 AND user_id = $2`, [r.post_id, r.user_id]);
const after = Number((await db.query(`SELECT count(*) FROM post_reactions WHERE post_id = $1`, [r.post_id])).rows[0].count);
const tallySum = Number((await db.query(`SELECT coalesce(sum(total), 0) AS s FROM post_reaction_counts WHERE post_id = $1`, [r.post_id])).rows[0].s);
const stored = Number((await db.query(`SELECT reaction_count FROM posts WHERE id = $1`, [r.post_id])).rows[0].reaction_count);

if (before === after) ok('changing a reaction updates the row rather than adding one');
else fail(`changing a reaction added a row (${before} -> ${after})`);
if (tallySum === stored) ok('per-kind tallies still sum to posts.reaction_count');
else fail(`tallies (${tallySum}) disagree with posts.reaction_count (${stored})`);

// ---------------------------------------------------------------------------
console.log('\n== EXPLAIN plans for the queries whose plan is claimed in §13 ==');
// ---------------------------------------------------------------------------
const plans = [
  ['§1  profile by handle', 0, [users[0].handle, other]],
  ['§3  followers list', 2, [celebrity, viewer, null, null, 20]],
  ['§4  user posts', 3, [post.author_id, viewer, null, null, 20]],
  ['§7  home feed', 9, [viewer, null, null, 20]],
  ['§9  message history', 14, [conv.conversation_id, conv.user_id, null, 30]],
];

for (const [label, idx, args] of plans) {
  const plan = await db.query(
    `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF) ${stmts[idx]}`, args);
  console.log(`\n  --- ${label} ---`);
  for (const row of plan.rows) console.log('    ' + row['QUERY PLAN']);
}

await db.close();

console.log(`\n${failures === 0 ? 'all checks passed' : `${failures} check(s) failed`}`);
process.exit(failures === 0 ? 0 : 1);
