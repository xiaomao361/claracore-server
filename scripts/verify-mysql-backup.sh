#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=mysql-backup-common.sh
source "${script_dir}/mysql-backup-common.sh"

usage() {
  cat >&2 <<'EOF'
用法：verify-mysql-backup.sh --backup-dir /absolute/backups --database claracore_server [--max-age-seconds 93600] [--status-file /absolute/backup-status.json]

检查指定数据库最新发布的 .sql.gz 及其 .sha256/.metadata：文件安全、SHA-256、gzip、metadata 与备份年龄。
成功只证明最新备份产物新鲜且传输完整，不证明它能恢复；恢复证明仍须运行 rehearse-mysql-restore.sh。
EOF
}

backup_dir=""
database=""
max_age_seconds="93600"
status_file=""
while (($# > 0)); do
  case "$1" in
    --backup-dir) [[ $# -ge 2 ]] || { usage; exit 2; }; backup_dir="$2"; shift 2 ;;
    --database) [[ $# -ge 2 ]] || { usage; exit 2; }; database="$2"; shift 2 ;;
    --max-age-seconds) [[ $# -ge 2 ]] || { usage; exit 2; }; max_age_seconds="$2"; shift 2 ;;
    --status-file) [[ $# -ge 2 ]] || { usage; exit 2; }; status_file="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 2 ;;
  esac
done

[[ -n "${backup_dir}" && -n "${database}" ]] || { usage; exit 2; }
backup_require_absolute_path "备份目录" "${backup_dir}"
[[ "${backup_dir}" != "/" ]] || backup_die "备份目录不能是文件系统根目录"
[[ -d "${backup_dir}" && ! -L "${backup_dir}" ]] || backup_die "备份目录必须是普通目录且不能是符号链接"
backup_validate_database_name "${database}"
[[ "${max_age_seconds}" =~ ^[1-9][0-9]*$ ]] || backup_die "最大备份年龄必须是正整数秒"
backup_require_command gzip
if [[ -n "${status_file}" ]]; then
  backup_require_absolute_path "状态回执" "${status_file}"
  [[ "${status_file}" != "/" ]] || backup_die "状态回执不能是文件系统根目录"
  status_dir="$(dirname "${status_file}")"
  [[ -d "${status_dir}" && ! -L "${status_dir}" ]] || backup_die "状态回执目录必须是普通目录且不能是符号链接"
  [[ ! -e "${status_file}" || (-f "${status_file}" && ! -L "${status_file}") ]] || backup_die "状态回执必须是普通文件且不能是符号链接"
  backup_require_command mktemp
  backup_require_command mv
fi

shopt -s nullglob
archives=("${backup_dir}"/claracore-server_"${database}"_????????T??????Z.sql.gz)
shopt -u nullglob
((${#archives[@]} > 0)) || backup_die "没有找到数据库 ${database} 的已发布备份"

latest=""
for candidate in "${archives[@]}"; do
  basename_candidate="$(basename "${candidate}")"
  [[ "${basename_candidate}" =~ ^claracore-server_${database}_([0-9]{8}T[0-9]{6}Z)\.sql\.gz$ ]] || continue
  [[ -f "${candidate}" && ! -L "${candidate}" ]] || backup_die "备份必须是普通文件且不能是符号链接: ${candidate}"
  if [[ -z "${latest}" || "${candidate}" > "${latest}" ]]; then
    latest="${candidate}"
  fi
done
[[ -n "${latest}" ]] || backup_die "没有找到名称有效的已发布备份"

archive_name="$(basename "${latest}")"
[[ "${archive_name}" =~ ^claracore-server_${database}_([0-9]{8}T[0-9]{6}Z)\.sql\.gz$ ]] || backup_die "最新备份文件名无效"
created_at_from_name="${BASH_REMATCH[1]}"
checksum="${latest}.sha256"
metadata="${latest}.metadata"
for companion in "${checksum}" "${metadata}"; do
  [[ -f "${companion}" && ! -L "${companion}" ]] || backup_die "缺少普通且非符号链接的 companion: ${companion}"
done

backup_sha256_verify "${latest}" "${checksum}"
gzip -t "${latest}"
actual_sha="$(backup_sha256_value "${latest}")"
actual_bytes="$(wc -c < "${latest}" | tr -d ' ')"

meta_format=""
meta_created_at=""
meta_database=""
meta_archive=""
meta_sha256=""
meta_bytes=""
meta_count=0
while IFS='=' read -r key value || [[ -n "${key:-}${value:-}" ]]; do
  [[ -n "${key}" && "${value}" != *$'\n'* ]] || backup_die "metadata 格式无效"
  case "${key}" in
    format) [[ -z "${meta_format}" ]] || backup_die "metadata 包含重复字段: format"; meta_format="${value}" ;;
    created_at) [[ -z "${meta_created_at}" ]] || backup_die "metadata 包含重复字段: created_at"; meta_created_at="${value}" ;;
    database) [[ -z "${meta_database}" ]] || backup_die "metadata 包含重复字段: database"; meta_database="${value}" ;;
    archive) [[ -z "${meta_archive}" ]] || backup_die "metadata 包含重复字段: archive"; meta_archive="${value}" ;;
    sha256) [[ -z "${meta_sha256}" ]] || backup_die "metadata 包含重复字段: sha256"; meta_sha256="${value}" ;;
    bytes) [[ -z "${meta_bytes}" ]] || backup_die "metadata 包含重复字段: bytes"; meta_bytes="${value}" ;;
    *) backup_die "metadata 包含未知字段: ${key}" ;;
  esac
  meta_count=$((meta_count + 1))
done < "${metadata}"

[[ "${meta_count}" == "6" && "${meta_format}" == "claracore-mysql-backup-v1" ]] || backup_die "metadata 格式或字段数量无效"
[[ "${meta_created_at}" == "${created_at_from_name}" ]] || backup_die "metadata created_at 与归档名不匹配"
[[ "${meta_database}" == "${database}" ]] || backup_die "metadata database 与请求不匹配"
[[ "${meta_archive}" == "${archive_name}" ]] || backup_die "metadata archive 与归档名不匹配"
[[ "${meta_sha256}" == "${actual_sha}" ]] || backup_die "metadata sha256 与归档不匹配"
[[ "${meta_bytes}" =~ ^[1-9][0-9]*$ && "${meta_bytes}" == "${actual_bytes}" ]] || backup_die "metadata bytes 与归档不匹配"

backup_created_epoch() {
  local timestamp="$1"
  if date -u -d "${timestamp:0:8} ${timestamp:9:2}:${timestamp:11:2}:${timestamp:13:2}" '+%s' >/dev/null 2>&1; then
    date -u -d "${timestamp:0:8} ${timestamp:9:2}:${timestamp:11:2}:${timestamp:13:2}" '+%s'
  else
    date -j -u -f '%Y%m%dT%H%M%SZ' "${timestamp}" '+%s'
  fi
}

created_epoch="$(backup_created_epoch "${meta_created_at}")" || backup_die "无法解析备份 created_at"
now_epoch="$(date -u '+%s')"
age_seconds=$((now_epoch - created_epoch))
((age_seconds >= -300)) || backup_die "最新备份时间超过当前时间 300 秒，拒绝通过"
((age_seconds >= 0)) || age_seconds=0
((age_seconds <= max_age_seconds)) || backup_die "最新备份已过期: age_seconds=${age_seconds}, max_age_seconds=${max_age_seconds}"

verified_at="$(date -u '+%Y%m%dT%H%M%SZ')"
receipt="$(printf '{"ok":true,"database":"%s","archive":"%s","created_at":"%s","verified_at":"%s","age_seconds":%d,"max_age_seconds":%d,"sha256":"verified","gzip":"verified","recoverability":"not_proven"}' \
  "${database}" "${archive_name}" "${meta_created_at}" "${verified_at}" "${age_seconds}" "${max_age_seconds}")"

if [[ -n "${status_file}" ]]; then
  partial_status="$(mktemp "${status_dir}/.backup-status.partial.XXXXXX")"
  cleanup_status() { rm -f -- "${partial_status}"; }
  trap cleanup_status EXIT
  chmod 0600 "${partial_status}"
  printf '%s\n' "${receipt}" > "${partial_status}"
  [[ ! -L "${status_file}" ]] || backup_die "状态回执在写入前变成了符号链接，拒绝覆盖"
  mv -f -- "${partial_status}" "${status_file}"
  partial_status=""
  trap - EXIT
fi

printf '%s\n' "${receipt}"
