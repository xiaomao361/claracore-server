package portableexport

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestVerifyReaderAtAcceptsCanonicalArchive(t *testing.T) {
	manifest, content := validFixture(t)
	raw := archiveFixture(t, manifest, content, nil, nil)
	got, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Format != FormatName || got.FormatVersion != FormatVersion || got.SchemaVersion != "0010" || got.TenantID != manifest.TenantID || got.Files != len(ContentFiles()) || got.Records != 1 || got.ArchiveBytes != int64(len(raw)) || got.UncompressedBytes <= 0 {
		t.Fatalf("unexpected verification: %+v", got)
	}
}

func TestVerifyReaderAtRejectsManifestAndContentAttacks(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest, map[string][]byte)
	}{
		{name: "unsupported format", mutate: func(m *Manifest, _ map[string][]byte) { m.Format = "other" }},
		{name: "incomplete", mutate: func(m *Manifest, _ map[string][]byte) { m.Complete = false }},
		{name: "non UTC time", mutate: func(m *Manifest, _ map[string][]byte) {
			m.GeneratedAt = m.GeneratedAt.In(time.FixedZone("east", 8*60*60))
		}},
		{name: "invalid tenant", mutate: func(m *Manifest, _ map[string][]byte) { m.TenantID = "../tenant" }},
		{name: "missing exclusion", mutate: func(m *Manifest, _ map[string][]byte) { m.Excluded = m.Excluded[:len(m.Excluded)-1] }},
		{name: "reordered receipt", mutate: func(m *Manifest, _ map[string][]byte) { m.Files[0], m.Files[1] = m.Files[1], m.Files[0] }},
		{name: "invalid digest", mutate: func(m *Manifest, _ map[string][]byte) { m.Files[1].SHA256 = strings.Repeat("A", 64) }},
		{name: "record mismatch", mutate: func(m *Manifest, _ map[string][]byte) { m.Files[5].Records++ }},
		{name: "tampered content", mutate: func(_ *Manifest, files map[string][]byte) { files["memories.jsonl"] = []byte("{\"id\":\"changed\"}\n") }},
		{name: "non object JSONL", mutate: func(m *Manifest, files map[string][]byte) {
			body := []byte("[]\n")
			files["memories.jsonl"] = body
			setReceipt(m, "memories.jsonl", body, 1)
		}},
		{name: "blank JSONL record", mutate: func(m *Manifest, files map[string][]byte) {
			body := []byte("\n")
			files["memories.jsonl"] = body
			setReceipt(m, "memories.jsonl", body, 1)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest, content := validFixture(t)
			tc.mutate(&manifest, content)
			raw := archiveFixture(t, manifest, content, nil, nil)
			if _, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), DefaultLimits()); err == nil {
				t.Fatal("malformed archive passed verification")
			}
		})
	}
}

func TestVerifyReaderAtRejectsZIPBoundaryAttacks(t *testing.T) {
	manifest, content := validFixture(t)
	t.Run("unknown manifest field", func(t *testing.T) {
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.Replace(body, []byte(`"complete":true`), []byte(`"complete":true,"unexpected":1`), 1)
		raw := archiveFixture(t, manifest, content, body, nil)
		if _, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), DefaultLimits()); err == nil {
			t.Fatal("unknown manifest field passed verification")
		}
	})
	t.Run("trailing manifest JSON", func(t *testing.T) {
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, []byte("\n{}\n")...)
		raw := archiveFixture(t, manifest, content, body, nil)
		if _, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), DefaultLimits()); err == nil {
			t.Fatal("trailing manifest JSON passed verification")
		}
	})
	t.Run("duplicate entry", func(t *testing.T) {
		raw := archiveFixture(t, manifest, content, nil, []zipFixtureEntry{{name: ReadmeName, body: []byte("duplicate")}})
		if _, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), DefaultLimits()); err == nil {
			t.Fatal("duplicate entry passed verification")
		}
	})
	t.Run("unsafe extra entry", func(t *testing.T) {
		raw := archiveFixture(t, manifest, content, nil, []zipFixtureEntry{{name: "../escape", body: []byte("x")}})
		if _, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), DefaultLimits()); err == nil {
			t.Fatal("unsafe path passed verification")
		}
	})
	t.Run("symlink entry", func(t *testing.T) {
		raw := archiveFixture(t, manifest, content, nil, nil, ReadmeName)
		if _, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), DefaultLimits()); err == nil {
			t.Fatal("symlink entry passed verification")
		}
	})
	t.Run("uncompressed limit", func(t *testing.T) {
		raw := archiveFixture(t, manifest, content, nil, nil)
		limits := DefaultLimits()
		limits.MaxUncompressedBytes = 1
		if _, err := VerifyReaderAt(bytes.NewReader(raw), int64(len(raw)), limits); err == nil {
			t.Fatal("oversized uncompressed archive passed verification")
		}
	})
}

