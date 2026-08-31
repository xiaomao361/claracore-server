package test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/adapters/backupstatus"
	"claracore-server/internal/adapters/restorestatus"
	"claracore-server/internal/buildprovenance"
	"claracore-server/internal/buildrepro"
)

func TestBackupMySQLPublishesAtomicPrivateBundle(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dumpArgs := filepath.Join(tmp, "dump-args")
	writeExecutable(t, filepath.Join(binDir, "mysqldump"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "${FAKE_DUMP_ARGS}"
[[ "$1" == --defaults-extra-file=* ]]
if [[ "${FAKE_DUMP_FAIL:-0}" == "1" ]]; then exit 7; fi
cat <<'SQL'
-- MySQL dump for ClaraCore
CREATE TABLE schema_migrations (version varchar(16), checksum char(64));
INSERT INTO schema_migrations VALUES ('0001', 'abc');
CREATE TABLE memories (id char(26), body text);
INSERT INTO memories VALUES ('01TEST', '代表性记忆');
SQL
`)
	defaults := filepath.Join(tmp, "client.cnf")
	secret := "super-secret-backup-password"
	if err := os.WriteFile(defaults, []byte("[client]\nuser=backup\npassword="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(tmp, "backups")
	output, err := runScript(root, "backup-mysql.sh", []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"FAKE_DUMP_ARGS=" + dumpArgs,
	}, "--defaults-file", defaults, "--database", "claracore_server", "--output-dir", outputDir)
	if err != nil {
		t.Fatalf("backup script failed: %v\n%s", err, output)
	}

	archives, _ := filepath.Glob(filepath.Join(outputDir, "*.sql.gz"))
	if len(archives) != 1 {
		t.Fatalf("want one published archive, got %v", archives)
	}
	archive := archives[0]
	checksum := archive + ".sha256"
	metadata := archive + ".metadata"
	for _, path := range []string{archive, checksum, metadata} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("missing bundle file %s: %v", path, statErr)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("bundle file must be owner-only: %s mode=%o", path, info.Mode().Perm())
		}
	}
	if partials, _ := filepath.Glob(filepath.Join(outputDir, ".*.partial.*")); len(partials) != 0 {
		t.Fatalf("successful publication left partial files: %v", partials)
	}

	dump := readGzip(t, archive)
	if !strings.Contains(dump, "CREATE TABLE memories") || !strings.Contains(dump, "代表性记忆") {
		t.Fatalf("archive does not contain representative SQL: %s", dump)
	}
	archiveBytes, _ := os.ReadFile(archive)
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(archiveBytes))
	checksumBody, _ := os.ReadFile(checksum)
	if !strings.HasPrefix(string(checksumBody), wantSHA+"  "+filepath.Base(archive)) {
		t.Fatalf("checksum does not bind the published basename: %s", checksumBody)
	}
	metadataBody, _ := os.ReadFile(metadata)
	allOutput := output + string(checksumBody) + string(metadataBody)
	if strings.Contains(allOutput, secret) {
		t.Fatal("backup credentials leaked into output or companion files")
	}
	argsBody, _ := os.ReadFile(dumpArgs)
	args := string(argsBody)
	if !strings.HasPrefix(args, "--defaults-extra-file="+defaults+"\n") || strings.Contains(args, "--databases") {
		t.Fatalf("mysqldump must receive defaults first and no source-targeting --databases flag:\n%s", args)
	}
}

func TestBackupMySQLCleansPartialBundleOnDumpFailure(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "mysqldump"), "#!/usr/bin/env bash\nexit 9\n")
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(tmp, "backups")
	output, err := runScript(root, "backup-mysql.sh", []string{"PATH=" + binDir + ":" + os.Getenv("PATH")},
		"--defaults-file", defaults, "--database", "claracore_server", "--output-dir", outputDir)
	if err == nil {
		t.Fatalf("failed dump must fail the script: %s", output)
	}
	entries, readErr := os.ReadDir(outputDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed dump published or retained files: %v", entries)
	}
}

func TestBackupMySQLRejectsUnsafeCredentialFile(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := runScript(root, "backup-mysql.sh", nil,
		"--defaults-file", defaults, "--database", "claracore_server", "--output-dir", filepath.Join(tmp, "backups"))
	if err == nil || !strings.Contains(output, "group/other") {
		t.Fatalf("world-readable credentials must be rejected: err=%v output=%s", err, output)
	}
}

func TestBackupMySQLRejectsConcurrentOutputDirectory(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dumpMarker := filepath.Join(tmp, "dump-called")
	writeExecutable(t, filepath.Join(binDir, "mysqldump"), "#!/usr/bin/env bash\ntouch \"${FAKE_DUMP_MARKER}\"\n")
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(tmp, "backups")
	if err := os.MkdirAll(filepath.Join(outputDir, ".claracore-backup.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, runErr := runScript(root, "backup-mysql.sh", []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"), "FAKE_DUMP_MARKER=" + dumpMarker,
	}, "--defaults-file", defaults, "--database", "claracore_server", "--output-dir", outputDir)
	if runErr == nil || !strings.Contains(output, "已有备份任务或遗留锁") {
		t.Fatalf("overlapping backup must fail before dump: err=%v output=%s", runErr, output)
	}
	if _, err := os.Stat(dumpMarker); !os.IsNotExist(err) {
		t.Fatalf("mysqldump must not run while output lock exists: %v", err)
	}
}

func TestRestoreRehearsalVerifiesChecksumRestoresAndDropsIsolationDB(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mysqlLog := filepath.Join(tmp, "mysql.log")
	importBody := filepath.Join(tmp, "import.sql")
	writeFakeMySQL(t, filepath.Join(binDir, "mysql"))
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\nuser=restore\npassword=hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Add(-time.Hour).Format("20060102T150405Z")
	backup := filepath.Join(tmp, "claracore-server_claracore_server_"+stamp+".sql.gz")
	writeGzip(t, backup, "CREATE TABLE schema_migrations (version varchar(16));\nCREATE TABLE memories (id char(26));\n")
	writeChecksum(t, backup)
	report := filepath.Join(tmp, "snapshot.restore-report.tsv")
	statusFile := filepath.Join(tmp, "restore-status.json")

	output, err := runScript(root, "rehearse-mysql-restore.sh", []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"FAKE_MYSQL_LOG=" + mysqlLog,
		"FAKE_IMPORT_BODY=" + importBody,
	}, "--defaults-file", defaults, "--backup", backup, "--confirm-isolated-server", "--report", report,
		"--status-file", statusFile, "--max-age-seconds", "2592000")
	if err != nil {
		t.Fatalf("restore rehearsal failed: %v\n%s", err, output)
	}
	logBody, _ := os.ReadFile(mysqlLog)
	logText := string(logBody)
	for _, want := range []string{"CREATE DATABASE `ccs_restore_", "DROP DATABASE IF EXISTS `ccs_restore_", "CHECK TABLE `ccs_restore_"} {
		if !strings.Contains(logText, want) {
			t.Errorf("restore flow log missing %q:\n%s", want, logText)
		}
	}
	imported, _ := os.ReadFile(importBody)
	if !strings.Contains(string(imported), "CREATE TABLE memories") {
		t.Fatalf("backup was not streamed into the isolated database: %s", imported)
	}
	reportBody, readErr := os.ReadFile(report)
	if readErr != nil {
		t.Fatal(readErr)
	}
	reportText := string(reportBody)
	for _, want := range []string{"restore\ttable_count\t3", "table\tmemories\t2", "table\tschema_migrations\t8", "migration\t0008\t" + strings.Repeat("b", 64)} {
		if !strings.Contains(reportText, want) {
			t.Errorf("restore report missing %q:\n%s", want, reportText)
		}
	}
	if strings.Contains(logText, "hidden") {
		t.Fatal("restore credentials leaked into mysql arguments")
	}
	status, parseErr := restorestatus.NewFile(statusFile).Read()
	if parseErr != nil {
		t.Fatalf("application reader must accept restore rehearsal receipt: %v", parseErr)
	}
	if status.Archive != filepath.Base(backup) || status.Report != filepath.Base(report) || status.TableCount != 3 ||
		status.MigrationCount != 2 || status.LatestMigration != "0008" || status.MaxAge != 30*24*time.Hour ||
		!status.TechnicalRestoreVerified || status.SemanticInspection != "not_proven" {
		t.Fatalf("unexpected technical restore status: %+v", status)
	}
	reportHash := sha256.Sum256(reportBody)
	if status.ReportSHA256 != fmt.Sprintf("%x", reportHash) {
		t.Fatalf("restore receipt report hash = %s, want %x", status.ReportSHA256, reportHash)
	}
	statusInfo, statErr := os.Stat(statusFile)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if statusInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("restore status receipt must be owner-only, mode=%o", statusInfo.Mode().Perm())
	}
}

