package buildprovenance

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
	"sort"
	"strings"

	"claracore-server/internal/buildrepro"
)

const (
	StatementType     = "https://in-toto.io/Statement/v1"
	PredicateType     = "https://slsa.dev/provenance/v1"
	BuildType         = "https://github.com/xiaomao361/claracore-server/blob/main/docs/09-release-provenance.md#build-type-v1"
	BuilderID         = "https://github.com/xiaomao361/claracore-server/blob/main/docs/09-release-provenance.md#local-builder-v1"
	ExpectedGoVersion = "go1.26.6"
	FileName          = "BUILD_PROVENANCE.intoto.json"
	BuildInputsName   = "BUILD_INPUTS.sha256"
	SourceManifest    = "SOURCE_MANIFEST.sha256"
	maxStatementBytes = 256 * 1024
)

type Descriptor struct {
	Name      string            `json:"name"`
	Digest    map[string]string `json:"digest"`
	MediaType string            `json:"mediaType,omitempty"`
}

type ExternalParameters struct {
	Version    string `json:"version"`
	TargetOS   string `json:"targetOS"`
	TargetArch string `json:"targetArch"`
}

type InternalParameters struct {
	CGOEnabled   bool   `json:"cgoEnabled"`
	GoToolchain  string `json:"goToolchain"`
	SourceDigest string `json:"sourceDigest"`
	Trimpath     bool   `json:"trimpath"`
}

type BuildDefinition struct {
	BuildType            string             `json:"buildType"`
	ExternalParameters   ExternalParameters `json:"externalParameters"`
	InternalParameters   InternalParameters `json:"internalParameters"`
	ResolvedDependencies []Descriptor       `json:"resolvedDependencies"`
}

type Builder struct {
	ID      string            `json:"id"`
	Version map[string]string `json:"version"`
}

type RunDetails struct {
	Builder    Builder      `json:"builder"`
	Byproducts []Descriptor `json:"byproducts"`
}

type Predicate struct {
	BuildDefinition BuildDefinition `json:"buildDefinition"`
	RunDetails      RunDetails      `json:"runDetails"`
}

type Statement struct {
	Type          string       `json:"_type"`
	Subject       []Descriptor `json:"subject"`
	PredicateType string       `json:"predicateType"`
	Predicate     Predicate    `json:"predicate"`
}

type GenerateInput struct {
	RepoDir      string
	ReleaseDir   string
	BinaryPath   string
	Version      string
	SourceDigest string
	TargetOS     string
	TargetArch   string
}

type VerifyInput struct {
	ReleaseDir     string
	BinaryPath     string
	ProvenancePath string
	Version        string
	SourceDigest   string
	TargetOS       string
	TargetArch     string
	GoVersion      string
}

type Verification struct {
	OK                 bool   `json:"ok"`
	Status             string `json:"status"`
	PredicateType      string `json:"predicateType"`
	BuilderID          string `json:"builderId"`
	Subject            string `json:"subject"`
	SubjectSHA256      string `json:"subjectSha256"`
	SourceDigest       string `json:"sourceDigest"`
	ResolvedInputs     int    `json:"resolvedInputs"`
	VerifiedByproducts int    `json:"verifiedByproducts"`
}

// CaptureInputs records the exact repository-owned release workflow inputs
// before any build or supply-chain command runs. Generate later requires the
// same bytes, closing the gap where a long release could otherwise attest to
// files changed after they were executed.
func CaptureInputs(repoDir, releaseDir string) error {
	if err := requireAbsoluteDirectory(repoDir); err != nil {
		return fmt.Errorf("repo directory: %w", err)
	}
	if err := requireAbsoluteDirectory(releaseDir); err != nil {
		return fmt.Errorf("release directory: %w", err)
	}
	body, err := buildInputManifest(repoDir)
	if err != nil {
		return err
	}
	return writeNewFile(filepath.Join(releaseDir, BuildInputsName), body)
}

