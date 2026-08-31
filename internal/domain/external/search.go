package external

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"

	"claracore-server/internal/platform"
)

const (
	externalCandidateCap = 500
	defaultSearchLimit   = 10
	maxSearchLimit       = 50
)

// Search compares the query with body-free mirror vectors. The authoritative
// body is neither loaded nor returned; callers receive enough source metadata
// to open the upstream document deliberately.
func (s *Service) Search(ctx context.Context, in SearchInput) (SearchResult, error) {
	if s.store == nil {
		return SearchResult{}, errors.New("external: search store is unavailable")
	}
	if in.TenantID == "" || len(in.SpaceIDs) == 0 {
		return SearchResult{}, errors.New("external: search scope is incomplete")
	}
	query := strings.TrimSpace(in.Query)
	limit := in.Limit
	if limit == 0 {
		limit = defaultSearchLimit
	}
	if limit < 1 || limit > maxSearchLimit {
		return SearchResult{}, errors.New("external: search limit must be between 1 and 50")
	}
	if query == "" || s.embedder == nil {
		return SearchResult{Hits: []SearchHit{}}, nil
	}
	vectors, err := s.embedder.Embed(ctx, []string{query})
	if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
		return SearchResult{Hits: []SearchHit{}}, nil
	}
	candidates, err := s.store.SearchCandidates(ctx, SearchQuery{
		TenantID: in.TenantID, SpaceIDs: append([]platform.ID(nil), in.SpaceIDs...),
		Query: query, Model: s.embedder.Model(), Limit: externalCandidateCap,
	})
	if err != nil {
		return SearchResult{}, err
	}
	queryVector := vectors[0]
	hits := make([]SearchHit, 0, len(candidates))
	compatible := false
	for _, candidate := range candidates {
		if candidate.Document.TenantID != in.TenantID || !containsID(in.SpaceIDs, candidate.Document.SpaceID) {
			return SearchResult{}, errors.New("external: store returned a candidate outside the search scope")
		}
		if len(candidate.Vector) != len(queryVector) {
			continue
		}
		compatible = true
		hits = append(hits, SearchHit{
			Document: candidate.Document, SourceName: candidate.SourceName, SourceSystem: candidate.SourceSystem,
			LastSyncStatus: candidate.LastSyncStatus, LastSucceededAt: candidate.LastSucceededAt,
			Score: cosine(queryVector, candidate.Vector),
		})
	}
	slices.SortFunc(hits, func(a, b SearchHit) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		if !a.Document.UpdatedAt.Equal(b.Document.UpdatedAt) {
			if a.Document.UpdatedAt.After(b.Document.UpdatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(string(a.Document.ID), string(b.Document.ID))
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return SearchResult{Hits: hits, SemanticAvailable: compatible}, nil
}

func containsID(ids []platform.ID, id platform.ID) bool {
	return slices.Contains(ids, id)
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
