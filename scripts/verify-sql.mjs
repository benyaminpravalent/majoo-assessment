// Verify the social-media database assessment against a real PostgreSQL engine.
//
//   npm install --no-save @electric-sql/pglite
//   node scripts/verify-sql.mjs
//
// PGlite is PostgreSQL compiled to WebAssembly, so this runs the genuine query
// planner and the genuine constraint machinery without needing a server
// installed. It:
//
//   1. applies schema.sql, indexes.sql and seed.sql;
//   2. executes every statement in queries.sql with bound parameters;
//   3. checks that every trigger-maintained counter agrees with the rows it
//      counts;
//   4. checks that each documented edge case is actually rejected;
//   5. prints the EXPLAIN plan for the queries whose plan is claimed in
//      queries.sql §13.
//
// What it does NOT do is produce timings. The seed volumes are small and PGlite
// is single-connection WebAssembly, so any wall-clock number here would say
// nothing about production. Plan shape and index choice are what is verified.
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
const read = (f) => fs.readFileSync(path.join(sqlDir, f), 'utf8');

let failures = 0;
const ok = (msg) => console.log(`  ok    ${msg}`);
const fail = (msg, detail) => {
  failures += 1;
  console.error(`  FAIL  ${msg}`);
  if (detail) console.error(`        ${detail}`);
};

const db = await PGlite.create({ extensions: { citext, pg_trgm } });
console.log((await db.query('select version()')).rows[0].version);

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
