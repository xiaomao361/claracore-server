package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPrometheusRulesPassPinnedPromtool(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "check-prometheus-rules.sh"), "check")
	if body, runErr := cmd.CombinedOutput(); runErr != nil ||
		!strings.Contains(string(body), "tool=promtool@3.14.0") ||
		!strings.Contains(string(body), "alerts=22") ||
		!strings.Contains(string(body), "result=verified") {
		t.Fatalf("pinned promtool rejected alert rules: %v\n%s", runErr, body)
	}
}

func TestPrometheusRuleBundleRejectsUnsafeTargetsBeforeDownloadingTool(t *testing.T) {
	root := repoRoot(t)
	missingRules := t.TempDir()
	symlinkStage := filepath.Join(t.TempDir(), "stage-link")
	if err := os.Symlink(missingRules, symlinkStage); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"relative stage": {"bundle", "release"},
		"symlink stage":  {"bundle", symlinkStage},
		"missing rules":  {"bundle", missingRules},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(root, "scripts", "check-prometheus-rules.sh"), args...)
			if body, runErr := cmd.CombinedOutput(); runErr == nil || !strings.Contains(string(body), "Prometheus 规则") {
				t.Fatalf("unsafe rules bundle target must fail before tool download: err=%v output=%s", runErr, body)
			}
		})
	}
}

