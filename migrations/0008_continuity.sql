-- Team Shared Line: stable owner, one current position, append-only position history,
-- and explicit milestone handoffs. No space_id is added: Server Continuity keeps the
-- Desktop ownership model and is tenant-scoped rather than inventing space semantics.

CREATE TABLE continuity_lines (
  id              CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id       CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  owner_user_id   CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  owner_agent_id  CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  title           VARCHAR(200) NOT NULL,
  status          VARCHAR(16)  NOT NULL,
  created_at      DATETIME(3)  NOT NULL,
  updated_at      DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_continuity_owner (tenant_id, owner_agent_id, status, updated_at, id),
  KEY idx_continuity_tenant (tenant_id, status, updated_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE continuity_positions (
  id                     CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id              CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  line_id                CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  summary                MEDIUMTEXT  NOT NULL,
  interpretation_status  VARCHAR(24) NOT NULL,
  facts_used             JSON        NOT NULL,
  next_step              TEXT        NOT NULL,
  writer_user_id         CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  writer_agent_id        CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  version                INT         NOT NULL,
  created_at             DATETIME(3) NOT NULL,
  updated_at             DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_continuity_position_line (line_id),
  KEY idx_continuity_position_tenant (tenant_id, updated_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE continuity_position_history (
  id                     CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id              CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  line_id                CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  position_id            CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  position_version       INT         NOT NULL,
  summary                MEDIUMTEXT  NOT NULL,
  interpretation_status  VARCHAR(24) NOT NULL,
  facts_used             JSON        NOT NULL,
  next_step              TEXT        NOT NULL,
  writer_user_id         CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  writer_agent_id        CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at             DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_continuity_history_line (tenant_id, line_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE continuity_handoffs (
  id               CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id        CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  line_id          CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  writer_user_id   CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  writer_agent_id  CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  objective        TEXT        NOT NULL,
  completed        JSON        NOT NULL,
  open_items       JSON        NOT NULL,
  next_step        TEXT        NOT NULL,
  created_at       DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_continuity_handoff_line (tenant_id, line_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
