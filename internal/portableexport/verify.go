package portableexport

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	defaultMaxArchiveBytes      int64 = 16 << 30
	defaultMaxFileBytes         int64 = 16 << 30
	defaultMaxUncompressedBytes int64 = 64 << 30
	defaultMaxJSONLLineBytes    int64 = 32 << 20
	defaultMaxRecords           int64 = 100_000_000
)

type Limits struct {
	MaxArchiveBytes      int64
	MaxFileBytes         int64
	MaxUncompressedBytes int64
	MaxJSONLLineBytes    int64
	MaxRecords           int64
}

func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes: defaultMaxArchiveBytes, MaxFileBytes: defaultMaxFileBytes,
		MaxUncompressedBytes: defaultMaxUncompressedBytes,
		MaxJSONLLineBytes:    defaultMaxJSONLLineBytes, MaxRecords: defaultMaxRecords,
	}
}

type Verification struct {
	OK                bool      `json:"ok"`
	Format            string    `json:"format"`
	FormatVersion     int       `json:"formatVersion"`
	SchemaVersion     string    `json:"schemaVersion"`
	TenantID          string    `json:"tenantId"`
	GeneratedAt       time.Time `json:"generatedAt"`
	Files             int       `json:"files"`
	Records           int64     `json:"records"`
	ArchiveBytes      int64     `json:"archiveBytes"`
	UncompressedBytes int64     `json:"uncompressedBytes"`
}

// Archive is a fully verified, read-only team export. The underlying file
// remains open until Close, so callers can safely make additional semantic
// passes over the exact bytes that were verified.
type Archive struct {
	file         *os.File
	manifest     Manifest
	verification Verification
	files        map[string]*zip.File
}

func OpenFile(path string, limits Limits) (*Archive, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("portable export: inspect archive: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("portable export: archive must be a regular file, not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("portable export: open archive: %w", err)
	}
	openedInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("portable export: inspect opened archive: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() {
		f.Close()
		return nil, errors.New("portable export: archive changed while it was opened")
	}
	verification, manifest, files, err := verifyReaderAt(f, openedInfo.Size(), limits)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Archive{file: f, manifest: manifest, verification: verification, files: files}, nil
}

func VerifyFile(path string, limits Limits) (Verification, error) {
	archive, err := OpenFile(path, limits)
	if err != nil {
		return Verification{}, err
	}
	defer archive.Close()
	return archive.Verification(), nil
}

func VerifyReaderAt(reader io.ReaderAt, size int64, limits Limits) (Verification, error) {
	verification, _, _, err := verifyReaderAt(reader, size, limits)
	return verification, err
}

