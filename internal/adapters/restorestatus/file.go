package restorestatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"claracore-server/internal/ports"
)

const (
	maxReceiptBytes = 8 * 1024
	timestampLayout = "20060102T150405Z"
)

var (
	databaseName    = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	hexSHA256       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	migrationName   = regexp.MustCompile(`^[0-9]{4,16}$`)
	restoreReport   = regexp.MustCompile(`^[0-9A-Za-z._-]+\.restore-report\.tsv$`)
	archiveFilename = regexp.MustCompile(`^claracore-server_([A-Za-z0-9_]+)_([0-9]{8}T[0-9]{6}Z)\.sql\.gz$`)
)

type File struct {
	path string
}

func NewFile(path string) *File { return &File{path: path} }

type receipt struct {
	OK                 bool   `json:"ok"`
	Database           string `json:"database"`
	Archive            string `json:"archive"`
	ArchiveSHA256      string `json:"archive_sha256"`
	Report             string `json:"report"`
	ReportSHA256       string `json:"report_sha256"`
	RestoredAt         string `json:"restored_at"`
	MaxAgeSeconds      int64  `json:"max_age_seconds"`
	TableCount         int    `json:"table_count"`
	MigrationCount     int    `json:"migration_count"`
	LatestMigration    string `json:"latest_migration"`
	TableChecks        string `json:"table_checks"`
	RowCounts          string `json:"row_counts"`
	TargetCleanup      string `json:"target_cleanup"`
	SemanticInspection string `json:"semantic_inspection"`
}

func (f *File) Read() (ports.RestoreStatus, error) {
	info, err := os.Lstat(f.path)
	if err != nil {
		return ports.RestoreStatus{}, fmt.Errorf("read restore receipt: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ports.RestoreStatus{}, errors.New("restore receipt must be a regular non-symlink file")
	}
	if info.Size() <= 0 || info.Size() > maxReceiptBytes {
		return ports.RestoreStatus{}, fmt.Errorf("restore receipt size must be 1..%d bytes", maxReceiptBytes)
	}

	file, err := os.Open(f.path)
	if err != nil {
		return ports.RestoreStatus{}, fmt.Errorf("open restore receipt: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return ports.RestoreStatus{}, fmt.Errorf("stat opened restore receipt: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return ports.RestoreStatus{}, errors.New("restore receipt changed while opening")
	}
	if openedInfo.Size() <= 0 || openedInfo.Size() > maxReceiptBytes {
		return ports.RestoreStatus{}, fmt.Errorf("opened restore receipt size must be 1..%d bytes", maxReceiptBytes)
	}

	decoder := json.NewDecoder(io.LimitReader(file, maxReceiptBytes+1))
	decoder.DisallowUnknownFields()
	var raw receipt
	if err := decoder.Decode(&raw); err != nil {
		return ports.RestoreStatus{}, fmt.Errorf("decode restore receipt: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ports.RestoreStatus{}, errors.New("restore receipt must contain exactly one JSON object")
	}
	if !raw.OK || !databaseName.MatchString(raw.Database) {
		return ports.RestoreStatus{}, errors.New("restore receipt status or database is invalid")
	}
	matches := archiveFilename.FindStringSubmatch(raw.Archive)
	if filepath.Base(raw.Archive) != raw.Archive || len(matches) != 3 || matches[1] != raw.Database {
		return ports.RestoreStatus{}, errors.New("restore receipt archive name is invalid")
	}
	archiveCreatedAt, err := time.Parse(timestampLayout, matches[2])
	if err != nil {
		return ports.RestoreStatus{}, errors.New("restore receipt archive timestamp is invalid")
	}
	if !hexSHA256.MatchString(raw.ArchiveSHA256) || !hexSHA256.MatchString(raw.ReportSHA256) {
		return ports.RestoreStatus{}, errors.New("restore receipt SHA-256 value is invalid")
	}
	if filepath.Base(raw.Report) != raw.Report || !restoreReport.MatchString(raw.Report) {
		return ports.RestoreStatus{}, errors.New("restore receipt report name is invalid")
	}
	restoredAt, err := time.Parse(timestampLayout, raw.RestoredAt)
	if err != nil || restoredAt.Before(archiveCreatedAt.Add(-5*time.Minute)) {
		return ports.RestoreStatus{}, errors.New("restore receipt restored_at is invalid")
	}
	if raw.MaxAgeSeconds < 1 || raw.MaxAgeSeconds > int64((365*24*time.Hour)/time.Second) {
		return ports.RestoreStatus{}, errors.New("restore receipt age budget is invalid")
	}
	if raw.TableCount < 1 || raw.TableCount > 100_000 || raw.MigrationCount < 1 || raw.MigrationCount > 10_000 || !migrationName.MatchString(raw.LatestMigration) {
		return ports.RestoreStatus{}, errors.New("restore receipt table or migration summary is invalid")
	}
	if raw.TableChecks != "verified" || raw.RowCounts != "recorded" || raw.TargetCleanup != "verified" || raw.SemanticInspection != "not_proven" {
		return ports.RestoreStatus{}, errors.New("restore receipt evidence marker is invalid")
	}
	if strings.Contains(raw.Report, "..") {
		return ports.RestoreStatus{}, errors.New("restore receipt report name is unsafe")
	}

	return ports.RestoreStatus{
		Database:                 raw.Database,
		Archive:                  raw.Archive,
		ArchiveSHA256:            raw.ArchiveSHA256,
		Report:                   raw.Report,
		ReportSHA256:             raw.ReportSHA256,
		RestoredAt:               restoredAt,
		MaxAge:                   time.Duration(raw.MaxAgeSeconds) * time.Second,
		TableCount:               raw.TableCount,
		MigrationCount:           raw.MigrationCount,
		LatestMigration:          raw.LatestMigration,
		TechnicalRestoreVerified: true,
		SemanticInspection:       raw.SemanticInspection,
	}, nil
}