func TestRestoreRehearsalRejectsTamperBeforeCreatingDatabase(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeMySQL(t, filepath.Join(binDir, "mysql"))
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(tmp, "snapshot.sql.gz")
	writeGzip(t, backup, "SELECT 1;\n")
	writeChecksum(t, backup)
	f, err := os.OpenFile(backup, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("tampered")
	_ = f.Close()
	mysqlLog := filepath.Join(tmp, "mysql.log")

	output, runErr := runScript(root, "rehearse-mysql-restore.sh", []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"), "FAKE_MYSQL_LOG=" + mysqlLog,
	}, "--defaults-file", defaults, "--backup", backup, "--confirm-isolated-server")
	if runErr == nil {
		t.Fatalf("tampered archive must fail: %s", output)
	}
	if logBody, _ := os.ReadFile(mysqlLog); len(logBody) != 0 {
		t.Fatalf("checksum failure must happen before any database command: %s", logBody)
	}
}

func TestRestoreRehearsalRequiresExplicitIsolationConfirmation(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(tmp, "snapshot.sql.gz")
	writeGzip(t, backup, "SELECT 1;\n")
	writeChecksum(t, backup)

	output, runErr := runScript(root, "rehearse-mysql-restore.sh", nil,
		"--defaults-file", defaults, "--backup", backup)
	if runErr == nil || !strings.Contains(output, "--confirm-isolated-server") {
		t.Fatalf("create/drop rehearsal must require explicit isolation confirmation: err=%v output=%s", runErr, output)
	}
}

func TestRestoreRehearsalRefusesStatusReceiptWhileTargetIsKept(t *testing.T) {
	root := repoRoot(t)
	output, runErr := runScript(root, "rehearse-mysql-restore.sh", nil,
		"--defaults-file", "/missing/client.cnf", "--backup", "/missing/archive.sql.gz",
		"--confirm-isolated-server", "--keep-target", "--status-file", "/missing/status.json")
	if runErr == nil || !strings.Contains(output, "不能与 --keep-target 同时使用") {
		t.Fatalf("a retained inspection database must not publish cleanup-complete status: err=%v output=%s", runErr, output)
	}
}

func TestRestoreRehearsalRejectsSourceTargetingSQLBeforeCreatingDatabase(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeMySQL(t, filepath.Join(binDir, "mysql"))
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(tmp, "source-targeting.sql.gz")
	writeGzip(t, backup, "CREATE DATABASE `claracore_server`;\nUSE `claracore_server`;\nSELECT 1;\n")
	writeChecksum(t, backup)
	mysqlLog := filepath.Join(tmp, "mysql.log")

	output, runErr := runScript(root, "rehearse-mysql-restore.sh", []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"), "FAKE_MYSQL_LOG=" + mysqlLog,
	}, "--defaults-file", defaults, "--backup", backup, "--confirm-isolated-server")
	if runErr == nil || !strings.Contains(output, "USE/CREATE DATABASE/DROP DATABASE") {
		t.Fatalf("source-targeting SQL must be rejected: err=%v output=%s", runErr, output)
	}
	if logBody, _ := os.ReadFile(mysqlLog); len(logBody) != 0 {
		t.Fatalf("unsafe SQL must be rejected before any mysql command: %s", logBody)
	}
}

func TestRestoreRehearsalDropsIsolationDBWhenValidationFails(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeMySQL(t, filepath.Join(binDir, "mysql"))
	defaults := filepath.Join(tmp, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Add(-time.Hour).Format("20060102T150405Z")
	backup := filepath.Join(tmp, "claracore-server_claracore_server_"+stamp+".sql.gz")
	writeGzip(t, backup, "CREATE TABLE schema_migrations (version varchar(16));\n")
	writeChecksum(t, backup)
	mysqlLog := filepath.Join(tmp, "mysql.log")
	report := filepath.Join(tmp, "failed.restore-report.tsv")
	statusFile := filepath.Join(tmp, "restore-status.json")
	const previousStatus = "previous successful restore receipt\n"
	if err := os.WriteFile(statusFile, []byte(previousStatus), 0o600); err != nil {
		t.Fatal(err)
	}

	output, runErr := runScript(root, "rehearse-mysql-restore.sh", []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"FAKE_MYSQL_LOG=" + mysqlLog,
		"FAKE_IMPORT_BODY=" + filepath.Join(tmp, "import.sql"),
		"FAKE_CHECK_FAIL=1",
	}, "--defaults-file", defaults, "--backup", backup, "--confirm-isolated-server", "--report", report, "--status-file", statusFile)
	if runErr == nil || !strings.Contains(output, "CHECK TABLE 未通过") {
		t.Fatalf("failed table validation must fail the rehearsal: err=%v output=%s", runErr, output)
	}
	logBody, _ := os.ReadFile(mysqlLog)
	logText := string(logBody)
	if !strings.Contains(logText, "CREATE DATABASE `ccs_restore_") || !strings.Contains(logText, "DROP DATABASE IF EXISTS `ccs_restore_") {
		t.Fatalf("failed rehearsal must clean only its created isolation DB:\n%s", logText)
	}
	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Fatalf("failed rehearsal must not publish a report: %v", err)
	}
	statusBody, readErr := os.ReadFile(statusFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(statusBody) != previousStatus {
		t.Fatalf("failed rehearsal must preserve the last successful status receipt: %q", statusBody)
	}
}

