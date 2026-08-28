# Security Policy

## Supported versions

Until stable releases are published, security fixes target the current `main` branch. After versioned releases begin, this table will list supported release lines explicitly.

## Reporting a vulnerability

Please use GitHub's **Report a vulnerability** private security-advisory flow for this repository. Do not open a public Issue for an exploitable vulnerability.

Include the affected version or commit, deployment shape, reproduction steps, impact, and any suggested mitigation. Remove real credentials and personal or production data from logs and screenshots.

We will acknowledge a complete report as soon as maintainers are available, coordinate validation and remediation privately, and publish a disclosure only after affected operators have a reasonable upgrade path.

## Operational security notes

- Run the application as a dedicated non-root user behind HTTPS.
- In production, set `CCS_ENV=production`, `CCS_WEB_SECURE_COOKIE=true`, and an explicit HTTPS `CCS_PUBLIC_MCP_URL`.
- Store database, embedding, and MCP credentials outside the release directory with owner-only permissions.
- Rotate any Bearer token that appears in chat, shell history, logs, screenshots, or issue reports.
- Restrict `CCS_INGEST_ROOT` to a dedicated read-only tree, or leave it empty to disable Web ingestion.
- Back up and test restore procedures before migrations. A successful checksum alone does not prove semantic restorability.
