package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

const DemoSeedKey = "demo-seed-20260831-multi-user-v1"

type DemoCredential struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	AgentSlug string `json:"agentSlug"`
	Token     string `json:"token"`
}

type DemoManifest struct {
	SeedKey     string        `json:"seedKey"`
	TenantID    platform.ID   `json:"tenantId"`
	ReceiptID   platform.ID   `json:"receiptId,omitempty"`
	UserIDs     []platform.ID `json:"userIds"`
	AgentIDs    []platform.ID `json:"agentIds"`
	TokenIDs    []platform.ID `json:"tokenIds"`
	SpaceIDs    []platform.ID `json:"spaceIds"`
	MemoryIDs   []platform.ID `json:"memoryIds"`
	CardIDs     []platform.ID `json:"cardIds"`
	LineIDs     []platform.ID `json:"lineIds"`
	LinkIDs     []platform.ID `json:"linkIds"`
	ActivityIDs []platform.ID `json:"activityIds"`
}

type DemoSeedResult struct {
	Manifest       DemoManifest `json:"manifest"`
	AlreadyApplied bool         `json:"alreadyApplied"`
	Complete       bool         `json:"complete"`
}

type demoReceipt struct {
	SeedKey  string       `json:"seedKey"`
	Manifest DemoManifest `json:"manifest"`
}

type demoProfile struct {
	Username, DisplayName, AgentSlug, AgentName string
	MemoryTitle, MemoryBody                     string
	CardTitle, CardType, CardUnderstanding      string
}

var demoProfiles = []demoProfile{
	{"demo-product", "[演示] 产品负责人", "product-agent", "[演示] 产品 Agent", "[演示] 发布范围判断", "发布演示前先区分必须完成、可延后和仅观察三类范围。", "[演示] 发布范围收敛", "产品协作", "用明确的验收边界代替模糊的完成感，并把未验证项单独留下。"},
	{"demo-engineering", "[演示] 工程师", "engineering-agent", "[演示] 工程 Agent", "[演示] 小步验证习惯", "先完成最小闭环，再根据真实失败补抽象和保护。", "[演示] 小步工程闭环", "工程实践", "代码、测试、部署和业务验收是不同证据层，不能互相替代。"},
	{"demo-design", "[演示] 设计师", "design-agent", "[演示] 设计 Agent", "[演示] 界面评审重点", "先检查层级、状态与可操作性，再讨论装饰性风格。", "[演示] 界面细节检查", "设计工程", "一致的状态反馈和克制的动效比单个视觉亮点更能提升完成度。"},
	{"demo-operations", "[演示] 运维", "operations-agent", "[演示] 运维 Agent", "[演示] 变更前检查", "远端操作前确认实例、备份、回滚点和健康检查口径。", "[演示] 运行变更收口", "运行维护", "发布成功只证明制品运行，仍需实时健康和人工业务验收。"},
	{"demo-support", "[演示] 客户成功", "support-agent", "[演示] 客户成功 Agent", "[演示] 反馈记录边界", "保留用户原话、发生场景和影响，不把一次反馈直接升级为普遍结论。", "[演示] 反馈转知识", "客户反馈", "把事实、解释和待验证假设分开，后续判断才能追溯。"},
}

func NewDemoCredentials() ([]DemoCredential, error) {
	out := make([]DemoCredential, 0, len(demoProfiles))
	for _, profile := range demoProfiles {
		password, err := demoSecret(18)
		if err != nil {
			return nil, err
		}
		token, _, _, err := governance.NewTokenSecret()
		if err != nil {
			return nil, err
		}
		out = append(out, DemoCredential{Username: profile.Username, Password: password, AgentSlug: profile.AgentSlug, Token: token})
	}
	return out, nil
}

