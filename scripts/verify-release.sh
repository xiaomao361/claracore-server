#!/usr/bin/env bash
set -euo pipefail

release_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
manifest="${release_dir}/MANIFEST.sha256"

fail() {
  echo "发布目录验证失败: $*" >&2
  exit 1
}

[[ -f "${manifest}" && ! -L "${manifest}" ]] || fail "MANIFEST.sha256 缺失、不是普通文件或是符号链接"

listed="$(mktemp)"
actual="$(mktemp)"
trap 'rm -f -- "${listed}" "${actual}"' EXIT

count=0
while IFS= read -r line || [[ -n "${line}" ]]; do
  [[ "${line}" =~ ^[0-9a-f]{64}\ \ (\./[0-9A-Za-z._/-]+)$ ]] || fail "清单包含非法行"
  path="${BASH_REMATCH[1]}"
  case "${path}" in
    *"/../"*|*"/./"*|"./.."|"./MANIFEST.sha256") fail "清单包含不安全路径" ;;
  esac
  [[ -f "${release_dir}/${path#./}" && ! -L "${release_dir}/${path#./}" ]] || fail "${path} 缺失、不是普通文件或是符号链接"
  printf '%s\n' "${path}" >> "${listed}"
  count=$((count + 1))
done < "${manifest}"
(( count > 0 )) || fail "清单为空"

LC_ALL=C sort "${listed}" -o "${listed}"
if [[ "$(uniq "${listed}" | wc -l | tr -d ' ')" != "${count}" ]]; then
  fail "清单包含重复路径"
fi

(
  cd "${release_dir}"
  find . \( -type f -o -type l \) ! -path './MANIFEST.sha256' -print | LC_ALL=C sort > "${actual}"
)
if ! cmp -s "${listed}" "${actual}"; then
  fail "目录文件集合与清单不一致（可能缺失、被替换或混入额外文件）"
fi

(
  cd "${release_dir}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum --check --strict MANIFEST.sha256
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 --check MANIFEST.sha256
  else
    fail "需要 sha256sum 或 shasum"
  fi
)

echo "发布目录完整性验证通过: ${count} 个文件"
