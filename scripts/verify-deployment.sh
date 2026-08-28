#!/usr/bin/env bash
set -euo pipefail

base_url="${1:-http://127.0.0.1:51890}"
health_body="$(mktemp)"
trap 'rm -f -- "${health_body}"' EXIT

curl --fail --silent --show-error "${base_url}/healthz" > "${health_body}"
if ! grep -q '"ok":true' "${health_body}"; then
  echo "healthz 未返回 ok=true" >&2
  exit 1
fi
curl --fail --silent --show-error "${base_url}/login" >/dev/null

mcp_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "${base_url}/mcp")"
if [[ "${mcp_status}" != "401" ]]; then
  echo "无凭证访问 /mcp 应为 401，实际 ${mcp_status}" >&2
  exit 1
fi

echo "部署探针通过：MySQL 健康、Web 可达、MCP 认证边界生效"