func demoSecret(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成演示密码: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func ApplyDemoSeed(ctx context.Context, db *sql.DB, hasher ports.PasswordHasher, tenantID platform.ID, credentials []DemoCredential, now time.Time) (DemoSeedResult, error) {
	if tenantID == "" {
		return DemoSeedResult{}, errors.New("demo seed: 必须显式指定租户")
	}
	if len(credentials) != len(demoProfiles) {
		return DemoSeedResult{}, fmt.Errorf("demo seed: 需要 %d 组凭据", len(demoProfiles))
	}
	for i, profile := range demoProfiles {
		if credentials[i].Username != profile.Username || credentials[i].AgentSlug != profile.AgentSlug || credentials[i].Password == "" || credentials[i].Token == "" {
			return DemoSeedResult{}, fmt.Errorf("demo seed: 第 %d 组凭据与固定画像不匹配", i+1)
		}
	}

	if existing, ok, err := readDemoManifest(ctx, db, tenantID); err != nil {
		return DemoSeedResult{}, err
	} else if ok {
		complete, err := verifyDemoManifest(ctx, db, existing)
		return DemoSeedResult{Manifest: existing, AlreadyApplied: true, Complete: complete}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return DemoSeedResult{}, fmt.Errorf("demo seed: 开启事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if existing, ok, err := readDemoManifest(ctx, tx, tenantID); err != nil {
		return DemoSeedResult{}, err
	} else if ok {
		return DemoSeedResult{Manifest: existing, AlreadyApplied: true, Complete: true}, nil
	}

	fixed := &platform.FixedClock{T: now.UTC().AddDate(0, 0, -6)}
	ids := platform.NewIDGenerator(fixed)
	govStore := &GovernanceStore{ex: tx}
	knowStore := &KnowledgeStore{ex: tx}
	continuityStore := &ContinuityStore{ex: tx}
	activityStore := &ActivityStore{ex: tx}
	gov := governance.NewService(govStore, fixed, ids, hasher, nil)
	know := knowledge.NewService(knowStore, fixed, ids, nil)
	lines := continuity.NewService(continuityStore, fixed, ids)
	activities := activity.NewService(activityStore, fixed, ids)

	var tenant governance.Tenant
	var tenantIDRaw string
	var tenantCreated time.Time
	if err := tx.QueryRowContext(ctx, `SELECT id, name, status, created_at FROM tenants WHERE id = ?`, string(tenantID)).Scan(&tenantIDRaw, &tenant.Name, &tenant.Status, &tenantCreated); err != nil {
		return DemoSeedResult{}, fmt.Errorf("demo seed: 解析指定租户: %w", err)
	}
	tenant.ID, tenant.CreatedAt = platform.ID(tenantIDRaw), tenantCreated.UTC()
	if !tenant.IsActive() {
		return DemoSeedResult{}, errors.New("demo seed: 指定租户未启用")
	}
	users, err := gov.ListUsers(ctx, tenant.ID)
	if err != nil {
		return DemoSeedResult{}, err
	}
	var admin governance.User
	for _, user := range users {
		if user.IsAdmin() && user.IsActive() {
			admin = user
			break
		}
	}
	if admin.ID == "" {
		return DemoSeedResult{}, errors.New("demo seed: 没有可用管理员")
	}
	spaces, err := gov.ListSpaces(ctx, tenant.ID)
	if err != nil {
		return DemoSeedResult{}, err
	}
	var team governance.Space
	for _, space := range spaces {
		if space.Type == governance.SpaceTeam {
			team = space
			break
		}
	}
	if team.ID == "" {
		return DemoSeedResult{}, errors.New("demo seed: 没有团队空间")
	}

	manifest := DemoManifest{SeedKey: DemoSeedKey, TenantID: tenant.ID}
	createdUsers := make([]governance.User, 0, len(demoProfiles))
	createdAgents := make([]governance.Agent, 0, len(demoProfiles))
	personalSpaces := make([]governance.Space, 0, len(demoProfiles))
	for i, profile := range demoProfiles {
		fixed.T = now.UTC().AddDate(0, 0, i-6)
		user, personal, err := gov.CreateUser(ctx, governance.CreateUserInput{TenantID: tenant.ID, Username: profile.Username, DisplayName: profile.DisplayName, Password: credentials[i].Password})
		if err != nil {
			return DemoSeedResult{}, fmt.Errorf("demo seed: 创建用户 %s: %w", profile.Username, err)
		}
		agent, err := gov.CreateAgent(ctx, tenant.ID, user.ID, profile.AgentSlug, profile.AgentName)
		if err != nil {
			return DemoSeedResult{}, err
		}
		hash, err := governance.ParseToken(credentials[i].Token)
		if err != nil {
			return DemoSeedResult{}, err
		}
		prefix := credentials[i].Token[:12]
		token := governance.Token{ID: ids.New(), TenantID: tenant.ID, UserID: user.ID, AgentID: agent.ID, Name: "[演示] 展示设备", Prefix: prefix, Hash: hash, CreatedAt: fixed.Now(), ExpiresAt: demoTimePtr(now.UTC().AddDate(0, 1, 0))}
		if err := govStore.CreateToken(ctx, token); err != nil {
			return DemoSeedResult{}, err
		}
		createdUsers = append(createdUsers, user)
		createdAgents = append(createdAgents, agent)
		personalSpaces = append(personalSpaces, personal)
		manifest.UserIDs = append(manifest.UserIDs, user.ID)
		manifest.AgentIDs = append(manifest.AgentIDs, agent.ID)
		manifest.TokenIDs = append(manifest.TokenIDs, token.ID)
		manifest.SpaceIDs = append(manifest.SpaceIDs, personal.ID)
	}

	fixed.T = now.UTC().AddDate(0, 0, -4)
	productSpace, err := gov.CreateProjectSpace(ctx, tenant.ID, createdUsers[0].ID, "[演示] 产品发布协作")
	if err != nil {
		return DemoSeedResult{}, err
	}
	reliabilitySpace, err := gov.CreateProjectSpace(ctx, tenant.ID, createdUsers[1].ID, "[演示] 可靠性改进")
	if err != nil {
		return DemoSeedResult{}, err
	}
	manifest.SpaceIDs = append(manifest.SpaceIDs, productSpace.ID, reliabilitySpace.ID)
	for _, grant := range []struct {
		space platform.ID
		user  int
		write bool
	}{
		{team.ID, 0, true}, {team.ID, 1, true}, {team.ID, 2, true}, {team.ID, 3, true}, {team.ID, 4, true},
		{productSpace.ID, 1, true}, {productSpace.ID, 2, true}, {productSpace.ID, 4, false},
		{reliabilitySpace.ID, 0, false}, {reliabilitySpace.ID, 3, true}, {reliabilitySpace.ID, 4, false},
	} {
		if err := gov.GrantSpaceAccess(ctx, tenant.ID, grant.space, createdUsers[grant.user].ID, admin.ID, true, grant.write); err != nil {
			return DemoSeedResult{}, err
		}
	}

	sharedTargets := []platform.ID{team.ID, productSpace.ID, productSpace.ID, reliabilitySpace.ID, team.ID}
	createdMemories := make([]knowledge.Memory, 0, len(demoProfiles)*2)
	createdCards := make([]knowledge.Card, 0, len(demoProfiles)*2)
	for i, profile := range demoProfiles {
		fixed.T = now.UTC().AddDate(0, 0, i-5).Add(time.Duration(i) * time.Hour)
		agentID := createdAgents[i].ID
		for _, target := range []platform.ID{personalSpaces[i].ID, sharedTargets[i]} {
			memory, err := know.CreateMemory(ctx, knowledge.CreateMemoryInput{TenantID: tenant.ID, SpaceID: target, AuthorUserID: createdUsers[i].ID, AuthorAgentID: &agentID, Title: profile.MemoryTitle, Body: profile.MemoryBody, Labels: []string{"demo-seed", DemoSeedKey}})
			if err != nil {
				return DemoSeedResult{}, err
			}
			createdMemories = append(createdMemories, memory)
			manifest.MemoryIDs = append(manifest.MemoryIDs, memory.ID)
		}
		for j, target := range []platform.ID{personalSpaces[i].ID, sharedTargets[i]} {
			status := knowledge.CardActive
			if i == 2 && j == 1 {
				status = knowledge.CardDraft
			}
			card, err := know.CreateCard(ctx, knowledge.CreateCardInput{TenantID: tenant.ID, SpaceID: target, AuthorUserID: createdUsers[i].ID, AuthorAgentID: &agentID, Title: profile.CardTitle, CardType: profile.CardType, Source: "[演示] 团队协作样例", Original: profile.MemoryBody, Distilled: profile.CardUnderstanding, Understanding: profile.CardUnderstanding, WhyKeep: "用于演示多用户知识协作与治理页面。", Unresolved: "这是演示数据，不代表真实业务结论。", Status: status, RecallTriggers: []string{"当团队讨论" + profile.CardType + "时", "当需要展示多用户协作数据时"}, Labels: []string{"demo-seed", DemoSeedKey}})
			if err != nil {
				return DemoSeedResult{}, err
			}
			createdCards = append(createdCards, card)
			manifest.CardIDs = append(manifest.CardIDs, card.ID)
		}
	}

	for i := 0; i < 3; i++ {
		fixed.T = now.UTC().AddDate(0, 0, i-2)
		packet, err := lines.Update(ctx, continuity.UpdateInput{TenantID: tenant.ID, UserID: createdUsers[i].ID, AgentID: createdAgents[i].ID, Title: "[演示] " + []string{"发布准备协作", "可靠性问题跟进", "界面验收收敛"}[i], Summary: []string{"已完成范围盘点，正在确认展示口径。", "已定位主要风险，等待验证修复后的运行状态。", "已完成静态检查，等待人工视觉验收。"}[i], InterpretationStatus: []string{continuity.PositionActive, continuity.PositionNeedsReview, continuity.PositionStale}[i], FactsUsed: []platform.ID{createdMemories[i*2+1].ID, createdCards[i*2+1].ID}, NextStep: []string{"补齐演示账号并确认权限。", "执行恢复演练并记录回执。", "在桌面与窄屏各走一遍关键状态。"}[i]})
		if err != nil {
			return DemoSeedResult{}, err
		}
		manifest.LineIDs = append(manifest.LineIDs, packet.Line.ID)
		if _, err := lines.Handoff(ctx, continuity.HandoffInput{TenantID: tenant.ID, UserID: createdUsers[i].ID, AgentID: createdAgents[i].ID, LineID: packet.Line.ID, Objective: "演示跨角色工作的当前位置与交接", Completed: []string{"已记录当前事实", "已明确下一步"}, OpenItems: []string{"等待下一角色确认"}, NextStep: packet.Line.Position.NextStep}); err != nil {
			return DemoSeedResult{}, err
		}
	}

	for i := range createdUsers {
		fixed.T = now.UTC().AddDate(0, 0, i-4).Add(30 * time.Minute)
		entityID, spaceID, agentID := manifest.CardIDs[i*2+1], sharedTargets[i], createdAgents[i].ID
		metadata, _ := json.Marshal(map[string]any{"seedKey": DemoSeedKey, "actor": demoProfiles[i].Username})
		event, err := activities.Record(ctx, activity.RecordInput{TenantID: tenant.ID, UserID: createdUsers[i].ID, AgentID: &agentID, Source: activity.SourceMCP, Kind: activity.KindTrace, Access: activity.AccessWrite, Outcome: activity.OutcomeSuccess, Action: "demo_card.create", EntityType: knowledge.EntityCard, EntityID: &entityID, SpaceID: &spaceID, Metadata: metadata})
		if err != nil {
			return DemoSeedResult{}, err
		}
		manifest.ActivityIDs = append(manifest.ActivityIDs, event.ID)
	}

	fixed.T = now.UTC()
	receiptMetadata, err := json.Marshal(demoReceipt{SeedKey: DemoSeedKey, Manifest: manifest})
	if err != nil {
		return DemoSeedResult{}, err
	}
	receipt, err := activities.Record(ctx, activity.RecordInput{TenantID: tenant.ID, UserID: admin.ID, Source: activity.SourceCLI, Kind: activity.KindGovernance, Access: activity.AccessWrite, Outcome: activity.OutcomeSuccess, Action: "demo_seed.apply", Metadata: receiptMetadata})
	if err != nil {
		return DemoSeedResult{}, err
	}
	manifest.ReceiptID = receipt.ID
	// Rewrite the receipt once so its manifest also names the receipt row itself.
	receiptMetadata, _ = json.Marshal(demoReceipt{SeedKey: DemoSeedKey, Manifest: manifest})
	if _, err := tx.ExecContext(ctx, `UPDATE audit_logs SET metadata = ? WHERE id = ? AND tenant_id = ?`, string(receiptMetadata), string(receipt.ID), string(tenant.ID)); err != nil {
		return DemoSeedResult{}, fmt.Errorf("demo seed: 更新回执: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DemoSeedResult{}, fmt.Errorf("demo seed: 提交事务: %w", err)
	}
	committed = true
	return DemoSeedResult{Manifest: manifest, Complete: true}, nil
}

func InspectDemoSeed(ctx context.Context, db *sql.DB, tenantID platform.ID) (DemoSeedResult, error) {
	manifest, ok, err := readDemoManifest(ctx, db, tenantID)
	if err != nil || !ok {
		return DemoSeedResult{}, err
	}
	complete, err := verifyDemoManifest(ctx, db, manifest)
	return DemoSeedResult{Manifest: manifest, AlreadyApplied: true, Complete: complete}, err
}

func CleanupDemoSeed(ctx context.Context, db *sql.DB, tenantID platform.ID) (DemoManifest, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return DemoManifest{}, err
	}
	defer tx.Rollback()
	manifest, ok, err := readDemoManifest(ctx, tx, tenantID)
	if err != nil || !ok {
		return DemoManifest{}, err
	}
	complete, err := verifyDemoManifest(ctx, tx, manifest)
	if err != nil {
		return DemoManifest{}, err
	}
	if !complete {
		return DemoManifest{}, errors.New("demo seed: 数据已不完整，拒绝自动清理")
	}
	if err := ensureNoDemoDrift(ctx, tx, manifest); err != nil {
		return DemoManifest{}, err
	}
	if err := deleteDemoRows(ctx, tx, manifest); err != nil {
		return DemoManifest{}, err
	}
	if err := tx.Commit(); err != nil {
		return DemoManifest{}, err
	}
	return manifest, nil
}

func ensureNoDemoDrift(ctx context.Context, ex sqlExecutor, manifest DemoManifest) error {
	checks := []struct {
		table, column string
		expected      int
	}{
		{"agents", "user_id", len(manifest.AgentIDs)},
		{"tokens", "user_id", len(manifest.TokenIDs)},
		{"spaces", "owner_user_id", len(manifest.SpaceIDs)},
		{"memories", "author_user_id", len(manifest.MemoryIDs)},
		{"cards", "author_user_id", len(manifest.CardIDs)},
		{"continuity_lines", "owner_user_id", len(manifest.LineIDs)},
	}
	for _, check := range checks {
		var count int
		args := append([]any{string(manifest.TenantID)}, idArgs(manifest.UserIDs)...)
		query := `SELECT COUNT(*) FROM ` + check.table + ` WHERE tenant_id = ? AND ` + check.column + ` IN (` + placeholders(len(manifest.UserIDs)) + `)`
		if err := ex.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return err
		}
		if count != check.expected {
			return fmt.Errorf("demo seed: %s 已产生回执外数据（当前 %d，预期 %d），拒绝自动清理", check.table, count, check.expected)
		}
	}
	return nil
}

func readDemoManifest(ctx context.Context, ex sqlExecutor, tenantID platform.ID) (DemoManifest, bool, error) {
	query := `SELECT id, tenant_id, metadata FROM audit_logs WHERE action = 'demo_seed.apply' AND JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.seedKey')) = ?`
	args := []any{DemoSeedKey}
	if tenantID != "" {
		query += " AND tenant_id = ?"
		args = append(args, string(tenantID))
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT 1"
	var receiptID, storedTenant string
	var raw []byte
	err := ex.QueryRowContext(ctx, query, args...).Scan(&receiptID, &storedTenant, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DemoManifest{}, false, nil
	}
	if err != nil {
		return DemoManifest{}, false, fmt.Errorf("demo seed: 读取回执: %w", err)
	}
	var receipt demoReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return DemoManifest{}, false, fmt.Errorf("demo seed: 回执损坏: %w", err)
	}
	if receipt.SeedKey != DemoSeedKey || receipt.Manifest.TenantID != platform.ID(storedTenant) {
		return DemoManifest{}, false, errors.New("demo seed: 回执身份不一致")
	}
	receipt.Manifest.ReceiptID = platform.ID(receiptID)
	return receipt.Manifest, true, nil
}

func verifyDemoManifest(ctx context.Context, ex sqlExecutor, manifest DemoManifest) (bool, error) {
	checks := []struct {
		table string
		ids   []platform.ID
	}{{"users", manifest.UserIDs}, {"agents", manifest.AgentIDs}, {"tokens", manifest.TokenIDs}, {"spaces", manifest.SpaceIDs}, {"memories", manifest.MemoryIDs}, {"cards", manifest.CardIDs}, {"continuity_lines", manifest.LineIDs}, {"links", manifest.LinkIDs}, {"audit_logs", append(append([]platform.ID{}, manifest.ActivityIDs...), manifest.ReceiptID)}}
	for _, check := range checks {
		if len(check.ids) == 0 {
			continue
		}
		var count int
		args := append([]any{string(manifest.TenantID)}, idArgs(check.ids)...)
		query := `SELECT COUNT(*) FROM ` + check.table + ` WHERE tenant_id = ? AND id IN (` + placeholders(len(check.ids)) + `)`
		if err := ex.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return false, err
		}
		if count != len(check.ids) {
			return false, nil
		}
	}
	return true, nil
}