func TestVerifyMySQLBackupAcceptsFreshCompleteBundle(t *testing.T) {
	root := repoRoot(t)
	backupDir := t.TempDir()
	archive := writeBackupBundle(t, backupDir, "claracore_server", time.Now().UTC().Add(-time.Minute))
	statusFile := filepath.Join(t.TempDir(), "backup-status.json")

	output, err := runScript(root, "verify-mysql-backup.sh", nil,
		"--backup-dir", backupDir, "--database", "claracore_server", "--max-age-seconds", "3600", "--status-file", statusFile)
	if err != nil {
		t.Fatalf("fresh backup verification failed: %v\n%s", err, output)
	}
	for _, want := range []string{`"ok":true`, `"archive":"` + filepath.Base(archive) + `"`, `"verified_at":"`, `"sha256":"verified"`, `"gzip":"verified"`, `"recoverability":"not_proven"`} {
		if !strings.Contains(output, want) {
			t.Errorf("verification output missing %q: %s", want, output)
		}
	}
	receipt, readErr := os.ReadFile(statusFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(receipt)) != strings.TrimSpace(output) {
		t.Fatalf("atomic status receipt must match stdout:\nfile=%s\nout=%s", receipt, output)
	}
	info, statErr := os.Stat(statusFile)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("status receipt must be owner-only, mode=%o", info.Mode().Perm())
	}
	status, parseErr := backupstatus.NewFile(statusFile).Read()
	if parseErr != nil {
		t.Fatalf("application reader must accept verifier receipt: %v", parseErr)
	}
	if status.Archive != filepath.Base(archive) || status.MaxAge != time.Hour || !status.IntegrityVerified || status.Recoverability != "not_proven" {
		t.Fatalf("unexpected application backup status: %+v", status)
	}
}

func TestVerifyMySQLBackupRejectsStaleBundle(t *testing.T) {
	root := repoRoot(t)
	backupDir := t.TempDir()
	writeBackupBundle(t, backupDir, "claracore_server", time.Now().UTC().Add(-2*time.Hour))

	output, err := runScript(root, "verify-mysql-backup.sh", nil,
		"--backup-dir", backupDir, "--database", "claracore_server", "--max-age-seconds", "3600")
	if err == nil || !strings.Contains(output, "已过期") {
		t.Fatalf("stale backup must fail freshness verification: err=%v output=%s", err, output)
	}
}

func TestVerifyMySQLBackupRejectsTamperAndMissingCompanion(t *testing.T) {
	root := repoRoot(t)
	t.Run("tampered archive", func(t *testing.T) {
		backupDir := t.TempDir()
		archive := writeBackupBundle(t, backupDir, "claracore_server", time.Now().UTC())
		statusFile := filepath.Join(t.TempDir(), "backup-status.json")
		const previousReceipt = "previous successful receipt\n"
		if err := os.WriteFile(statusFile, []byte(previousReceipt), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("tampered")
		_ = f.Close()
		output, runErr := runScript(root, "verify-mysql-backup.sh", nil,
			"--backup-dir", backupDir, "--database", "claracore_server", "--status-file", statusFile)
		if runErr == nil || strings.Contains(output, `"ok":true`) {
			t.Fatalf("tampered backup must fail: err=%v output=%s", runErr, output)
		}
		after, readErr := os.ReadFile(statusFile)
		if readErr != nil || string(after) != previousReceipt {
			t.Fatalf("failed verification must retain previous successful receipt: err=%v body=%q", readErr, after)
		}
	})

	t.Run("missing metadata", func(t *testing.T) {
		backupDir := t.TempDir()
		archive := writeBackupBundle(t, backupDir, "claracore_server", time.Now().UTC())
		if err := os.Remove(archive + ".metadata"); err != nil {
			t.Fatal(err)
		}
		output, runErr := runScript(root, "verify-mysql-backup.sh", nil,
			"--backup-dir", backupDir, "--database", "claracore_server")
		if runErr == nil || !strings.Contains(output, "companion") {
			t.Fatalf("missing metadata must fail: err=%v output=%s", runErr, output)
		}
	})
}

func TestVerifyMySQLBackupRejectsSymlinkArchive(t *testing.T) {
	root := repoRoot(t)
	backupDir := t.TempDir()
	stamp := time.Now().UTC().Format("20060102T150405Z")
	target := filepath.Join(t.TempDir(), "target.sql.gz")
	writeGzip(t, target, "SELECT 1;\n")
	archive := filepath.Join(backupDir, "claracore-server_claracore_server_"+stamp+".sql.gz")
	if err := os.Symlink(target, archive); err != nil {
		t.Fatal(err)
	}

	output, err := runScript(root, "verify-mysql-backup.sh", nil,
		"--backup-dir", backupDir, "--database", "claracore_server")
	if err == nil || !strings.Contains(output, "符号链接") {
		t.Fatalf("symlink archive must fail: err=%v output=%s", err, output)
	}
}

func TestVulnerabilityCheckRejectsUnsafeBinaryTargetBeforeScanning(t *testing.T) {
	root := repoRoot(t)
	target := filepath.Join(t.TempDir(), "server")
	if err := os.WriteFile(target, []byte("not-a-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "server-link")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"missing target":  {"binary"},
		"relative target": {"binary", "claracore-server"},
		"symlink target":  {"binary", symlink},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(root, "scripts", "check-vulnerabilities.sh"), args...)
			if body, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(body), "二进制漏洞扫描") {
				t.Fatalf("unsafe target must fail before scanning: err=%v output=%s", err, body)
			}
		})
	}
}

func TestLicenseCheckRejectsUnsafeBundleTargetsBeforeInstallingTool(t *testing.T) {
	root := repoRoot(t)
	stage := t.TempDir()
	symlink := filepath.Join(t.TempDir(), "stage-link")
	if err := os.Symlink(stage, symlink); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"invalid architecture": {"bundle", "386", stage},
		"relative directory":   {"bundle", "amd64", "release"},
		"symlink directory":    {"bundle", "amd64", symlink},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(root, "scripts", "check-licenses.sh"), args...)
			if body, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(body), "许可证") {
				t.Fatalf("unsafe license bundle target must fail before tool install: err=%v output=%s", err, body)
			}
		})
	}
}