func Generate(input GenerateInput) error {
	if err := validateGenerateInput(input); err != nil {
		return err
	}
	inputManifest, err := buildInputManifest(input.RepoDir)
	if err != nil {
		return err
	}
	inputsPath := filepath.Join(input.ReleaseDir, BuildInputsName)
	if err := requireUnchangedInputSnapshot(inputsPath, inputManifest); err != nil {
		return err
	}

	statement, err := expectedStatement(VerifyInput{
		ReleaseDir: input.ReleaseDir, BinaryPath: input.BinaryPath,
		Version: input.Version, SourceDigest: input.SourceDigest,
		TargetOS: input.TargetOS, TargetArch: input.TargetArch, GoVersion: ExpectedGoVersion,
	})
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(statement, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provenance: %w", err)
	}
	body = append(body, '\n')
	if bytes.Contains(body, []byte("/Users/")) || bytes.Contains(body, []byte("/tmp/")) {
		return errors.New("provenance contains a build-machine absolute path")
	}
	if err := writeNewFile(filepath.Join(input.ReleaseDir, FileName), body); err != nil {
		return err
	}
	_, err = Verify(VerifyInput{
		ReleaseDir: input.ReleaseDir, BinaryPath: input.BinaryPath,
		Version: input.Version, SourceDigest: input.SourceDigest,
		TargetOS: input.TargetOS, TargetArch: input.TargetArch, GoVersion: ExpectedGoVersion,
	})
	return err
}

func requireUnchangedInputSnapshot(path string, expected []byte) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return writeNewFile(path, expected)
	}
	if err != nil {
		return fmt.Errorf("inspect build input snapshot: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("build input snapshot must be an ordinary file, not a symlink")
	}
	actual, err := readBoundedRegularFile(path, 256*1024)
	if err != nil {
		return fmt.Errorf("read build input snapshot: %w", err)
	}
	if !bytes.Equal(actual, expected) {
		return errors.New("release build inputs changed after the pre-build snapshot")
	}
	return nil
}

func Verify(input VerifyInput) (Verification, error) {
	if input.ProvenancePath == "" {
		input.ProvenancePath = filepath.Join(input.ReleaseDir, FileName)
	}
	if err := validateVerifyInput(input); err != nil {
		return Verification{}, err
	}
	body, err := readBoundedRegularFile(input.ProvenancePath, maxStatementBytes)
	if err != nil {
		return Verification{}, fmt.Errorf("read provenance: %w", err)
	}
	if bytes.Contains(body, []byte("/Users/")) || bytes.Contains(body, []byte("/tmp/")) {
		return Verification{}, errors.New("provenance contains a build-machine absolute path")
	}
	var actual Statement
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&actual); err != nil {
		return Verification{}, fmt.Errorf("decode provenance: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Verification{}, err
	}
	expected, err := expectedStatement(input)
	if err != nil {
		return Verification{}, err
	}
	actualJSON, _ := json.Marshal(actual)
	expectedJSON, _ := json.Marshal(expected)
	if !bytes.Equal(actualJSON, expectedJSON) {
		return Verification{}, errors.New("provenance fields or bound artifact digests do not match this release")
	}
	return Verification{
		OK: true, Status: "verified", PredicateType: PredicateType, BuilderID: BuilderID,
		Subject: actual.Subject[0].Name, SubjectSHA256: actual.Subject[0].Digest["sha256"],
		SourceDigest:       input.SourceDigest,
		ResolvedInputs:     len(actual.Predicate.BuildDefinition.ResolvedDependencies),
		VerifiedByproducts: len(actual.Predicate.RunDetails.Byproducts),
	}, nil
}

