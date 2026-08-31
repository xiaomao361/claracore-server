-- Close the password-reset-vs-login race for persisted Web sessions.
--
-- Deleting all existing sessions is necessary but not sufficient: a login may
-- have verified the old password just before a reset, then insert its session
-- just after the reset's DELETE. Each session therefore records the user's
-- credential generation and ResumeSession compares it with the current row.

ALTER TABLE users
  ADD COLUMN auth_version BIGINT UNSIGNED NOT NULL DEFAULT 1 AFTER password_hash;

ALTER TABLE sessions
  ADD COLUMN auth_version BIGINT UNSIGNED NOT NULL DEFAULT 1 AFTER user_id;
