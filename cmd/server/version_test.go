package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestVersionInfoIsMachineReadable(t *testing.T) {
	var out bytes.Buffer
	digest := strings.Repeat("a", 64)
	binary := strings.Repeat("b", 64)
	if err := writeVersionInfo(&out, "v-test", digest, binary); err != nil {
		t.Fatal(err)
	}
	var got versionInfo
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("version output is not JSON: %v\n%s", err, out.String())
	}
	if got.Version != "v-test" || got.SourceDigest != digest || got.BinaryDigest != binary || got.GoVersion != runtime.Version() || got.GOOS != runtime.GOOS || got.GOARCH != runtime.GOARCH {
		t.Fatalf("unexpected version output: %+v", got)
	}
}

func TestVersionFlagDoesNotLoadConfigurationOrDependencies(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-version")
	cmd.Env = append(os.Environ(), "CCS_INTENTIONAL_VERSION_PROBE=must-not-be-read")
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("version flag must work without loading strict CCS configuration: %v\n%s", err, body)
	}
	var got versionInfo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("version flag output is not clean JSON: %v\n%s", err, body)
	}
	if got.Version != version || got.SourceDigest != sourceDigest || len(got.BinaryDigest) != 64 || strings.TrimSpace(string(body)) == "" {
		t.Fatalf("version flag output = %+v", got)
	}
	if _, err := hex.DecodeString(got.BinaryDigest); err != nil {
		t.Fatalf("version flag binary digest is not SHA-256 hex: %q", got.BinaryDigest)
	}
}

func TestExecutableSHA256MatchesCurrentTestBinary(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(body))
	got, err := executableSHA256()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("executable digest = %s, want %s", got, want)
	}
}
