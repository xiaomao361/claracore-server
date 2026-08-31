package governance

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

func TestGovernancePagesAreBoundedStableAndTenantScoped(t *testing.T) {
	store := newFakeStore()
	svc, clock := newTestService(t, store)
	ids := platform.NewIDGenerator(clock)
	ctx := context.Background()
	tenantID, foreignTenantID := ids.New(), ids.New()
	userIDs := []platform.ID{ids.New(), ids.New(), ids.New(), ids.New()}
	for i, id := range userIDs {
		store.users[id] = User{
			ID: id, TenantID: tenantID, Username: "member-" + string(rune('a'+i)),
			PasswordHash: "must-never-reach-summary", DisplayName: "Member", Role: RoleMember,
			Status: StatusActive, CreatedAt: clock.Now(),
		}
	}
	foreignID := ids.New()
	store.users[foreignID] = User{ID: foreignID, TenantID: foreignTenantID, Username: "foreign", PasswordHash: "foreign-secret", Status: StatusActive}

	first, err := svc.ListUserSummariesPage(ctx, GovernancePageInput{TenantID: tenantID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == "" || first.Items[0].ID != userIDs[3] || first.Items[1].ID != userIDs[2] {
		t.Fatalf("unexpected first member page: %+v", first)
	}
	// A newer insert between requests must not shift or duplicate the next page.
	newerID := ids.New()
	store.users[newerID] = User{ID: newerID, TenantID: tenantID, Username: "newer", PasswordHash: "secret", Status: StatusActive}
	second, err := svc.ListUserSummariesPage(ctx, GovernancePageInput{TenantID: tenantID, Cursor: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.Items[0].ID != userIDs[1] || second.Items[1].ID != userIDs[0] {
		t.Fatalf("keyset page shifted after insert: %+v", second)
	}
	for _, item := range append(first.Items, second.Items...) {
		if item.TenantID != tenantID || item.ID == foreignID || item.ID == newerID {
			t.Fatalf("page leaked or shifted scope: %+v", item)
		}
	}

	if _, err := svc.ListUserSummariesPage(ctx, GovernancePageInput{TenantID: tenantID, Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid cursor must fail closed, got %v", err)
	}
}

func TestGovernanceSpaceGrantAndTokenOwnershipProjections(t *testing.T) {
	store := newFakeStore()
	svc, clock := newTestService(t, store)
	ids := platform.NewIDGenerator(clock)
	ctx := context.Background()
	tenantID, foreignTenantID := ids.New(), ids.New()
	ownerID, memberID, foreignUserID := ids.New(), ids.New(), ids.New()
	store.users[ownerID] = User{ID: ownerID, TenantID: tenantID, Username: "owner", PasswordHash: "owner-secret", Status: StatusActive}
	store.users[memberID] = User{ID: memberID, TenantID: tenantID, Username: "member", PasswordHash: "member-secret", Status: StatusDisabled}
	store.users[foreignUserID] = User{ID: foreignUserID, TenantID: foreignTenantID, Username: "foreign", PasswordHash: "foreign-secret", Status: StatusActive}
	projectID, personalID := ids.New(), ids.New()
	store.spaces[projectID] = Space{ID: projectID, TenantID: tenantID, Type: SpaceProject, Name: "Project", OwnerUserID: &ownerID}
	store.spaces[personalID] = Space{ID: personalID, TenantID: tenantID, Type: SpacePersonal, Name: "Personal", OwnerUserID: &ownerID}
	store.perms[permKey{projectID, memberID}] = Permission{SpaceID: projectID, UserID: memberID, CanRead: true, GrantedBy: ownerID}

	grants, err := svc.ListSpaceGrantPage(ctx, GovernancePageInput{TenantID: tenantID, SpaceID: projectID, Limit: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(grants.Items) != 1 || grants.Items[0].User.ID != memberID || grants.Items[0].User.IsActive() || grants.Items[0].Permission == nil {
		t.Fatalf("grant projection must exclude owner and foreign tenant: %+v", grants.Items)
	}
	if _, err := svc.ListSpaceGrantPage(ctx, GovernancePageInput{TenantID: tenantID, SpaceID: personalID}); !errors.Is(err, ErrSpaceForbidden) {
		t.Fatalf("personal space must stay outside governance page, got %v", err)
	}
	if _, err := svc.ListSpaceGrantPage(ctx, GovernancePageInput{SpaceID: projectID}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing tenant must fail before storage lookup, got %v", err)
	}

	agentID, tokenID := ids.New(), ids.New()
	store.tokens[tokenID] = Token{ID: tokenID, TenantID: tenantID, UserID: memberID, AgentID: agentID, Name: "Laptop", Prefix: "ccs_test", Hash: "credential-hash", CreatedAt: time.Now()}
	if _, err := svc.FindOwnTokenSummary(ctx, tenantID, ownerID, tokenID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("another user's token must be indistinguishable from missing, got %v", err)
	}
	got, err := svc.FindOwnTokenSummary(ctx, tenantID, memberID, tokenID)
	if err != nil || got.ID != tokenID || got.Prefix != "ccs_test" {
		t.Fatalf("owner should receive safe token summary: %+v err=%v", got, err)
	}
}

func TestGovernanceTokenSpaceAndAgentPages(t *testing.T) {
	store := newFakeStore()
	svc, clock := newTestService(t, store)
	ids := platform.NewIDGenerator(clock)
	ctx := context.Background()
	tenantID, userID := ids.New(), ids.New()
	store.users[userID] = User{ID: userID, TenantID: tenantID, Username: "member", PasswordHash: "secret", Status: StatusActive}

	if got, err := svc.FindUserSummary(ctx, tenantID, userID); err != nil || got.ID != userID {
		t.Fatalf("safe user lookup failed: %+v err=%v", got, err)
	}
	if _, err := svc.FindUserSummary(ctx, "", userID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing user scope must fail, got %v", err)
	}

	agentID := ids.New()
	tokenIDs := []platform.ID{ids.New(), ids.New(), ids.New()}
	for i, tokenID := range tokenIDs {
		store.tokens[tokenID] = Token{
			ID: tokenID, TenantID: tenantID, UserID: userID, AgentID: agentID,
			Name: "device", Prefix: "ccs", Hash: "hash", CreatedAt: clock.Now().Add(time.Duration(i) * time.Minute),
		}
	}
	firstTokens, err := svc.ListTokenSummariesPage(ctx, GovernancePageInput{TenantID: tenantID, UserID: userID, Limit: 2})
	if err != nil || len(firstTokens.Items) != 2 || firstTokens.NextCursor == "" {
		t.Fatalf("bounded token first page failed: %+v err=%v", firstTokens, err)
	}
	secondTokens, err := svc.ListTokenSummariesPage(ctx, GovernancePageInput{TenantID: tenantID, UserID: userID, Cursor: firstTokens.NextCursor, Limit: 2})
	if err != nil || len(secondTokens.Items) != 1 || secondTokens.Items[0].ID != tokenIDs[0] {
		t.Fatalf("bounded token second page failed: %+v err=%v", secondTokens, err)
	}
	if _, err := svc.ListTokenSummariesPage(ctx, GovernancePageInput{TenantID: tenantID}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("token list without owner must fail, got %v", err)
	}
	if _, err := svc.FindOwnTokenSummary(ctx, tenantID, userID, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("token lookup without id must fail, got %v", err)
	}

	personalID, teamID, projectID := ids.New(), ids.New(), ids.New()
	store.spaces[personalID] = Space{ID: personalID, TenantID: tenantID, Type: SpacePersonal, OwnerUserID: &userID}
	store.spaces[teamID] = Space{ID: teamID, TenantID: tenantID, Type: SpaceTeam}
	store.spaces[projectID] = Space{ID: projectID, TenantID: tenantID, Type: SpaceProject, OwnerUserID: &userID}
	spaces, err := svc.ListSpacePage(ctx, GovernancePageInput{TenantID: tenantID, Limit: 1})
	if err != nil || len(spaces.Items) != 1 || spaces.Items[0].ID != projectID || spaces.NextCursor == "" {
		t.Fatalf("space page must exclude personal and paginate: %+v err=%v", spaces, err)
	}
	lastSpace, err := svc.ListSpacePage(ctx, GovernancePageInput{TenantID: tenantID, Cursor: spaces.NextCursor})
	if err != nil || len(lastSpace.Items) != 1 || lastSpace.Items[0].ID != teamID {
		t.Fatalf("space next page failed: %+v err=%v", lastSpace, err)
	}

	// Exercise the overview hard cap with more agents than the caller may request.
	for i := 0; i < 22; i++ {
		agent := ids.New()
		token := ids.New()
		store.tokens[token] = Token{ID: token, TenantID: tenantID, UserID: userID, AgentID: agent, Hash: string(token), CreatedAt: clock.Now().Add(time.Duration(i) * time.Hour)}
	}
	agents, err := svc.ListAgentAccessSummaries(ctx, tenantID, userID, 999)
	if err != nil || len(agents) != maxOverviewAgentLimit {
		t.Fatalf("agent overview must clamp its limit: len=%d err=%v", len(agents), err)
	}
	if _, err := svc.ListAgentAccessSummaries(ctx, "", userID, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("agent summary without tenant must fail, got %v", err)
	}
	if defaults, err := svc.ListAgentAccessSummaries(ctx, tenantID, userID, 0); err != nil || len(defaults) != 6 {
		t.Fatalf("agent summary default limit failed: len=%d err=%v", len(defaults), err)
	}

	// Both malformed base64 and valid base64 carrying a non-ULID fail closed.
	for _, cursor := range []string{"*", base64.RawURLEncoding.EncodeToString([]byte("not-an-id"))} {
		if _, err := svc.ListSpacePage(ctx, GovernancePageInput{TenantID: tenantID, Cursor: cursor}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid cursor %q must fail closed, got %v", cursor, err)
		}
	}
	if _, err := svc.ListSpacePage(ctx, GovernancePageInput{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing tenant must fail, got %v", err)
	}
	// The public maximum is clamped even if a caller asks for an excessive page.
	if _, err := svc.ListUserSummariesPage(ctx, GovernancePageInput{TenantID: tenantID, Limit: maxGovernancePageLimit + 1}); err != nil {
		t.Fatal(err)
	}
}
