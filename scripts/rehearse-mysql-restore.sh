#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=mysql-backup-common.sh
source "${script_dir}/mysql-backup-common.sh"

usage() {
  cat >&2 <<'EOF'
用法：rehearse-mysql-restore.sh --defaults-file /absolute/client.cnf --backup /absolute/backup.sql.gz --confirm-isolated-server [--report /absolute/report.tsv] [--status-file /absolute/restore-status.json] [--max-age-seconds 2592000] [--keep-target]

脚本只会创建名称以 ccs_restore_ 开头且本次随机生成的隔离数据库；默认验证后删除。
--confirm-isolated-server 表示操作员确认该凭据连接的是允许执行 CREATE/DROP 的恢复演练环境。
--keep-target 会保留隔离库供人工抽查，命令结束时打印准确库名，需由操作员确认后删除。
--status-file 只允许与默认自动清理组合；成功后原子写入不含数据、凭据或临时库名的技术恢复回执。
EOF
}

defaults_file=""
backup=""
report=""
status_file=""
max_age_seconds="2592000"
keep_target=0
confirmed_isolated=0
while (($# > 0)); do
  case "$1" in
    --defaults-file) [[ $# -ge 2 ]] || { usage; exit 2; }; defaults_file="$2"; shift 2 ;;
    --backup) [[ $# -ge 2 ]] || { usage; exit 2; }; backup="$2"; shift 2 ;;
    --report) [[ $# -ge 2 ]] || { usage; exit 2; }; report="$2"; shift 2 ;;
    --status-file) [[ $# -ge 2 ]] || { usage; exit 2; }; status_file="$2"; shift 2 ;;
    --max-age-seconds) [[ $# -ge 2 ]] || { usage; exit 2; }; max_age_seconds="$2"; shift 2 ;;
    --confirm-isolated-server) confirmed_isolated=1; shift ;;
    --keep-target) keep_target=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 2 ;;
  esac
done

[[ -n "${defaults_file}" && -n "${backup}" ]] || { usage; exit 2; }
((confirmed_isolated == 1)) || backup_die "恢复演练会创建并删除临时数据库；请在隔离环境确认后传 --confirm-isolated-server"
[[ "${max_age_seconds}" =~ ^[1-9][0-9]*$ ]] || backup_die "最大恢复演练年龄必须是正整数秒"
((max_age_seconds <= 31536000)) || backup_die "最大恢复演练年龄不能超过 31536000 秒"
[[ -z "${status_file}" || "${keep_target}" == "0" ]] || backup_die "--status-file 不能与 --keep-target 同时使用；人工抽查完成前不能发布清理完成回执"
backup_validate_defaults_file "${defaults_file}"
backup_require_absolute_path "备份文件" "${backup}"
[[ -f "${backup}" && ! -L "${backup}" ]] || backup_die "备份必须是普通 .sql.gz 文件且不能是符号链接"
[[ "${backup}" == *.sql.gz ]] || backup_die "备份文件必须以 .sql.gz 结尾"
checksum="${backup}.sha256"
[[ -f "${checksum}" && ! -L "${checksum}" ]] || backup_die "缺少普通校验和文件: ${checksum}"
if [[ -z "${report}" ]]; then
  report="${backup}.restore-report.tsv"
fi
backup_require_absolute_path "恢复报告" "${report}"
[[ "${report}" != "${backup}" && "${report}" != "${checksum}" ]] || backup_die "恢复报告不能覆盖备份或校验和"
[[ ! -e "${report}" ]] || backup_die "恢复报告已经存在，拒绝覆盖: ${report}"
if [[ -n "${status_file}" ]]; then
  backup_require_absolute_path "恢复状态回执" "${status_file}"
  [[ "${status_file}" != "/" && "${status_file}" != "${backup}" && "${status_file}" != "${checksum}" && "${status_file}" != "${report}" ]] || \
    backup_die "恢复状态回执路径无效或会覆盖现有产物"
  status_dir="$(dirname "${status_file}")"
  [[ -d "${status_dir}" && ! -L "${status_dir}" ]] || backup_die "恢复状态回执目录必须是普通目录且不能是符号链接"
  [[ ! -e "${status_file}" || (-f "${status_file}" && ! -L "${status_file}") ]] || \
    backup_die "恢复状态回执必须是普通文件且不能是符号链接"
  archive_name="$(basename "${backup}")"
  [[ "${archive_name}" =~ ^claracore-server_([A-Za-z0-9_]+)_([0-9]{8}T[0-9]{6}Z)\.sql\.gz$ ]] || \
    backup_die "写入状态回执时备份名必须符合 claracore-server_<database>_<UTC>.sql.gz"
  source_database="${BASH_REMATCH[1]}"
  report_name="$(basename "${report}")"
  [[ "${report_name}" =~ ^[0-9A-Za-z._-]+\.restore-report\.tsv$ ]] || \
    backup_die "写入状态回执时恢复报告名必须以 .restore-report.tsv 结尾且只含安全字符"
fi
backup_require_command mysql
backup_require_command gzip
backup_require_sha256
backup_sha256_verify "${backup}" "${checksum}"
gzip -t "${backup}"
# A target database argument is not a sandbox if the SQL stream contains USE,
# CREATE DATABASE, or DROP DATABASE. Refuse such dumps before any mysql call.
if gzip -cd "${backup}" | awk '
  {
    line = toupper($0)
    if (line ~ /CREATE[[:space:]]+DATABASE/ || line ~ /DROP[[:space:]]+DATABASE/ ||
        line ~ /^[[:space:]]*USE[[:space:]]+/ ||
        line ~ /^[[:space:]]*\/\*![0-9]+[[:space:]]+USE[[:space:]]+/) {
      unsafe = 1
    }
  }
  END { exit unsafe ? 0 : 1 }
'; then
  backup_die "备份包含 USE/CREATE DATABASE/DROP DATABASE，拒绝在恢复演练中执行"
fi

umask 077
target="ccs_restore_$(date -u '+%Y%m%dT%H%M%SZ')_$$_${RANDOM}"
backup_validate_database_name "${target}"
created=0
cleanup_target() {
  if ((created == 1 && keep_target == 0)); then
    if mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
      -e "DROP DATABASE IF EXISTS \`${target}\`" >/dev/null; then
      created=0
    else
      echo "警告：隔离恢复库清理失败，请人工检查 ${target}" >&2
      return 1
    fi
  fi
  return 0
}
trap 'cleanup_target || true' EXIT

existing="$(mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
  -e "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='${target}'")"
[[ "${existing}" == "0" ]] || backup_die "随机隔离数据库名意外已存在，拒绝继续: ${target}"
mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
  -e "CREATE DATABASE \`${target}\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci" >/dev/null
created=1

gzip -cd "${backup}" | mysql --defaults-extra-file="${defaults_file}" "${target}"

table_count="$(mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
  -e "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='${target}' AND TABLE_TYPE='BASE TABLE'")"
[[ "${table_count}" =~ ^[0-9]+$ && "${table_count}" -gt 0 ]] || backup_die "隔离恢复后没有基础表"

migration_table="$(mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
  -e "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='${target}' AND TABLE_NAME='schema_migrations'")"
[[ "${migration_table}" == "1" ]] || backup_die "隔离恢复缺少 schema_migrations，备份不是完整 ClaraCore 数据库"

tables=()
while IFS= read -r table; do
  tables[${#tables[@]}]="${table}"
done < <(mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
  -e "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA='${target}' AND TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME")
[[ "${#tables[@]}" -eq "${table_count}" ]] || backup_die "表清单数量与 information_schema 统计不一致"

report_dir="$(dirname "${report}")"
mkdir -p -- "${report_dir}"
[[ -d "${report_dir}" && ! -L "${report_dir}" ]] || backup_die "恢复报告目录必须是普通目录且不能是符号链接"
partial_report="$(mktemp "${report_dir}/.$(basename "${report}").partial.XXXXXX")"
partial_status=""
cleanup_outputs() {
  [[ -z "${partial_report}" ]] || rm -f -- "${partial_report}"
  [[ -z "${partial_status}" ]] || rm -f -- "${partial_status}"
  cleanup_target || true
}
trap cleanup_outputs EXIT

archive_sha="$(backup_sha256_value "${backup}")"
{
  printf 'kind\tname\tvalue\n'
  printf 'backup\tsha256\t%s\n' "${archive_sha}"
  printf 'restore\ttable_count\t%s\n' "${table_count}"
} > "${partial_report}"

for table in "${tables[@]}"; do
  [[ "${table}" =~ ^[A-Za-z0-9_]+$ ]] || backup_die "恢复得到非法表名，拒绝拼接 SQL: ${table}"
  check_result="$(mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
    -e "CHECK TABLE \`${target}\`.\`${table}\` QUICK")"
  [[ "${check_result}" == *$'\tstatus\tOK' ]] || backup_die "CHECK TABLE 未通过: ${table}"
  row_count="$(mysql --defaults-extra-file="${defaults_file}" --batch --skip-column-names \
    -e "SELECT COUNT(*) FROM \`${target}\`.\`${table}\`")"
  [[ "${row_count}" =~ ^[0-9]+$ ]] || backup_die "无法读取恢复表行数: ${table}"
  printf 'table\t%s\t%s\n' "${table}" "${row_count}" >> "${partial_report}"
done

migration_output="$(mysql --defaults-extra-file="${defaults_file}" --batch --raw --skip-column-names \
  -e "SELECT CONCAT('migration\\t', version, '\\t', checksum) FROM \`${target}\`.schema_migrations ORDER BY version")"
migration_count=0
latest_migration=""
while IFS=$'\t' read -r kind migration checksum_value extra; do
  [[ "${kind}" == "migration" && "${migration}" =~ ^[0-9]{4,16}$ && "${checksum_value}" =~ ^[0-9a-f]{64}$ && -z "${extra:-}" ]] || \
    backup_die "恢复得到无效的 migration 账本行"
  printf 'migration\t%s\t%s\n' "${migration}" "${checksum_value}" >> "${partial_report}"
  migration_count=$((migration_count + 1))
  latest_migration="${migration}"
done <<< "${migration_output}"
((migration_count > 0)) || backup_die "恢复后的 schema_migrations 为空"
report_sha="$(backup_sha256_value "${partial_report}")"

if ((keep_target == 1)); then
  backup_publish_file "${partial_report}" "${report}"
  partial_report=""
  echo "隔离恢复演练通过，数据库已保留供人工抽查: ${target}"
  echo "抽查完成后仅删除该隔离库: DROP DATABASE \`${target}\`;"
else
  cleanup_target || backup_die "隔离恢复库未能删除，不发布技术恢复状态回执"
  backup_publish_file "${partial_report}" "${report}"
  partial_report=""
  if [[ -n "${status_file}" ]]; then
    restored_at="$(date -u '+%Y%m%dT%H%M%SZ')"
    receipt="$(printf '{"ok":true,"database":"%s","archive":"%s","archive_sha256":"%s","report":"%s","report_sha256":"%s","restored_at":"%s","max_age_seconds":%d,"table_count":%d,"migration_count":%d,"latest_migration":"%s","table_checks":"verified","row_counts":"recorded","target_cleanup":"verified","semantic_inspection":"not_proven"}' \
      "${source_database}" "${archive_name}" "${archive_sha}" "${report_name}" "${report_sha}" "${restored_at}" \
      "${max_age_seconds}" "${table_count}" "${migration_count}" "${latest_migration}")"
    partial_status="$(mktemp "${status_dir}/.restore-status.partial.XXXXXX")"
    chmod 0600 "${partial_status}"
    printf '%s\n' "${receipt}" > "${partial_status}"
    [[ ! -L "${status_file}" ]] || backup_die "恢复状态回执在写入前变成了符号链接，拒绝覆盖"
    mv -f -- "${partial_status}" "${status_file}"
    partial_status=""
  fi
  echo "隔离恢复演练通过，临时数据库已删除: ${target}"
fi
echo "表级恢复报告: ${report}"
if [[ -n "${status_file}" ]]; then
  echo "技术恢复状态回执: ${status_file}"
fi
