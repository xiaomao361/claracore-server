#!/usr/bin/env bash
set -euo pipefail

# Usage: verify-deployment.sh [public-or-app-base-url] [loopback-metrics-base-url]

base_url="${1:-http://127.0.0.1:51890}"
metrics_url="${2:-}"
live_body="$(mktemp)"
ready_body="$(mktemp)"
metrics_body="$(mktemp)"
trap 'rm -f -- "${live_body}" "${ready_body}" "${metrics_body}"' EXIT

json_string() {
  local file="$1" key="$2"
  sed -n "s/.*\"${key}\":\"\([^\"]*\)\".*/\1/p" "${file}" | head -n 1
}

curl --fail --silent --show-error "${base_url}/livez" > "${live_body}"
if ! grep -q '"status":"alive"' "${live_body}"; then
  echo "livez 未返回 alive" >&2
  exit 1
fi

curl --fail --silent --show-error "${base_url}/readyz" > "${ready_body}"
if ! grep -q '"ok":true' "${ready_body}" || ! grep -q '"schema":"current"' "${ready_body}"; then
  echo "readyz 未证明 MySQL 可达且迁移与当前二进制一致" >&2
  exit 1
fi
for key in version sourceDigest binaryDigest env; do
  live_value="$(json_string "${live_body}" "${key}")"
  ready_value="$(json_string "${ready_body}" "${key}")"
  if [[ -z "${live_value}" || "${live_value}" != "${ready_value}" ]]; then
    echo "livez/readyz 的 ${key} 缺失或不一致" >&2
    exit 1
  fi
done
source_digest="$(json_string "${ready_body}" sourceDigest)"
binary_digest="$(json_string "${ready_body}" binaryDigest)"
environment="$(json_string "${ready_body}" env)"
if [[ ! "${source_digest}" =~ ^([0-9a-f]{64}|development)$ || ! "${binary_digest}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "探针未返回有效的源码或二进制摘要" >&2
  exit 1
fi
if [[ "${environment}" == "production" && "${source_digest}" == "development" ]]; then
  echo "生产实例使用未绑定源码摘要的 development 构建" >&2
  exit 1
fi
curl --fail --silent --show-error "${base_url}/login" >/dev/null

mcp_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "${base_url}/mcp")"
if [[ "${mcp_status}" != "401" ]]; then
  echo "无凭证访问 /mcp 应为 401，实际 ${mcp_status}" >&2
  exit 1
fi

if [[ -n "${metrics_url}" ]]; then
  curl --fail --silent --show-error "${metrics_url}/metrics" > "${metrics_body}"
  if ! grep -q '^claracore_build_info{' "${metrics_body}" ||
     ! grep -q '^claracore_instance_accepting_requests 1$' "${metrics_body}" ||
     ! grep -q '^claracore_backup_receipt_available 1$' "${metrics_body}" ||
     ! grep -q '^claracore_backup_fresh 1$' "${metrics_body}" ||
     ! grep -q '^claracore_restore_receipt_available 1$' "${metrics_body}" ||
     ! grep -q '^claracore_restore_fresh 1$' "${metrics_body}" ||
     ! grep -q '^claracore_integrity_receipt_available 1$' "${metrics_body}" ||
     ! grep -q '^claracore_integrity_fresh 1$' "${metrics_body}" ||
     ! grep -q '^claracore_integrity_verified 1$' "${metrics_body}" ||
     ! grep -q '^claracore_integrity_violations 0$' "${metrics_body}"; then
    echo "metrics 未证明实例接流量、备份/恢复有效且数据完整性检查新鲜无违规" >&2
    exit 1
  fi
fi

echo "部署探针通过：进程存活、源码/二进制身份一致、MySQL/迁移 ready、Web 可达、MCP 认证边界生效${metrics_url:+、备份/恢复与数据完整性指标健康}"
