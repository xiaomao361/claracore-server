#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-check}"
target_arch="${2:-}"
binary_path="${3:-}"
stage_dir="${4:-}"
version="${5:-}"
tool_module="github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0"
tool_root=""

cleanup() {
  [[ -z "${tool_root}" ]] || rm -rf -- "${tool_root}"
}
trap cleanup EXIT

validate_arch() {
  case "$1" in
    amd64|arm64) ;;
    *) echo "SBOM 只支持 linux/amd64 或 linux/arm64" >&2; exit 2 ;;
  esac
}

case "${mode}" in
  check)
    [[ -z "${target_arch}" && -z "${binary_path}" && -z "${stage_dir}" && -z "${version}" ]] || {
      echo "SBOM 检查模式不接受其它参数" >&2
      exit 2
    }
    ;;
  bundle)
    validate_arch "${target_arch}"
    [[ "${binary_path}" == /* ]] || {
      echo "SBOM 输入二进制必须使用绝对路径" >&2
      exit 2
    }
    [[ -f "${binary_path}" && ! -L "${binary_path}" && -x "${binary_path}" ]] || {
      echo "SBOM 输入必须是可执行普通文件且不能是符号链接" >&2
      exit 2
    }
    [[ "${stage_dir}" == /* ]] || {
      echo "SBOM 制品目录必须使用绝对路径" >&2
      exit 2
    }
    [[ -d "${stage_dir}" && ! -L "${stage_dir}" ]] || {
      echo "SBOM 制品目录必须是普通目录且不能是符号链接" >&2
      exit 2
    }
    [[ "$(dirname "${binary_path}")" == "${stage_dir}" && "$(basename "${binary_path}")" == "claracore-server" ]] || {
      echo "SBOM 只能绑定制品目录内的 claracore-server 二进制" >&2
      exit 2
    }
    [[ "${version}" =~ ^[0-9A-Za-z._-]+$ ]] || {
      echo "SBOM 版本号只能包含字母、数字、点、下划线和短横线" >&2
      exit 2
    }
    for output in SBOM.cdx.json SBOM_REVIEW.txt; do
      [[ ! -e "${stage_dir}/${output}" && ! -L "${stage_dir}/${output}" ]] || {
        echo "SBOM 目标已存在，拒绝覆盖: ${output}" >&2
        exit 2
      }
    done
    ;;
  *)
    echo "用法: generate-sbom.sh check | bundle <amd64|arm64> <absolute-binary> <absolute-stage-dir> <version>" >&2
    exit 2
    ;;
esac

tool_root="$(mktemp -d)"
GOBIN="${tool_root}" GOTOOLCHAIN=auto go install "${tool_module}"
sbom_tool="${tool_root}/cyclonedx-gomod"

generate() {
  local arch="$1"
  local binary="$2"
  local output="$3"
  local release_version="$4"

  GOOS=linux GOARCH="${arch}" CGO_ENABLED=0 GOTOOLCHAIN=auto \
    "${sbom_tool}" bin -json -std -noserial -notimestamp -output-version 1.6 \
      -version "${release_version}" -output "${output}" "${binary}"
  (
    cd "${repo_dir}"
    go run ./scripts/validate-sbom.go \
      -sbom "${output}" -binary "${binary}" -version "${release_version}" -arch "${arch}"
  )
}

if [[ "${mode}" == "check" ]]; then
  for arch in amd64 arm64; do
    check_dir="${tool_root}/${arch}"
    mkdir -p "${check_dir}"
    binary="${check_dir}/claracore-server"
    output="${check_dir}/SBOM.cdx.json"
    (
      cd "${repo_dir}"
      CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
        go build -trimpath \
          -ldflags "-X main.version=sbom-check -X main.sourceDigest=0000000000000000000000000000000000000000000000000000000000000000" \
          -o "${binary}" ./cmd/server
    )
    generate "${arch}" "${binary}" "${output}" sbom-check >/dev/null
    echo "sbom=verified target=linux/${arch} tool=cyclonedx-gomod@v1.10.0 format=cyclonedx-1.6"
  done
  exit 0
fi

generate "${target_arch}" "${binary_path}" "${stage_dir}/SBOM.cdx.json" "${version}" \
  > "${stage_dir}/SBOM_REVIEW.txt"