func TestSBOMGeneratorRejectsUnsafeBundleTargetsBeforeInstallingTool(t *testing.T) {
	root := repoRoot(t)
	stage := t.TempDir()
	binary := filepath.Join(stage, "claracore-server")
	if err := os.WriteFile(binary, []byte("not-a-go-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkBinary := filepath.Join(stage, "server-link")
	if err := os.Symlink(binary, symlinkBinary); err != nil {
		t.Fatal(err)
	}
	symlinkStage := filepath.Join(t.TempDir(), "stage-link")
	if err := os.Symlink(stage, symlinkStage); err != nil {
		t.Fatal(err)
	}
	outsideBinary := filepath.Join(t.TempDir(), "claracore-server")
	if err := os.WriteFile(outsideBinary, []byte("not-a-go-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"invalid architecture": {"bundle", "386", binary, stage, "test"},
		"relative binary":      {"bundle", "amd64", "claracore-server", stage, "test"},
		"symlink binary":       {"bundle", "amd64", symlinkBinary, stage, "test"},
		"outside binary":       {"bundle", "amd64", outsideBinary, stage, "test"},
		"relative stage":       {"bundle", "amd64", binary, "release", "test"},
		"symlink stage":        {"bundle", "amd64", binary, symlinkStage, "test"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(root, "scripts", "generate-sbom.sh"), args...)
			if body, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(body), "SBOM") {
				t.Fatalf("unsafe SBOM target must fail before tool install: err=%v output=%s", err, body)
			}
		})
	}
}

