package knowledge

import (
	"context"
	"strings"
	"unicode"

	"claracore-server/internal/platform"
)

// duplicateSemanticThreshold is deliberately conservative: duplicate
// governance is a hint for a human, not another recall path. A lower threshold
// would turn "same topic" into "same fact" and encourage destructive cleanup.
const duplicateSemanticThreshold = 0.90

type CardDuplicateCandidate struct {
	Card     Card
	Score    float64
	Exact    bool
	Semantic bool
}

// FindCardDuplicateCandidates finds active cards that may duplicate the target.
//
// It reuses the normal tenant/space-filtered two-stage card search. The target
// may itself be draft or deprecated, but candidates are intentionally active:
// governance needs a likely canonical card to compare against, not another
// unresolved draft. No write or merge happens here.
func (s *Service) FindCardDuplicateCandidates(ctx context.Context, tenantID, cardID platform.ID, spaces []platform.ID) ([]CardDuplicateCandidate, error) {
	if err := requireSpaces(spaces); err != nil {
		return nil, err
	}
	card, err := s.store.FindCard(ctx, tenantID, cardID, spaces)
	if err != nil {
		return nil, err
	}

	searchSpaces := make([]SearchSpace, 0, len(spaces))
	for _, id := range spaces {
		searchSpaces = append(searchSpaces, SearchSpace{ID: id})
	}
	// Match the exact text shape used by the stored card embedding. Without the
	// same explicit truncation, a very long card could be compared against a
	// vector built from only its first MaxEmbedTextLen runes.
	query := truncateRunes(cardEmbedText(*card), MaxEmbedTextLen)
	if query == "" {
		return []CardDuplicateCandidate{}, nil
	}
	res, err := s.SearchCards(ctx, SearchCardsInput{
		TenantID: tenantID,
		Spaces:   searchSpaces,
		Query:    query,
		Limit:    20,
	})
	if err != nil {
		return nil, err
	}

	want := normalizeDuplicateText(query)
	out := make([]CardDuplicateCandidate, 0, len(res.Hits))
	for _, hit := range res.Hits {
		if hit.Card.ID == card.ID {
			continue
		}
		exact := normalizeDuplicateText(cardEmbedText(hit.Card)) == want
		if !exact && (!hit.Semantic || hit.Score < duplicateSemanticThreshold) {
			continue
		}
		out = append(out, CardDuplicateCandidate{
			Card: hit.Card, Score: hit.Score, Exact: exact, Semantic: hit.Semantic,
		})
	}
	return out, nil
}

// normalizeDuplicateText only powers exact-equality labeling. It deliberately
// does not remove punctuation or rewrite words: that would silently turn a
// semantic judgment into an "exact" one.
func normalizeDuplicateText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(s))
}
