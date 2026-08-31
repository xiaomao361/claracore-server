// Package external defines the source-side contract for read-only knowledge mirrors.
//
// External documents are deliberately not Memories or Cards. Their body remains
// authoritative at the source; ClaraCore may retain metadata and derived vectors,
// but callers cannot edit, promote, or label the mirror itself.
package external

import "context"

// DocumentMeta is the stable, body-free identity returned by source enumeration.
type DocumentMeta struct {
	SourceID   string
	DocumentID string
	Kind       string
	System     string
	Title      string
	SourceRef  string
	Revision   string
	UpdatedAt  string
}

// Page is one bounded slice of a source enumeration.
type Page struct {
	Items  []DocumentMeta
	Total  int
	Limit  int
	Offset int
}

// Document is fetched transiently when a synchronizer needs to derive an index.
// Content must not be persisted as the local mirror's authoritative body.
type Document struct {
	DocumentMeta
	Content    string
	SourceType string
	CapturedAt string
}

// Source is the minimum contract required for local index synchronization.
// Search-only MCP services do not satisfy this interface.
type Source interface {
	ListDocuments(ctx context.Context, offset, limit int) (Page, error)
	GetDocument(ctx context.Context, documentID, sourceID string) (Document, error)
}