func verifyReaderAt(reader io.ReaderAt, size int64, limits Limits) (Verification, Manifest, map[string]*zip.File, error) {
	if err := validateLimits(limits); err != nil {
		return Verification{}, Manifest{}, nil, err
	}
	if reader == nil || size <= 0 || size > limits.MaxArchiveBytes {
		return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: archive size %d is outside the allowed range", size)
	}
	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: open ZIP: %w", err)
	}
	wantContent := ContentFiles()
	if len(zr.File) != len(wantContent)+1 {
		return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: archive has %d entries, want %d", len(zr.File), len(wantContent)+1)
	}
	for i, want := range append(wantContent, ManifestName) {
		if zr.File[i].Name != want {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: archive entry %d is %q, want %q", i, zr.File[i].Name, want)
		}
	}
	allowed := make(map[string]bool, len(wantContent)+1)
	for _, name := range wantContent {
		allowed[name] = true
	}
	allowed[ManifestName] = true
	files := make(map[string]*zip.File, len(zr.File))
	var uncompressed int64
	for _, file := range zr.File {
		if !allowed[file.Name] {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: unexpected or unsafe entry %q", file.Name)
		}
		if _, exists := files[file.Name]; exists {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: duplicate entry %q", file.Name)
		}
		if strings.HasSuffix(file.Name, "/") || file.Mode()&os.ModeSymlink != 0 || file.Mode().Type() != 0 {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: entry %q is not a regular file", file.Name)
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: entry %q uses unsupported compression method %d", file.Name, file.Method)
		}
		entrySize := int64(file.UncompressedSize64)
		if file.UncompressedSize64 > uint64(limits.MaxFileBytes) {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: entry %q exceeds the file size limit", file.Name)
		}
		if uncompressed > limits.MaxUncompressedBytes-entrySize {
			return Verification{}, Manifest{}, nil, errors.New("portable export: uncompressed content exceeds the total size limit")
		}
		uncompressed += entrySize
		files[file.Name] = file
	}
	if uncompressed > limits.MaxUncompressedBytes {
		return Verification{}, Manifest{}, nil, errors.New("portable export: uncompressed content exceeds the total size limit")
	}
	manifestFile := files[ManifestName]
	if manifestFile == nil {
		return Verification{}, Manifest{}, nil, errors.New("portable export: manifest.json is missing")
	}
	if manifestFile.UncompressedSize64 > manifestMaxSize {
		return Verification{}, Manifest{}, nil, errors.New("portable export: manifest.json exceeds the format limit")
	}
	manifestBody, err := readEntry(manifestFile, manifestMaxSize)
	if err != nil {
		return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: read manifest.json: %w", err)
	}
	manifest, err := decodeManifest(manifestBody, wantContent)
	if err != nil {
		return Verification{}, Manifest{}, nil, err
	}

	var totalRecords int64
	for i, receipt := range manifest.Files {
		file := files[receipt.Name]
		var records int64
		var digest string
		if i == 0 {
			digest, err = hashEntry(file, limits.MaxFileBytes)
		} else {
			digest, records, err = verifyJSONLEntry(file, limits)
		}
		if err != nil {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: verify %s: %w", receipt.Name, err)
		}
		if digest != receipt.SHA256 {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: SHA-256 mismatch for %s", receipt.Name)
		}
		if records != receipt.Records {
			return Verification{}, Manifest{}, nil, fmt.Errorf("portable export: record count mismatch for %s: got %d, manifest says %d", receipt.Name, records, receipt.Records)
		}
		if totalRecords > limits.MaxRecords-records {
			return Verification{}, Manifest{}, nil, errors.New("portable export: record count exceeds the total limit")
		}
		totalRecords += records
	}
	verification := Verification{
		OK: true, Format: manifest.Format, FormatVersion: manifest.FormatVersion,
		SchemaVersion: manifest.SchemaVersion, TenantID: manifest.TenantID,
		GeneratedAt: manifest.GeneratedAt, Files: len(manifest.Files), Records: totalRecords,
		ArchiveBytes: size, UncompressedBytes: uncompressed,
	}
	return verification, manifest, files, nil
}

func (a *Archive) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
}

func (a *Archive) Verification() Verification {
	if a == nil {
		return Verification{}
	}
	return a.verification
}

func (a *Archive) Manifest() Manifest {
	if a == nil {
		return Manifest{}
	}
	manifest := a.manifest
	manifest.Files = append([]FileReceipt(nil), a.manifest.Files...)
	manifest.Excluded = append([]string(nil), a.manifest.Excluded...)
	return manifest
}