func TestPrometheusRulesCoverExportedOperationalFailures(t *testing.T) {
	rulesBody, err := os.ReadFile("../deploy/claracore-server.rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	rules := string(rulesBody)
	for _, alert := range []string{
		"ClaraCoreTargetDown",
		"ClaraCoreNotAcceptingRequests",
		"ClaraCoreDevelopmentBuildInProduction",
		"ClaraCoreMixedBuildArtifacts",
		"ClaraCoreBackupReceiptMissing",
		"ClaraCoreBackupStale",
		"ClaraCoreRestoreReceiptMissing",
		"ClaraCoreRestoreStale",
		"ClaraCoreIntegrityReceiptMissing",
		"ClaraCoreIntegrityStale",
		"ClaraCoreIntegrityViolations",
		"ClaraCoreHTTPPanics",
		"ClaraCoreSustainedServerErrors",
		"ClaraCoreP95LatencyOverflow",
		"ClaraCoreMySQLPoolStatsMissing",
		"ClaraCoreMySQLPoolSaturation",
		"ClaraCoreMySQLConnectionWaitPressure",
		"ClaraCoreExternalHealthUnavailable",
		"ClaraCoreExternalSyncFailed",
		"ClaraCoreExternalSyncStale",
		"ClaraCoreExternalSyncClaimExpired",
		"ClaraCoreExternalSyncRequestBacklog",
	} {
		if !strings.Contains(rules, "alert: "+alert) {
			t.Errorf("alert rules missing %s", alert)
		}
	}
	if got := strings.Count(rules, "      - alert:"); got != 22 {
		t.Fatalf("alert count = %d, want 22", got)
	}
	if strings.Count(rules, "        for:") != 22 {
		t.Fatal("every alert must have a persistence window to suppress transient noise")
	}
	for _, severity := range []string{"severity: critical", "severity: warning"} {
		if !strings.Contains(rules, severity) {
			t.Errorf("rules missing %q", severity)
		}
	}
	if !strings.Contains(rules, "claracore_backup_receipt_available == 1") {
		t.Fatal("stale-backup alert must not duplicate the missing-receipt root cause")
	}
	if !strings.Contains(rules, "claracore_restore_receipt_available == 1") ||
		!strings.Contains(rules, "claracore_restore_fresh == 0") {
		t.Fatal("stale-restore alert must require a valid receipt and detect policy-age breach")
	}
	if !strings.Contains(rules, "(claracore_backup_receipt_available == 1)") {
		t.Fatal("missing-restore alert must be suppressed while the backup root cause is already unhealthy")
	}
	if !strings.Contains(rules, "claracore_integrity_receipt_available == 1") || !strings.Contains(rules, "claracore_integrity_verified == 1") || !strings.Contains(rules, "claracore_integrity_violations > 0") {
		t.Fatal("integrity alerts must distinguish missing, stale healthy evidence, and completed violations")
	}
	if !strings.Contains(rules, `up{job="claracore-server"} == 1`) {
		t.Fatal("draining alert must not duplicate target-down alerts")
	}
	if !strings.Contains(rules, `environment="production"`) || !strings.Contains(rules, `source_digest="development"`) {
		t.Fatal("production development-build alert must use the bounded build-info labels")
	}
	if !strings.Contains(rules, "count by (job, version, source_digest, binary_digest, environment)") {
		t.Fatal("mixed-build alert must group by version, source digest, binary digest, and environment")
	}
	if strings.Contains(rules, "rate(claracore_http_") || strings.Contains(rules, "increase(claracore_http_") {
		t.Fatal("rolling HTTP gauges must not be treated as counters")
	}
	if got := strings.Count(rules, "claracore_external_health_snapshot_available == 1"); got != 4 {
		t.Fatalf("external symptom alerts gated by a usable snapshot = %d, want 4", got)
	}
	if !strings.Contains(rules, "claracore_external_health_snapshot_age_seconds > 120") {
		t.Fatal("external monitoring alert must detect a sampler that stopped after a successful snapshot")
	}
	if !strings.Contains(rules, "claracore_external_sources_failed == 0") {
		t.Fatal("stale-source alert must not duplicate an explicit sync-failure root cause")
	}
	if !strings.Contains(rules, "claracore_external_sync_claims_active == 0") ||
		!strings.Contains(rules, "claracore_external_sync_claims_expired == 0") {
		t.Fatal("request-backlog alert must not duplicate active or expired claim states")
	}
}

func TestPrometheusRulesReferenceOnlyExportedClaraCoreMetrics(t *testing.T) {
	rulesBody, err := os.ReadFile("../deploy/claracore-server.rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, err := os.ReadFile("../cmd/server/metrics.go")
	if err != nil {
		t.Fatal(err)
	}
	exported := string(metricsBody)
	metricPattern := regexp.MustCompile(`claracore_[a-z0-9_]+`)
	seen := map[string]bool{}
	for _, name := range metricPattern.FindAllString(string(rulesBody), -1) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(exported, `"`+name+`"`) {
			t.Errorf("alert rule references metric not exported by current binary: %s", name)
		}
	}
	if len(seen) < 8 {
		t.Fatalf("rules reference only %d ClaraCore metrics; contract scan is unexpectedly narrow", len(seen))
	}
}

func TestPrometheusRuleUnitTestsCoverSuppressionContracts(t *testing.T) {
	body, err := os.ReadFile("../deploy/claracore-server.rules.test.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, contract := range []string{
		"target down and draining stay mutually exclusive",
		"backup missing and stale receipts do not duplicate",
		"external failures suppress stale duplicates",
		"active claims suppress manual request backlog",
		"ClaraCoreTargetDown",
		"ClaraCoreNotAcceptingRequests",
		"ClaraCoreBackupReceiptMissing",
		"ClaraCoreBackupStale",
		"ClaraCoreExternalSyncFailed",
		"ClaraCoreExternalSyncStale",
		"ClaraCoreExternalSyncRequestBacklog",
	} {
		if !strings.Contains(text, contract) {
			t.Errorf("Prometheus rule tests missing %q", contract)
		}
	}
	if got := strings.Count(text, "    alert_rule_test:"); got != 4 {
		t.Fatalf("rule behavior scenarios = %d, want 4", got)
	}
	if got := strings.Count(text, "        alertname:"); got != 7 {
		t.Fatalf("alert behavior assertions = %d, want 7", got)
	}
}

func TestPrometheusExampleLoadsRulesAndScrapesLoopback(t *testing.T) {
	body, err := os.ReadFile("../deploy/prometheus.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"rule_files:", "/etc/prometheus/claracore-server.rules.yml",
		"job_name: claracore-server", "127.0.0.1:51892",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Prometheus example missing %q", want)
		}
	}
	for _, forbidden := range []string{"0.0.0.0", "claracore.example.com", "alertmanagers:"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Prometheus example must not invent public exposure or organization routing: %q", forbidden)
		}
	}
}
