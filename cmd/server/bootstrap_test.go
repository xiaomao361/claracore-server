package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBootstrapPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte("temporary-password-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readBootstrapPassword(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "temporary-password-123" {
		t.Fatalf("密码读取结果错误: %q", got)
	}
}

func TestReadBootstrapPasswordRejectsUnsafeFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("relative path", func(t *testing.T) {
		if _, err := readBootstrapPassword("password"); err == nil {
			t.Fatal("相对路径应被拒绝")
		}
	})

	t.Run("permissions", func(t *testing.T) {
		path := filepath.Join(dir, "shared-password")
		if err := os.WriteFile(path, []byte("temporary-password-123"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readBootstrapPassword(path); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("宽松权限应被拒绝，得到 %v", err)
		}
	})

	t.Run("multiple lines", func(t *testing.T) {
		path := filepath.Join(dir, "multiline-password")
		if err := os.WriteFile(path, []byte("first-line\nsecond-line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readBootstrapPassword(path); err == nil {
			t.Fatal("多行密码文件应被拒绝")
		}
	})
}
