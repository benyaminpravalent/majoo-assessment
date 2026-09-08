-- 0001_init.down.sql
--
-- Reverses 0001_init.up.sql. Tables are dropped in reverse dependency order;
-- the indexes and triggers defined on them go with them, so only the shared
-- trigger function needs an explicit DROP.

DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS comments;
DROP TABLE IF EXISTS posts;
DROP TABLE IF EXISTS users;

DROP FUNCTION IF EXISTS set_updated_at();
