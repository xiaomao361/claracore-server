package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPReverseProxyKeepsLoopbackHost prevents the deployed reverse proxy from
// tripping the MCP SDK's DNS-rebinding protection. The application listens on
// loopback, so forwarding the public Host header makes every authenticated MCP
// POST fail with 403 before JSON-RPC is reached.
func TestMCPReverseProxyKeepsLoopbackHost(t *testing.T) {
	for _, path := range []string{
		"../deploy/claracore-server.nginx.conf",
		"../deploy/nginx.conf.example",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s: %v", path, err)
		}
		text := string(body)
		start := strings.Index(text, "location ")
		if start < 0 || !strings.Contains(text[start:], "/mcp") {
			t.Fatalf("%s 缺少 /mcp location", path)
		}
		mcp := text[start:]
		if next := strings.Index(mcp[1:], "location "); next >= 0 {
			mcp = mcp[:next+1]
		}
		if !strings.Contains(mcp, "proxy_set_header Host 127.0.0.1;") {
			t.Fatalf("%s 的 /mcp 必须把上游 Host 固定为 loopback", path)
		}
	}
}

func TestReverseProxyMirrorsApplicationRequestBoundaries(t *testing.T) {
	for _, path := range []string{
		"../deploy/claracore-server.nginx.conf",
		"../deploy/nginx.conf.example",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, want := range []string{
			"server_tokens off;",
			"client_header_timeout 10s;", "client_body_timeout 30s;",
			"keepalive_timeout 75s;", "send_timeout 60s;",
			"client_max_body_size 4m;", "client_max_body_size 1m;",
			"proxy_read_timeout 300s;", "proxy_send_timeout 300s;",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}

	envBody, err := os.ReadFile("../deploy/claracore-server.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CCS_HTTP_READ_HEADER_TIMEOUT_SEC=10", "CCS_HTTP_READ_TIMEOUT_SEC=30",
		"CCS_HTTP_WRITE_TIMEOUT_SEC=300", "CCS_HTTP_IDLE_TIMEOUT_SEC=120",
		"CCS_HTTP_MAX_HEADER_BYTES=65536", "CCS_WEB_MAX_FORM_BYTES=1048576", "CCS_EXPORT_MAX_BYTES=1073741824",
		"CCS_MCP_MAX_BODY_BYTES=4194304", "CCS_MCP_RATE_LIMIT=300", "CCS_MCP_TENANT_RATE_LIMIT=3000",
		"CCS_MCP_RATE_WINDOW_SEC=60",
		"CCS_METRICS_ADDR=127.0.0.1:51892",
		"CCS_INSTANCE_ID=",
		"CCS_MYSQL_TLS_MODE=disabled", "CCS_MYSQL_TLS_CA_FILE=", "CCS_MYSQL_TLS_SERVER_NAME=",
		"CCS_REDIS_TLS_MODE=disabled", "CCS_REDIS_TLS_CA_FILE=", "CCS_REDIS_TLS_SERVER_NAME=",
	} {
		if !strings.Contains(string(envBody), want) {
			t.Errorf("production environment template missing %q", want)
		}
	}
}

func TestVerifyClusterProvesEveryExpectedInstanceReachedPublicEntry(t *testing.T) {
	tmp := t.TempDir()
	fakeCurl := filepath.Join(tmp, "curl")
	const script = `#!/usr/bin/env bash
set -euo pipefail
output=""
url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    *) url="$1"; shift ;;
  esac
done
instance=""
case "${url}" in
  http://10.0.0.1:51890/*) instance="ccs-a" ;;
  http://10.0.0.2:51890/*) instance="ccs-b" ;;
  https://memory.example.test/readyz?cluster_probe=*)
    if [[ "${FAKE_CLUSTER_MODE:-balanced}" == "single" ]]; then
      instance="ccs-a"
    else
      attempt="${url##*=}"
      if (( attempt % 2 == 0 )); then instance="ccs-b"; else instance="ccs-a"; fi
    fi
    ;;
  *) exit 22 ;;
esac
if [[ "${url}" == */livez ]]; then status="alive"; else status="ready"; fi
digest="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
if [[ "${FAKE_CLUSTER_MODE:-balanced}" == "mismatch" && "${instance}" == "ccs-b" ]]; then
  digest="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
fi
if [[ "${FAKE_CLUSTER_MODE:-balanced}" == "development" ]]; then digest="development"; fi
binary_digest="cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
if [[ "${FAKE_CLUSTER_MODE:-balanced}" == "artifact_mismatch" && "${instance}" == "ccs-b" ]]; then
  binary_digest="dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
fi
printf '{"ok":true,"status":"%s","instanceId":"%s","version":"v-test","sourceDigest":"%s","binaryDigest":"%s","env":"production","mysql":"reachable","schema":"current","schemaVersion":"0010"}\n' "${status}" "${instance}" "${digest}" "${binary_digest}" > "${output}"
`
	if err := os.WriteFile(fakeCurl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"../scripts/verify-cluster.sh",
		"--public-url", "https://memory.example.test",
		"--instance", "ccs-a=http://10.0.0.1:51890",
		"--instance", "ccs-b=http://10.0.0.2:51890",
		"--attempts", "4",
	}
	run := func(mode string) (string, error) {
		cmd := exec.Command("bash", args...)
		cmd.Env = append(os.Environ(), "PATH="+tmp+":"+os.Getenv("PATH"), "FAKE_CLUSTER_MODE="+mode)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("balanced")
	if err != nil {
		t.Fatalf("集群探针失败: %v\n%s", err, out)
	}
	for _, want := range []string{"2 个实例均 live/ready", "源码摘要 aaaaaaaaaa", "二进制摘要 cccccccccc", "公开入口 4 次采样覆盖全部实例", "不等于滚动发布或故障切换验收"} {
		if !strings.Contains(out, want) {
			t.Errorf("集群探针成功输出缺少 %q: %s", want, out)
		}
	}

	out, err = run("single")
	if err == nil || !strings.Contains(out, "未返回实例: ccs-b") {
		t.Fatalf("公开入口未覆盖全部实例时必须失败: err=%v output=%s", err, out)
	}

	out, err = run("mismatch")
	if err == nil || !strings.Contains(out, "版本/sourceDigest/binaryDigest/env/schema 不一致") {
		t.Fatalf("同版本但源码摘要不同的实例必须失败: err=%v output=%s", err, out)
	}

	out, err = run("development")
	if err == nil || !strings.Contains(out, "生产实例 ccs-a 使用未绑定源码摘要") {
		t.Fatalf("生产实例不能使用默认 development 摘要: err=%v output=%s", err, out)
	}

	out, err = run("artifact_mismatch")
	if err == nil || !strings.Contains(out, "版本/sourceDigest/binaryDigest/env/schema 不一致") {
		t.Fatalf("同版本同源码但二进制不同的实例必须失败: err=%v output=%s", err, out)
	}
}

func TestPublicReverseProxyNeverExposesMetricsListener(t *testing.T) {
	for _, path := range []string{
		"../deploy/claracore-server.nginx.conf",
		"../deploy/nginx.conf.example",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		start := strings.Index(text, "location = /metrics")
		if start < 0 {
			t.Fatalf("%s must explicitly block public /metrics", path)
		}
		block := text[start:]
		if end := strings.Index(block, "}"); end >= 0 {
			block = block[:end+1]
		}
		if !strings.Contains(block, "return 404;") || strings.Contains(block, "proxy_pass") {
			t.Fatalf("%s metrics public block is unsafe: %s", path, block)
		}
	}
}

func TestVerifyDeploymentChecksLivenessReadinessAndAuthBoundary(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "curl.log")
	fakeCurl := filepath.Join(tmp, "curl")
	const script = `#!/usr/bin/env bash
set -euo pipefail
url=""
for arg in "$@"; do url="${arg}"; done
printf '%s\n' "${url}" >> "${FAKE_CURL_LOG}"
case "${url}" in
  */livez|*/readyz)
    source_digest="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    binary_digest="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    [[ "${FAKE_DEPLOY_MODE:-ok}" == "development" ]] && source_digest="development"
    if [[ "${url}" == */readyz && "${FAKE_DEPLOY_MODE:-ok}" == "mismatch" ]]; then
      binary_digest="cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    fi
    if [[ "${url}" == */livez ]]; then
      printf '{"ok":true,"status":"alive","version":"v-test","sourceDigest":"%s","binaryDigest":"%s","env":"production"}\n' "${source_digest}" "${binary_digest}"
    else
      printf '{"ok":true,"status":"ready","version":"v-test","sourceDigest":"%s","binaryDigest":"%s","env":"production","mysql":"reachable","schema":"current"}\n' "${source_digest}" "${binary_digest}"
    fi
    ;;
  */login) ;;
  */mcp) printf '401' ;;
  */metrics)
    restore_available=1
    [[ "${FAKE_DEPLOY_MODE:-ok}" == "restore_missing" ]] && restore_available=0
    integrity_verified=1
    integrity_violations=0
    [[ "${FAKE_DEPLOY_MODE:-ok}" == "integrity_violation" ]] && integrity_verified=0 && integrity_violations=2
    printf '%s\n' 'claracore_build_info{version="test",environment="test"} 1' 'claracore_instance_accepting_requests 1' 'claracore_backup_receipt_available 1' 'claracore_backup_fresh 1' "claracore_restore_receipt_available ${restore_available}" 'claracore_restore_fresh 1' 'claracore_integrity_receipt_available 1' 'claracore_integrity_fresh 1' "claracore_integrity_verified ${integrity_verified}" "claracore_integrity_violations ${integrity_violations}"
    ;;
  *) exit 22 ;;
esac
`
	if err := os.WriteFile(fakeCurl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "../scripts/verify-deployment.sh", "https://memory.example.test", "http://127.0.0.1:51892")
	cmd.Env = append(os.Environ(), "PATH="+tmp+":"+os.Getenv("PATH"), "FAKE_CURL_LOG="+logPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("部署探针失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "源码/二进制身份一致") || !strings.Contains(string(out), "MySQL/迁移 ready") {
		t.Fatalf("成功输出没有说明 readiness 边界: %s", out)
	}
	missingRestore := exec.Command("bash", "../scripts/verify-deployment.sh", "https://memory.example.test", "http://127.0.0.1:51892")
	missingRestore.Env = append(os.Environ(), "PATH="+tmp+":"+os.Getenv("PATH"), "FAKE_CURL_LOG="+logPath, "FAKE_DEPLOY_MODE=restore_missing")
	missingOut, missingErr := missingRestore.CombinedOutput()
	if missingErr == nil || !strings.Contains(string(missingOut), "备份/恢复有效且数据完整性检查新鲜无违规") {
		t.Fatalf("部署指标不能在缺少恢复演练回执时通过: err=%v output=%s", missingErr, missingOut)
	}
	violations := exec.Command("bash", "../scripts/verify-deployment.sh", "https://memory.example.test", "http://127.0.0.1:51892")
	violations.Env = append(os.Environ(), "PATH="+tmp+":"+os.Getenv("PATH"), "FAKE_CURL_LOG="+logPath, "FAKE_DEPLOY_MODE=integrity_violation")
	violationsOut, violationsErr := violations.CombinedOutput()
	if violationsErr == nil || !strings.Contains(string(violationsOut), "数据完整性检查新鲜无违规") {
		t.Fatalf("部署指标不能在数据关系违规时通过: err=%v output=%s", violationsErr, violationsOut)
	}
	for _, tc := range []struct {
		mode string
		want string
	}{
		{mode: "mismatch", want: "binaryDigest 缺失或不一致"},
		{mode: "development", want: "生产实例使用未绑定源码摘要"},
	} {
		bad := exec.Command("bash", "../scripts/verify-deployment.sh", "https://memory.example.test")
		bad.Env = append(os.Environ(), "PATH="+tmp+":"+os.Getenv("PATH"), "FAKE_CURL_LOG="+logPath, "FAKE_DEPLOY_MODE="+tc.mode)
		badOut, badErr := bad.CombinedOutput()
		if badErr == nil || !strings.Contains(string(badOut), tc.want) {
			t.Errorf("部署探针 mode=%s 必须失败并包含 %q: err=%v output=%s", tc.mode, tc.want, badErr, badOut)
		}
	}

	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		"https://memory.example.test/livez", "https://memory.example.test/readyz",
		"https://memory.example.test/login", "https://memory.example.test/mcp",
		"http://127.0.0.1:51892/metrics",
	} {
		if !strings.Contains(string(logBody), url) {
			t.Errorf("部署探针没有请求 %s: %s", url, logBody)
		}
	}
}

func TestServiceManagersLeaveTimeForDrainAndRequestShutdown(t *testing.T) {
	wants := map[string]string{
		"../deploy/claracore-server.service":        "TimeoutStopSec=30s",
		"../deploy/claracore-server.supervisor.ini": "stopwaitsecs=30",
	}
	for path, want := range wants {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s 必须包含 %q，覆盖默认 5s 摘流和 15s 请求关闭预算", path, want)
		}
	}

	envBody, err := os.ReadFile("../deploy/claracore-server.env.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envBody), "CCS_SHUTDOWN_DRAIN_SEC=5") {
		t.Fatal("生产配置模板必须显式记录默认摘流窗口")
	}
}