func expectedStatement(input VerifyInput) (Statement, error) {
	binaryInfo, err := inspectBinary(input.BinaryPath)
	if err != nil {
		return Statement{}, err
	}
	if binaryInfo.goVersion != input.GoVersion || binaryInfo.goos != input.TargetOS || binaryInfo.goarch != input.TargetArch || binaryInfo.cgoEnabled != "0" || !binaryInfo.trimpath {
		return Statement{}, fmt.Errorf("binary build settings do not match provenance target: go=%s os=%s arch=%s cgo=%s trimpath=%t",
			binaryInfo.goVersion, binaryInfo.goos, binaryInfo.goarch, binaryInfo.cgoEnabled, binaryInfo.trimpath)
	}
	sourcePath := filepath.Join(input.ReleaseDir, SourceManifest)
	sourceBody, err := readBoundedRegularFile(sourcePath, 2*1024*1024)
	if err != nil {
		return Statement{}, fmt.Errorf("read source manifest: %w", err)
	}
	if err := validateHashManifest(sourceBody); err != nil {
		return Statement{}, fmt.Errorf("validate source manifest: %w", err)
	}
	if got := hashBytes(sourceBody); got != input.SourceDigest {
		return Statement{}, fmt.Errorf("source manifest digest %s does not match linked source digest %s", got, input.SourceDigest)
	}
	inputsPath := filepath.Join(input.ReleaseDir, BuildInputsName)
	inputsBody, err := readBoundedRegularFile(inputsPath, 256*1024)
	if err != nil {
		return Statement{}, fmt.Errorf("read build inputs: %w", err)
	}
	if err := validateHashManifest(inputsBody); err != nil {
		return Statement{}, fmt.Errorf("validate build inputs: %w", err)
	}
	if _, err := buildrepro.Verify(buildrepro.VerifyInput{
		ReleaseDir: input.ReleaseDir, PrimaryBinary: input.BinaryPath,
		Identity: buildrepro.Identity{
			Version: input.Version, SourceDigest: input.SourceDigest,
			TargetOS: input.TargetOS, TargetArch: input.TargetArch, GoToolchain: input.GoVersion,
		},
	}); err != nil {
		return Statement{}, fmt.Errorf("verify bounded reproducibility receipt: %w", err)
	}

	byproducts := make([]Descriptor, 0, len(byproductNames()))
	for _, name := range byproductNames() {
		digest, err := hashRegularFile(filepath.Join(input.ReleaseDir, name))
		if err != nil {
			return Statement{}, fmt.Errorf("hash byproduct %s: %w", name, err)
		}
		byproducts = append(byproducts, descriptor(name, digest, mediaType(name)))
	}
	binaryDigest, err := hashRegularFile(input.BinaryPath)
	if err != nil {
		return Statement{}, fmt.Errorf("hash release binary: %w", err)
	}
	return Statement{
		Type:          StatementType,
		Subject:       []Descriptor{descriptor("claracore-server", binaryDigest, "application/vnd.claracore.server")},
		PredicateType: PredicateType,
		Predicate: Predicate{
			BuildDefinition: BuildDefinition{
				BuildType:          BuildType,
				ExternalParameters: ExternalParameters{Version: input.Version, TargetOS: input.TargetOS, TargetArch: input.TargetArch},
				InternalParameters: InternalParameters{CGOEnabled: false, GoToolchain: input.GoVersion, SourceDigest: input.SourceDigest, Trimpath: true},
				ResolvedDependencies: []Descriptor{
					descriptor(SourceManifest, hashBytes(sourceBody), "text/plain"),
					descriptor(BuildInputsName, hashBytes(inputsBody), "text/plain"),
				},
			},
			RunDetails: RunDetails{
				Builder:    Builder{ID: BuilderID, Version: map[string]string{"go": input.GoVersion, "releaseBuilder": "2"}},
				Byproducts: byproducts,
			},
		},
	}, nil
}

type binaryMetadata struct {
	goVersion  string
	goos       string
	goarch     string
	cgoEnabled string
	trimpath   bool
}

func inspectBinary(path string) (binaryMetadata, error) {
	if err := requireAbsoluteRegularFile(path); err != nil {
		return binaryMetadata{}, err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return binaryMetadata{}, fmt.Errorf("read binary build info: %w", err)
	}
	result := binaryMetadata{goVersion: info.GoVersion}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "GOOS":
			result.goos = setting.Value
		case "GOARCH":
			result.goarch = setting.Value
		case "CGO_ENABLED":
			result.cgoEnabled = setting.Value
		case "-trimpath":
			result.trimpath = setting.Value == "true"
		}
	}
	return result, nil
}

func buildInputManifest(repoDir string) ([]byte, error) {
	paths := packagingInputPaths()
	sort.Strings(paths)
	var output strings.Builder
	for _, rel := range paths {
		path := filepath.Join(repoDir, filepath.FromSlash(rel))
		digest, err := hashRegularFile(path)
		if err != nil {
			return nil, fmt.Errorf("hash build input %s: %w", rel, err)
		}
		fmt.Fprintf(&output, "%s  %s\n", digest, rel)
	}
	return []byte(output.String()), nil
}

