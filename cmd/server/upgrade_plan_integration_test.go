//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUpgradeCheckCommandReportsCurrentRealDatabase(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./cmd/server", "-check-upgrade")
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("upgrade command failed against integration database: %v\n%s", err, stderr.String())
	}
	var got upgradePlanResponse
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("upgrade command stdout is not clean JSON: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if !got.OK || got.Status != "current" || got.MySQL != "reachable" || got.Pending != 0 ||
		got.CurrentSchemaVersion != "0013" || got.TargetSchemaVersion != "0013" || got.RequiresMaintenance || got.RollbackMode != "binary_only" {
		t.Fatalf("unexpected upgrade command response: %+v", got)
	}
}
