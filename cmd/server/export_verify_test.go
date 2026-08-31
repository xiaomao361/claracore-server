package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/buildprovenance"
	"claracore-server/internal/buildrepro"
	"claracore-server/internal/portableexport"
	"claracore-server/internal/teamimport"
)

func TestVerifyExportFlagDoesNotLoadConfigurationOrDependencies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-export.zip")
	if err := os.WriteFile(path, commandExportFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", ".", "-verify-export", path)
	cmd.Env = append(os.Environ(), "CCS_HTTP_MAX_HEADER_BYTES=many", "CCS_MYSQL_PORT=not-a-port", "CCS_REDIS_ADDR=127.0.0.1:1")
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verify-export must work without loading configuration or dependencies: %v\n%s", err, body)
	}
	var got portableexport.Verification
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("verify-export output is not clean JSON: %v\n%s", err, body)
	}
	if !got.OK || got.Format != portableexport.FormatName || got.Records != 4 || got.Files != len(portableexport.ContentFiles()) {
		t.Fatalf("unexpected verification output: %+v", got)
	}
}

func TestVerifyProvenanceFlagDoesNotLoadConfigurationOrDependencies(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-verify-provenance", "relative-provenance.json")
	cmd.Env = append(os.Environ(), "CCS_MYSQL_HOST=")
	body, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(body), "构建 provenance 必须使用绝对路径") {
		t.Fatalf("verify-provenance must reject its path before loading configuration: err=%v\n%s", err, body)
	}
	if strings.Contains(string(body), "配置加载失败") {
		t.Fatalf("verify-provenance loaded runtime configuration:\n%s", body)
	}
}

func TestVerifyProvenanceCommandValidatesItsOwnReleaseBeforeConfiguration(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	release := t.TempDir()
	binary := filepath.Join(release, "claracore-server")
	goModBody, err := os.ReadFile(filepath.Join(repo, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goModDigest := sha256.Sum256(goModBody)
	sourceBody := []byte(fmt.Sprintf("%x  go.mod\n", goModDigest))
	sourceDigest := fmt.Sprintf("%x", sha256.Sum256(sourceBody))
	const releaseVersion = "test-provenance-cli"
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
		"-ldflags", "-X main.version="+releaseVersion+" -X main.sourceDigest="+sourceDigest,
		"-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=go1.26.6")
	if body, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build provenance verifier binary: %v\n%s", err, body)
	}
	if err := os.WriteFile(filepath.Join(release, buildprovenance.SourceManifest), sourceBody, 0o644); err != nil {
		t.Fatal(err)
	}
	rebuilt := filepath.Join(t.TempDir(), "claracore-server.rebuild")
	binaryBody, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rebuilt, binaryBody, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := buildrepro.Generate(buildrepro.GenerateInput{
		ReleaseDir: release, PrimaryBinary: binary, RebuiltBinary: rebuilt,
		Identity: buildrepro.Identity{
			Version: releaseVersion, SourceDigest: sourceDigest,
			TargetOS: runtime.GOOS, TargetArch: runtime.GOARCH, GoToolchain: "go1.26.6",
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"BUILDINFO.txt", "PROMETHEUS_RULES_CHECK.txt", "SBOM.cdx.json", "SBOM_REVIEW.txt",
		"THIRD_PARTY_LICENSES.csv", "THIRD_PARTY_REVIEW.txt", "VULNERABILITY_SCAN.txt",
	} {
		if err := os.WriteFile(filepath.Join(release, name), []byte("receipt="+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := buildprovenance.Generate(buildprovenance.GenerateInput{
		RepoDir: repo, ReleaseDir: release, BinaryPath: binary,
		Version: releaseVersion, SourceDigest: sourceDigest,
		TargetOS: runtime.GOOS, TargetArch: runtime.GOARCH,
	}); err != nil {
		t.Fatal(err)
	}
	provenance := filepath.Join(release, buildprovenance.FileName)
	verify := exec.Command(binary, "-verify-provenance", provenance)
	verify.Env = append(os.Environ(), "CCS_HTTP_MAX_HEADER_BYTES=many", "CCS_MYSQL_PORT=not-a-port")
	body, err := verify.CombinedOutput()
	if err != nil {
		t.Fatalf("packaged binary provenance verification failed: %v\n%s", err, body)
	}
	var result buildprovenance.Verification
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("provenance CLI output is not clean JSON: %v\n%s", err, body)
	}
	if !result.OK || result.Status != "verified" || result.Subject != "claracore-server" {
		t.Fatalf("unexpected provenance verification: %+v", result)
	}
}

func TestPlanImportFlagDoesNotLoadConfigurationOrDependencies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-export.zip")
	if err := os.WriteFile(path, commandExportFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", ".", "-plan-import", path, "-import-admin", "alice")
	cmd.Env = append(os.Environ(), "CCS_HTTP_MAX_HEADER_BYTES=many", "CCS_MYSQL_PORT=not-a-port", "CCS_REDIS_ADDR=127.0.0.1:1")
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plan-import must work without loading configuration or dependencies: %v\n%s", err, body)
	}
	var got teamimport.Plan
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("plan-import output is not clean JSON: %v\n%s", err, body)
	}
	if !got.OK || got.SelectedAdmin != "alice" || got.Records != 4 || !got.RequiresEmptyDeployment || got.CredentialsImported {
		t.Fatalf("unexpected import plan: %+v", got)
	}
}

func TestImportExportRequiresExplicitEmptyDeploymentConfirmationBeforeConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-export.zip")
	if err := os.WriteFile(path, commandExportFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", ".", "-import-export", path, "-import-admin", "alice")
	cmd.Env = append(os.Environ(), "CCS_HTTP_MAX_HEADER_BYTES=many", "CCS_MYSQL_PORT=not-a-port")
	body, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("import-export without explicit confirmation must fail:\n%s", body)
	}
	if !bytes.Contains(body, []byte("-confirm-import-empty")) {
		t.Fatalf("failure must name the explicit confirmation boundary:\n%s", body)
	}
	if bytes.Contains(body, []byte("配置")) {
		t.Fatalf("confirmation gate must run before configuration loading:\n%s", body)
	}
}

func TestCheckImportRequiresAdministratorBeforeConfiguration(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-check-import")
	cmd.Env = append(os.Environ(), "CCS_HTTP_MAX_HEADER_BYTES=many", "CCS_MYSQL_PORT=not-a-port")
	body, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check-import without the selected administrator must fail:\n%s", body)
	}
	if !bytes.Contains(body, []byte("-import-admin")) {
		t.Fatalf("failure must name the missing administrator boundary:\n%s", body)
	}
	if bytes.Contains(body, []byte("配置加载失败")) {
		t.Fatalf("administrator gate must run before configuration loading:\n%s", body)
	}
}

