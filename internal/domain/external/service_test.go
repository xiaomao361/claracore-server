package external

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

type fakeStore struct {
	source    MirrorSource
	states    map[string]IndexState
	touched   []MirrorDocument
	saved     []MirrorDocument
	vectors   []DocumentVector
	begun     []SyncRun
	completed int
	failed    string
	missing   int
	health    []SourceHealth
	healthErr error
	runs      []SyncRunReceipt
	runsErr   error
	runLimit  int
	queued    []SyncRequest
	queueOK   bool
	claim     SyncRequestClaim
	claimOK   bool
	claimName string
	resolved  []struct {
		claim SyncRequestClaim
		retry bool
	}
	search    []SearchCandidate
	searchErr error
	lastQuery SearchQuery
}

func (f *fakeStore) EnsureSource(_ context.Context, source MirrorSource) (MirrorSource, error) {
	if f.source.ID == "" {
		f.source = source
	}
	return f.source, nil
}
func (f *fakeStore) QueueSyncRequest(_ context.Context, request SyncRequest) (bool, error) {
	f.queued = append(f.queued, request)
	return f.queueOK, nil
}
func (f *fakeStore) ClaimSyncRequest(_ context.Context, tenantID platform.ID, sourceName string, claimToken platform.ID, leaseUntil, _ time.Time) (SyncRequestClaim, bool, error) {
	f.claimName = sourceName
	claim := f.claim
	claim.TenantID, claim.ClaimToken, claim.LeaseUntil = tenantID, claimToken, leaseUntil
	return claim, f.claimOK, nil
}
func (f *fakeStore) ResolveSyncRequest(_ context.Context, claim SyncRequestClaim, retry bool, _ time.Time) error {
	f.resolved = append(f.resolved, struct {
		claim SyncRequestClaim
		retry bool
	}{claim: claim, retry: retry})
	return nil
}
func (f *fakeStore) BeginSync(_ context.Context, run SyncRun) error {
	f.begun = append(f.begun, run)
	return nil
}
func (f *fakeStore) FindIndexState(_ context.Context, _ platform.ID, externalID, _ string) (IndexState, bool, error) {
	state, ok := f.states[externalID]
	return state, ok, nil
}
func (f *fakeStore) TouchDocument(_ context.Context, document MirrorDocument) error {
	f.touched = append(f.touched, document)
	return nil
}
func (f *fakeStore) SaveIndexedDocument(_ context.Context, document MirrorDocument, vector DocumentVector) error {
	f.saved = append(f.saved, document)
	f.vectors = append(f.vectors, vector)
	return nil
}
func (f *fakeStore) CompleteSync(_ context.Context, _, _ platform.ID, _ SyncReport, _ time.Time) (int, error) {
	f.completed++
	return f.missing, nil
}
func (f *fakeStore) FailSync(_ context.Context, _, _ platform.ID, reason string, _ time.Time) error {
	f.failed = reason
	return nil
}
func (f *fakeStore) ListSourceHealth(context.Context, platform.ID) ([]SourceHealth, error) {
	return f.health, f.healthErr
}
func (f *fakeStore) ListSyncRuns(_ context.Context, _ platform.ID, limit int) ([]SyncRunReceipt, error) {
	f.runLimit = limit
	return f.runs, f.runsErr
}
func (f *fakeStore) SearchCandidates(_ context.Context, q SearchQuery) ([]SearchCandidate, error) {
	f.lastQuery = q
	return f.search, f.searchErr
}

type fakeRemote struct {
	pages     map[int]Page
	documents map[string]Document
	gets      int
	err       error
}

func (f *fakeRemote) ListDocuments(_ context.Context, offset, _ int) (Page, error) {
	if f.err != nil {
		return Page{}, f.err
	}
	return f.pages[offset], nil
}
func (f *fakeRemote) GetDocument(_ context.Context, documentID, _ string) (Document, error) {
	f.gets++
	if f.err != nil {
		return Document{}, f.err
	}
	return f.documents[documentID], nil
}

type fakeEmbedder struct {
	model   string
	inputs  [][]string
	vectors [][]float32
	err     error
}

func (f *fakeEmbedder) Model() string { return f.model }
func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.inputs = append(f.inputs, append([]string(nil), texts...))
	if f.err != nil {
		return nil, f.err
	}
	return f.vectors, nil
}

