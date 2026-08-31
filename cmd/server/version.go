package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
)

type versionInfo struct {
	Version      string `json:"version"`
	SourceDigest string `json:"sourceDigest"`
	BinaryDigest string `json:"binaryDigest"`
	GoVersion    string `json:"goVersion"`
	GOOS         string `json:"goos"`
	GOARCH       string `json:"goarch"`
}

// writeVersionInfo is deliberately independent of configuration and external
// dependencies so an operator can inspect an unpacked binary before install.
func writeVersionInfo(w io.Writer, releaseVersion, buildSourceDigest, buildBinaryDigest string) error {
	return json.NewEncoder(w).Encode(versionInfo{
		Version: releaseVersion, SourceDigest: buildSourceDigest, BinaryDigest: buildBinaryDigest,
		GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	})
}

// executableSHA256 identifies the exact bytes the current process started
// from. It complements sourceDigest: identical source can still be compiled
// with a different toolchain or flags, and a rollout must not hide that drift.
func executableSHA256() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open executable: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash executable: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