func TestBackupTimerIsCatchUpSafeAndNeverDeletesHistory(t *testing.T) {
	timerBody, err := os.ReadFile("../deploy/claracore-server-backup.timer")
	if err != nil {
		t.Fatal(err)
	}
	timer := string(timerBody)
	for _, want := range []string{"OnCalendar=", "Persistent=true", "RandomizedDelaySec=", "Unit=claracore-server-backup.service"} {
		if !strings.Contains(timer, want) {
			t.Errorf("backup timer missing %q", want)
		}
	}

	serviceBody, err := os.ReadFile("../deploy/claracore-server-backup.service")
	if err != nil {
		t.Fatal(err)
	}
	service := string(serviceBody)
	for _, want := range []string{
		"User=claracore", "Group=claracore", "UMask=0077",
		"/etc/claracore-server/mysql-backup.cnf", "backup-mysql.sh",
		"verify-mysql-backup.sh", "--status-file /var/lib/claracore-server/backup-status.json",
		"ReadWritePaths=/var/backups/claracore-server /var/lib/claracore-server",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("backup service missing %q", want)
		}
	}
	lower := strings.ToLower(service)
	for _, forbidden := range []string{"rm -", "find ", "--delete", "-delete", "unlink"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("backup service must not delete historical backups; found %q", forbidden)
		}
	}
	envBody, err := os.ReadFile("../deploy/claracore-server.env.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envBody), "CCS_BACKUP_STATUS_FILE=/var/lib/claracore-server/backup-status.json") {
		t.Fatal("production environment template must wire the backup receipt into the admin dashboard")
	}
	if !strings.Contains(string(envBody), "CCS_RESTORE_STATUS_FILE=/var/lib/claracore-server/restore-status.json") {
		t.Fatal("production environment template must wire the restore rehearsal receipt into the admin dashboard")
	}
}