func newTestService(store *fakeStore, embedder *fakeEmbedder) *Service {
	clock := platform.FixedClock{T: time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)}
	return NewService(store, clock, platform.NewIDGenerator(clock), embedder)
}

func TestRunsValidatesScopeAndBoundsLimit(t *testing.T) {
	store := &fakeStore{runs: []SyncRunReceipt{{ID: "run-1"}}}
	svc := newTestService(store, &fakeEmbedder{})
	got, err := svc.Runs(context.Background(), "tenant", 0)
	if err != nil || len(got) != 1 || store.runLimit != 20 {
		t.Fatalf("default receipt query failed: got=%+v limit=%d err=%v", got, store.runLimit, err)
	}
	if _, err := svc.Runs(context.Background(), "tenant", 999); err != nil || store.runLimit != 100 {
		t.Fatalf("receipt limit must be capped at 100: limit=%d err=%v", store.runLimit, err)
	}
	if _, err := svc.Runs(context.Background(), "", 10); err == nil {
		t.Fatal("empty tenant scope must be rejected")
	}
}

func TestSyncRequestLifecycleIsDurableAndValidated(t *testing.T) {
	store := &fakeStore{
		source:  MirrorSource{ID: "source", TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "team-docs", System: "shared-docs", Status: SourceActive},
		queueOK: true,
		claim:   SyncRequestClaim{RequestID: "request", ExternalSourceID: "source"}, claimOK: true,
	}
	svc := newTestService(store, &fakeEmbedder{})
	accepted, err := svc.RequestSync(context.Background(), RequestSyncInput{
		TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: " team-docs ", System: "shared-docs",
	})
	if err != nil || !accepted || len(store.queued) != 1 || store.queued[0].ExternalSourceID != "source" {
		t.Fatalf("durable request was not queued: accepted=%v queued=%+v err=%v", accepted, store.queued, err)
	}
	claim, claimed, err := svc.ClaimSyncRequest(context.Background(), "tenant", " team-docs ", 10*time.Minute)
	if err != nil || !claimed || claim.RequestID != "request" || claim.ClaimToken == "" || store.claimName != "team-docs" {
		t.Fatalf("durable request was not claimed: claimed=%v claim=%+v name=%q err=%v", claimed, claim, store.claimName, err)
	}
	if err := svc.ResolveSyncRequest(context.Background(), claim, true); err != nil || len(store.resolved) != 1 || !store.resolved[0].retry {
		t.Fatalf("retry resolution was not delegated: resolved=%+v err=%v", store.resolved, err)
	}
	if _, err := svc.RequestSync(context.Background(), RequestSyncInput{}); err == nil {
		t.Fatal("incomplete source identity must be rejected")
	}
	if _, _, err := svc.ClaimSyncRequest(context.Background(), "tenant", "team-docs", time.Second); err == nil {
		t.Fatal("unsafe claim duration must be rejected")
	}
	if err := svc.ResolveSyncRequest(context.Background(), SyncRequestClaim{}, false); err == nil {
		t.Fatal("incomplete claim resolution must be rejected")
	}
}

func testMeta(id, revision string) DocumentMeta {
	return DocumentMeta{
		SourceID: "capture-1", DocumentID: id, Kind: "yuque_document", System: "shared-docs",
		Title: "Runbook " + id, SourceRef: "https://docs.example/" + id, Revision: revision,
		UpdatedAt: "2026-08-29T00:00:00Z",
	}
}

