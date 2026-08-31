#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
用法：verify-cluster.sh --public-url https://claracore.example.com \
  --instance ccs-a=http://127.0.0.1:51890 \
  --instance ccs-b=http://127.0.0.1:52890 [--attempts 20]

只读验证每个实例的 live/ready、版本、源码/二进制摘要与 schema，并从负载均衡入口采样，
证明每个预期 instanceId 都实际返回过响应。不会修改服务或触发摘流。
EOF
  exit 2
}

public_url=""
attempts=0
instance_ids=()
instance_urls=()

valid_base_url() {
  [[ "$1" =~ ^https?://[^/?#]+/?$ ]]
}

add_instance() {
  local spec="$1" id url existing
  if [[ "${spec}" != *=* ]]; then
    echo "--instance 必须使用 id=http(s)://host:port" >&2
    exit 2
  fi
  id="${spec%%=*}"
  url="${spec#*=}"
  if [[ ! "${id}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]]; then
    echo "实例标签非法: ${id}" >&2
    exit 2
  fi
  if ! valid_base_url "${url}"; then
    echo "实例 URL 必须是无路径、query 或 fragment 的 http(s) base URL: ${url}" >&2
    exit 2
  fi
  url="${url%/}"
  for existing in "${instance_ids[@]:-}"; do
    if [[ "${existing}" == "${id}" ]]; then
      echo "实例标签重复: ${id}" >&2
      exit 2
    fi
  done
  instance_ids+=("${id}")
  instance_urls+=("${url}")
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --public-url)
      [[ $# -ge 2 ]] || usage
      public_url="$2"
      shift 2
      ;;
    --public-url=*)
      public_url="${1#*=}"
      shift
      ;;
    --instance)
      [[ $# -ge 2 ]] || usage
      add_instance "$2"
      shift 2
      ;;
    --instance=*)
      add_instance "${1#*=}"
      shift
      ;;
    --attempts)
      [[ $# -ge 2 ]] || usage
      attempts="$2"
      shift 2
      ;;
    --attempts=*)
      attempts="${1#*=}"
      shift
      ;;
    -h|--help) usage ;;
    *) echo "未知参数: $1" >&2; usage ;;
  esac
done

if ! valid_base_url "${public_url}"; then
  echo "--public-url 必须是无路径、query 或 fragment 的 http(s) base URL" >&2
  exit 2
fi
public_url="${public_url%/}"
if (( ${#instance_ids[@]} < 2 )); then
  echo "集群验收至少需要两个显式实例" >&2
  exit 2
fi
if [[ ! "${attempts}" =~ ^[0-9]+$ ]]; then
  echo "--attempts 必须是整数" >&2
  exit 2
fi
if (( attempts == 0 )); then
  attempts=$(( ${#instance_ids[@]} * 10 ))
  (( attempts < 20 )) && attempts=20
fi
if (( attempts < ${#instance_ids[@]} || attempts > 200 )); then
  echo "--attempts 必须不小于实例数且不超过 200" >&2
  exit 2
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${tmp_dir}"' EXIT

fetch() {
  local url="$1" output="$2"
  curl --fail --silent --show-error \
    --connect-timeout 5 --max-time 10 \
    --header 'Accept: application/json' \
    --header 'Cache-Control: no-cache' \
    --header 'Connection: close' \
    --output "${output}" "${url}"
}

json_string() {
  local file="$1" key="$2"
  sed -n "s/.*\"${key}\":\"\([^\"]*\)\".*/\1/p" "${file}" | head -n 1
}

require_probe() {
  local file="$1" status="$2" id="$3"
  if ! grep -q '"ok":true' "${file}" || [[ "$(json_string "${file}" status)" != "${status}" ]] || [[ "$(json_string "${file}" instanceId)" != "${id}" ]]; then
    echo "探针未返回预期的 ok/status/instanceId（期望 ${status}/${id}）" >&2
    return 1
  fi
}

expected_version=""
expected_source_digest=""
expected_binary_digest=""
expected_environment=""
expected_schema=""
seen=()
for ((i = 0; i < ${#instance_ids[@]}; i++)); do
  id="${instance_ids[$i]}"
  url="${instance_urls[$i]}"
  live="${tmp_dir}/live-${i}.json"
  ready="${tmp_dir}/ready-${i}.json"
  fetch "${url}/livez" "${live}"
  require_probe "${live}" "alive" "${id}"
  fetch "${url}/readyz" "${ready}"
  require_probe "${ready}" "ready" "${id}"
  if ! grep -q '"mysql":"reachable"' "${ready}" || ! grep -q '"schema":"current"' "${ready}"; then
    echo "实例 ${id} 未证明 MySQL reachable 且 schema current" >&2
    exit 1
  fi
  version="$(json_string "${ready}" version)"
  build_source_digest="$(json_string "${ready}" sourceDigest)"
  build_binary_digest="$(json_string "${ready}" binaryDigest)"
  environment="$(json_string "${ready}" env)"
  schema="$(json_string "${ready}" schemaVersion)"
  if [[ -z "${version}" || ! "${build_source_digest}" =~ ^([0-9a-f]{64}|development)$ || ! "${build_binary_digest}" =~ ^[0-9a-f]{64}$ || -z "${environment}" || -z "${schema}" ]]; then
    echo "实例 ${id} 缺少有效的 version、sourceDigest、binaryDigest、env 或 schemaVersion" >&2
    exit 1
  fi
  if [[ "${environment}" == "production" && "${build_source_digest}" == "development" ]]; then
    echo "生产实例 ${id} 使用未绑定源码摘要的 development 构建" >&2
    exit 1
  fi
  if [[ -z "${expected_version}" ]]; then
    expected_version="${version}"
    expected_source_digest="${build_source_digest}"
    expected_binary_digest="${build_binary_digest}"
    expected_environment="${environment}"
    expected_schema="${schema}"
  elif [[ "${version}" != "${expected_version}" || "${build_source_digest}" != "${expected_source_digest}" || "${build_binary_digest}" != "${expected_binary_digest}" || "${environment}" != "${expected_environment}" || "${schema}" != "${expected_schema}" ]]; then
    echo "实例版本/sourceDigest/binaryDigest/env/schema 不一致：${id}=${version}/${build_source_digest}/${build_binary_digest}/${environment}/${schema}，期望 ${expected_version}/${expected_source_digest}/${expected_binary_digest}/${expected_environment}/${expected_schema}" >&2
    exit 1
  fi
  seen+=(0)
done

for ((attempt = 1; attempt <= attempts; attempt++)); do
  public_body="${tmp_dir}/public-${attempt}.json"
  fetch "${public_url}/readyz?cluster_probe=${attempt}" "${public_body}"
  public_id="$(json_string "${public_body}" instanceId)"
  matched=-1
  for ((i = 0; i < ${#instance_ids[@]}; i++)); do
    if [[ "${instance_ids[$i]}" == "${public_id}" ]]; then
      matched=$i
      break
    fi
  done
  if (( matched < 0 )); then
    echo "公开入口返回未知或空 instanceId: ${public_id:-<empty>}" >&2
    exit 1
  fi
  require_probe "${public_body}" "ready" "${public_id}"
  if [[ "$(json_string "${public_body}" version)" != "${expected_version}" || "$(json_string "${public_body}" sourceDigest)" != "${expected_source_digest}" || "$(json_string "${public_body}" binaryDigest)" != "${expected_binary_digest}" || "$(json_string "${public_body}" env)" != "${expected_environment}" || "$(json_string "${public_body}" schemaVersion)" != "${expected_schema}" ]]; then
    echo "公开入口返回的版本/sourceDigest/binaryDigest/env/schema 与逐实例基线不一致" >&2
    exit 1
  fi
  seen[matched]=1
done

missing=()
for ((i = 0; i < ${#instance_ids[@]}; i++)); do
  (( seen[i] == 1 )) || missing+=("${instance_ids[$i]}")
done
if (( ${#missing[@]} > 0 )); then
  echo "公开入口在 ${attempts} 次采样中未返回实例: ${missing[*]}" >&2
  exit 1
fi

echo "集群探针通过：${#instance_ids[@]} 个实例均 live/ready，版本 ${expected_version}，源码摘要 ${expected_source_digest}，二进制摘要 ${expected_binary_digest}，环境 ${expected_environment}，schema ${expected_schema}；公开入口 ${attempts} 次采样覆盖全部实例（只证明当前分发，不等于滚动发布或故障切换验收）"
