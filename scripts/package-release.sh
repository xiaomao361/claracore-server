#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${1:-dev}"
target_arch="${2:-amd64}"
output_dir="${3:-${repo_dir}/dist}"

case "${target_arch}" in
  amd64|arm64) ;;
  *) echo "不支持的 Linux 架构: ${target_arch}（仅 amd64/arm64）" >&2; exit 2 ;;
esac
if [[ ! "${version}" =~ ^[0-9A-Za-z._-]+$ ]]; then
  echo "版本号只能包含字母、数字、点、下划线和短横线" >&2
  exit 2
fi

stage_root="$(mktemp -d)"
trap 'rm -rf -- "${stage_root}"' EXIT
package_name="claracore-server_${version}_linux_${target_arch}"
stage_dir="${stage_root}/${package_name}"
mkdir -p "${stage_dir}/deploy" "${output_dir}"

(
  cd "${repo_dir}"
  CGO_ENABLED=0 GOOS=linux GOARCH="${target_arch}" \
    go build -trimpath -ldflags "-s -w -X main.version=${version}" \
    -o "${stage_dir}/claracore-server" ./cmd/server
)
cp "${repo_dir}/deploy/claracore-server.service" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.supervisor.ini" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.nginx.conf" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/claracore-server.env.example" "${stage_dir}/deploy/"
cp "${repo_dir}/deploy/nginx.conf.example" "${stage_dir}/deploy/"
cp "${repo_dir}/scripts/verify-deployment.sh" "${stage_dir}/"
cp "${repo_dir}/docs/07-deployment.md" "${stage_dir}/DEPLOYMENT.md"
cp "${repo_dir}/LICENSE" "${stage_dir}/"
cp "${repo_dir}/README.md" "${stage_dir}/"

archive="${output_dir}/${package_name}.tar.gz"
tar -C "${stage_root}" -czf "${archive}" "${package_name}"
(
  cd "${output_dir}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${package_name}.tar.gz" > "${package_name}.tar.gz.sha256"
  else
    shasum -a 256 "${package_name}.tar.gz" > "${package_name}.tar.gz.sha256"
  fi
)
echo "已生成 ${archive}"
echo "校验和 ${archive}.sha256"