func TestSyncIndexesChangedDocumentsAndTouchesUnchanged(t *testing.T) {
	store := &fakeStore{
		states:  map[string]IndexState{"stable": {DocumentID: "existing-id", Revision: "rev-stable", Ready: true}},
		missing: 2,
	}
	changed := testMeta("changed", "rev-new")
	stable := testMeta("stable", "rev-stable")
	remote := &fakeRemote{
		pages: map[int]Page{0: {Items: []DocumentMeta{stable, changed}, Total: 2, Limit: 2, Offset: 0}},
		documents: map[string]Document{
			"changed": {DocumentMeta: changed, Content: "authoritative body"},
		},
	}
	embedder := &fakeEmbedder{model: "bge-m3", vectors: [][]float32{{0.1, 0.2}}}
	report, err := newTestService(store, embedder).Sync(context.Background(), SyncInput{
		TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "team-docs", System: "shared-docs", PageSize: 2, MaxDuration: 10 * time.Minute, Remote: remote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Seen != 2 || report.Indexed != 1 || report.Unchanged != 1 || report.Missing != 2 || store.completed != 1 || store.failed != "" {
		t.Fatalf("unexpected report/store state: report=%+v store=%+v", report, store)
	}
	if remote.gets != 1 || len(store.touched) != 1 || store.touched[0].ID != "existing-id" || len(store.saved) != 1 {
		t.Fatalf("unchanged content must avoid body fetch and embedding: gets=%d touched=%+v saved=%+v", remote.gets, store.touched, store.saved)
	}
	if len(embedder.inputs) != 1 || len(embedder.inputs[0]) != 1 || !strings.Contains(embedder.inputs[0][0], "authoritative body") {
		t.Fatalf("changed body must be embedded transiently: %+v", embedder.inputs)
	}
	if len(store.vectors) != 1 || store.vectors[0].InputHash == "" || store.vectors[0].Revision != "rev-new" {
		t.Fatalf("derived vector receipt is incomplete: %+v", store.vectors)
	}
}

func TestSyncFailureKeepsSnapshotOpenAndWritesFailureReceipt(t *testing.T) {
	store := &fakeStore{states: map[string]IndexState{}}
	meta := testMeta("changed", "rev-new")
	remote := &fakeRemote{
		pages:     map[int]Page{0: {Items: []DocumentMeta{meta}, Total: 1, Limit: 1, Offset: 0}},
		documents: map[string]Document{"changed": {DocumentMeta: meta, Content: "body"}},
	}
	embedder := &fakeEmbedder{model: "bge-m3", err: errors.New("model unavailable")}
	_, err := newTestService(store, embedder).Sync(context.Background(), SyncInput{
		TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "team-docs", System: "shared-docs", PageSize: 1, MaxDuration: 10 * time.Minute, Remote: remote,
	})
	if err == nil || store.completed != 0 || store.failed == "" {
		t.Fatalf("failed sync must not finalize missing detection: err=%v completed=%d failed=%q", err, store.completed, store.failed)
	}
	if len(store.saved) != 0 {
		t.Fatal("failed embedding must not publish metadata without its matching vector")
	}
}

func TestSyncRejectsSnapshotDriftAndDocumentRevisionRace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		remote   *fakeRemote
		pageSize int
		want     string
	}{
		{
			name: "total changes between pages",
			remote: &fakeRemote{pages: map[int]Page{
				0: {Items: []DocumentMeta{testMeta("a", "1")}, Total: 2, Limit: 1, Offset: 0},
				1: {Items: []DocumentMeta{testMeta("b", "1")}, Total: 3, Limit: 1, Offset: 1},
			}, documents: map[string]Document{"a": {DocumentMeta: testMeta("a", "1"), Content: "a"}}},
			pageSize: 1, want: "changed during enumeration",
		},
		{
			name: "revision changes before body fetch",
			remote: &fakeRemote{pages: map[int]Page{
				0: {Items: []DocumentMeta{testMeta("a", "listed")}, Total: 1, Limit: 1, Offset: 0},
			}, documents: map[string]Document{"a": {DocumentMeta: testMeta("a", "newer"), Content: "a"}}},
			pageSize: 1, want: "changed between enumeration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{states: map[string]IndexState{}}
			embedder := &fakeEmbedder{model: "bge-m3", vectors: [][]float32{{1}}}
			_, err := newTestService(store, embedder).Sync(context.Background(), SyncInput{
				TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "team-docs", System: "shared-docs", PageSize: tc.pageSize, MaxDuration: 10 * time.Minute, Remote: tc.remote,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) || store.completed != 0 || store.failed == "" {
				t.Fatalf("expected guarded failure %q, got err=%v completed=%d failed=%q", tc.want, err, store.completed, store.failed)
			}
		})
	}
}

func TestIndexTextIsBoundedAndHashMatchesDerivedInput(t *testing.T) {
	doc := Document{DocumentMeta: testMeta("a", "1"), Content: strings.Repeat("知", MaxIndexTextRunes+100)}
	text := indexText(doc)
	if len([]rune(text)) != MaxIndexTextRunes {
		t.Fatalf("index text must be rune-bounded: %d", len([]rune(text)))
	}
	if len(hashText(text)) != 64 {
		t.Fatal("derived input hash must be SHA-256 hex")
	}
}

