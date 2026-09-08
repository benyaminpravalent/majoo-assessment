-- Promote an account to administrator.
--
--   psql "$DATABASE_URL" -v email="'ben@example.com'" -f scripts/promote_admin.sql
--
-- Or, inside a compose stack:
--
--   docker compose exec -T postgres psql -U blog -d blog \
--     -c "UPDATE users SET role = 'admin' WHERE email = 'ben@example.com';"
--
-- This is a deliberate out-of-band operation. No request body and no API
-- endpoint can grant the admin role, so a bug in registration or profile
-- updates cannot escalate privileges — the only path runs through someone with
-- database credentials.
--
-- Email addresses are stored folded to lower case (users_email_lowercase), so
-- the predicate below lowercases its input to match.

UPDATE users
   SET role = 'admin'
 WHERE email = lower(:email)
RETURNING id, email, username, role;
