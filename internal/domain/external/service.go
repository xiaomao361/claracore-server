package external

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

type Service struct {
	store    Store
	clock    platform.Clock
	ids      *platform.IDGenerator
	embedder ports.Embedder
}

func NewService(store Store, clock platform.Clock, ids *platform.IDGenerator, embedder ports.Embedder) *Service {
	return &Service{store: store, clock: clock, ids: ids, embedder: embedder}
}

func (s *Service) Health(ctx context.Context, tenantID platform.ID) ([]SourceHealth, error) {
	if s.store == nil || tenantID == "" {
		return nil, errors.New("external: health scope is incomplete")
	}
	return s.store.ListSourceHealth(ctx, tenantID)
}

func (s *Service) Runs(ctx context.Context, tenantID platform.ID, limit int) ([]SyncRunReceipt, error) {
	if s.store == nil || tenantID == "" {
		return nil, errors.New("external: sync receipt scope is incomplete")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return s.store.ListSyncRuns(ctx, tenantID, limit)
}

type RequestSyncInput struct {
	TenantID platform.ID
	SpaceID  platform.ID
	Kind     string
	Name     string
	System   string
}

// RequestSync durably coalesces one pending manual refresh per source. A
// claimed request is separate, so one follow-up may be queued while a worker
// is already executing the previous request.
func (s *Service) RequestSync(ctx context.Context, in RequestSyncInput) (bool, error) {
	if s.store == nil || s.clock == nil || s.ids == nil {
		return false, errors.New("external: sync request dependencies are incomplete")
	}
	now := s.clock.Now()
	sourceInput := MirrorSource{
		ID: s.ids.New(), TenantID: in.TenantID, SpaceID: in.SpaceID,
		Kind: strings.TrimSpace(in.Kind), Name: strings.TrimSpace(in.Name), System: strings.TrimSpace(in.System),
		Status: SourceActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := sourceInput.validate(); err != nil {
		return false, err
	}
	source, err := s.store.EnsureSource(ctx, sourceInput)
	if err != nil {
		return false, err
	}
	return s.store.QueueSyncRequest(ctx, SyncRequest{
		ID: s.ids.New(), TenantID: source.TenantID, ExternalSourceID: source.ID, RequestedAt: now,
	})
}

func (s *Service) ClaimSyncRequest(ctx context.Context, tenantID platform.ID, sourceName string, maxDuration time.Duration) (SyncRequestClaim, bool, error) {
	if s.store == nil || s.clock == nil || s.ids == nil || tenantID == "" || strings.TrimSpace(sourceName) == "" {
		return SyncRequestClaim{}, false, errors.New("external: sync request claim scope is incomplete")
	}
	if maxDuration < time.Minute || maxDuration > 24*time.Hour {
		return SyncRequestClaim{}, false, errors.New("external: sync request claim duration must be between 1m and 24h")
	}
	now := s.clock.Now()
	return s.store.ClaimSyncRequest(ctx, tenantID, strings.TrimSpace(sourceName), s.ids.New(), now.Add(maxDuration+10*time.Second), now)
}

func (s *Service) ResolveSyncRequest(ctx context.Context, claim SyncRequestClaim, retry bool) error {
	if s.store == nil || s.clock == nil || claim.RequestID == "" || claim.TenantID == "" || claim.ExternalSourceID == "" || claim.ClaimToken == "" {
		return errors.New("external: sync request resolution is incomplete")
	}
	return s.store.ResolveSyncRequest(ctx, claim, retry, s.clock.Now())
}

type SyncInput struct {
	TenantID    platform.ID
	SpaceID     platform.ID
	Kind        string
	Name        string
	System      string
	PageSize    int
	MaxDuration time.Duration
	Remote      Source
}

// Sync enumerates one complete upstream snapshot. Bodies exist only in the
// current call while vectors are derived; Store never receives them.
func (s *Service) Sync(ctx context.Context, in SyncInput) (report SyncReport, retErr error) {
	if s.store == nil || s.clock == nil || s.ids == nil || s.embedder == nil || in.Remote == nil {
		return report, errors.New("external: sync dependencies are incomplete")
	}
	if in.PageSize < 1 || in.PageSize > 500 {
		return report, errors.New("external: page size must be between 1 and 500")
	}
	if in.MaxDuration < time.Minute || in.MaxDuration > 24*time.Hour {
		return report, errors.New("external: max sync duration must be between 1m and 24h")
	}
	now := s.clock.Now()
	sourceInput := MirrorSource{
		ID: s.ids.New(), TenantID: in.TenantID, SpaceID: in.SpaceID,
		Kind: strings.TrimSpace(in.Kind), Name: strings.TrimSpace(in.Name), System: strings.TrimSpace(in.System),
		Status: SourceActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := sourceInput.validate(); err != nil {
		return report, err
	}
	source, err := s.store.EnsureSource(ctx, sourceInput)
	if err != nil {
		return report, err
	}
	run := SyncRun{ID: s.ids.New(), TenantID: source.TenantID, ExternalSourceID: source.ID, Status: SyncRunning, StartedAt: now, LeaseUntil: now.Add(in.MaxDuration + 10*time.Second), CreatedAt: now}
	if err := s.store.BeginSync(ctx, run); err != nil {
		return report, err
	}
	report.SourceID, report.RunID = source.ID, run.ID
	defer func() {
		if retErr == nil {
			return
		}
		reason := truncateRunes(retErr.Error(), MaxSyncErrorLen)
		if failErr := s.store.FailSync(context.WithoutCancel(ctx), source.ID, run.ID, reason, s.clock.Now()); failErr != nil {
			retErr = errors.Join(retErr, failErr)
		}
	}()

	seenIDs := make(map[string]struct{})
	expectedTotal := -1
	for offset := 0; ; {
		page, err := in.Remote.ListDocuments(ctx, offset, in.PageSize)
		if err != nil {
			return report, err
		}
		if page.Offset != offset || page.Limit != in.PageSize || page.Total < 0 || page.Total > MaxSyncDocuments {
			return report, errors.New("external: source returned an invalid enumeration page")
		}
		if expectedTotal < 0 {
			expectedTotal = page.Total
		} else if page.Total != expectedTotal {
			return report, errors.New("external: source changed during enumeration; retry a fresh snapshot")
		}
		if len(page.Items) == 0 && offset < expectedTotal {
			return report, errors.New("external: source enumeration ended before its declared total")
		}
		if err := s.syncPage(ctx, source, run.ID, in.Remote, page.Items, seenIDs, &report); err != nil {
			return report, err
		}
		offset += len(page.Items)
		if offset == expectedTotal {
			break
		}
		if offset > expectedTotal || len(page.Items) < in.PageSize {
			return report, errors.New("external: source returned an inconsistent enumeration total")
		}
	}
	missing, err := s.store.CompleteSync(ctx, source.ID, run.ID, report, s.clock.Now())
	if err != nil {
		return report, err
	}
	report.Missing = missing
	return report, nil
}

type pageDocument struct {
	meta     DocumentMeta
	document Document
	mirror   MirrorDocument
	text     string
}

func (s *Service) syncPage(ctx context.Context, source MirrorSource, runID platform.ID, remote Source, items []DocumentMeta, seen map[string]struct{}, report *SyncReport) error {
	pending := make([]pageDocument, 0, len(items))
	for _, meta := range items {
		if meta.System != source.System || strings.TrimSpace(meta.DocumentID) == "" || strings.TrimSpace(meta.Revision) == "" {
			return errors.New("external: enumerated document escaped source identity boundary")
		}
		if _, duplicate := seen[meta.DocumentID]; duplicate {
			return fmt.Errorf("external: duplicate document identity %q in one snapshot", meta.DocumentID)
		}
		seen[meta.DocumentID] = struct{}{}
		state, found, err := s.store.FindIndexState(ctx, source.ID, meta.DocumentID, s.embedder.Model())
		if err != nil {
			return err
		}
		mirror := mirrorFromMeta(s.ids.New(), source, runID, meta, s.clock.Now())
		if found && state.Ready && state.Revision == meta.Revision {
			mirror.ID = state.DocumentID
			if err := mirror.validate(); err != nil {
				return err
			}
			if err := s.store.TouchDocument(ctx, mirror); err != nil {
				return err
			}
			report.Seen++
			report.Unchanged++
			continue
		}
		doc, err := remote.GetDocument(ctx, meta.DocumentID, meta.SourceID)
		if err != nil {
			return err
		}
		if doc.DocumentID != meta.DocumentID || doc.SourceID != meta.SourceID || doc.System != source.System || doc.Revision != meta.Revision {
			return errors.New("external: document changed between enumeration and body fetch")
		}
		text := indexText(doc)
		if text == "" {
			return fmt.Errorf("external: document %q has no indexable text", meta.DocumentID)
		}
		pending = append(pending, pageDocument{meta: meta, document: doc, mirror: mirror, text: text})
	}
	if len(pending) == 0 {
		return nil
	}
	texts := make([]string, len(pending))
	for i := range pending {
		texts[i] = pending[i].text
	}
	vectors, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("external: embedding batch failed: %w", err)
	}
	if len(vectors) != len(pending) {
		return fmt.Errorf("external: embedder returned %d vectors for %d documents", len(vectors), len(pending))
	}
	dim := len(vectors[0])
	if dim == 0 {
		return errors.New("external: embedder returned an empty vector")
	}
	for i, item := range pending {
		if len(vectors[i]) != dim {
			return errors.New("external: embedder returned inconsistent vector dimensions")
		}
		vector := DocumentVector{
			ID: s.ids.New(), TenantID: source.TenantID, ExternalDocumentID: item.mirror.ID,
			Model: s.embedder.Model(), Revision: item.meta.Revision, InputHash: hashText(item.text), Vector: vectors[i],
			CreatedAt: s.clock.Now(), UpdatedAt: s.clock.Now(),
		}
		if err := item.mirror.validate(); err != nil {
			return err
		}
		if err := vector.validate(); err != nil {
			return err
		}
		if err := s.store.SaveIndexedDocument(ctx, item.mirror, vector); err != nil {
			return err
		}
		report.Seen++
		report.Indexed++
	}
	return nil
}

func mirrorFromMeta(id platform.ID, source MirrorSource, runID platform.ID, meta DocumentMeta, at time.Time) MirrorDocument {
	return MirrorDocument{
		ID: id, TenantID: source.TenantID, SpaceID: source.SpaceID, ExternalSourceID: source.ID,
		UpstreamSourceID: meta.SourceID, ExternalID: meta.DocumentID, Kind: meta.Kind,
		Title: meta.Title, SourceRef: meta.SourceRef, Revision: meta.Revision, UpstreamUpdatedAt: meta.UpdatedAt,
		State: DocumentActive, LastSeenRunID: runID, CreatedAt: at, UpdatedAt: at,
	}
}

func indexText(doc Document) string {
	return truncateRunes(strings.TrimSpace(strings.TrimSpace(doc.Title)+"\n\n"+strings.TrimSpace(doc.Content)), MaxIndexTextRunes)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