func TestHealthIsTenantScopedThroughStore(t *testing.T) {
	store := &fakeStore{health: []SourceHealth{{Name: "atlas-docs", DocumentCount: 3}}}
	svc := newTestService(store, &fakeEmbedder{model: "bge-m3"})
	health, err := svc.Health(context.Background(), "tenant")
	if err != nil || len(health) != 1 || health[0].DocumentCount != 3 {
		t.Fatalf("unexpected health: %+v err=%v", health, err)
	}
	if _, err := svc.Health(context.Background(), ""); err == nil {
		t.Fatal("empty tenant scope must fail")
	}
	store.healthErr = errors.New("db unavailable")
	if _, err := svc.Health(context.Background(), "tenant"); err == nil {
		t.Fatal("store health failure must propagate")
	}
}

func TestSearchIsPermissionScopedRankedAndBodyFree(t *testing.T) {
	now := time.Date(2026, 8, 29, 7, 30, 0, 0, time.UTC)
	lastSuccess := now.Add(-time.Hour)
	store := &fakeStore{search: []SearchCandidate{
		{Document: MirrorDocument{ID: "b", TenantID: "tenant", SpaceID: "team", Title: "B", SourceRef: "https://docs/b", Revision: "2", UpdatedAt: now}, SourceName: "atlas", SourceSystem: "shared-docs", LastSyncStatus: SyncFailed, LastSucceededAt: &lastSuccess, Vector: []float32{0, 1}},
		{Document: MirrorDocument{ID: "a", TenantID: "tenant", SpaceID: "team", Title: "A", SourceRef: "https://docs/a", Revision: "1", UpdatedAt: now.Add(-time.Minute)}, SourceName: "atlas", SourceSystem: "shared-docs", LastSyncStatus: SyncSucceeded, LastSucceededAt: &lastSuccess, Vector: []float32{1, 0}},
	}}
	embedder := &fakeEmbedder{model: "bge-m3", vectors: [][]float32{{1, 0}}}
	result, err := newTestService(store, embedder).Search(context.Background(), SearchInput{
		TenantID: "tenant", SpaceIDs: []platform.ID{"team"}, Query: "runbook", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SemanticAvailable || len(result.Hits) != 2 || result.Hits[0].Document.ID != "a" {
		t.Fatalf("unexpected ranked result: %+v", result)
	}
	if store.lastQuery.TenantID != "tenant" || len(store.lastQuery.SpaceIDs) != 1 || store.lastQuery.SpaceIDs[0] != "team" || store.lastQuery.Limit != externalCandidateCap {
		t.Fatalf("permission scope must reach the store unchanged: %+v", store.lastQuery)
	}
	if result.Hits[0].Document.SourceRef == "" || result.Hits[0].SourceName != "atlas" {
		t.Fatalf("source metadata must remain actionable: %+v", result.Hits[0])
	}
}

func TestSearchFailsClosedOnStoreScopeEscapeAndDegradesWithoutVectors(t *testing.T) {
	store := &fakeStore{search: []SearchCandidate{{Document: MirrorDocument{ID: "x", TenantID: "other", SpaceID: "team"}, Vector: []float32{1}}}}
	embedder := &fakeEmbedder{model: "model", vectors: [][]float32{{1}}}
	_, err := newTestService(store, embedder).Search(context.Background(), SearchInput{TenantID: "tenant", SpaceIDs: []platform.ID{"team"}, Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("store scope escape must fail closed: %v", err)
	}

	store.search = nil
	embedder.err = errors.New("offline")
	result, err := newTestService(store, embedder).Search(context.Background(), SearchInput{TenantID: "tenant", SpaceIDs: []platform.ID{"team"}, Query: "x"})
	if err != nil || result.SemanticAvailable || len(result.Hits) != 0 {
		t.Fatalf("embedder failure should degrade explicitly: %+v err=%v", result, err)
	}
	clock := platform.FixedClock{T: time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)}
	without := NewService(store, clock, platform.NewIDGenerator(clock), nil)
	result, err = without.Search(context.Background(), SearchInput{TenantID: "tenant", SpaceIDs: []platform.ID{"team"}, Query: "x"})
	if err != nil || result.SemanticAvailable {
		t.Fatalf("disabled semantic search should return an honest empty result: %+v err=%v", result, err)
	}
}

func TestSearchValidatesScopeAndLimit(t *testing.T) {
	svc := newTestService(&fakeStore{}, &fakeEmbedder{model: "model", vectors: [][]float32{{1}}})
	for _, input := range []SearchInput{
		{TenantID: "", SpaceIDs: []platform.ID{"space"}, Query: "x"},
		{TenantID: "tenant", Query: "x"},
		{TenantID: "tenant", SpaceIDs: []platform.ID{"space"}, Query: "x", Limit: 51},
	} {
		if _, err := svc.Search(context.Background(), input); err == nil {
			t.Fatalf("invalid search input must fail: %+v", input)
		}
	}
}

func TestSearchPropagatesStoreFailureAndSkipsWrongDimension(t *testing.T) {
	embedder := &fakeEmbedder{model: "model", vectors: [][]float32{{1, 0}}}
	store := &fakeStore{searchErr: errors.New("database unavailable")}
	svc := newTestService(store, embedder)
	_, err := svc.Search(context.Background(), SearchInput{TenantID: "tenant", SpaceIDs: []platform.ID{"space"}, Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("store failure must propagate: %v", err)
	}
	store.searchErr = nil
	store.search = []SearchCandidate{{Document: MirrorDocument{ID: "x", TenantID: "tenant", SpaceID: "space"}, Vector: []float32{1}}}
	result, err := svc.Search(context.Background(), SearchInput{TenantID: "tenant", SpaceIDs: []platform.ID{"space"}, Query: "x"})
	if err != nil || result.SemanticAvailable || len(result.Hits) != 0 {
		t.Fatalf("wrong-model dimension must not be compared: %+v err=%v", result, err)
	}
}

func TestSearchUsesStableRecencyAndIDTies(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{search: []SearchCandidate{
		{Document: MirrorDocument{ID: "b", TenantID: "tenant", SpaceID: "space", UpdatedAt: now}, Vector: []float32{0, 0}},
		{Document: MirrorDocument{ID: "a", TenantID: "tenant", SpaceID: "space", UpdatedAt: now}, Vector: []float32{0, 0}},
		{Document: MirrorDocument{ID: "new", TenantID: "tenant", SpaceID: "space", UpdatedAt: now.Add(time.Minute)}, Vector: []float32{0, 0}},
	}}
	result, err := newTestService(store, &fakeEmbedder{model: "model", vectors: [][]float32{{1, 0}}}).Search(context.Background(), SearchInput{
		TenantID: "tenant", SpaceIDs: []platform.ID{"space"}, Query: "x", Limit: 2,
	})
	if err != nil || len(result.Hits) != 2 || result.Hits[0].Document.ID != "new" || result.Hits[1].Document.ID != "a" {
		t.Fatalf("zero-score ties must be deterministic: %+v err=%v", result, err)
	}
}

func TestSearchRejectsMissingStoreAndEmptyQueryAvoidsEmbedding(t *testing.T) {
	clock := platform.FixedClock{T: time.Now().UTC()}
	embedder := &fakeEmbedder{model: "model", vectors: [][]float32{{1}}}
	if _, err := NewService(nil, clock, platform.NewIDGenerator(clock), embedder).Search(context.Background(), SearchInput{TenantID: "tenant", SpaceIDs: []platform.ID{"space"}, Query: "x"}); err == nil {
		t.Fatal("missing store must fail")
	}
	result, err := NewService(&fakeStore{}, clock, platform.NewIDGenerator(clock), embedder).Search(context.Background(), SearchInput{TenantID: "tenant", SpaceIDs: []platform.ID{"space"}, Query: "  "})
	if err != nil || result.SemanticAvailable || len(embedder.inputs) != 0 {
		t.Fatalf("empty query must not call embedding: %+v inputs=%+v err=%v", result, embedder.inputs, err)
	}
}

func TestMirrorModelsRejectIncompleteOrOversizedState(t *testing.T) {
	now := time.Now().UTC()
	source := MirrorSource{ID: "id", TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "docs", System: "system", Status: SourceActive, CreatedAt: now, UpdatedAt: now}
	if err := source.validate(); err != nil {
		t.Fatal(err)
	}
	invalidSource := source
	invalidSource.Status = "disabled"
	if err := invalidSource.validate(); err == nil {
		t.Fatal("unsupported source status must fail")
	}
	invalidSource = source
	invalidSource.Name = strings.Repeat("n", 121)
	if err := invalidSource.validate(); err == nil {
		t.Fatal("oversized source name must fail")
	}

	doc := MirrorDocument{
		ID: "doc", TenantID: "tenant", SpaceID: "space", ExternalSourceID: "source", UpstreamSourceID: "capture",
		ExternalID: "external", Kind: "kind", Title: "title", SourceRef: "https://docs.example/doc", Revision: "rev",
		State: DocumentActive, LastSeenRunID: "run", CreatedAt: now, UpdatedAt: now,
	}
	if err := doc.validate(); err != nil {
		t.Fatal(err)
	}
	invalidDoc := doc
	invalidDoc.State = DocumentMissing
	if err := invalidDoc.validate(); err == nil {
		t.Fatal("sync cannot publish a missing document")
	}
	invalidDoc = doc
	invalidDoc.SourceRef = strings.Repeat("x", 2049)
	if err := invalidDoc.validate(); err == nil {
		t.Fatal("oversized source reference must fail")
	}

	vector := DocumentVector{ID: "vector", TenantID: "tenant", ExternalDocumentID: "doc", Model: "model", Revision: "rev", InputHash: strings.Repeat("a", 64), Vector: []float32{1}, CreatedAt: now, UpdatedAt: now}
	if err := vector.validate(); err != nil {
		t.Fatal(err)
	}
	vector.Vector = nil
	if err := vector.validate(); err == nil {
		t.Fatal("empty vector must fail")
	}
	vector.Vector = []float32{1}
	vector.Model = strings.Repeat("m", 65)
	if err := vector.validate(); err == nil {
		t.Fatal("oversized vector model must fail")
	}
}

func TestSyncRejectsInvalidBoundsDuplicatesAndBadVectors(t *testing.T) {
	validMeta := testMeta("a", "1")
	for _, tc := range []struct {
		name     string
		input    SyncInput
		embedder *fakeEmbedder
		want     string
	}{
		{
			name: "page size", input: SyncInput{TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "docs", System: "shared-docs", PageSize: 0, MaxDuration: time.Minute, Remote: &fakeRemote{}},
			embedder: &fakeEmbedder{model: "model"}, want: "page size",
		},
		{
			name: "duration", input: SyncInput{TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "docs", System: "shared-docs", PageSize: 1, MaxDuration: time.Second, Remote: &fakeRemote{}},
			embedder: &fakeEmbedder{model: "model"}, want: "duration",
		},
		{
			name: "duplicate", input: SyncInput{TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "docs", System: "shared-docs", PageSize: 2, MaxDuration: time.Minute, Remote: &fakeRemote{
				pages: map[int]Page{0: {Items: []DocumentMeta{validMeta, validMeta}, Total: 2, Limit: 2, Offset: 0}}, documents: map[string]Document{"a": {DocumentMeta: validMeta, Content: "body"}},
			}}, embedder: &fakeEmbedder{model: "model", vectors: [][]float32{{1}}}, want: "duplicate",
		},
		{
			name: "vector count", input: SyncInput{TenantID: "tenant", SpaceID: "space", Kind: "atlas_mcp", Name: "docs", System: "shared-docs", PageSize: 1, MaxDuration: time.Minute, Remote: &fakeRemote{
				pages: map[int]Page{0: {Items: []DocumentMeta{validMeta}, Total: 1, Limit: 1, Offset: 0}}, documents: map[string]Document{"a": {DocumentMeta: validMeta, Content: "body"}},
			}}, embedder: &fakeEmbedder{model: "model", vectors: nil}, want: "0 vectors",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{states: map[string]IndexState{}}
			_, err := newTestService(store, tc.embedder).Sync(context.Background(), tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSyncRejectsMissingDependenciesBeforeStartingReceipt(t *testing.T) {
	clock := platform.FixedClock{T: time.Now().UTC()}
	svc := NewService(nil, clock, platform.NewIDGenerator(clock), &fakeEmbedder{model: "model"})
	_, err := svc.Sync(context.Background(), SyncInput{Remote: &fakeRemote{}, PageSize: 1, MaxDuration: time.Minute})
	if err == nil || !strings.Contains(err.Error(), "dependencies") {
		t.Fatalf("missing store must fail before any source call: %v", err)
	}
}
