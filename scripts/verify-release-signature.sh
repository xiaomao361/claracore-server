#!/usr/bin/env bash
set -euo pipefail

archive="${1:-}"
checksum="${2:-}"
signature="${3:-}"
public_key="${4:-}"

fail() {
  echo "发布签名验证失败: $*" >&2
  exit 1
}

[[ $# -eq 4 ]] || fail "用法: $0 <archive.tar.gz> <archive.tar.gz.sha256> <archive.tar.gz.sha256.sig> <trusted-ed25519-public-key.pem>"
command -v openssl >/dev/null 2>&1 || fail "需要 openssl"

for pair in "归档:${archive}" "校验和:${checksum}" "签名:${signature}" "公钥:${public_key}"; do
  label="${pair%%:*}"
  path="${pair#*:}"
  [[ -f "${path}" && ! -L "${path}" ]] || fail "${label}必须是普通文件且不能是符号链接"
done

archive_dir="$(cd "$(dirname "${archive}")" && pwd -P)"
checksum_dir="$(cd "$(dirname "${checksum}")" && pwd -P)"
signature_dir="$(cd "$(dirname "${signature}")" && pwd -P)"
[[ "${archive_dir}" == "${checksum_dir}" && "${archive_dir}" == "${signature_dir}" ]] || \
  fail "归档、校验和与签名必须位于同一目录"

archive_name="$(basename "${archive}")"
checksum_name="$(basename "${checksum}")"
signature_name="$(basename "${signature}")"
[[ "${archive_name}" =~ ^[0-9A-Za-z._-]+\.tar\.gz$ ]] || fail "归档文件名不安全"
[[ "${checksum_name}" == "${archive_name}.sha256" ]] || fail "校验和文件名与归档不匹配"
[[ "${signature_name}" == "${checksum_name}.sig" ]] || fail "签名文件名与校验和不匹配"

IFS= read -r checksum_line < "${checksum}" || fail "无法读取校验和文件"
[[ "$(wc -l < "${checksum}" | tr -d ' ')" == "1" ]] || fail "校验和文件只能包含一条记录"
[[ "${checksum_line}" =~ ^([0-9a-f]{64})\ \ ([0-9A-Za-z._-]+\.tar\.gz)$ ]] || fail "校验和文件格式不安全"
expected_hash="${BASH_REMATCH[1]}"
[[ "${BASH_REMATCH[2]}" == "${archive_name}" ]] || fail "校验和绑定的归档名不匹配"

public_key_description="$(openssl pkey -pubin -in "${public_key}" -text_pub -noout 2>/dev/null)" || \
  fail "无法读取受信任公钥"
[[ "${public_key_description}" == ED25519\ Public-Key:* ]] || fail "受信任公钥必须是 Ed25519"
openssl pkeyutl -verify -rawin -pubin -inkey "${public_key}" \
  -in "${checksum}" -sigfile "${signature}" >/dev/null 2>&1 || fail "Ed25519 签名无效"

if command -v sha256sum >/dev/null 2>&1; then
  actual_hash="$(sha256sum "${archive}" | awk 'NR == 1 {print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual_hash="$(shasum -a 256 "${archive}" | awk 'NR == 1 {print $1}')"
else
  fail "需要 sha256sum 或 shasum"
fi
[[ "${actual_hash}" == "${expected_hash}" ]] || fail "归档 SHA-256 与已签名校验和不一致"

echo "发布者签名与归档 SHA-256 验证通过: ${archive_name}"
