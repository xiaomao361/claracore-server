#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${1:-dev}"
target_arch="${2:-amd64}"
output_dir="${3:-${repo_dir}/dist}"
signing_key="${4:-}"

case "${target_arch}" in
  amd64|arm64) ;;
  *) echo "不支持的 Linux 架构: ${target_arch}（仅 amd64/arm64）" >&2; exit 2 ;;
esac
if [[ ! "${version}" =~ ^[0-9A-Za-z._-]+$ ]]; then
  echo "版本号只能包含字母、数字、点、下划线和短横线" >&2
  exit 2
fi

file_mode() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

if [[ -n "${signing_key}" ]]; then
  [[ "${signing_key}" == /* ]] || {
    echo "Ed25519 签名私钥必须使用绝对路径" >&2
    exit 2
  }
  [[ -f "${signing_key}" && ! -L "${signing_key}" ]] || {
    echo "Ed25519 签名私钥必须是普通文件且不能是符号链接" >&2
    exit 2
  }
  [[ -O "${signing_key}" ]] || {
    echo "Ed25519 签名私钥必须由当前用户拥有" >&2
    exit 2
  }
  signing_key_mode="$(file_mode "${signing_key}")"
  [[ "${signing_key_mode}" =~ ^[0-7]{3,4}$ ]] || {
    echo "无法识别 Ed25519 签名私钥权限: ${signing_key_mode}" >&2
    exit 2
  }
  normalized_signing_key_mode="${signing_key_mode#0}"
  [[ -n "${normalized_signing_key_mode}" ]] || normalized_signing_key_mode="0"
  if (( (8#${normalized_signing_key_mode} & 8#077) != 0 )); then
    echo "Ed25519 签名私钥不能授予 group/other 权限（当前 ${signing_key_mode}，要求 0600 或更严格）" >&2
    exit 2
  fi
  command -v openssl >/dev/null 2>&1 || {
    echo "签名发布需要 openssl" >&2
    exit 2
  }
  signing_key_description="$(openssl pkey -in "${signing_key}" -text_pub -noout 2>/dev/null)" || {
    echo "无法读取 Ed25519 签名私钥" >&2
    exit 2
  }
  [[ "${signing_key_description}" == ED25519\ Public-Key:* ]] || {
    echo "发布签名私钥必须是 Ed25519" >&2
    exit 2
  }
fi

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

hash_stream() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

# Bind the runtime identity to the exact application source tree, including
# uncommitted files that actually participate in the build. This prevents two
# binaries with the same human version but different source from appearing
# identical during a multi-instance rollout.
source_digest="$({
  cd "${repo_dir}"
  {
    printf '%s\n' go.mod go.sum
    find cmd internal migrations -type f \( \
      -name '*.go' -o -name '*.sql' -o -name '*.html' -o \
      -name '*.css' -o -name '*.js' \
    \) -print
  } | LC_ALL=C sort | while IFS= read -r source_file; do
    printf '%s  %s\n' "$(hash_file "${source_file}")" "${source_file}"
  done
} | hash_stream)"
[[ "${source_digest}" =~ ^[0-9a-f]{64}$ ]] || {
  echo "无法生成有效的源码摘要" >&2
  exit 1
}

stage_root="$(mktemp -d)"
archive_partial=""
checksum_partial=""
signature_partial=""
cleanup() {
  rm -rf -- "${stage_root}"
  [[ -z "${archive_partial}" ]] || rm -f -- "${archive_partial}"
  [[ -z "${checksum_partial}" ]] || rm -f -- "${checksum_partial}"
  [[ -z "${signature_partial}" ]] || rm -f -- "${signature_partial}"
}
trap cleanup EXIT
package_name="claracore-server_${version}_linux_${target_arch}"
stage_dir="${stage_root}/${package_name}"
mkdir -p "${stage_dir}/deploy" "${output_dir}"
archive="${output_dir}/${package_name}.tar.gz"
signature="${archive}.sha256.sig"
if [[ -z "${signing_key}" && ( -e "${signature}" || -L "${signature}" ) ]]; then
  echo "目标目录已有同名签名，拒绝用未签名构建覆盖归档: ${signature}" >&2
  exit 2
fi

# Publish the exact canonical stream whose digest is linked into the binary.
# Provenance can then bind both the human-readable source list and the same
# sourceDigest without trusting an opaque value copied from build arguments.
(
  cd "${repo_dir}"
  {
    printf '%s\n' go.mod go.sum
    find cmd internal migrations -type f \( \
      -name '*.go' -o -name '*.sql' -o -name '*.html' -o \
      -name '*.css' -o -name '*.js' \
    \) -print
  } | LC_ALL=C sort | while IFS= read -r source_file; do
    printf '%s  %s\n' "$(hash_file "${source_file}")" "${source_file}"
  done
) > "${stage_dir}/SOURCE_MANIFEST.sha256"
[[ "$(hash_file "${stage_dir}/SOURCE_MANIFEST.sha256")" == "${source_digest}" ]] || {
  echo "发布源码清单与已链接 sourceDigest 不一致" >&2
  exit 1
}
(
  cd "${repo_dir}"
  GOTOOLCHAIN=go1.26.6 go run ./scripts/release-provenance \
    -mode snapshot-inputs -repo "${repo_dir}" -stage "${stage_dir}"
)

build_release_binary() {
  local output="$1"
  local build_cache="$2"
  (
    cd "${repo_dir}"
    env -u GOCACHEPROG \
      GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=go1.26.6 GOCACHE="${build_cache}" \
      CGO_ENABLED=0 GOOS=linux GOARCH="${target_arch}" \
      go build -trimpath -buildvcs=false \
        -ldflags "-X main.version=${version} -X main.sourceDigest=${source_digest}" \
        -o "${output}" ./cmd/server
  )
}

# Compile the exact release twice with separate, newly created Go build caches.
# Matching bytes are a bounded same-host reproducibility result; module cache,
# host OS and toolchain distribution remain inside the local builder boundary.
primary_cache="${stage_root}/gocache-primary"
rebuild_cache="${stage_root}/gocache-rebuild"
rebuild_dir="${stage_root}/rebuild"
mkdir -p "${primary_cache}" "${rebuild_cache}" "${rebuild_dir}"
build_release_binary "${stage_dir}/claracore-server" "${primary_cache}"
build_release_binary "${rebuild_dir}/claracore-server.rebuild" "${rebuild_cache}"
(
  cd "${repo_dir}"
  GOTOOLCHAIN=go1.26.6 go run ./scripts/release-reproducibility \
    -mode generate -stage "${stage_dir}" \
    -primary "${stage_dir}/claracore-server" \
    -rebuilt "${rebuild_dir}/claracore-server.rebuild" \
    -version "${version}" -source-digest "${source_digest}" -arch "${target_arch}"
)

# Scan the exact cross-compiled Linux artifact, not only the developer's host
# source configuration. A finding or unavailable vulnerability database blocks
# publication. The signed outer checksum and inner manifest bind this receipt
# and the build information to the same binary bytes.
if ! "${repo_dir}/scripts/check-vulnerabilities.sh" binary "${stage_dir}/claracore-server" \
  > "${stage_dir}/VULNERABILITY_SCAN.txt"; then
  echo "Linux 发布制品漏洞扫描未通过：" >&2
  sed 's/^/  /' "${stage_dir}/VULNERABILITY_SCAN.txt" >&2
  exit 1
fi
(
  cd "${stage_dir}"
  GOTOOLCHAIN=auto go version -m ./claracore-server > BUILDINFO.txt
)
"${repo_dir}/scripts/check-licenses.sh" bundle "${target_arch}" "${stage_dir}"
"${repo_dir}/scripts/generate-sbom.sh" bundle "${target_arch}" \
  "${stage_dir}/claracore-server" "${stage_dir}" "${version}"
cp "${repo_dir}/deploy/claracore-server.service" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.supervisor.ini" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.nginx.conf" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.env.example" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/nginx.conf.example" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/mysql-client.cnf.example" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.rules.yml" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.rules.test.yml" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/prometheus.yml.example" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server-backup.service" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server-backup.timer" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server-integrity.service" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server-integrity.timer" "${stage_dir}/deploy/"
"${repo_dir}/scripts/check-prometheus-rules.sh" bundle "${stage_dir}"
cp "${repo_dir}/scripts/verify-deployment.sh" "${stage_dir}/"
cp "${repo_dir}/scripts/verify-cluster.sh" "${stage_dir}/"
cp "${repo_dir}/scripts/backup-mysql.sh" "${stage_dir}/"
cp "${repo_dir}/scripts/verify-mysql-backup.sh" "${stage_dir}/"
cp "${repo_dir}/scripts/rehearse-mysql-restore.sh" "${stage_dir}/"
cp "${repo_dir}/scripts/mysql-backup-common.sh" "${stage_dir}/"
cp "${repo_dir}/scripts/verify-release.sh" "${stage_dir}/"
cp "${repo_dir}/scripts/verify-release-signature.sh" "${stage_dir}/"
cp "${repo_dir}/docs/07-deployment.md" "${stage_dir}/DEPLOYMENT.md"
cp "${repo_dir}/docs/09-release-provenance.md" "${stage_dir}/PROVENANCE.md"
cp "${repo_dir}/LICENSE" "${stage_dir}/"
cp "${repo_dir}/README.md" "${stage_dir}/"

# Re-read every application source after all long-running build and review
# steps. A release never attests to a pre-build source/input snapshot if the
# working tree changed while it was being assembled.
final_source_manifest="${stage_root}/SOURCE_MANIFEST.final.sha256"
(
  cd "${repo_dir}"
  {
    printf '%s\n' go.mod go.sum
    find cmd internal migrations -type f \( \
      -name '*.go' -o -name '*.sql' -o -name '*.html' -o \
      -name '*.css' -o -name '*.js' \
    \) -print
  } | LC_ALL=C sort | while IFS= read -r source_file; do
    printf '%s  %s\n' "$(hash_file "${source_file}")" "${source_file}"
  done
) > "${final_source_manifest}"
if ! cmp -s "${stage_dir}/SOURCE_MANIFEST.sha256" "${final_source_manifest}"; then
  echo "发布期间应用源码发生变化；拒绝为混合输入生成 provenance" >&2
  exit 1
fi

(
  cd "${repo_dir}"
  GOTOOLCHAIN=go1.26.6 go run ./scripts/release-provenance \
    -mode generate -repo "${repo_dir}" -stage "${stage_dir}" \
    -binary "${stage_dir}/claracore-server" -version "${version}" \
    -source-digest "${source_digest}" -arch "${target_arch}"
)

# The outer archive checksum protects transport. This inner manifest lets an
# operator verify the exact unpacked release before switching the current link.
(
  cd "${stage_dir}"
  while IFS= read -r file; do
    printf '%s  %s\n' "$(hash_file "${file}")" "${file}"
  done < <(find . -type f ! -name MANIFEST.sha256 -print | LC_ALL=C sort)
) > "${stage_dir}/MANIFEST.sha256"

archive_partial="$(mktemp "${output_dir}/.${package_name}.tar.gz.partial.XXXXXX")"
checksum_partial="$(mktemp "${output_dir}/.${package_name}.tar.gz.sha256.partial.XXXXXX")"
(
  cd "${repo_dir}"
  GOTOOLCHAIN=go1.26.6 go run ./scripts/release-archive -source "${stage_dir}" > "${archive_partial}"
)
printf '%s  %s\n' "$(hash_file "${archive_partial}")" "${package_name}.tar.gz" > "${checksum_partial}"
if [[ -n "${signing_key}" ]]; then
  signature_partial="$(mktemp "${output_dir}/.${package_name}.tar.gz.sha256.sig.partial.XXXXXX")"
  if ! openssl pkeyutl -sign -rawin -inkey "${signing_key}" \
    -in "${checksum_partial}" -out "${signature_partial}" >/dev/null 2>&1; then
    echo "Ed25519 签名失败；没有发布新的归档、校验和或签名" >&2
    exit 1
  fi
fi
mv -f -- "${archive_partial}" "${archive}"
archive_partial=""
mv -f -- "${checksum_partial}" "${archive}.sha256"
checksum_partial=""
if [[ -n "${signing_key}" ]]; then
  mv -f -- "${signature_partial}" "${signature}"
  signature_partial=""
fi
echo "已生成 ${archive}"
echo "校验和 ${archive}.sha256"
if [[ -n "${signing_key}" ]]; then
  echo "Ed25519 签名 ${signature}"
else
  echo "未生成发布者签名；生产发布请把 0600 Ed25519 私钥绝对路径作为第 4 个参数" >&2
fi
