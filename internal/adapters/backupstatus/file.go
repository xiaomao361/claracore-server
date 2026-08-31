package backupstatus

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

var databaseName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

type File struct {
	path string
}

func NewFile(path string) *File { return &File{path: path} }

type receipt struct {
	OK             bool   `json:"ok"`
	Database       string `json:"database"`
	Archive        string `json:"archive"`
	CreatedAt      string `json:"created_at"`
	VerifiedAt     string `json:"verified_at"`
	AgeSeconds     int64  `json:"age_seconds"`
	MaxAgeSeconds  int64  `json:"max_age_seconds"`
	SHA256         string `json:"sha256"`
	Gzip           string `json:"gzip"`
	Recoverability string `json:"recoverability"`
}

func (f *File) Read() (ports.BackupStatus, error) {
	info, err := os.Lstat(f.path)
	if err != nil {
		return ports.BackupStatus{}, fmt.Errorf("read backup receipt: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ports.BackupStatus{}, errors.New("backup receipt must be a regular non-symlink file")
	}
	if info.Size() <= 0 || info.Size() > maxReceiptBytes {
		return ports.BackupStatus{}, fmt.Errorf("backup receipt size must be 1..%d bytes", maxReceiptBytes)
	}

	file, err := os.Open(f.path)
	if err != nil {
		return ports.BackupStatus{}, fmt.Errorf("open backup receipt: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return ports.BackupStatus{}, fmt.Errorf("stat opened backup receipt: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return ports.BackupStatus{}, errors.New("backup receipt changed while opening")
	}
	if openedInfo.Size() <= 0 || openedInfo.Size() > maxReceiptBytes {
		return ports.BackupStatus{}, fmt.Errorf("opened backup receipt size must be 1..%d bytes", maxReceiptBytes)
	}

	decoder := json.NewDecoder(io.LimitReader(file, maxReceiptBytes+1))
	decoder.DisallowUnknownFields()
	var raw receipt
	if err := decoder.Decode(&raw); err != nil {
		return ports.BackupStatus{}, fmt.Errorf("decode backup receipt: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ports.BackupStatus{}, errors.New("backup receipt must contain exactly one JSON object")
	}
	if !raw.OK || !databaseName.MatchString(raw.Database) {
		return ports.BackupStatus{}, errors.New("backup receipt status or database is invalid")
	}
	wantArchivePrefix := "claracore-server_" + raw.Database + "_"
	if filepath.Base(raw.Archive) != raw.Archive || !strings.HasPrefix(raw.Archive, wantArchivePrefix) || !strings.HasSuffix(raw.Archive, ".sql.gz") {
		return ports.BackupStatus{}, errors.New("backup receipt archive name is invalid")
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(raw.Archive, wantArchivePrefix), ".sql.gz")
	createdAt, err := time.Parse(timestampLayout, raw.CreatedAt)
	if err != nil || stamp != raw.CreatedAt {
		return ports.BackupStatus{}, errors.New("backup receipt created_at is invalid or does not match archive")
	}
	verifiedAt, err := time.Parse(timestampLayout, raw.VerifiedAt)
	if err != nil || verifiedAt.Before(createdAt.Add(-5*time.Minute)) {
		return ports.BackupStatus{}, errors.New("backup receipt verified_at is invalid")
	}
	if raw.AgeSeconds < 0 || raw.MaxAgeSeconds < 1 || raw.MaxAgeSeconds > int64((365*24*time.Hour)/time.Second) {
		return ports.BackupStatus{}, errors.New("backup receipt age budget is invalid")
	}
	if raw.SHA256 != "verified" || raw.Gzip != "verified" || raw.Recoverability != "not_proven" {
		return ports.BackupStatus{}, errors.New("backup receipt integrity or recoverability marker is invalid")
	}

	return ports.BackupStatus{
		Database:          raw.Database,
		Archive:           raw.Archive,
		CreatedAt:         createdAt,
		VerifiedAt:        verifiedAt,
		MaxAge:            time.Duration(raw.MaxAgeSeconds) * time.Second,
		IntegrityVerified: true,
		Recoverability:    raw.Recoverability,
	}, nil
}
