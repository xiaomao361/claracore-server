-- Tenant capacity is an explicit, low-frequency administrator diagnostic, but
-- it still must not degrade into a full-table scan as more teams share one
-- deployment. Tables that already have a tenant-leading access path are left
-- unchanged; these five fill the remaining knowledge/index gaps.

ALTER TABLE memories
  ADD KEY idx_memories_tenant_capacity (tenant_id);

ALTER TABLE cards
  ADD KEY idx_cards_tenant_capacity (tenant_id);

ALTER TABLE card_recall_triggers
  ADD KEY idx_triggers_tenant_capacity (tenant_id);

ALTER TABLE links
  ADD KEY idx_links_tenant_capacity (tenant_id);

ALTER TABLE embeddings
  ADD KEY idx_embeddings_tenant_capacity (tenant_id);
