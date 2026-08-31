-- External live-source mirrors. The local database stores discovery metadata and
-- derived vectors only; the authoritative document body remains at the source.

CREATE TABLE external_sources (
  id                 CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id          CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  space_id           CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind               VARCHAR(32)  NOT NULL,
  name               VARCHAR(120) NOT NULL,
  source_system      VARCHAR(120) NOT NULL,
  status             VARCHAR(16)  NOT NULL,
  last_sync_status   VARCHAR(16)  NOT NULL,
  last_started_at    DATETIME(3)  NULL,
  last_succeeded_at  DATETIME(3)  NULL,
  last_error         VARCHAR(512) NULL,
  sync_run_id        CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  sync_lease_until   DATETIME(3)  NULL,
  document_count     INT          NOT NULL,
  created_at         DATETIME(3)  NOT NULL,
  updated_at         DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_external_source_name (tenant_id, name),
  KEY idx_external_source_space (tenant_id, space_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE external_sync_runs (
  id                 CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id          CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  external_source_id CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status             VARCHAR(16)  NOT NULL,
  seen_count         INT          NOT NULL,
  indexed_count      INT          NOT NULL,
  unchanged_count    INT          NOT NULL,
  missing_count      INT          NOT NULL,
  error_message      VARCHAR(512) NULL,
  started_at         DATETIME(3)  NOT NULL,
  finished_at        DATETIME(3)  NULL,
  created_at         DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_external_run_source (tenant_id, external_source_id, started_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE external_documents (
  id                 CHAR(26)      CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id          CHAR(26)      CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  space_id           CHAR(26)      CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  external_source_id CHAR(26)      CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  upstream_source_id VARCHAR(255)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  external_id        VARCHAR(255)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind               VARCHAR(64)   NOT NULL,
  title              VARCHAR(500)  NOT NULL,
  source_ref         VARCHAR(2048) NOT NULL,
  revision           VARCHAR(128)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  upstream_updated_at VARCHAR(64)  NULL,
  state              VARCHAR(16)   NOT NULL,
  last_seen_run_id   CHAR(26)      CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at         DATETIME(3)   NOT NULL,
  updated_at         DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_external_document (external_source_id, external_id),
  KEY idx_external_document_search (tenant_id, space_id, state, external_source_id),
  KEY idx_external_document_seen (external_source_id, last_seen_run_id, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE external_document_vectors (
  id                   CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id            CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  external_document_id CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  model                VARCHAR(64) NOT NULL,
  revision             VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  input_hash           CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  vector               BLOB        NOT NULL,
  dim                  INT         NOT NULL,
  created_at           DATETIME(3) NOT NULL,
  updated_at           DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_external_vector (external_document_id, model),
  KEY idx_external_vector_model (tenant_id, model, external_document_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
