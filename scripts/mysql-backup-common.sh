#!/usr/bin/env bash

# Shared safety checks for the MySQL backup and restore-rehearsal entrypoints.
# This file is sourced; callers own `set -euo pipefail` and cleanup traps.

backup_die() {
  echo "$*" >&2
  exit 2
}

backup_require_command() {
  command -v "$1" >/dev/null 2>&1 || backup_die "缺少必需命令: $1"
}

backup_require_absolute_path() {
  local label="$1"
  local value="$2"
  [[ "${value}" == /* ]] || backup_die "${label} 必须是绝对路径"
}

backup_publish_file() {
  local source="$1"
  local destination="$2"
  # source and destination are created in the same directory. A hard link is an
  # atomic no-replace publication primitive on both GNU/Linux and macOS.
  ln "${source}" "${destination}" || backup_die "目标已存在，拒绝覆盖: ${destination}"
  rm -f -- "${source}"
}

backup_validate_database_name() {
  local value="$1"
  [[ "${value}" =~ ^[A-Za-z0-9_]+$ ]] || backup_die "数据库名只能包含字母、数字和下划线"
}

backup_file_mode() {
  local path="$1"
  if stat -f '%Lp' "${path}" >/dev/null 2>&1; then
    stat -f '%Lp' "${path}"
  else
    stat -c '%a' "${path}"
  fi
}

backup_validate_defaults_file() {
  local path="$1"
  backup_require_absolute_path "MySQL defaults file" "${path}"
  [[ -f "${path}" && ! -L "${path}" ]] || backup_die "MySQL defaults file 必须是普通文件且不能是符号链接"
  [[ -O "${path}" ]] || backup_die "MySQL defaults file 必须由当前用户拥有"

  local mode
  mode="$(backup_file_mode "${path}")"
  [[ "${mode}" =~ ^[0-7]{3,4}$ ]] || backup_die "无法识别 MySQL defaults file 权限: ${mode}"
  local normalized="${mode#0}"
  [[ -n "${normalized}" ]] || normalized="0"
  if (( (8#${normalized} & 8#077) != 0 )); then
    backup_die "MySQL defaults file 不能授予 group/other 权限（当前 ${mode}，要求 0600 或更严格）"
  fi
}

backup_require_sha256() {
  command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || \
    backup_die "缺少 SHA-256 工具（sha256sum 或 shasum）"
}

backup_sha256_value() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk 'NR == 1 {print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${file}" | awk 'NR == 1 {print $1}'
  else
    backup_die "缺少 SHA-256 工具（sha256sum 或 shasum）"
  fi
}

backup_sha256_verify() {
  local file="$1"
  local checksum="$2"
  local directory checksum_basename expected_basename
  directory="$(dirname "${checksum}")"
  checksum_basename="$(basename "${checksum}")"
  expected_basename="$(basename "${file}")"

  [[ "$(dirname "${file}")" == "${directory}" ]] || backup_die "备份与校验和必须位于同一目录"
  [[ "${expected_basename}" =~ ^[A-Za-z0-9._-]+$ ]] || backup_die "备份文件名只能包含字母、数字、点、下划线和短横线"
  local expected_hash checksum_target extra
  read -r expected_hash checksum_target extra < "${checksum}" || backup_die "无法读取校验和文件"
  [[ "${expected_hash}" =~ ^[0-9a-fA-F]{64}$ && "${checksum_target}" == "${expected_basename}" && -z "${extra:-}" ]] || \
    backup_die "校验和文件格式或目标文件名不匹配"
  [[ "$(wc -l < "${checksum}" | tr -d ' ')" == "1" ]] || backup_die "校验和文件只能包含一条记录"
  backup_require_sha256
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "${directory}" && sha256sum -c "${checksum_basename}") >/dev/null
  elif command -v shasum >/dev/null 2>&1; then
    (cd "${directory}" && shasum -a 256 -c "${checksum_basename}") >/dev/null
  fi
}
