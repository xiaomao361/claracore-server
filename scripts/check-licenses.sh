#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-check}"
target_arch="${2:-}"
stage_dir="${3:-}"
tool_module="github.com/google/go-licenses/v2@v2.0.1"
tool_root="$(mktemp -d)"

cleanup() {
  rm -rf -- "${tool_root}"
}
trap cleanup EXIT

case "${mode}" in
  check)
    [[ -z "${target_arch}" && -z "${stage_dir}" ]] || {
      echo "许可证检查模式不接受架构或输出目录" >&2
      exit 2
    }
    ;;
  bundle)
    case "${target_arch}" in
      amd64|arm64) ;;
      *) echo "许可证制品只支持 linux/amd64 或 linux/arm64" >&2; exit 2 ;;
    esac
    [[ "${stage_dir}" == /* ]] || {
      echo "许可证制品目录必须是绝对路径" >&2
      exit 2
    }
    [[ -d "${stage_dir}" && ! -L "${stage_dir}" ]] || {
      echo "许可证制品目录必须是普通目录且不能是符号链接" >&2
      exit 2
    }
    [[ ! -e "${stage_dir}/THIRD_PARTY_LICENSES.csv" && ! -L "${stage_dir}/THIRD_PARTY_LICENSES.csv" ]] || {
      echo "许可证报告目标已存在，拒绝覆盖" >&2
      exit 2
    }
    [[ ! -e "${stage_dir}/third_party_licenses" && ! -L "${stage_dir}/third_party_licenses" ]] || {
      echo "第三方许可证目录已存在，拒绝覆盖" >&2
      exit 2
    }
    ;;
  *)
    echo "用法: check-licenses.sh check | bundle <amd64|arm64> <absolute-stage-dir>" >&2
    exit 2
    ;;
esac

GOBIN="${tool_root}" GOTOOLCHAIN=auto go install "${tool_module}"
license_tool="${tool_root}/go-licenses"

validate_non_go_warnings() {
  local warnings_file="$1"
  local bad
  bad="$(awk '/^\// && $0 !~ /\.s$/ { print }' "${warnings_file}")"
  if [[ -n "${bad}" ]]; then
    echo "许可证工具发现未经审核的非 Go 依赖文件（仅允许当前纯汇编 .s）：" >&2
    printf '%s\n' "${bad}" >&2
    exit 1
  fi
}

run_for_arch() {
  local arch="$1"
  local command="$2"
  local warnings_file="$3"
  shift 3
  (
    cd "${repo_dir}"
    GOOS=linux GOARCH="${arch}" CGO_ENABLED=0 \
      "${license_tool}" "${command}" ./cmd/server --ignore=claracore-server "$@"
  ) 2> "${warnings_file}"
  validate_non_go_warnings "${warnings_file}"
}

if [[ "${mode}" == "check" ]]; then
  for arch in amd64 arm64; do
    warnings="${tool_root}/check-${arch}.log"
    run_for_arch "${arch}" check "${warnings}"
    echo "licenses=verified target=linux/${arch} tool=go-licenses@v2.0.1"
  done
  exit 0
fi

check_warnings="${tool_root}/check-${target_arch}.log"
report_warnings="${tool_root}/report-${target_arch}.log"
save_warnings="${tool_root}/save-${target_arch}.log"
run_for_arch "${target_arch}" check "${check_warnings}"
run_for_arch "${target_arch}" report "${report_warnings}" > "${stage_dir}/THIRD_PARTY_LICENSES.csv"
run_for_arch "${target_arch}" save "${save_warnings}" \
  --save_path="${stage_dir}/third_party_licenses"

[[ -s "${stage_dir}/THIRD_PARTY_LICENSES.csv" ]] || {
  echo "第三方许可证报告为空" >&2
  exit 1
}
if grep -q ',Unknown\($\|,\)' "${stage_dir}/THIRD_PARTY_LICENSES.csv"; then
  echo "第三方许可证报告含未知许可证" >&2
  exit 1
fi
grep -q '^github.com/go-sql-driver/mysql,.*,MPL-2.0$' "${stage_dir}/THIRD_PARTY_LICENSES.csv" || {
  echo "MySQL 驱动的 MPL-2.0 归属缺失" >&2
  exit 1
}
for required in \
  "github.com/go-sql-driver/mysql/LICENSE" \
  "github.com/go-sql-driver/mysql/driver.go"; do
  [[ -f "${stage_dir}/third_party_licenses/${required}" ]] || {
    echo "MPL-2.0 分发材料缺失: ${required}" >&2
    exit 1
  }
done

{
  echo "tool=go-licenses@v2.0.1 target=linux/${target_arch} cgo=disabled"
  echo "result=verified"
  echo "non_go_review=assembly_only"
  cat "${check_warnings}" "${report_warnings}" "${save_warnings}" |
    sed -n 's/.*] "\([^"]*\)" contains non-Go code.*/\1/p' |
    LC_ALL=C sort -u |
    sed 's/^/assembly_package=/'
} > "${stage_dir}/THIRD_PARTY_REVIEW.txt"
