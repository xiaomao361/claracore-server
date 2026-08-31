-- Human governance pages use descending ULID keyset pagination. Keep tenant
-- isolation and page order in one access path so a growing team does not turn
-- member, token, or space administration into an unbounded filesort.

ALTER TABLE users
  ADD KEY idx_users_tenant_page (tenant_id, id);

ALTER TABLE tokens
  ADD KEY idx_tokens_tenant_user_page (tenant_id, user_id, id);

ALTER TABLE spaces
  ADD KEY idx_spaces_tenant_page (tenant_id, id);