func TestImportCheckReceiptWriterIsCleanMachineReadableJSON(t *testing.T) {
	var out bytes.Buffer
	result := mysql.TenantImportCheckResult{
		OK: true, Status: "verified", TenantID: "01K3V000000000000000000000", AdminUsername: "alice",
		Records: 4, MembersDisabled: 0, SourceCredentialsImported: false,
		ImportAuditID: "01K3V000000000000000000099", ImportedAt: time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC),
		IndexingStatus: "not_required",
	}
	if err := writeImportCheckResult(&out, result); err != nil {
		t.Fatal(err)
	}
	var got mysql.TenantImportCheckResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("import check receipt is not clean JSON: %v\n%s", err, out.Bytes())
	}
	if !got.OK || got.Status != "verified" || got.TenantID != result.TenantID || got.ImportAuditID != result.ImportAuditID {
		t.Fatalf("unexpected import check receipt: %+v", got)
	}
}

func TestDataIntegrityWriterIsCleanMachineReadableJSON(t *testing.T) {
	var out bytes.Buffer
	result := mysql.DataIntegrityResult{
		OK: false, Status: "violations_found", SchemaVersion: "0010",
		Snapshot: "repeatable_read_read_only", RulesChecked: 29, Violations: 2,
		Issues: []mysql.DataIntegrityIssue{{Domain: "knowledge", Rule: "label_entity", Violations: 2}},
	}
	if err := writeDataIntegrityResult(&out, result); err != nil {
		t.Fatal(err)
	}
	var got mysql.DataIntegrityResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("data integrity receipt is not clean JSON: %v\n%s", err, out.Bytes())
	}
	if got.OK || got.Status != "violations_found" || got.RulesChecked != 29 || got.Violations != 2 || len(got.Issues) != 1 {
		t.Fatalf("unexpected data integrity receipt: %+v", got)
	}
}

func commandExportFixture(t *testing.T) []byte {
	t.Helper()
	content := make(map[string][]byte)
	content[portableexport.ReadmeName] = []byte("portable export\n")
	for _, name := range portableexport.DataFiles() {
		content[name] = nil
	}
	content["members.jsonl"] = []byte("{\"id\":\"01K3V000000000000000000001\",\"username\":\"alice\",\"display_name\":\"Alice\",\"role\":\"admin\",\"must_change_password\":false,\"status\":\"active\",\"created_at\":\"2026-08-29T01:02:03Z\"}\n")
	content["agents.jsonl"] = []byte("{\"id\":\"01K3V000000000000000000002\",\"user_id\":\"01K3V000000000000000000001\",\"slug\":\"codex\",\"display_name\":\"Codex\",\"created_at\":\"2026-08-29T01:02:03Z\"}\n")
	content["spaces.jsonl"] = []byte("{\"id\":\"01K3V000000000000000000003\",\"type\":\"team\",\"name\":\"Team\",\"owner_user_id\":null,\"created_at\":\"2026-08-29T01:02:03Z\"}\n{\"id\":\"01K3V000000000000000000004\",\"type\":\"personal\",\"name\":\"Alice\",\"owner_user_id\":\"01K3V000000000000000000001\",\"created_at\":\"2026-08-29T01:02:03Z\"}\n")
	manifest := portableexport.Manifest{
		Format: portableexport.FormatName, FormatVersion: portableexport.FormatVersion,
		SchemaVersion: "0010", TenantID: "01K3V000000000000000000000", TenantName: "Team",
		TenantCreatedAt: time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC),
		GeneratedAt:     time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC), Complete: true,
		Excluded: portableexport.ExcludedData(),
	}
	for _, name := range portableexport.ContentFiles() {
		body := content[name]
		sum := sha256.Sum256(body)
		records := int64(0)
		if name == "members.jsonl" || name == "agents.jsonl" {
			records = 1
		}
		if name == "spaces.jsonl" {
			records = 2
		}
		manifest.Files = append(manifest.Files, portableexport.FileReceipt{Name: name, Records: records, SHA256: hex.EncodeToString(sum[:])})
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, name := range portableexport.ContentFiles() {
		writer, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content[name]); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := zw.Create(portableexport.ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(writer).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
