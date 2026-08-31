-- Durable, coalesced manual refresh requests for external sources. Pending and
-- claimed requests are separate so an administrator can queue one follow-up
-- while a worker is already synchronizing, and an expired claim can be
-- recovered by another instance.

ALTER TABLE external_sources
  ADD COLUMN sync_request_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER sync_lease_until,
  ADD COLUMN sync_requested_at DATETIME(3) NULL AFTER sync_request_id,
  ADD COLUMN sync_request_claim_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER sync_requested_at,
  ADD COLUMN sync_request_claim_token CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER sync_request_claim_id,
  ADD COLUMN sync_request_claim_until DATETIME(3) NULL AFTER sync_request_claim_token,
  ADD KEY idx_external_sync_request (tenant_id, status, sync_request_id, sync_request_claim_until);