// OpenDataset opens one verified JSONL dataset. README and manifest are not
// exposed through this method because import code must never interpret them as records.
func (a *Archive) OpenDataset(name string) (io.ReadCloser, error) {
	if a == nil || a.file == nil {
		return nil, errors.New("portable export: archive is closed")
	}
	allowed := false
	for _, dataset := range datasetNames {
		if name == dataset {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("portable export: %q is not a dataset", name)
	}
	file := a.files[name]
	if file == nil {
		return nil, fmt.Errorf("portable export: dataset %q is missing", name)
	}
	return file.Open()
}

func validateLimits(limits Limits) error {
	if limits.MaxArchiveBytes <= 0 || limits.MaxFileBytes <= 0 || limits.MaxUncompressedBytes <= 0 || limits.MaxJSONLLineBytes <= 0 || limits.MaxRecords <= 0 || limits.MaxJSONLLineBytes > int64(int(^uint(0)>>1)) {
		return errors.New("portable export: invalid verifier limits")
	}
	return nil
}

func decodeManifest(body []byte, wantFiles []string) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("portable export: decode manifest.json: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("portable export: manifest.json has trailing content")
	}
	if manifest.Format != FormatName || manifest.FormatVersion != FormatVersion || !manifest.Complete {
		return Manifest{}, errors.New("portable export: unsupported or incomplete manifest")
	}
	if len(manifest.SchemaVersion) != 4 || !allDigits(manifest.SchemaVersion) {
		return Manifest{}, errors.New("portable export: invalid schema version")
	}
	if !isULID(manifest.TenantID) || strings.TrimSpace(manifest.TenantName) == "" || len(manifest.TenantName) > 512 {
		return Manifest{}, errors.New("portable export: invalid tenant metadata")
	}
	_, generatedOffset := manifest.GeneratedAt.Zone()
	_, tenantOffset := manifest.TenantCreatedAt.Zone()
	if manifest.GeneratedAt.IsZero() || generatedOffset != 0 || manifest.TenantCreatedAt.IsZero() || tenantOffset != 0 || manifest.TenantCreatedAt.After(manifest.GeneratedAt) {
		return Manifest{}, errors.New("portable export: tenant_created_at and generated_at must be ordered non-zero UTC timestamps")
	}
	if len(manifest.Files) != len(wantFiles) {
		return Manifest{}, errors.New("portable export: manifest file set is incomplete")
	}
	for i, want := range wantFiles {
		receipt := manifest.Files[i]
		if receipt.Name != want {
			return Manifest{}, fmt.Errorf("portable export: manifest file %d is %q, want %q", i, receipt.Name, want)
		}
		if receipt.Records < 0 || (i == 0 && receipt.Records != 0) || !isLowerSHA256(receipt.SHA256) {
			return Manifest{}, fmt.Errorf("portable export: invalid receipt for %s", receipt.Name)
		}
	}
	wantExcluded := ExcludedData()
	if len(manifest.Excluded) != len(wantExcluded) {
		return Manifest{}, errors.New("portable export: manifest exclusions are incomplete")
	}
	for i, want := range wantExcluded {
		if manifest.Excluded[i] != want {
			return Manifest{}, errors.New("portable export: manifest exclusions do not match format version 1")
		}
	}
	return manifest, nil
}

func readEntry(file *zip.File, max int64) ([]byte, error) {
	r, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, errors.New("entry exceeds size limit")
	}
	return body, nil
}

func hashEntry(file *zip.File, max int64) (string, error) {
	r, err := file.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(r, max+1))
	if err != nil {
		return "", err
	}
	if written != int64(file.UncompressedSize64) || written > max {
		return "", errors.New("entry size does not match ZIP metadata")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyJSONLEntry(file *zip.File, limits Limits) (string, int64, error) {
	r, err := file.Open()
	if err != nil {
		return "", 0, err
	}
	defer r.Close()
	hash := sha256.New()
	limited := io.LimitReader(r, limits.MaxFileBytes+1)
	scanner := bufio.NewScanner(io.TeeReader(limited, hash))
	scanner.Buffer(make([]byte, 64<<10), int(limits.MaxJSONLLineBytes))
	var records int64
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return "", 0, errors.New("JSONL contains a blank record")
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal(line, &record); err != nil || record == nil {
			return "", 0, errors.New("JSONL record is not one JSON object")
		}
		records++
		if records > limits.MaxRecords {
			return "", 0, errors.New("record count exceeds the per-file limit")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", 0, fmt.Errorf("scan JSONL: %w", err)
	}
	if int64(file.UncompressedSize64) > limits.MaxFileBytes {
		return "", 0, errors.New("entry exceeds size limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), records, nil
}

func isLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func allDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func isULID(value string) bool {
	if len(value) != 26 {
		return false
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, char := range value {
		if !strings.ContainsRune(alphabet, char) {
			return false
		}
	}
	return value[0] <= '7'
}
