#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-check}"
stage_dir="${2:-}"
prometheus_module="github.com/prometheus/prometheus@v0.314.0"
prometheus_sum="h1:YjsimqsIi6/mOtzZcrPEYUALO6zpfaht9O5sXqDz2vg="
prometheus_revision="d7598b7141418fa35be2b5ec5d0fefb634199610"
prometheus_version="3.14.0"
go_toolchain="go1.26.6"
tool_root=""
receipt_path=""

cleanup() {
  [[ -z "${tool_root}" ]] || rm -rf -- "${tool_root}"
}
trap cleanup EXIT

case "${mode}" in
  check)
    [[ -z "${stage_dir}" ]] || {
      echo "Prometheus 规则检查模式不接受制品目录" >&2
      exit 2
    }
    rules_file="${repo_dir}/deploy/claracore-server.rules.yml"
    rules_test_file="${repo_dir}/deploy/claracore-server.rules.test.yml"
    ;;
  bundle)
    [[ "${stage_dir}" == /* ]] || {
      echo "Prometheus 规则制品目录必须使用绝对路径" >&2
      exit 2
    }
    [[ -d "${stage_dir}" && ! -L "${stage_dir}" ]] || {
      echo "Prometheus 规则制品目录必须是普通目录且不能是符号链接" >&2
      exit 2
    }
    rules_file="${stage_dir}/deploy/claracore-server.rules.yml"
    rules_test_file="${stage_dir}/deploy/claracore-server.rules.test.yml"
    receipt_path="${stage_dir}/PROMETHEUS_RULES_CHECK.txt"
    [[ ! -e "${receipt_path}" && ! -L "${receipt_path}" ]] || {
      echo "Prometheus 规则回执已存在，拒绝覆盖" >&2
      exit 2
    }
    ;;
  *)
    echo "用法: check-prometheus-rules.sh check | bundle <absolute-stage-dir>" >&2
    exit 2
    ;;
esac

[[ -f "${rules_file}" && ! -L "${rules_file}" ]] || {
  echo "Prometheus 规则必须是普通文件且不能是符号链接" >&2
  exit 2
}
[[ -f "${rules_test_file}" && ! -L "${rules_test_file}" ]] || {
  echo "Prometheus 规则单元测试必须是普通文件且不能是符号链接" >&2
  exit 2
}

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

download_json="$(GOWORK=off GOTOOLCHAIN="${go_toolchain}" go mod download -json "${prometheus_module}")"
download_sum="$(printf '%s\n' "${download_json}" | sed -n 's/^[[:space:]]*"Sum": "\([^"]*\)",*$/\1/p')"
download_revision="$(printf '%s\n' "${download_json}" | sed -n 's/^[[:space:]]*"Hash": "\([0-9a-f]*\)",*$/\1/p')"
module_dir="$(printf '%s\n' "${download_json}" | sed -n 's/^[[:space:]]*"Dir": "\([^"]*\)",*$/\1/p')"
[[ "${download_sum}" == "${prometheus_sum}" && "${download_revision}" == "${prometheus_revision}" ]] || {
  echo "Prometheus 模块校验和或 revision 与固定版本不符" >&2
  exit 1
}
[[ -n "${module_dir}" && -d "${module_dir}" && ! -L "${module_dir}" ]] || {
  echo "Prometheus 固定模块源码目录不可用" >&2
  exit 1
}

tool_root="$(mktemp -d)"
(
  cd "${module_dir}"
  GOWORK=off GOTOOLCHAIN="${go_toolchain}" go build -trimpath -tags=netgo,builtinassets \
    -ldflags "-X github.com/prometheus/common/version.Version=${prometheus_version} \
      -X github.com/prometheus/common/version.Revision=${prometheus_revision} \
      -X github.com/prometheus/common/version.Branch=release-3.14 \
      -X github.com/prometheus/common/version.BuildUser=claracore-server \
      -X github.com/prometheus/common/version.BuildDate=20260817-13:38:44" \
    -o "${tool_root}/promtool" ./cmd/promtool
)
promtool="${tool_root}/promtool"
tool_version="$(${promtool} --version 2>&1)"
[[ "${tool_version}" == *"promtool, version ${prometheus_version}"* && \
   "${tool_version}" == *"revision: ${prometheus_revision}"* && \
   "${tool_version}" == *"go version:       ${go_toolchain}"* ]] || {
  echo "固定 promtool 身份、revision 或 Go 工具链不符" >&2
  printf '%s\n' "${tool_version}" >&2
  exit 1
}

check_output="$(${promtool} check rules "${rules_file}" 2>&1)" || {
  echo "promtool 拒绝 ClaraCore 告警规则" >&2
  printf '%s\n' "${check_output}" >&2
  exit 1
}
[[ "${check_output}" == *"SUCCESS: 22 rules found"* ]] || {
  echo "promtool 回执没有确认预期的 22 条规则" >&2
  printf '%s\n' "${check_output}" >&2
  exit 1
}
test_output="$(
  cd "$(dirname "${rules_test_file}")"
  "${promtool}" test rules "$(basename "${rules_test_file}")" 2>&1
)" || {
  echo "promtool 规则行为单元测试失败" >&2
  printf '%s\n' "${test_output}" >&2
  exit 1
}
[[ "${test_output}" == *"SUCCESS"* ]] || {
  echo "promtool 没有确认规则行为单元测试成功" >&2
  printf '%s\n' "${test_output}" >&2
  exit 1
}

receipt="$(printf '%s\n' \
  "tool=promtool@${prometheus_version} source=${prometheus_module} module_sum=${prometheus_sum}" \
  "revision=${prometheus_revision} go=${go_toolchain}" \
  "rules=claracore-server.rules.yml sha256=$(hash_file "${rules_file}") alerts=22" \
  "tests=claracore-server.rules.test.yml sha256=$(hash_file "${rules_test_file}") unit_tests=verified" \
  "result=verified")"
if [[ "${mode}" == "bundle" ]]; then
  printf '%s\n' "${receipt}" > "${receipt_path}"
else
  printf '%s\n' "${receipt}"
fi
