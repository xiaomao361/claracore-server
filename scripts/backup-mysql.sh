#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=mysql-backup-common.sh
source "${script_dir}/mysql-backup-common.sh"

usage() {
  cat >&2 <<'EOF'
用法：backup-mysql.sh --defaults-file /absolute/client.cnf --database claracore_server --output-dir /absolute/backups

client.cnf 使用 MySQL [client] 格式并必须由当前用户拥有，权限 0600 或更严格。
脚本不会把密码放进参数、日志或产物；成功时原子生成 .sql.gz、.sha256 与 .metadata。
EOF
}

defaults_file=""
database=""
output_dir=""
while (($# > 0)); do
  case "$1" in
    --defaults-file) [[ $# -ge 2 ]] || { usage; exit 2; }; defaults_file="$2"; shift 2 ;;
    --database) [[ $# -ge 2 ]] || { usage; exit 2; }; database="$2"; shift 2 ;;
    --output-dir) [[ $# -ge 2 ]] || { usage; exit 2; }; output_dir="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 2 ;;
  esac
done

[[ -n "${defaults_file}" && -n "${database}" && -n "${output_dir}" ]] || { usage; exit 2; }
backup_validate_defaults_file "${defaults_file}"
backup_validate_database_name "${database}"
backup_require_absolute_path "输出目录" "${output_dir}"
[[ "${output_dir}" != "/" ]] || backup_die "输出目录不能是文件系统根目录"
backup_require_command mysqldump
backup_require_command gzip
backup_require_sha256

umask 077
mkdir -p -- "${output_dir}"
[[ -d "${output_dir}" && ! -L "${output_dir}" ]] || backup_die "输出目录必须是普通目录且不能是符号链接"

lock_dir="${output_dir}/.claracore-backup.lock"
if ! mkdir "${lock_dir}" 2>/dev/null; then
  backup_die "同一输出目录已有备份任务或遗留锁: ${lock_dir}"
fi

partial_archive=""
partial_checksum=""
partial_metadata=""
checksum_published=0
metadata_published=0
archive_published=0
cleanup() {
  [[ -z "${partial_archive}" ]] || rm -f -- "${partial_archive}"
  [[ -z "${partial_checksum}" ]] || rm -f -- "${partial_checksum}"
  [[ -z "${partial_metadata}" ]] || rm -f -- "${partial_metadata}"
  if ((archive_published == 0)); then
    ((checksum_published == 0)) || rm -f -- "${checksum}"
    ((metadata_published == 0)) || rm -f -- "${metadata}"
  fi
  rmdir "${lock_dir}" 2>/dev/null || true
}
trap cleanup EXIT

timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
base="claracore-server_${database}_${timestamp}"
archive="${output_dir}/${base}.sql.gz"
checksum="${archive}.sha256"
metadata="${archive}.metadata"
[[ ! -e "${archive}" && ! -e "${checksum}" && ! -e "${metadata}" ]] || backup_die "同名备份已经存在: ${archive}"

partial_archive="$(mktemp "${output_dir}/.${base}.sql.gz.partial.XXXXXX")"
partial_checksum="$(mktemp "${output_dir}/.${base}.sha256.partial.XXXXXX")"
partial_metadata="$(mktemp "${output_dir}/.${base}.metadata.partial.XXXXXX")"

# --defaults-extra-file must be the first option understood by MySQL clients.
# Do not use --databases: the dump must be restorable into an isolated rehearsal DB
# without CREATE/USE statements ever pointing back at the source database.
mysqldump --defaults-extra-file="${defaults_file}" \
  --single-transaction --quick --routines --triggers --events --hex-blob \
  --set-gtid-purged=OFF --no-tablespaces --default-character-set=utf8mb4 \
  "${database}" | gzip -9 > "${partial_archive}"

gzip -t "${partial_archive}"
archive_sha="$(backup_sha256_value "${partial_archive}")"
printf '%s  %s\n' "${archive_sha}" "$(basename "${archive}")" > "${partial_checksum}"
archive_bytes="$(wc -c < "${partial_archive}" | tr -d ' ')"
cat > "${partial_metadata}" <<EOF
format=claracore-mysql-backup-v1
created_at=${timestamp}
database=${database}
archive=$(basename "${archive}")
sha256=${archive_sha}
bytes=${archive_bytes}
EOF

# The archive is the publication marker: companions become visible first, then
# the complete archive is renamed into place as the final atomic step.
backup_publish_file "${partial_checksum}" "${checksum}"
partial_checksum=""
checksum_published=1
backup_publish_file "${partial_metadata}" "${metadata}"
partial_metadata=""
metadata_published=1
backup_publish_file "${partial_archive}" "${archive}"
partial_archive=""
archive_published=1

echo "备份已生成: ${archive}"
echo "完整性校验: ${checksum}"
echo "恢复状态: 尚未演练；请运行 rehearse-mysql-restore.sh"
