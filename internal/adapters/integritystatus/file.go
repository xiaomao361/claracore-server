package integritystatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"claracore-server/internal/ports"
)

const (
	Format          = "claracore-data-integrity/v1"
	maxReceiptBytes = 16 * 1024
)

var (
	schemaVersion = regexp.MustCompile(`^[0-9]{4,16}$`)
	domainName    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	ruleName      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

type File struct{ path string }

func NewFile(path string) *File { return &File{path: path} }

type issue struct {
	Domain     string `json:"domain"`
	Rule       string `json:"rule"`
	Violations int64  `json:"violations"`
}

type receipt struct {
	Format        string  `json:"format"`
	Status        string  `json:"status"`
	SchemaVersion string  `json:"schema_version"`
	CheckedAt     string  `json:"checked_at"`
	MaxAgeSeconds int64   `json:"max_age_seconds"`
	RulesChecked  int     `json:"rules_checked"`
	Violations    int64   `json:"violations"`
	Issues        []issue `json:"issues"`
}

func Publish(path string, status ports.IntegrityStatus) error {
	raw := receipt{
		Format: Format, Status: "violations_found", SchemaVersion: status.SchemaVersion,
		CheckedAt: status.CheckedAt.UTC().Format(time.RFC3339Nano), MaxAgeSeconds: int64(status.MaxAge / time.Second),
		RulesChecked: status.RulesChecked, Violations: status.Violations,
		Issues: make([]issue, 0, len(status.Issues)),
	}
	if status.Verified {
		raw.Status = "verified"
	}
	for _, item := range status.Issues {
		raw.Issues = append(raw.Issues, issue{Domain: item.Domain, Rule: item.Rule, Violations: item.Violations})
	}
	if _, err := validate(raw); err != nil {
		return fmt.Errorf("validate data integrity receipt: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("data integrity receipt target must be a regular non-symlink file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect data integrity receipt target: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".claracore-integrity-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary data integrity receipt: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("protect temporary data integrity receipt: %w", err)
	}
	if err := json.NewEncoder(tmp).Encode(raw); err != nil {
		return fmt.Errorf("encode data integrity receipt: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync data integrity receipt: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close data integrity receipt: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish data integrity receipt: %w", err)
	}
	keep = true
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (f *File) Read() (ports.IntegrityStatus, error) {
	info, err := os.Lstat(f.path)
	if err != nil {
		return ports.IntegrityStatus{}, fmt.Errorf("read data integrity receipt: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt must be a regular non-symlink file")
	}
	if info.Size() <= 0 || info.Size() > maxReceiptBytes {
		return ports.IntegrityStatus{}, fmt.Errorf("data integrity receipt size must be 1..%d bytes", maxReceiptBytes)
	}
	file, err := os.Open(f.path)
	if err != nil {
		return ports.IntegrityStatus{}, fmt.Errorf("open data integrity receipt: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return ports.IntegrityStatus{}, fmt.Errorf("stat opened data integrity receipt: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxReceiptBytes+1))
	decoder.DisallowUnknownFields()
	var raw receipt
	if err := decoder.Decode(&raw); err != nil {
		return ports.IntegrityStatus{}, fmt.Errorf("decode data integrity receipt: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt must contain exactly one JSON object")
	}
	status, err := validate(raw)
	if err != nil {
		return ports.IntegrityStatus{}, err
	}
	return status, nil
}

func validate(raw receipt) (ports.IntegrityStatus, error) {
	if raw.Format != Format || !schemaVersion.MatchString(raw.SchemaVersion) {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt format or schema is invalid")
	}
	checkedAt, err := time.Parse(time.RFC3339Nano, raw.CheckedAt)
	if err != nil || checkedAt.Location() != time.UTC {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt checked_at must be UTC")
	}
	if raw.MaxAgeSeconds < 60 || raw.MaxAgeSeconds > int64((365*24*time.Hour)/time.Second) || raw.RulesChecked < 1 || raw.RulesChecked > 10_000 || raw.Violations < 0 || len(raw.Issues) > raw.RulesChecked {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt bounds are invalid")
	}
	verified := raw.Status == "verified"
	if !verified && raw.Status != "violations_found" {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt status is invalid")
	}
	issues := make([]ports.IntegrityIssue, 0, len(raw.Issues))
	var sum int64
	seen := make(map[string]bool, len(raw.Issues))
	for _, item := range raw.Issues {
		key := item.Domain + "." + item.Rule
		if !domainName.MatchString(item.Domain) || !ruleName.MatchString(item.Rule) || item.Violations < 1 || seen[key] || sum > (1<<63-1)-item.Violations {
			return ports.IntegrityStatus{}, errors.New("data integrity receipt issue is invalid")
		}
		seen[key] = true
		sum += item.Violations
		issues = append(issues, ports.IntegrityIssue{Domain: item.Domain, Rule: item.Rule, Violations: item.Violations})
	}
	if (verified && (raw.Violations != 0 || len(issues) != 0)) || (!verified && (raw.Violations == 0 || len(issues) == 0)) || sum != raw.Violations {
		return ports.IntegrityStatus{}, errors.New("data integrity receipt result is inconsistent")
	}
	return ports.IntegrityStatus{
		Verified: verified, SchemaVersion: raw.SchemaVersion, CheckedAt: checkedAt,
		MaxAge: time.Duration(raw.MaxAgeSeconds) * time.Second, RulesChecked: raw.RulesChecked,
		Violations: raw.Violations, Issues: issues,
	}, nil
}