func TestReleasePackageIsSelfVerifyingAndIncludesRecoveryToolchain(t *testing.T) {
	root := repoRoot(t)
	outputDir := t.TempDir()
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Fatal("release signing gate requires openssl")
	}
	keyDir := t.TempDir()
	privateKey := filepath.Join(keyDir, "release-private.pem")
	publicKey := filepath.Join(keyDir, "release-public.pem")
	if body, keyErr := exec.Command(openssl, "genpkey", "-algorithm", "ED25519", "-out", privateKey).CombinedOutput(); keyErr != nil {
		t.Fatalf("generate Ed25519 key: %v\n%s", keyErr, body)
	}
	if err := os.Chmod(privateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if body, keyErr := exec.Command(openssl, "pkey", "-in", privateKey, "-pubout", "-out", publicKey).CombinedOutput(); keyErr != nil {
		t.Fatalf("derive Ed25519 public key: %v\n%s", keyErr, body)
	}

	insecureKey := filepath.Join(keyDir, "insecure-private.pem")
	privateBody, err := os.ReadFile(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(insecureKey, privateBody, 0o644); err != nil {
		t.Fatal(err)
	}
	insecureCmd := exec.Command(filepath.Join(root, "scripts", "package-release.sh"), "must-reject-insecure-key", "amd64", outputDir, insecureKey)
	if insecureBody, insecureErr := insecureCmd.CombinedOutput(); insecureErr == nil || !strings.Contains(string(insecureBody), "不能授予 group/other 权限") {
		t.Fatalf("package release must reject group-readable private keys: err=%v\n%s", insecureErr, insecureBody)
	}

	cmd := exec.Command(filepath.Join(root, "scripts", "package-release.sh"), "test-recovery-toolchain", "amd64", outputDir, privateKey)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("package release failed: %v\n%s", err, body)
	}
	archive := filepath.Join(outputDir, "claracore-server_test-recovery-toolchain_linux_amd64.tar.gz")
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	want := map[string]bool{
		"claracore-server":             false,
		"BUILDINFO.txt":                false,
		"VULNERABILITY_SCAN.txt":       false,
		"THIRD_PARTY_LICENSES.csv":     false,
		"THIRD_PARTY_REVIEW.txt":       false,
		"SBOM.cdx.json":                false,
		"SBOM_REVIEW.txt":              false,
		"PROMETHEUS_RULES_CHECK.txt":   false,
		"BUILD_REPRODUCIBILITY.json":   false,
		"BUILD_PROVENANCE.intoto.json": false,
		"BUILD_INPUTS.sha256":          false,
		"SOURCE_MANIFEST.sha256":       false,
		"third_party_licenses/github.com/go-sql-driver/mysql/LICENSE":   false,
		"third_party_licenses/github.com/go-sql-driver/mysql/driver.go": false,
		"verify-release.sh":                         false,
		"verify-release-signature.sh":               false,
		"verify-cluster.sh":                         false,
		"MANIFEST.sha256":                           false,
		"LICENSE":                                   false,
		"README.md":                                 false,
		"backup-mysql.sh":                           false,
		"verify-mysql-backup.sh":                    false,
		"rehearse-mysql-restore.sh":                 false,
		"mysql-backup-common.sh":                    false,
		"deploy/claracore-server-backup.service":    false,
		"deploy/claracore-server-backup.timer":      false,
		"deploy/claracore-server-integrity.service": false,
		"deploy/claracore-server-integrity.timer":   false,
		"deploy/mysql-client.cnf.example":           false,
		"deploy/claracore-server.rules.yml":         false,
		"deploy/claracore-server.rules.test.yml":    false,
		"deploy/prometheus.yml.example":             false,
		"DEPLOYMENT.md":                             false,
		"PROVENANCE.md":                             false,
	}
	contents := make(map[string]string)
	tr := tar.NewReader(zr)
	for {
		header, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		for suffix := range want {
			if strings.HasSuffix(header.Name, "/"+suffix) {
				want[suffix] = true
				if suffix == "BUILDINFO.txt" || suffix == "VULNERABILITY_SCAN.txt" || suffix == "THIRD_PARTY_LICENSES.csv" || suffix == "THIRD_PARTY_REVIEW.txt" || suffix == "SBOM.cdx.json" || suffix == "SBOM_REVIEW.txt" || suffix == "PROMETHEUS_RULES_CHECK.txt" || suffix == "BUILD_REPRODUCIBILITY.json" || suffix == "BUILD_PROVENANCE.intoto.json" || suffix == "BUILD_INPUTS.sha256" || suffix == "SOURCE_MANIFEST.sha256" || suffix == "deploy/claracore-server.rules.yml" || suffix == "deploy/claracore-server.rules.test.yml" {
					body, readErr := io.ReadAll(tr)
					if readErr != nil {
						t.Fatal(readErr)
					}
					contents[suffix] = string(body)
				}
				if strings.HasSuffix(suffix, ".sh") && header.Mode&0o111 == 0 {
					t.Errorf("packaged operational script is not executable: %s mode=%o", header.Name, header.Mode)
				}
			}
		}
	}
	if scan := contents["VULNERABILITY_SCAN.txt"]; !strings.Contains(scan, "scanner=govulncheck@v1.7.0 mode=binary") || !strings.Contains(scan, "No vulnerabilities found") {
		t.Fatalf("release vulnerability receipt is incomplete: %s", scan)
	}
	if build := contents["BUILDINFO.txt"]; !strings.Contains(build, "go1.26.6") || !strings.Contains(build, "github.com/modelcontextprotocol/go-sdk") {
		t.Fatalf("release build information does not identify toolchain and modules: %s", build)
	}
	if licenses := contents["THIRD_PARTY_LICENSES.csv"]; !strings.Contains(licenses, "github.com/go-sql-driver/mysql") || !strings.Contains(licenses, "MPL-2.0") || strings.Contains(licenses, "Unknown") {
		t.Fatalf("release third-party license report is incomplete: %s", licenses)
	}
	if review := contents["THIRD_PARTY_REVIEW.txt"]; !strings.Contains(review, "tool=go-licenses@v2.0.1") || !strings.Contains(review, "target=linux/amd64") || !strings.Contains(review, "non_go_review=assembly_only") || strings.Contains(review, "/Users/") {
		t.Fatalf("release third-party review receipt is incomplete: %s", review)
	}
	if review := contents["SBOM_REVIEW.txt"]; !strings.Contains(review, "tool=cyclonedx-gomod@v1.10.0") || !strings.Contains(review, "format=CycloneDX@1.6") || !strings.Contains(review, "target=linux/amd64") || !strings.Contains(review, "result=verified") || !strings.Contains(review, "standard_library=go1.26.6") || strings.Contains(review, "/Users/") || strings.Contains(review, "/tmp/") {
		t.Fatalf("release SBOM review receipt is incomplete: %s", review)
	}
	rulesReceipt := contents["PROMETHEUS_RULES_CHECK.txt"]
	wantRulesSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(contents["deploy/claracore-server.rules.yml"])))
	wantRuleTestsSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(contents["deploy/claracore-server.rules.test.yml"])))
	for _, want := range []string{
		"tool=promtool@3.14.0",
		"source=github.com/prometheus/prometheus@v0.314.0",
		"revision=d7598b7141418fa35be2b5ec5d0fefb634199610",
		"go=go1.26.6",
		"sha256=" + wantRulesSHA,
		"alerts=22",
		"tests=claracore-server.rules.test.yml sha256=" + wantRuleTestsSHA,
		"unit_tests=verified",
		"result=verified",
	} {
		if !strings.Contains(rulesReceipt, want) {
			t.Fatalf("release Prometheus rule receipt misses %q: %s", want, rulesReceipt)
		}
	}
	if strings.Contains(rulesReceipt, "/Users/") || strings.Contains(rulesReceipt, "/tmp/") {
		t.Fatal("release Prometheus rule receipt leaks build-machine paths")
	}
	wantSourceDigest := releaseSourceDigest(t, root)
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(contents["SOURCE_MANIFEST.sha256"]))); got != wantSourceDigest {
		t.Fatalf("release source manifest digest = %s, want linked digest %s", got, wantSourceDigest)
	}
	buildInputs := contents["BUILD_INPUTS.sha256"]
	for _, want := range []string{"scripts/package-release.sh", "scripts/release-provenance/main.go", "scripts/release-reproducibility/main.go", "scripts/release-archive/main.go", "docs/09-release-provenance.md"} {
		if !strings.Contains(buildInputs, "  "+want+"\n") {
			t.Fatalf("release build inputs miss %s: %s", want, buildInputs)
		}
	}
	if strings.Contains(buildInputs, "/Users/") || strings.Contains(buildInputs, "/tmp/") {
		t.Fatal("release build inputs leak build-machine paths")
	}
	var reproducibility buildrepro.Receipt
	reproducibilityBody := contents[buildrepro.FileName]
	if err := json.Unmarshal([]byte(reproducibilityBody), &reproducibility); err != nil {
		t.Fatalf("release reproducibility receipt is not valid JSON: %v", err)
	}
	if reproducibility.Schema != buildrepro.Schema || reproducibility.Scope != buildrepro.Scope || reproducibility.Result != buildrepro.ResultMatched || reproducibility.Identity.Version != "test-recovery-toolchain" || reproducibility.Identity.SourceDigest != wantSourceDigest || reproducibility.Identity.TargetOS != "linux" || reproducibility.Identity.TargetArch != "amd64" || reproducibility.Identity.GoToolchain != "go1.26.6" {
		t.Fatalf("release reproducibility identity is wrong: %+v", reproducibility)
	}
	if !reproducibility.Settings.Trimpath || reproducibility.Settings.CGOEnabled || reproducibility.Settings.BuildVCS || reproducibility.Primary.Digest["sha256"] == "" || reproducibility.Primary.Digest["sha256"] != reproducibility.Rebuild.Digest["sha256"] {
		t.Fatalf("release reproducibility result is incomplete: %+v", reproducibility)
	}
	if strings.Contains(reproducibilityBody, "/Users/") || strings.Contains(reproducibilityBody, "/tmp/") || strings.Contains(reproducibilityBody, "startedAt") || strings.Contains(reproducibilityBody, "finishedAt") {
		t.Fatal("release reproducibility receipt is nondeterministic or leaks build-machine paths")
	}
	var provenance buildprovenance.Statement
	provenanceBody := contents["BUILD_PROVENANCE.intoto.json"]
	if err := json.Unmarshal([]byte(provenanceBody), &provenance); err != nil {
		t.Fatalf("release provenance is not valid JSON: %v", err)
	}
	if provenance.Type != buildprovenance.StatementType || provenance.PredicateType != buildprovenance.PredicateType || provenance.Predicate.BuildDefinition.BuildType != buildprovenance.BuildType || provenance.Predicate.RunDetails.Builder.ID != buildprovenance.BuilderID {
		t.Fatalf("release provenance identity is wrong: %+v", provenance)
	}
	if len(provenance.Subject) != 1 || provenance.Subject[0].Name != "claracore-server" || provenance.Predicate.BuildDefinition.ExternalParameters.Version != "test-recovery-toolchain" || provenance.Predicate.BuildDefinition.ExternalParameters.TargetOS != "linux" || provenance.Predicate.BuildDefinition.ExternalParameters.TargetArch != "amd64" || provenance.Predicate.BuildDefinition.InternalParameters.SourceDigest != wantSourceDigest {
		t.Fatalf("release provenance subject or parameters are wrong: %+v", provenance)
	}
	if len(provenance.Predicate.BuildDefinition.ResolvedDependencies) != 2 || len(provenance.Predicate.RunDetails.Byproducts) != 8 || provenance.Predicate.RunDetails.Builder.Version["releaseBuilder"] != "2" {
		t.Fatalf("release provenance evidence set is incomplete: %+v", provenance.Predicate)
	}
	if strings.Contains(provenanceBody, "startedOn") || strings.Contains(provenanceBody, "finishedOn") || strings.Contains(provenanceBody, "invocationId") || strings.Contains(provenanceBody, "/Users/") || strings.Contains(provenanceBody, "/tmp/") {
		t.Fatal("release provenance is nondeterministic or leaks build-machine paths")
	}
	var sbom struct {
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
		Metadata    struct {
			Tools []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"tools"`
			Component struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				PURL    string `json:"purl"`
			} `json:"component"`
			Properties []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"properties"`
		} `json:"metadata"`
		Components []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			PURL    string `json:"purl"`
		} `json:"components"`
	}
	sbomBody := contents["SBOM.cdx.json"]
	if err := json.Unmarshal([]byte(sbomBody), &sbom); err != nil {
		t.Fatalf("release SBOM is not valid JSON: %v", err)
	}
	if sbom.BOMFormat != "CycloneDX" || sbom.SpecVersion != "1.6" || len(sbom.Metadata.Tools) != 1 || sbom.Metadata.Tools[0].Name != "cyclonedx-gomod" || sbom.Metadata.Tools[0].Version != "v1.10.0" {
		t.Fatalf("release SBOM format or tool identity is wrong: format=%s version=%s tools=%+v", sbom.BOMFormat, sbom.SpecVersion, sbom.Metadata.Tools)
	}
	if sbom.Metadata.Component.Name != "claracore-server" || sbom.Metadata.Component.Version != "test-recovery-toolchain" || !strings.Contains(sbom.Metadata.Component.PURL, "goarch=amd64&goos=linux") {
		t.Fatalf("release SBOM main component is wrong: %+v", sbom.Metadata.Component)
	}
	wantComponents := map[string]string{
		"github.com/go-sql-driver/mysql":         "v1.10.0",
		"github.com/modelcontextprotocol/go-sdk": "v1.7.0",
		"std":                                    "go1.26.6",
	}
	for _, component := range sbom.Components {
		if wantVersion, exists := wantComponents[component.Name]; exists {
			if component.Version != wantVersion || !strings.Contains(component.PURL, "goarch=amd64&goos=linux") {
				t.Fatalf("release SBOM component is wrong: %+v", component)
			}
			delete(wantComponents, component.Name)
		}
	}
	if len(wantComponents) != 0 {
		t.Fatalf("release SBOM misses required runtime components: %v", wantComponents)
	}
	if strings.Contains(sbomBody, "/Users/") || strings.Contains(sbomBody, "/tmp/") {
		t.Fatal("release SBOM leaks build-machine absolute paths")
	}
	sbomBinarySHA := ""
	for _, property := range sbom.Metadata.Properties {
		if property.Name == "cdx:gomod:binary:hash:SHA-256" {
			sbomBinarySHA = property.Value
		}
	}
	if sbomBinarySHA == "" {
		t.Fatal("release SBOM does not bind the binary SHA-256")
	}
	for name, found := range want {
		if !found {
			t.Errorf("release archive does not include %s", name)
		}
	}

	archiveBody, readErr := os.ReadFile(archive)
	if readErr != nil {
		t.Fatal(readErr)
	}
	outerChecksum, readErr := os.ReadFile(archive + ".sha256")
	if readErr != nil {
		t.Fatal(readErr)
	}
	wantArchiveHash := fmt.Sprintf("%x  %s", sha256.Sum256(archiveBody), filepath.Base(archive))
	if !strings.HasPrefix(string(outerChecksum), wantArchiveHash) {
		t.Fatalf("outer archive checksum does not bind the release artifact: %s", outerChecksum)
	}
	signature := archive + ".sha256.sig"
	signatureBody, readErr := os.ReadFile(signature)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(signatureBody) != 64 {
		t.Fatalf("Ed25519 signature must be 64 bytes, got %d", len(signatureBody))
	}
	unsignedOverwrite := exec.Command(filepath.Join(root, "scripts", "package-release.sh"), "test-recovery-toolchain", "amd64", outputDir)
	if overwriteBody, overwriteErr := unsignedOverwrite.CombinedOutput(); overwriteErr == nil || !strings.Contains(string(overwriteBody), "拒绝用未签名构建覆盖归档") {
		t.Fatalf("unsigned rebuild must not leave a stale trusted signature beside replaced artifacts: err=%v\n%s", overwriteErr, overwriteBody)
	}
	verifySignature := filepath.Join(root, "scripts", "verify-release-signature.sh")
	runSignatureVerifier := func(key string) (string, error) {
		verifyCmd := exec.Command(verifySignature, archive, archive+".sha256", signature, key)
		out, verifyErr := verifyCmd.CombinedOutput()
		return string(out), verifyErr
	}
	if verifyBody, verifyErr := runSignatureVerifier(publicKey); verifyErr != nil || !strings.Contains(verifyBody, "发布者签名与归档 SHA-256 验证通过") {
		t.Fatalf("fresh signed release must pass publisher verification: %v\n%s", verifyErr, verifyBody)
	}

	wrongPrivateKey := filepath.Join(keyDir, "wrong-private.pem")
	wrongPublicKey := filepath.Join(keyDir, "wrong-public.pem")
	if out, keyErr := exec.Command(openssl, "genpkey", "-algorithm", "ED25519", "-out", wrongPrivateKey).CombinedOutput(); keyErr != nil {
		t.Fatalf("generate wrong key: %v\n%s", keyErr, out)
	}
	if out, keyErr := exec.Command(openssl, "pkey", "-in", wrongPrivateKey, "-pubout", "-out", wrongPublicKey).CombinedOutput(); keyErr != nil {
		t.Fatalf("derive wrong public key: %v\n%s", keyErr, out)
	}
	if verifyBody, verifyErr := runSignatureVerifier(wrongPublicKey); verifyErr == nil || !strings.Contains(verifyBody, "Ed25519 签名无效") {
		t.Fatalf("wrong publisher key must fail: err=%v\n%s", verifyErr, verifyBody)
	}

	if err := os.WriteFile(archive+".sha256", append(outerChecksum, []byte("tampered\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if verifyBody, verifyErr := runSignatureVerifier(publicKey); verifyErr == nil {
		t.Fatalf("tampered signed checksum must fail: %s", verifyBody)
	}
	if err := os.WriteFile(archive+".sha256", outerChecksum, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(archive, append(archiveBody, []byte("tampered")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if verifyBody, verifyErr := runSignatureVerifier(publicKey); verifyErr == nil || !strings.Contains(verifyBody, "归档 SHA-256") {
		t.Fatalf("tampered archive must fail the authenticated checksum: err=%v\n%s", verifyErr, verifyBody)
	}
	if err := os.WriteFile(archive, archiveBody, 0o644); err != nil {
		t.Fatal(err)
	}

	tamperedSignature := append([]byte(nil), signatureBody...)
	tamperedSignature[0] ^= 0xff
	if err := os.WriteFile(signature, tamperedSignature, 0o644); err != nil {
		t.Fatal(err)
	}
	if verifyBody, verifyErr := runSignatureVerifier(publicKey); verifyErr == nil || !strings.Contains(verifyBody, "Ed25519 签名无效") {
		t.Fatalf("tampered signature must fail: err=%v\n%s", verifyErr, verifyBody)
	}
	if err := os.WriteFile(signature, signatureBody, 0o644); err != nil {
		t.Fatal(err)
	}

	extractRoot := t.TempDir()
	extractTarGz(t, archive, extractRoot)
	releaseDir := filepath.Join(extractRoot, "claracore-server_test-recovery-toolchain_linux_amd64")
	verify := filepath.Join(releaseDir, "verify-release.sh")
	verifyCmd := exec.Command(verify)
	verifyCmd.Dir = releaseDir
	if verifyBody, verifyErr := verifyCmd.CombinedOutput(); verifyErr != nil {
		t.Fatalf("fresh release must pass its inner manifest verifier: %v\n%s", verifyErr, verifyBody)
	}
	rebuiltArchive := renderReleaseArchive(t, root, releaseDir)
	if !bytes.Equal(rebuiltArchive, archiveBody) {
		t.Fatalf("deterministic archive writer did not reproduce the packaged archive: got sha256=%x want sha256=%x",
			sha256.Sum256(rebuiltArchive), sha256.Sum256(archiveBody))
	}
	differentTime := time.Date(2040, 7, 6, 5, 4, 3, 0, time.FixedZone("different", 8*60*60))
	if err := filepath.WalkDir(releaseDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, differentTime, differentTime)
	}); err != nil {
		t.Fatal(err)
	}
	if rebuiltAfterMtimeChange := renderReleaseArchive(t, root, releaseDir); !bytes.Equal(rebuiltAfterMtimeChange, archiveBody) {
		t.Fatalf("release archive bytes changed when only extracted mtimes changed: got sha256=%x want sha256=%x",
			sha256.Sum256(rebuiltAfterMtimeChange), sha256.Sum256(archiveBody))
	}

	binaryBody, readErr := os.ReadFile(filepath.Join(releaseDir, "claracore-server"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Contains(binaryBody, []byte("test-recovery-toolchain")) {
		t.Fatal("packaged binary does not contain the requested linked release version")
	}
	if !bytes.Contains(binaryBody, []byte("verify-export")) {
		t.Fatal("packaged binary does not contain the offline team export verifier")
	}
	if !bytes.Contains(binaryBody, []byte("plan-import")) {
		t.Fatal("packaged binary does not contain the offline team import planner")
	}
	if !bytes.Contains(binaryBody, []byte("import-export")) {
		t.Fatal("packaged binary does not contain the empty-deployment team importer")
	}
	if !bytes.Contains(binaryBody, []byte("check-import")) {
		t.Fatal("packaged binary does not contain the read-only team import receipt checker")
	}
	if !bytes.Contains(binaryBody, []byte("check-integrity")) {
		t.Fatal("packaged binary does not contain the read-only data integrity checker")
	}
	if !bytes.Contains(binaryBody, []byte("verify-provenance")) {
		t.Fatal("packaged binary does not contain the offline provenance verifier")
	}
	binarySHA := fmt.Sprintf("%x", sha256.Sum256(binaryBody))
	if reproducibility.Primary.Digest["sha256"] != binarySHA || reproducibility.Rebuild.Digest["sha256"] != binarySHA {
		t.Fatalf("release reproducibility SHA-256 does not bind packaged binary: primary=%s rebuilt=%s want=%s",
			reproducibility.Primary.Digest["sha256"], reproducibility.Rebuild.Digest["sha256"], binarySHA)
	}
	if provenance.Subject[0].Digest["sha256"] != binarySHA {
		t.Fatalf("release provenance subject SHA-256 = %s, want %s", provenance.Subject[0].Digest["sha256"], binarySHA)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(binaryBody)); sbomBinarySHA != got {
		t.Fatalf("release SBOM binary SHA-256 = %s, want %s", sbomBinarySHA, got)
	}
	validateProvenance := exec.Command("go", "run", "./scripts/release-provenance",
		"-mode", "validate", "-stage", releaseDir,
		"-binary", filepath.Join(releaseDir, "claracore-server"),
		"-version", "test-recovery-toolchain", "-source-digest", wantSourceDigest, "-arch", "amd64")
	validateProvenance.Dir = root
	if body, validateErr := validateProvenance.CombinedOutput(); validateErr != nil || !strings.Contains(string(body), `"status":"verified"`) {
		t.Fatalf("independent provenance validator rejected fresh package: err=%v output=%s", validateErr, body)
	}
	validateReproducibility := exec.Command("go", "run", "./scripts/release-reproducibility",
		"-mode", "validate", "-stage", releaseDir,
		"-primary", filepath.Join(releaseDir, "claracore-server"),
		"-version", "test-recovery-toolchain", "-source-digest", wantSourceDigest, "-arch", "amd64")
	validateReproducibility.Dir = root
	if body, validateErr := validateReproducibility.CombinedOutput(); validateErr != nil || !strings.Contains(string(body), `"status":"verified"`) {
		t.Fatalf("independent reproducibility validator rejected fresh package: err=%v output=%s", validateErr, body)
	}
	tamperedReproducibilityPath := filepath.Join(releaseDir, "tampered-reproducibility.json")
	tamperedReproducibility := strings.Replace(reproducibilityBody, binarySHA, strings.Repeat("0", 64), 1)
	if err := os.WriteFile(tamperedReproducibilityPath, []byte(tamperedReproducibility), 0o600); err != nil {
		t.Fatal(err)
	}
	validateTamperedReproducibility := exec.Command("go", "run", "./scripts/release-reproducibility",
		"-mode", "validate", "-stage", releaseDir,
		"-primary", filepath.Join(releaseDir, "claracore-server"), "-receipt", tamperedReproducibilityPath,
		"-version", "test-recovery-toolchain", "-source-digest", wantSourceDigest, "-arch", "amd64")
	validateTamperedReproducibility.Dir = root
	if body, validateErr := validateTamperedReproducibility.CombinedOutput(); validateErr == nil || !strings.Contains(string(body), "does not match") {
		t.Fatalf("tampered reproducibility receipt must fail: err=%v output=%s", validateErr, body)
	}
	if err := os.Remove(tamperedReproducibilityPath); err != nil {
		t.Fatal(err)
	}
	tamperedProvenancePath := filepath.Join(releaseDir, "tampered-provenance.json")
	tamperedProvenance := strings.Replace(provenanceBody, binarySHA, strings.Repeat("0", 64), 1)
	if err := os.WriteFile(tamperedProvenancePath, []byte(tamperedProvenance), 0o600); err != nil {
		t.Fatal(err)
	}
	validateTamperedProvenance := exec.Command("go", "run", "./scripts/release-provenance",
		"-mode", "validate", "-stage", releaseDir,
		"-binary", filepath.Join(releaseDir, "claracore-server"), "-provenance", tamperedProvenancePath,
		"-version", "test-recovery-toolchain", "-source-digest", wantSourceDigest, "-arch", "amd64")
	validateTamperedProvenance.Dir = root
	if body, validateErr := validateTamperedProvenance.CombinedOutput(); validateErr == nil || !strings.Contains(string(body), "do not match") {
		t.Fatalf("tampered provenance subject must fail: err=%v output=%s", validateErr, body)
	}
	if err := os.Remove(tamperedProvenancePath); err != nil {
		t.Fatal(err)
	}
	validateSBOM := exec.Command("go", "run", "./scripts/validate-sbom.go",
		"-sbom", filepath.Join(releaseDir, "SBOM.cdx.json"),
		"-binary", filepath.Join(releaseDir, "claracore-server"),
		"-version", "test-recovery-toolchain", "-arch", "amd64")
	validateSBOM.Dir = root
	if body, validateErr := validateSBOM.CombinedOutput(); validateErr != nil || !strings.Contains(string(body), "result=verified") {
		t.Fatalf("independent SBOM validator rejected fresh package: err=%v output=%s", validateErr, body)
	}
	tamperedSBOM := strings.ReplaceAll(sbomBody, "goarch=amd64", "goarch=arm64")
	tamperedSBOMPath := filepath.Join(t.TempDir(), "SBOM.cdx.json")
	if err := os.WriteFile(tamperedSBOMPath, []byte(tamperedSBOM), 0o600); err != nil {
		t.Fatal(err)
	}
	validateTampered := exec.Command("go", "run", "./scripts/validate-sbom.go",
		"-sbom", tamperedSBOMPath,
		"-binary", filepath.Join(releaseDir, "claracore-server"),
		"-version", "test-recovery-toolchain", "-arch", "amd64")
	validateTampered.Dir = root
	if body, validateErr := validateTampered.CombinedOutput(); validateErr == nil || !strings.Contains(string(body), "目标为 linux/arm64") {
		t.Fatalf("SBOM target tamper must fail: err=%v output=%s", validateErr, body)
	}
	if !bytes.Contains(binaryBody, []byte(wantSourceDigest)) {
		t.Fatalf("packaged binary does not contain exact source digest %s", wantSourceDigest)
	}

	readme := filepath.Join(releaseDir, "README.md")
	originalReadme, readErr := os.ReadFile(readme)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := os.WriteFile(readme, append(originalReadme, []byte("\ntampered\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if verifyBody, verifyErr := exec.Command(verify).CombinedOutput(); verifyErr == nil {
		t.Fatalf("inner manifest must reject modified release files:\n%s", verifyBody)
	}
	if err := os.WriteFile(readme, originalReadme, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "unexpected.txt"), []byte("not packaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if verifyBody, verifyErr := exec.Command(verify).CombinedOutput(); verifyErr == nil {
		t.Fatalf("inner manifest must reject extra release files:\n%s", verifyBody)
	}
}

func renderReleaseArchive(t *testing.T, root, source string) []byte {
	t.Helper()
	cmd := exec.Command("go", "run", "./scripts/release-archive", "-source", source)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=go1.26.6")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("render deterministic release archive: %v\n%s", err, stderr.String())
	}
	return stdout.Bytes()
}

func releaseSourceDigest(t *testing.T, root string) string {
	t.Helper()
	paths := []string{"go.mod", "go.sum"}
	wantedExtension := map[string]bool{
		".go": true, ".sql": true, ".html": true, ".css": true, ".js": true,
	}
	for _, dir := range []string{"cmd", "internal", "migrations"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() && wantedExtension[filepath.Ext(entry.Name())] {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				paths = append(paths, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(paths)
	combined := sha256.New()
	for _, rel := range paths {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		fileHash := sha256.Sum256(body)
		fmt.Fprintf(combined, "%x  %s\n", fileHash, rel)
	}
	return fmt.Sprintf("%x", combined.Sum(nil))
}

func extractTarGz(t *testing.T, archive, destination string) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	root := filepath.Clean(destination) + string(os.PathSeparator)
	for {
		header, nextErr := tr.Next()
		if nextErr == io.EOF {
			return
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		clean := filepath.Clean(header.Name)
		target := filepath.Join(destination, clean)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || !strings.HasPrefix(target, root) {
			t.Fatalf("release archive contains unsafe path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o755); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("release archive must not contain links or special files: %q type=%d", header.Name, header.Typeflag)
		}
	}
}

func runScript(root, name string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command(filepath.Join(root, "scripts", name), args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	body, err := cmd.CombinedOutput()
	return string(body), err
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func readGzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func writeGzip(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeChecksum(t *testing.T, archive string) {
	t.Helper()
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("%x  %s\n", sha256.Sum256(body), filepath.Base(archive))
	if err := os.WriteFile(archive+".sha256", []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeBackupBundle(t *testing.T, directory, database string, createdAt time.Time) string {
	t.Helper()
	stamp := createdAt.UTC().Format("20060102T150405Z")
	archive := filepath.Join(directory, "claracore-server_"+database+"_"+stamp+".sql.gz")
	writeGzip(t, archive, "CREATE TABLE memories (id char(26));\n")
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	if err := os.WriteFile(archive+".sha256", []byte(hash+"  "+filepath.Base(archive)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf("format=claracore-mysql-backup-v1\ncreated_at=%s\ndatabase=%s\narchive=%s\nsha256=%s\nbytes=%d\n",
		stamp, database, filepath.Base(archive), hash, len(body))
	if err := os.WriteFile(archive+".metadata", []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive
}

func writeFakeMySQL(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/usr/bin/env bash
set -euo pipefail
printf 'CALL' >> "${FAKE_MYSQL_LOG}"
for arg in "$@"; do printf '\t%s' "$arg" >> "${FAKE_MYSQL_LOG}"; done
printf '\n' >> "${FAKE_MYSQL_LOG}"
sql=""
for ((i=1; i<=$#; i++)); do
  if [[ "${!i}" == "-e" ]]; then j=$((i+1)); sql="${!j}"; break; fi
done
if [[ -z "${sql}" ]]; then cat > "${FAKE_IMPORT_BODY}"; exit 0; fi
case "${sql}" in
  *"SCHEMA_NAME="*) echo 0 ;;
  *"CREATE DATABASE"*) ;;
  *"DROP DATABASE"*) ;;
  *"TABLE_NAME='schema_migrations'"*) echo 1 ;;
  *"COUNT(*) FROM information_schema.TABLES"*) echo 3 ;;
  *"SELECT TABLE_NAME FROM information_schema.TABLES"*) printf 'memories\nschema_migrations\ntenants\n' ;;
  *"CHECK TABLE"*)
    if [[ "${FAKE_CHECK_FAIL:-0}" == "1" ]]; then printf 'table\tcheck\terror\tcorrupt\n'; else printf 'table\tcheck\tstatus\tOK\n'; fi ;;
  *"SELECT COUNT(*) FROM"*"schema_migrations"*) echo 8 ;;
  *"SELECT COUNT(*) FROM"*"memories"*) echo 2 ;;
  *"SELECT COUNT(*) FROM"*"tenants"*) echo 1 ;;
  *"SELECT CONCAT("*)
    raw=0
    for arg in "$@"; do [[ "${arg}" == "--raw" ]] && raw=1; done
    if [[ "${raw}" == "1" ]]; then
      printf 'migration\t0001\t%s\nmigration\t0008\t%s\n' "$(printf 'a%.0s' {1..64})" "$(printf 'b%.0s' {1..64})"
    else
      printf 'migration\\t0001\\t%s\nmigration\\t0008\\t%s\n' "$(printf 'a%.0s' {1..64})" "$(printf 'b%.0s' {1..64})"
    fi ;;
  *) echo "unexpected SQL: ${sql}" >&2; exit 41 ;;
esac
`)
}
