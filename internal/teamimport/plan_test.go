package teamimport

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/portableexport"
)

const (
	testTenant = "01K3V000000000000000000000"
	testUser   = "01K3V000000000000000000001"
	testAgent  = "01K3V000000000000000000002"
	testTeam   = "01K3V000000000000000000003"
	testPerson = "01K3V000000000000000000004"
)

func TestAnalyzeBuildsExplicitCredentialTakeoverPlan(t *testing.T) {
	archive := openPlanFixture(t, validPlanContent(), nil)
	plan, err := Analyze(archive, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.OK || plan.TenantID != testTenant || plan.SelectedAdmin != "alice" || plan.Records != 4 || plan.MembersDisabledOnImport != 0 || !plan.IDsPreserved || !plan.RequiresEmptyDeployment || plan.CredentialsImported || len(plan.AdminCandidates) != 1 || plan.AdminCandidates[0] != "alice" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestAnalyzeWithoutSelectionListsCandidatesAndDisablesEveryone(t *testing.T) {
	archive := openPlanFixture(t, validPlanContent(), nil)
	plan, err := Analyze(archive, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.SelectedAdmin != "" || plan.MembersDisabledOnImport != 1 || len(plan.AdminCandidates) != 1 {
		t.Fatalf("unexpected planning-only takeover boundary: %+v", plan)
	}
}

func TestAnalyzeRejectsSemanticAndSchemaAttacks(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
	}{
		{name: "unknown member field", mutate: func(files map[string][]byte) {
			files["members.jsonl"] = bytes.Replace(files["members.jsonl"], []byte(`"status":"active"`), []byte(`"status":"active","password_hash":"leak"`), 1)
		}},
		{name: "missing personal space", mutate: func(files map[string][]byte) {
			files["spaces.jsonl"] = []byte(spaceJSON(testTeam, "team", "Team", nil) + "\n")
		}},
		{name: "dangling memory author", mutate: func(files map[string][]byte) {
			files["memories.jsonl"] = []byte(`{"id":"01K3V000000000000000000005","space_id":"01K3V000000000000000000003","author_user_id":"01K3V000000000000000000099","author_agent_id":null,"title":"Memory","body":"body","status":"active","copied_from_type":null,"copied_from_id":null,"version":1,"created_at":"2026-08-29T01:02:03Z","updated_at":"2026-08-29T01:02:03Z"}` + "\n")
		}},
		{name: "invalid permission", mutate: func(files map[string][]byte) {
			files["space_permissions.jsonl"] = []byte(`{"space_id":"01K3V000000000000000000004","user_id":"01K3V000000000000000000001","can_read":false,"can_write":true,"granted_by":"01K3V000000000000000000001","created_at":"2026-08-29T01:02:03Z"}` + "\n")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := validPlanContent()
			tc.mutate(content)
			archive := openPlanFixture(t, content, nil)
			if _, err := Analyze(archive, "alice"); err == nil {
				t.Fatal("invalid import archive passed semantic preflight")
			}
		})
	}
}

func TestAnalyzeRejectsNonAdminTakeover(t *testing.T) {
	content := validPlanContent()
	content["members.jsonl"] = bytes.Replace(content["members.jsonl"], []byte(`"role":"admin"`), []byte(`"role":"member"`), 1)
	archive := openPlanFixture(t, content, nil)
	if _, err := Analyze(archive, "alice"); err == nil || !strings.Contains(err.Error(), "no administrator") {
		t.Fatalf("non-admin takeover error = %v", err)
	}
}

func validPlanContent() map[string][]byte {
	content := make(map[string][]byte)
	for _, name := range portableexport.DataFiles() {
		content[name] = nil
	}
	content[portableexport.ReadmeName] = []byte("portable team export\n")
	content["members.jsonl"] = []byte(`{"id":"` + testUser + `","username":"alice","display_name":"Alice","role":"admin","must_change_password":false,"status":"active","created_at":"2026-08-29T01:02:03Z"}` + "\n")
	content["agents.jsonl"] = []byte(`{"id":"` + testAgent + `","user_id":"` + testUser + `","slug":"codex","display_name":"Codex","created_at":"2026-08-29T01:02:03Z"}` + "\n")
	owner := testUser
	content["spaces.jsonl"] = []byte(spaceJSON(testTeam, "team", "Team", nil) + "\n" + spaceJSON(testPerson, "personal", "Alice", &owner) + "\n")
	return content
}

func spaceJSON(id, kind, name string, owner *string) string {
	ownerJSON := "null"
	if owner != nil {
		ownerJSON = `"` + *owner + `"`
	}
	return `{"id":"` + id + `","type":"` + kind + `","name":"` + name + `","owner_user_id":` + ownerJSON + `,"created_at":"2026-08-29T01:02:03Z"}`
}

func openPlanFixture(t *testing.T, content map[string][]byte, mutateManifest func(*portableexport.Manifest)) *portableexport.Archive {
	t.Helper()
	manifest := portableexport.Manifest{
		Format: portableexport.FormatName, FormatVersion: portableexport.FormatVersion,
		SchemaVersion: "0010", TenantID: testTenant, TenantName: "Team",
		TenantCreatedAt: time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC),
		GeneratedAt:     time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC), Complete: true,
		Excluded: portableexport.ExcludedData(),
	}
	for _, name := range portableexport.ContentFiles() {
		body := content[name]
		records := int64(0)
		if strings.HasSuffix(name, ".jsonl") {
			records = int64(bytes.Count(body, []byte("\n")))
		}
		sum := sha256.Sum256(body)
		manifest.Files = append(manifest.Files, portableexport.FileReceipt{Name: name, Records: records, SHA256: hex.EncodeToString(sum[:])})
	}
	if mutateManifest != nil {
		mutateManifest(&manifest)
	}
	var raw bytes.Buffer
	zw := zip.NewWriter(&raw)
	for _, name := range portableexport.ContentFiles() {
		writer, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content[name]); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := zw.Create(portableexport.ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(writer).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "export.zip")
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := portableexport.OpenFile(path, portableexport.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { archive.Close() })
	return archive
}
