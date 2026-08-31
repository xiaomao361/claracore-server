package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteArchiveIsDeterministicAndNormalized(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "release")
	if err := os.MkdirAll(filepath.Join(source, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "README.md"), "read me\n", 0o600)
	writeTestFile(t, filepath.Join(source, "claracore-server"), "binary\n", 0o711)
	longDir := filepath.Join(source, strings.Repeat("long-directory-", 8))
	if err := os.MkdirAll(longDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(longDir, "NOTICE"), "notice\n", 0o644)

	first := archiveBytes(t, source)
	differentTime := time.Date(2037, 6, 5, 4, 3, 2, 0, time.FixedZone("different", 9*60*60))
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, differentTime, differentTime)
	})
	if err != nil {
		t.Fatal(err)
	}
	second := archiveBytes(t, source)
	if !bytes.Equal(first, second) {
		t.Fatal("archive bytes changed when only source timestamps changed")
	}

	zr, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if !zr.ModTime.IsZero() || zr.Name != "" || zr.Comment != "" || zr.OS != 255 {
		t.Fatalf("gzip header is not normalized: mod=%s name=%q comment=%q os=%d", zr.ModTime, zr.Name, zr.Comment, zr.OS)
	}

	type metadata struct {
		name       string
		mode       int64
		uid, gid   int
		uname      string
		gname      string
		modTime    time.Time
		accessTime time.Time
		changeTime time.Time
	}
	var got []metadata
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, metadata{
			name: header.Name, mode: header.Mode, uid: header.Uid, gid: header.Gid,
			uname: header.Uname, gname: header.Gname, modTime: header.ModTime,
			accessTime: header.AccessTime, changeTime: header.ChangeTime,
		})
	}
	wantNames := []string{
		"release/",
		"release/README.md",
		"release/claracore-server",
		"release/deploy/",
		"release/" + strings.Repeat("long-directory-", 8) + "/",
		"release/" + strings.Repeat("long-directory-", 8) + "/NOTICE",
	}
	gotNames := make([]string, 0, len(got))
	for _, item := range got {
		gotNames = append(gotNames, item.name)
		if item.uid != 0 || item.gid != 0 || item.uname != "" || item.gname != "" {
			t.Fatalf("archive owner metadata is not normalized: %+v", item)
		}
		if !item.modTime.Equal(epoch) || !item.accessTime.IsZero() || !item.changeTime.IsZero() {
			t.Fatalf("archive timestamps are not normalized: %+v", item)
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("archive order/names = %q, want %q", gotNames, wantNames)
	}
	if got[1].mode != 0o644 || got[2].mode != 0o755 || got[3].mode != 0o755 {
		t.Fatalf("archive modes are not normalized: %+v", got)
	}
}

func TestWriteArchiveRejectsUnsafeInputs(t *testing.T) {
	if err := writeArchive(io.Discard, "relative-release"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative source error = %v", err)
	}

	parent := t.TempDir()
	source := filepath.Join(parent, "release")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "safe"), "safe\n", 0o644)
	symlink := filepath.Join(source, "linked")
	if err := os.Symlink(filepath.Join(source, "safe"), symlink); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := writeArchive(io.Discard, source); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("symlink source error = %v", err)
	}

	rootLink := filepath.Join(parent, "release-link")
	if err := os.Symlink(source, rootLink); err != nil {
		t.Fatal(err)
	}
	if err := writeArchive(io.Discard, rootLink); err == nil || !strings.Contains(err.Error(), "ordinary directory") {
		t.Fatalf("symlink root error = %v", err)
	}
}

func archiveBytes(t *testing.T, source string) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := writeArchive(&output, source); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}
