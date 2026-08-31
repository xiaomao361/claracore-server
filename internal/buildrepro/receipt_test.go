package buildrepro

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateAndVerifyBoundedReproducibilityReceipt(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	release := t.TempDir()
	primary := filepath.Join(release, "claracore-server")
	rebuiltDir := t.TempDir()
	rebuilt := filepath.Join(rebuiltDir, "claracore-server.rebuild")
	identity := Identity{
		Version: "test-repro", SourceDigest: strings.Repeat("a", 64),
		TargetOS: "linux", TargetArch: "amd64", GoToolchain: "go1.26.6",
	}
	buildTestBinary(t, repo, primary, filepath.Join(t.TempDir(), "gocache-primary"), identity)
	buildTestBinary(t, repo, rebuilt, filepath.Join(t.TempDir(), "gocache-rebuilt"), identity)

	if err := Generate(GenerateInput{
		ReleaseDir: release, PrimaryBinary: primary, RebuiltBinary: rebuilt, Identity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	verification, err := Verify(VerifyInput{
		ReleaseDir: release, PrimaryBinary: primary, Identity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.OK || verification.Status != "verified" || verification.Scope != Scope || verification.Target != "linux/amd64" {
		t.Fatalf("unexpected verification: %+v", verification)
	}
	body, err := os.ReadFile(filepath.Join(release, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), repo) || strings.Contains(string(body), release) || strings.Contains(string(body), "time") {
		t.Fatalf("receipt is nondeterministic or leaks build paths: %s", body)
	}

	tampered := strings.Replace(string(body), verification.BinarySHA256, strings.Repeat("0", 64), 1)
	tamperedPath := filepath.Join(release, "tampered-reproducibility.json")
	if err := os.WriteFile(tamperedPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(VerifyInput{
		ReleaseDir: release, PrimaryBinary: primary, ReceiptPath: tamperedPath, Identity: identity,
	}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered receipt must fail: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, body, 0o644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(release, "symlink.json")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(VerifyInput{
		ReleaseDir: release, PrimaryBinary: primary, ReceiptPath: symlink, Identity: identity,
	}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink receipt must fail: %v", err)
	}
}

func TestGenerateRejectsDifferentRebuiltBytes(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	release := t.TempDir()
	primary := filepath.Join(release, "claracore-server")
	rebuilt := filepath.Join(t.TempDir(), "claracore-server.rebuild")
	identity := Identity{
		Version: "test-repro", SourceDigest: strings.Repeat("b", 64),
		TargetOS: "linux", TargetArch: "amd64", GoToolchain: "go1.26.6",
	}
	buildTestBinary(t, repo, primary, filepath.Join(t.TempDir(), "gocache-primary"), identity)
	buildTestBinary(t, repo, rebuilt, filepath.Join(t.TempDir(), "gocache-rebuilt"), Identity{
		Version: "different-version", SourceDigest: identity.SourceDigest,
		TargetOS: identity.TargetOS, TargetArch: identity.TargetArch, GoToolchain: identity.GoToolchain,
	})
	if err := Generate(GenerateInput{
		ReleaseDir: release, PrimaryBinary: primary, RebuiltBinary: rebuilt, Identity: identity,
	}); err == nil || !strings.Contains(err.Error(), "not reproducible") {
		t.Fatalf("different rebuilt bytes must fail: %v", err)
	}
}

func TestGenerateRejectsVCSStampedBinary(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	release := t.TempDir()
	primary := filepath.Join(release, "claracore-server")
	identity := Identity{
		Version: "test-repro", SourceDigest: strings.Repeat("c", 64),
		TargetOS: "linux", TargetArch: "amd64", GoToolchain: "go1.26.6",
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=true",
		"-ldflags", "-X main.version="+identity.Version+" -X main.sourceDigest="+identity.SourceDigest,
		"-o", primary, "./cmd/server")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=go1.26.6", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOCACHE="+filepath.Join(t.TempDir(), "gocache"))
	if body, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build VCS-stamped binary: %v\n%s", err, body)
	}
	if err := Generate(GenerateInput{
		ReleaseDir: release, PrimaryBinary: primary, RebuiltBinary: primary, Identity: identity,
	}); err == nil || !strings.Contains(err.Error(), "buildVCS=true") {
		t.Fatalf("VCS-stamped binary must fail: %v", err)
	}
}

func buildTestBinary(t *testing.T, repo, output, cache string, identity Identity) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
		"-ldflags", "-X main.version="+identity.Version+" -X main.sourceDigest="+identity.SourceDigest,
		"-o", output, "./cmd/server")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN="+identity.GoToolchain, "CGO_ENABLED=0", "GOOS="+identity.TargetOS, "GOARCH="+identity.TargetArch, "GOCACHE="+cache)
	if body, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test binary: %v\n%s", err, body)
	}
}
