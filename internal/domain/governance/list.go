package governance

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"claracore-server/internal/platform"
)

const (
	defaultGovernancePageLimit = 30
	maxGovernancePageLimit     = 100
	maxOverviewAgentLimit      = 20
)

// UserSummary is the governance-list projection. PasswordHash is deliberately
// absent: member and permission pages never need authentication material.
type UserSummary struct {
	ID           platform.ID
	TenantID     platform.ID
	Username     string
	DisplayName  string
	Role         string
	MustChangePw bool
	Status       string
	CreatedAt    time.Time
}

func (u UserSummary) IsActive() bool { return u.Status == StatusActive }

// TokenSummary is safe for human governance views. The bearer hash is not part
// of this read model and therefore cannot accidentally reach the Web host.
type TokenSummary struct {
	ID         platform.ID
	TenantID   platform.ID
	UserID     platform.ID
	AgentID    platform.ID
	Name       string
	Prefix     string
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

type SpaceGrantSummary struct {
	User       UserSummary
	Permission *Permission
}

type AgentAccessSummary struct {
	AgentID      platform.ID
	TokenCount   int
	ActiveTokens int
	LastUsedAt   *time.Time
	LastIssuedAt time.Time
}

type GovernancePageInput struct {
	TenantID platform.ID
	UserID   platform.ID
	SpaceID  platform.ID
	Cursor   string
	Limit    int
}

type UserSummaryPage struct {
	Items      []UserSummary
	NextCursor string
}

type TokenSummaryPage struct {
	Items      []TokenSummary
	NextCursor string
}

type SpacePage struct {
	Items      []Space
	NextCursor string
}

type SpaceGrantPage struct {
	Space      Space
	Items      []SpaceGrantSummary
	NextCursor string
}

type GovernancePageQuery struct {
	TenantID    platform.ID
	UserID      platform.ID
	SpaceID     platform.ID
	OwnerUserID platform.ID
	BeforeID    platform.ID
	Limit       int
}

func (s *Service) ListUserSummariesPage(ctx context.Context, in GovernancePageInput) (UserSummaryPage, error) {
	query, limit, err := governancePageQuery(in)
	if err != nil {
		return UserSummaryPage{}, err
	}
	items, err := s.store.ListUserSummaries(ctx, query)
	if err != nil {
		return UserSummaryPage{}, err
	}
	page := UserSummaryPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeGovernanceCursor(items[limit-1].ID)
	}
	return page, nil
}

func (s *Service) FindUserSummary(ctx context.Context, tenantID, userID platform.ID) (UserSummary, error) {
	if tenantID == "" || userID == "" {
		return UserSummary{}, fmt.Errorf("%w: 缺少用户身份", ErrInvalidArgument)
	}
	return s.store.FindUserSummary(ctx, tenantID, userID)
}

func (s *Service) ListTokenSummariesPage(ctx context.Context, in GovernancePageInput) (TokenSummaryPage, error) {
	if in.UserID == "" {
		return TokenSummaryPage{}, fmt.Errorf("%w: 缺少用户", ErrInvalidArgument)
	}
	query, limit, err := governancePageQuery(in)
	if err != nil {
		return TokenSummaryPage{}, err
	}
	items, err := s.store.ListTokenSummaries(ctx, query)
	if err != nil {
		return TokenSummaryPage{}, err
	}
	page := TokenSummaryPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeGovernanceCursor(items[limit-1].ID)
	}
	return page, nil
}

func (s *Service) ListSpacePage(ctx context.Context, in GovernancePageInput) (SpacePage, error) {
	query, limit, err := governancePageQuery(in)
	if err != nil {
		return SpacePage{}, err
	}
	items, err := s.store.ListGovernedSpaces(ctx, query)
	if err != nil {
		return SpacePage{}, err
	}
	page := SpacePage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeGovernanceCursor(items[limit-1].ID)
	}
	return page, nil
}

func (s *Service) ListSpaceGrantPage(ctx context.Context, in GovernancePageInput) (SpaceGrantPage, error) {
	if in.TenantID == "" {
		return SpaceGrantPage{}, fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}
	if in.SpaceID == "" {
		return SpaceGrantPage{}, fmt.Errorf("%w: 缺少空间", ErrInvalidArgument)
	}
	space, err := s.store.FindSpace(ctx, in.TenantID, in.SpaceID)
	if err != nil {
		return SpaceGrantPage{}, err
	}
	if space.Type == SpacePersonal {
		return SpaceGrantPage{}, ErrSpaceForbidden
	}
	query, limit, err := governancePageQuery(in)
	if err != nil {
		return SpaceGrantPage{}, err
	}
	if space.OwnerUserID != nil {
		query.OwnerUserID = *space.OwnerUserID
	}
	items, err := s.store.ListSpaceGrantSummaries(ctx, query)
	if err != nil {
		return SpaceGrantPage{}, err
	}
	page := SpaceGrantPage{Space: *space, Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeGovernanceCursor(items[limit-1].User.ID)
	}
	return page, nil
}

func (s *Service) FindOwnTokenSummary(ctx context.Context, tenantID, userID, tokenID platform.ID) (TokenSummary, error) {
	if tenantID == "" || userID == "" || tokenID == "" {
		return TokenSummary{}, fmt.Errorf("%w: 缺少 token 身份", ErrInvalidArgument)
	}
	return s.store.FindTokenSummary(ctx, tenantID, userID, tokenID)
}

func (s *Service) ListAgentAccessSummaries(ctx context.Context, tenantID, userID platform.ID, limit int) ([]AgentAccessSummary, error) {
	if tenantID == "" || userID == "" {
		return nil, fmt.Errorf("%w: 缺少身份", ErrInvalidArgument)
	}
	if limit <= 0 {
		limit = 6
	}
	if limit > maxOverviewAgentLimit {
		limit = maxOverviewAgentLimit
	}
	return s.store.ListAgentAccessSummaries(ctx, tenantID, userID, limit)
}

func governancePageQuery(in GovernancePageInput) (GovernancePageQuery, int, error) {
	if in.TenantID == "" {
		return GovernancePageQuery{}, 0, fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultGovernancePageLimit
	case limit > maxGovernancePageLimit:
		limit = maxGovernancePageLimit
	}
	before, err := decodeGovernanceCursor(in.Cursor)
	if err != nil {
		return GovernancePageQuery{}, 0, err
	}
	return GovernancePageQuery{
		TenantID: in.TenantID, UserID: in.UserID, SpaceID: in.SpaceID,
		BeforeID: before, Limit: limit + 1,
	}, limit, nil
}

func encodeGovernanceCursor(id platform.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeGovernanceCursor(cursor string) (platform.ID, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	id := platform.ID(raw)
	if _, ok := id.Timestamp(); !ok {
		return "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	return id, nil
}
