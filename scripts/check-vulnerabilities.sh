#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-source}"
target="${2:-}"
scanner="golang.org/x/vuln/cmd/govulncheck@v1.7.0"

case "${mode}" in
  source)
    [[ -z "${target}" ]] || {
      echo "源码漏洞扫描不接受目标路径" >&2
      exit 2
    }
    cd "${repo_dir}"
    echo "scanner=govulncheck@v1.7.0 mode=source database=https://vuln.go.dev"
    GOTOOLCHAIN=auto go run "${scanner}" -show version ./...
    ;;
  binary)
    [[ -n "${target}" ]] || {
      echo "二进制漏洞扫描需要一个目标文件" >&2
      exit 2
    }
    [[ "${target}" == /* ]] || {
      echo "二进制漏洞扫描目标必须是绝对路径" >&2
      exit 2
    }
    [[ -f "${target}" && ! -L "${target}" ]] || {
      echo "二进制漏洞扫描目标必须是普通文件且不能是符号链接" >&2
      exit 2
    }
    echo "scanner=govulncheck@v1.7.0 mode=binary database=https://vuln.go.dev"
    GOTOOLCHAIN=auto go run "${scanner}" -mode=binary -show version "${target}"
    ;;
  *)
    echo "用法: check-vulnerabilities.sh source | binary /absolute/path/to/binary" >&2
    exit 2
    ;;
esac
