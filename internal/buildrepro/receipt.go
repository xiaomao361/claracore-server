// Package buildrepro validates the bounded, same-host reproducibility check
// performed by the release builder. It does not claim hermetic or cross-host
// reproducibility.
package buildrepro

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	FileName        = "BUILD_REPRODUCIBILITY.json"
	Schema          = "https://github.com/xiaomao361/claracore-server/blob/main/docs/09-release-provenance.md#reproducibility-receipt-v1"
	Scope           = "same-host-separate-empty-go-build-cache"
	ResultMatched   = "matched"
	maxReceiptBytes = 32 * 1024
	maxBinaryBytes  = 2 * 1024 * 1024 * 1024
)

type Identity struct {
	Version      string `json:"version"`
	SourceDigest string `json:"sourceDigest"`
	TargetOS     string `json:"targetOS"`
	TargetArch   string `json:"targetArch"`
	GoToolchain  string `json:"goToolchain"`
}

type BuildSettings struct {
	CGOEnabled bool `json:"cgoEnabled"`
	Trimpath   bool `json:"trimpath"`
	BuildVCS   bool `json:"buildVCS"`
}

type Artifact struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type Receipt struct {
	Schema   string        `json:"schema"`
	Scope    string        `json:"scope"`
	Identity Identity      `json:"identity"`
	Settings BuildSettings `json:"settings"`
	Primary  Artifact      `json:"primary"`
	Rebuild  Artifact      `json:"rebuild"`
	Result   string        `json:"result"`
}

type GenerateInput struct {
	ReleaseDir    string
	PrimaryBinary string
	RebuiltBinary string
	Identity      Identity
}

type VerifyInput struct {
	ReleaseDir    string
	PrimaryBinary string
	ReceiptPath   string
	Identity      Identity
}

type Verification struct {
	OK           bool   `json:"ok"`
	Status       string `json:"status"`
	Scope        string `json:"scope"`
	BinarySHA256 string `json:"binarySha256"`
	GoToolchain  string `json:"goToolchain"`
	Target       string `json:"target"`
}

func Generate(input GenerateInput) error {
	if err := requireAbsoluteDirectory(input.ReleaseDir); err != nil {
		return fmt.Errorf("release directory: %w", err)
	}
	if err := validateIdentity(input.Identity); err != nil {
		return err
	}
	primaryMeta, primaryDigest, err := inspectBinary(input.PrimaryBinary, input.Identity)
	if err != nil {
		return fmt.Errorf("inspect primary binary: %w", err)
	}
	rebuiltMeta, rebuiltDigest, err := inspectBinary(input.RebuiltBinary, input.Identity)
	if err != nil {
		return fmt.Errorf("inspect rebuilt binary: %w", err)
	}
	if primaryMeta != rebuiltMeta {
		return errors.New("primary and rebuilt binary settings differ")
	}
	if primaryDigest != rebuiltDigest {
		return fmt.Errorf("release binary is not reproducible in the bounded build: primary=%s rebuilt=%s", primaryDigest, rebuiltDigest)
	}
	receipt := expectedReceipt(input.Identity, primaryDigest)
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return fmt.Errorf("encode reproducibility receipt: %w", err)
	}
	body = append(body, '\n')
	if err := writeNewFile(filepath.Join(input.ReleaseDir, FileName), body); err != nil {
		return err
	}
	_, err = Verify(VerifyInput{
		ReleaseDir: input.ReleaseDir, PrimaryBinary: input.PrimaryBinary, Identity: input.Identity,
	})
	return err
}

func Verify(input VerifyInput) (Verification, error) {
	if input.ReceiptPath == "" {
		input.ReceiptPath = filepath.Join(input.ReleaseDir, FileName)
	}
	if err := requireAbsoluteDirectory(input.ReleaseDir); err != nil {
		return Verification{}, fmt.Errorf("release directory: %w", err)
	}
	if err := requirePathWithin(input.ReleaseDir, input.PrimaryBinary); err != nil {
		return Verification{}, fmt.Errorf("primary binary: %w", err)
	}
	if err := requirePathWithin(input.ReleaseDir, input.ReceiptPath); err != nil {
		return Verification{}, fmt.Errorf("reproducibility receipt: %w", err)
	}
	if err := validateIdentity(input.Identity); err != nil {
		return Verification{}, err
	}
	_, digest, err := inspectBinary(input.PrimaryBinary, input.Identity)
	if err != nil {
		return Verification{}, fmt.Errorf("inspect primary binary: %w", err)
	}
	body, err := readBoundedRegularFile(input.ReceiptPath, maxReceiptBytes)
	if err != nil {
		return Verification{}, fmt.Errorf("read reproducibility receipt: %w", err)
	}
	var actual Receipt
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&actual); err != nil {
		return Verification{}, fmt.Errorf("decode reproducibility receipt: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Verification{}, err
	}
	expected := expectedReceipt(input.Identity, digest)
	actualJSON, _ := json.Marshal(actual)
	expectedJSON, _ := json.Marshal(expected)
	if !bytes.Equal(actualJSON, expectedJSON) {
		return Verification{}, errors.New("reproducibility receipt does not match the release binary or expected build identity")
	}
	return Verification{
		OK: true, Status: "verified", Scope: Scope, BinarySHA256: digest,
		GoToolchain: input.Identity.GoToolchain,
		Target:      input.Identity.TargetOS + "/" + input.Identity.TargetArch,
	}, nil
}