func deleteDemoRows(ctx context.Context, tx *sql.Tx, m DemoManifest) error {
	cardArgs := idArgs(m.CardIDs)
	memoryArgs := idArgs(m.MemoryIDs)
	userArgs := idArgs(m.UserIDs)
	spaceArgs := idArgs(m.SpaceIDs)
	lineArgs := idArgs(m.LineIDs)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM embeddings WHERE (entity_type = 'card' AND entity_id IN (` + placeholders(len(cardArgs)) + `)) OR (entity_type = 'memory' AND entity_id IN (` + placeholders(len(memoryArgs)) + `)) OR (entity_type = 'card_trigger' AND entity_id IN (SELECT id FROM card_recall_triggers WHERE card_id IN (` + placeholders(len(cardArgs)) + `)))`, append(append(cardArgs, memoryArgs...), cardArgs...)},
		{`DELETE FROM links WHERE tenant_id = ? AND (from_id IN (` + placeholders(len(cardArgs)+len(memoryArgs)) + `) OR to_id IN (` + placeholders(len(cardArgs)+len(memoryArgs)) + `))`, append(append([]any{string(m.TenantID)}, append(cardArgs, memoryArgs...)...), append(cardArgs, memoryArgs...)...)},
		{`DELETE FROM labels WHERE tenant_id = ? AND entity_id IN (` + placeholders(len(cardArgs)+len(memoryArgs)) + `)`, append([]any{string(m.TenantID)}, append(cardArgs, memoryArgs...)...)},
		{`DELETE FROM card_recall_triggers WHERE tenant_id = ? AND card_id IN (` + placeholders(len(cardArgs)) + `)`, append([]any{string(m.TenantID)}, cardArgs...)},
		{`DELETE FROM cards WHERE tenant_id = ? AND id IN (` + placeholders(len(cardArgs)) + `)`, append([]any{string(m.TenantID)}, cardArgs...)},
		{`DELETE FROM memories WHERE tenant_id = ? AND id IN (` + placeholders(len(memoryArgs)) + `)`, append([]any{string(m.TenantID)}, memoryArgs...)},
		{`DELETE FROM continuity_handoffs WHERE tenant_id = ? AND line_id IN (` + placeholders(len(lineArgs)) + `)`, append([]any{string(m.TenantID)}, lineArgs...)},
		{`DELETE FROM continuity_position_history WHERE tenant_id = ? AND line_id IN (` + placeholders(len(lineArgs)) + `)`, append([]any{string(m.TenantID)}, lineArgs...)},
		{`DELETE FROM continuity_positions WHERE tenant_id = ? AND line_id IN (` + placeholders(len(lineArgs)) + `)`, append([]any{string(m.TenantID)}, lineArgs...)},
		{`DELETE FROM continuity_lines WHERE tenant_id = ? AND id IN (` + placeholders(len(lineArgs)) + `)`, append([]any{string(m.TenantID)}, lineArgs...)},
		{`DELETE FROM audit_logs WHERE tenant_id = ? AND (user_id IN (` + placeholders(len(userArgs)) + `) OR id = ?)`, append(append([]any{string(m.TenantID)}, userArgs...), string(m.ReceiptID))},
		{`DELETE FROM sessions WHERE tenant_id = ? AND user_id IN (` + placeholders(len(userArgs)) + `)`, append([]any{string(m.TenantID)}, userArgs...)},
		{`DELETE FROM tokens WHERE tenant_id = ? AND id IN (` + placeholders(len(m.TokenIDs)) + `)`, append([]any{string(m.TenantID)}, idArgs(m.TokenIDs)...)},
		{`DELETE FROM space_permissions WHERE user_id IN (` + placeholders(len(userArgs)) + `) OR granted_by IN (` + placeholders(len(userArgs)) + `) OR space_id IN (` + placeholders(len(spaceArgs)) + `)`, append(append(userArgs, userArgs...), spaceArgs...)},
		{`DELETE FROM spaces WHERE tenant_id = ? AND id IN (` + placeholders(len(spaceArgs)) + `)`, append([]any{string(m.TenantID)}, spaceArgs...)},
		{`DELETE FROM agents WHERE tenant_id = ? AND id IN (` + placeholders(len(m.AgentIDs)) + `)`, append([]any{string(m.TenantID)}, idArgs(m.AgentIDs)...)},
		{`DELETE FROM users WHERE tenant_id = ? AND id IN (` + placeholders(len(userArgs)) + `)`, append([]any{string(m.TenantID)}, userArgs...)},
	}
	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			return fmt.Errorf("demo seed: 清理失败: %w", err)
		}
	}
	return nil
}

func demoTimePtr(value time.Time) *time.Time { return &value }