func packagingInputPaths() []string {
	return []string{
		"LICENSE", "README.md", "docs/07-deployment.md", "docs/09-release-provenance.md",
		"deploy/claracore-server-backup.service", "deploy/claracore-server-backup.timer",
		"deploy/claracore-server-integrity.service", "deploy/claracore-server-integrity.timer",
		"deploy/claracore-server.env.example", "deploy/claracore-server.nginx.conf",
		"deploy/claracore-server.rules.test.yml", "deploy/claracore-server.rules.yml",
		"deploy/mysql-client.cnf.example", "deploy/nginx.conf.example", "deploy/prometheus.yml.example",
		"scripts/backup-mysql.sh", "scripts/check-licenses.sh", "scripts/check-prometheus-rules.sh",
		"scripts/check-vulnerabilities.sh", "scripts/generate-sbom.sh", "scripts/mysql-backup-common.sh",
		"scripts/package-release.sh", "scripts/rehearse-mysql-restore.sh", "scripts/release-archive/main.go",
		"scripts/release-provenance/main.go", "scripts/release-reproducibility/main.go",
		"scripts/validate-sbom.go", "scripts/verify-cluster.sh",
		"scripts/verify-deployment.sh", "scripts/verify-mysql-backup.sh", "scripts/verify-release-signature.sh",
		"scripts/verify-release.sh",
	}
}

func byproductNames() []string {
	return []string{
		"BUILDINFO.txt", buildrepro.FileName, "PROMETHEUS_RULES_CHECK.txt", "SBOM.cdx.json", "SBOM_REVIEW.txt",
		"THIRD_PARTY_LICENSES.csv", "THIRD_PARTY_REVIEW.txt", "VULNERABILITY_SCAN.txt",
	}
}

func descriptor(name, digest, media string) Descriptor {
	return Descriptor{Name: name, Digest: map[string]string{"sha256": digest}, MediaType: media}
}

func mediaType(name string) string {
	switch filepath.Ext(name) {
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	default:
		return "text/plain"
	}
}

func validateGenerateInput(input GenerateInput) error {
	if err := requireAbsoluteDirectory(input.RepoDir); err != nil {
		return fmt.Errorf("repo directory: %w", err)
	}
	if err := requireAbsoluteDirectory(input.ReleaseDir); err != nil {
		return fmt.Errorf("release directory: %w", err)
	}
	if err := requirePathWithin(input.ReleaseDir, input.BinaryPath); err != nil {
		return err
	}
	if input.Version == "" || !regexp.MustCompile(`^[0-9A-Za-z._-]+$`).MatchString(input.Version) {
		return errors.New("release version is invalid")
	}
	if !validDigest(input.SourceDigest) {
		return errors.New("source digest must be 64 lowercase hexadecimal characters")
	}
	if input.TargetArch != "amd64" && input.TargetArch != "arm64" {
		return errors.New("target architecture must be amd64 or arm64")
	}
	if input.TargetOS == "" || !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(input.TargetOS) {
		return errors.New("target operating system is invalid")
	}
	return nil
}

func validateVerifyInput(input VerifyInput) error {
	if err := requireAbsoluteDirectory(input.ReleaseDir); err != nil {
		return fmt.Errorf("release directory: %w", err)
	}
	for _, path := range []string{input.BinaryPath, input.ProvenancePath} {
		if err := requirePathWithin(input.ReleaseDir, path); err != nil {
			return err
		}
	}
	if input.Version == "" || input.TargetOS == "" || input.TargetArch == "" || input.GoVersion == "" || !validDigest(input.SourceDigest) {
		return errors.New("expected release identity is incomplete")
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
	if info.Size() > 2*1024*1024*1024 {
		return "", errors.New("file exceeds hashing size limit")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func hashBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%x", digest)
}

func validateHashManifest(body []byte) error {
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return errors.New("manifest must be non-empty and newline terminated")
	}
	linePattern := regexp.MustCompile(`^([0-9a-f]{64})  ([0-9A-Za-z._/-]+)$`)
	seen := make(map[string]struct{})
	previous := ""
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		match := linePattern.FindStringSubmatch(line)
		if match == nil {
			return errors.New("manifest contains an invalid line")
		}
		name := match[2]
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
		if clean != name || strings.HasPrefix(name, "/") || name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return errors.New("manifest contains an unsafe path")
		}
		if _, exists := seen[name]; exists {
			return errors.New("manifest contains duplicate paths")
		}
		if previous != "" && name <= previous {
			return errors.New("manifest paths are not in lexical order")
		}
		seen[name] = struct{}{}
		previous = name
	}
	return nil
}

func validDigest(value string) bool {
	return regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("provenance contains trailing JSON")
		}
		return fmt.Errorf("decode trailing provenance data: %w", err)
	}
	return nil
}