func expectedReceipt(identity Identity, digest string) Receipt {
	return Receipt{
		Schema: Schema, Scope: Scope, Identity: identity,
		Settings: BuildSettings{CGOEnabled: false, Trimpath: true, BuildVCS: false},
		Primary:  Artifact{Name: "claracore-server", Digest: map[string]string{"sha256": digest}},
		Rebuild:  Artifact{Name: "claracore-server.rebuild", Digest: map[string]string{"sha256": digest}},
		Result:   ResultMatched,
	}
}

type binaryMetadata struct {
	goVersion  string
	goos       string
	goarch     string
	cgoEnabled string
	trimpath   bool
	buildVCS   bool
}

func inspectBinary(path string, identity Identity) (binaryMetadata, string, error) {
	if err := requireAbsoluteRegularFile(path); err != nil {
		return binaryMetadata{}, "", err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return binaryMetadata{}, "", fmt.Errorf("read build info: %w", err)
	}
	metadata := binaryMetadata{goVersion: info.GoVersion}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "GOOS":
			metadata.goos = setting.Value
		case "GOARCH":
			metadata.goarch = setting.Value
		case "CGO_ENABLED":
			metadata.cgoEnabled = setting.Value
		case "-trimpath":
			metadata.trimpath = setting.Value == "true"
		}
		if setting.Key == "vcs" || strings.HasPrefix(setting.Key, "vcs.") {
			metadata.buildVCS = true
		}
	}
	if metadata.goVersion != identity.GoToolchain || metadata.goos != identity.TargetOS || metadata.goarch != identity.TargetArch || metadata.cgoEnabled != "0" || !metadata.trimpath || metadata.buildVCS {
		return binaryMetadata{}, "", fmt.Errorf("binary settings do not match bounded reproducible build: go=%s os=%s arch=%s cgo=%s trimpath=%t buildVCS=%t",
			metadata.goVersion, metadata.goos, metadata.goarch, metadata.cgoEnabled, metadata.trimpath, metadata.buildVCS)
	}
	digest, err := hashRegularFile(path)
	return metadata, digest, err
}

func validateIdentity(identity Identity) error {
	if identity.Version == "" || !regexp.MustCompile(`^[0-9A-Za-z._-]+$`).MatchString(identity.Version) {
		return errors.New("release version is invalid")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(identity.SourceDigest) {
		return errors.New("source digest must be 64 lowercase hexadecimal characters")
	}
	if !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(identity.TargetOS) || (identity.TargetArch != "amd64" && identity.TargetArch != "arm64") {
		return errors.New("target operating system or architecture is invalid")
	}
	if !regexp.MustCompile(`^go[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(identity.GoToolchain) {
		return errors.New("Go toolchain identity is invalid")
	}
	return nil
}

func requireAbsoluteDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path must be an ordinary directory, not a symlink")
	}
	return nil
}

func requireAbsoluteRegularFile(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path must be an ordinary file, not a symlink")
	}
	return nil
}

func requirePathWithin(root, path string) error {
	if err := requireAbsoluteRegularFile(path); err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("file must be inside the release directory")
	}
	return nil
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	if err := requireAbsoluteRegularFile(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return body, nil
}

func hashRegularFile(path string) (string, error) {
	if err := requireAbsoluteRegularFile(path); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > maxBinaryBytes {
		return "", errors.New("file exceeds hashing size limit")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func writeNewFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("reproducibility receipt contains trailing JSON")
		}
		return fmt.Errorf("decode reproducibility receipt trailer: %w", err)
	}
	return nil
}
