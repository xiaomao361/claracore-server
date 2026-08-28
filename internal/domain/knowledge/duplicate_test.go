package knowledge

import (
	"context"
	"testing"

	"claracore-server/internal/platform"
)

func TestFindCardDuplicateCandidatesMarksExactAndExcludesSelf(t *testing.T) {
	svc, _ := newTestService(t)
	first := seedCard(t, svc, spacePriv, func(in *CreateCardInput) {
		in.Title, in.Distilled, in.Understanding = "发布验收", "分层验证", "构建成功不等于业务成功"
	})
	second := seedCard(t, svc, spacePriv, func(in *CreateCardInput) {
		in.Title, in.Distilled, in.Understanding = "发布验收", "分层验证", "构建成功不等于业务成功"
	})

	// Exact matching must still work when semantic vectors are unavailable.
	got, err := svc.FindCardDuplicateCandidates(context.Background(), tenantA, second.ID, []platform.ID{spacePriv})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Card.ID != first.ID || !got[0].Exact {
		t.Fatalf("unexpected duplicate candidates: %+v", got)
	}
}

func TestNormalizeDuplicateTextOnlyFoldsCaseAndWhitespace(t *testing.T) {
	if normalizeDuplicateText("  Release\n Gate ") != normalizeDuplicateText("releasegate") {
		t.Fatal("case and whitespace should be normalized")
	}
	if normalizeDuplicateText("release-gate") == normalizeDuplicateText("release gate") {
		t.Fatal("punctuation must remain meaningful")
	}
}

func TestFindCardDuplicateCandidatesIncludesHighSemanticMatch(t *testing.T) {
	svc, _, _ := newSearchService(t, "发布", "验收", "分层")
	canonical := seedCard(t, svc, spacePriv, func(in *CreateCardInput) {
		in.Title, in.Distilled, in.Understanding = "发布验收", "分层验证", "构建通过后还要验证业务"
	})
	target := seedCard(t, svc, spacePriv, func(in *CreateCardInput) {
		in.Title, in.Distilled, in.Understanding = "发布后的验收", "需要分层检查", "不能只看构建结果"
	})
	indexAll(t, svc)

	got, err := svc.FindCardDuplicateCandidates(context.Background(), tenantA, target.ID, []platform.ID{spacePriv})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Card.ID != canonical.ID || got[0].Exact || !got[0].Semantic || got[0].Score < duplicateSemanticThreshold {
		t.Fatalf("unexpected semantic duplicate candidates: %+v", got)
	}
}