func TestVerifyFileRejectsSymlink(t *testing.T) {
	manifest, content := validFixture(t)
	raw := archiveFixture(t, manifest, content, nil, nil)
	dir := t.TempDir()
	target := dir + "/export.zip"
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	link := dir + "/link.zip"
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(link, DefaultLimits()); err == nil {
		t.Fatal("symlink archive passed verification")
	}
}

func TestOpenFileKeepsOnlyVerifiedDatasetsAvailable(t *testing.T) {
	manifest, content := validFixture(t)
	raw := archiveFixture(t, manifest, content, nil, nil)
	path := t.TempDir() + "/export.zip"
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := OpenFile(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	copyManifest := archive.Manifest()
	copyManifest.Files[0].Name = "changed"
	if archive.Manifest().Files[0].Name != ReadmeName {
		t.Fatal("Manifest returned mutable archive state")
	}
	r, err := archive.OpenDataset("memories.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(body, content["memories.jsonl"]) {
		t.Fatalf("dataset body = %q, err=%v", body, err)
	}
	if _, err := archive.OpenDataset(ReadmeName); err == nil {
		t.Fatal("README was exposed as a dataset")
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.OpenDataset("memories.jsonl"); err == nil {
		t.Fatal("closed archive still opened a dataset")
	}
}

type zipFixtureEntry struct {
	name string
	body []byte
}

func validFixture(t *testing.T) (Manifest, map[string][]byte) {
	t.Helper()
	content := make(map[string][]byte)
	content[ReadmeName] = []byte("ClaraCore portable team export\n")
	for _, name := range DataFiles() {
		content[name] = nil
	}
	content["memories.jsonl"] = []byte("{\"id\":\"01K3V000000000000000000001\",\"body\":\"hello\"}\n")
	manifest := Manifest{
		Format: FormatName, FormatVersion: FormatVersion, SchemaVersion: "0010",
		TenantID: "01K3V000000000000000000000", TenantName: "Team",
		TenantCreatedAt: time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC),
		GeneratedAt:     time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC), Complete: true,
		Excluded: ExcludedData(),
	}
	for _, name := range ContentFiles() {
		records := int64(0)
		if name == "memories.jsonl" {
			records = 1
		}
		body := content[name]
		sum := sha256.Sum256(body)
		manifest.Files = append(manifest.Files, FileReceipt{Name: name, Records: records, SHA256: hex.EncodeToString(sum[:])})
	}
	return manifest, content
}

func setReceipt(manifest *Manifest, name string, body []byte, records int64) {
	sum := sha256.Sum256(body)
	for i := range manifest.Files {
		if manifest.Files[i].Name == name {
			manifest.Files[i].Records = records
			manifest.Files[i].SHA256 = hex.EncodeToString(sum[:])
			return
		}
	}
}

func archiveFixture(t *testing.T, manifest Manifest, content map[string][]byte, manifestBody []byte, extra []zipFixtureEntry, symlinkNames ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	symlinks := make(map[string]bool)
	for _, name := range symlinkNames {
		symlinks[name] = true
	}
	for _, name := range ContentFiles() {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if symlinks[name] {
			header.SetMode(os.ModeSymlink | 0o777)
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content[name]); err != nil {
			t.Fatal(err)
		}
	}
	if manifestBody == nil {
		var err error
		manifestBody, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
	}
	writer, err := zw.Create(ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(manifestBody); err != nil {
		t.Fatal(err)
	}
	for _, entry := range extra {
		writer, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