func TestIntegrityTimerPublishesDailyBoundedReceipt(t *testing.T) {
	timerBody, err := os.ReadFile("../deploy/claracore-server-integrity.timer")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OnCalendar=", "Persistent=true", "RandomizedDelaySec=", "Unit=claracore-server-integrity.service"} {
		if !strings.Contains(string(timerBody), want) {
			t.Errorf("integrity timer missing %q", want)
		}
	}
	serviceBody, err := os.ReadFile("../deploy/claracore-server-integrity.service")
	if err != nil {
		t.Fatal(err)
	}
	service := string(serviceBody)
	for _, want := range []string{"Type=oneshot", "User=claracore", "Group=claracore", "UMask=0077", "EnvironmentFile=/etc/claracore-server/claracore-server.env", "claracore-server -check-integrity", "ReadWritePaths=/var/lib/claracore-server"} {
		if !strings.Contains(service, want) {
			t.Errorf("integrity service missing %q", want)
		}
	}
	envBody, err := os.ReadFile("../deploy/claracore-server.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CCS_INTEGRITY_STATUS_FILE=/var/lib/claracore-server/integrity-status.json", "CCS_INTEGRITY_MAX_AGE_SEC=90000"} {
		if !strings.Contains(string(envBody), want) {
			t.Errorf("production environment template missing %q", want)
		}
	}
}
