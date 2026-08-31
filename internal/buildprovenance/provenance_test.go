package buildprovenance

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"claracore-server/internal/buildrepro"
)

func TestGenerateAndVerifyBindsReleaseBinaryInputsAndByproducts(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	release := t.TempDir()
	binary := filepath.Join(release, "claracore-server")
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, "./cmd/server")
	build.Dir = repo
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOTOOLCHAIN=go1.26.6")
	if body, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test release binary: %v\n%s", err, body)
	}

	goModBody, err := os.ReadFile(filepath.Join(repo, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goModDigest := sha256.Sum256(goModBody)
	sourceBody := []byte(fmt.Sprintf("%x  go.mod\n", goModDigest))
	sourceDigest := fmt.Sprintf("%x", sha256.Sum256(sourceBody))
	if err := os.WriteFile(filepath.Join(release, SourceManifest), sourceBody, 0o644); err != nil {
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
			Version: "test-provenance", SourceDigest: sourceDigest,
			TargetOS: "linux", TargetArch: "amd64", GoToolchain: ExpectedGoVersion,
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range byproductNames() {
		if name == buildrepro.FileName {
			continue
		}
		if err := os.WriteFile(filepath.Join(release, name), []byte("receipt="+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Generate(GenerateInput{
		RepoDir: repo, ReleaseDir: release, BinaryPath: binary,
		Version: "test-provenance", SourceDigest: sourceDigest, TargetOS: "linux", TargetArch: "amd64",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(VerifyInput{
		ReleaseDir: release, BinaryPath: binary, Version: "test-provenance",
		SourceDigest: sourceDigest, TargetOS: "linux", TargetArch: "amd64", GoVersion: ExpectedGoVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Status != "verified" || result.ResolvedInputs != 2 || result.VerifiedByproducts != len(byproductNames()) {
		t.Fatalf("unexpected verification: %+v", result)
	}

	statementBody, err := os.ReadFile(filepath.Join(release, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(statementBody), "startedOn") || strings.Contains(string(statementBody), "finishedOn") || strings.Contains(string(statementBody), "invocationId") || strings.Contains(string(statementBody), repo) || strings.Contains(string(statementBody), release) {
		t.Fatalf("provenance is not deterministic or leaks local paths:\n%s", statementBody)
	}
	var statement Statement
	if err := json.Unmarshal(statementBody, &statement); err != nil {
		t.Fatal(err)
	}
	if statement.Type != StatementType || statement.PredicateType != PredicateType || statement.Predicate.BuildDefinition.BuildType != BuildType || statement.Predicate.RunDetails.Builder.ID != BuilderID {
		t.Fatalf("unexpected statement identity: %+v", statement)
	}

	tamperedPath := filepath.Join(release, "tampered-provenance.json")
	tampered := strings.Replace(string(statementBody), result.SubjectSHA256, strings.Repeat("0", 64), 1)
	if err := os.WriteFile(tamperedPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(VerifyInput{
		ReleaseDir: release, BinaryPath: binary, ProvenancePath: tamperedPath,
		Version: "test-provenance", SourceDigest: sourceDigest,
		TargetOS: "linux", TargetArch: "amd64", GoVersion: ExpectedGoVersion,
	}); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("tampered subject must fail: %v", err)
	}

	receipt := filepath.Join(release, byproductNames()[0])
	if err := os.WriteFile(receipt, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(VerifyInput{
		ReleaseDir: release, BinaryPath: binary, Version: "test-provenance",
		SourceDigest: sourceDigest, TargetOS: "linux", TargetArch: "amd64", GoVersion: ExpectedGoVersion,
	}); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("tampered byproduct must fail: %v", err)
	}
}

func TestVerifyRejectsUnsafeProvenancePath(t *testing.T) {
	release := t.TempDir()
	binary := filepath.Join(release, "claracore-server")
	if err := os.WriteFile(binary, []byte("not reached"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "provenance.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(release, FileName)
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(VerifyInput{
		ReleaseDir: release, BinaryPath: binary, ProvenancePath: inside,
		Version: "test", SourceDigest: strings.Repeat("a", 64),
		TargetOS: "linux", TargetArch: "amd64", GoVersion: ExpectedGoVersion,
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink provenance must fail: %v", err)
	}
}

func TestRequireUnchangedInputSnapshotFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BuildInputsName)
	original := []byte(strings.Repeat("a", 64) + "  scripts/package-release.sh\n")
	if err := requireUnchangedInputSnapshot(path, original); err != nil {
		t.Fatalf("create initial snapshot: %v", err)
	}
	if err := requireUnchangedInputSnapshot(path, original); err != nil {
		t.Fatalf("unchanged snapshot must pass: %v", err)
	}
	changed := []byte(strings.Repeat("b", 64) + "  scripts/package-release.sh\n")
	if err := requireUnchangedInputSnapshot(path, changed); err == nil || !strings.Contains(err.Error(), "changed after") {
		t.Fatalf("changed build input snapshot must fail: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "inputs")
	if err := os.WriteFile(outside, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := requireUnchangedInputSnapshot(path, original); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink snapshot must fail: %v", err)
	}
}
