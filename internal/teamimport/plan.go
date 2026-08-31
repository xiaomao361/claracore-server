// Package teamimport performs semantic preflight for portable team imports.
// It is deliberately database-free: operators can inspect an archive and its
// takeover boundary before supplying credentials or touching a deployment.
package teamimport

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"claracore-server/internal/platform"
	"claracore-server/internal/portableexport"
)

const MaxImportRecords int64 = 1_000_000

type DatasetPlan struct {
	Name    string `json:"name"`
	Records int64  `json:"records"`
}

type Plan struct {
	OK                      bool          `json:"ok"`
	FormatVersion           int           `json:"formatVersion"`
	SchemaVersion           string        `json:"schemaVersion"`
	TenantID                string        `json:"tenantId"`
	TenantName              string        `json:"tenantName"`
	GeneratedAt             time.Time     `json:"generatedAt"`
	Datasets                []DatasetPlan `json:"datasets"`
	Records                 int64         `json:"records"`
	AdminCandidates         []string      `json:"adminCandidates"`
	SelectedAdmin           string        `json:"selectedAdmin,omitempty"`
	MembersDisabledOnImport int64         `json:"membersDisabledOnImport"`
	IDsPreserved            bool          `json:"idsPreserved"`
	RequiresEmptyDeployment bool          `json:"requiresEmptyDeployment"`
	CredentialsImported     bool          `json:"credentialsImported"`
}

type memberRecord struct {
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	DisplayName        string    `json:"display_name"`
	Role               string    `json:"role"`
	MustChangePassword bool      `json:"must_change_password"`
	Status             string    `json:"status"`
	CreatedAt          time.Time `json:"created_at"`
}

type agentRecord struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

type spaceRecord struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Name        string    `json:"name"`
	OwnerUserID *string   `json:"owner_user_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type permissionRecord struct {
	SpaceID   string    `json:"space_id"`
	UserID    string    `json:"user_id"`
	CanRead   bool      `json:"can_read"`
	CanWrite  bool      `json:"can_write"`
	GrantedBy string    `json:"granted_by"`
	CreatedAt time.Time `json:"created_at"`
}

type memoryRecord struct {
	ID             string    `json:"id"`
	SpaceID        string    `json:"space_id"`
	AuthorUserID   string    `json:"author_user_id"`
	AuthorAgentID  *string   `json:"author_agent_id"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	Status         string    `json:"status"`
	CopiedFromType *string   `json:"copied_from_type"`
	CopiedFromID   *string   `json:"copied_from_id"`
	Version        int       `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type cardRecord struct {
	ID                string    `json:"id"`
	SpaceID           string    `json:"space_id"`
	AuthorUserID      string    `json:"author_user_id"`
	AuthorAgentID     *string   `json:"author_agent_id"`
	Title             string    `json:"title"`
	CardType          string    `json:"card_type"`
	Status            string    `json:"status"`
	Source            *string   `json:"source"`
	SourceName        *string   `json:"source_name"`
	SourceKey         *string   `json:"source_key"`
	SourceHash        *string   `json:"source_hash"`
	BodyOriginal      *string   `json:"body_original"`
	BodyDistilled     *string   `json:"body_distilled"`
	BodyUnderstanding *string   `json:"body_understanding"`
	BodyWhyKeep       *string   `json:"body_why_keep"`
	BodyUnresolved    *string   `json:"body_unresolved"`
	CopiedFromType    *string   `json:"copied_from_type"`
	CopiedFromID      *string   `json:"copied_from_id"`
	Version           int       `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type triggerRecord struct {
	ID          string    `json:"id"`
	CardID      string    `json:"card_id"`
	SpaceID     string    `json:"space_id"`
	TriggerText string    `json:"trigger_text"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
}

type labelRecord struct {
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"created_at"`
}

type linkRecord struct {
	ID        string    `json:"id"`
	FromType  string    `json:"from_type"`
	FromID    string    `json:"from_id"`
	ToType    string    `json:"to_type"`
	ToID      string    `json:"to_id"`
	Kind      string    `json:"kind"`
	Note      *string   `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

type lineRecord struct {
	ID           string    `json:"id"`
	OwnerUserID  string    `json:"owner_user_id"`
	OwnerAgentID string    `json:"owner_agent_id"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type positionRecord struct {
	ID                   string    `json:"id"`
	LineID               string    `json:"line_id"`
	Summary              string    `json:"summary"`
	InterpretationStatus string    `json:"interpretation_status"`
	FactsUsed            []string  `json:"facts_used"`
	NextStep             string    `json:"next_step"`
	WriterUserID         string    `json:"writer_user_id"`
	WriterAgentID        string    `json:"writer_agent_id"`
	Version              int       `json:"version"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type historyRecord struct {
	ID                   string    `json:"id"`
	LineID               string    `json:"line_id"`
	PositionID           string    `json:"position_id"`
	PositionVersion      int       `json:"position_version"`
	Summary              string    `json:"summary"`
	InterpretationStatus string    `json:"interpretation_status"`
	FactsUsed            []string  `json:"facts_used"`
	NextStep             string    `json:"next_step"`
	WriterUserID         string    `json:"writer_user_id"`
	WriterAgentID        string    `json:"writer_agent_id"`
	CreatedAt            time.Time `json:"created_at"`
}

type handoffRecord struct {
	ID            string    `json:"id"`
	LineID        string    `json:"line_id"`
	WriterUserID  string    `json:"writer_user_id"`
	WriterAgentID string    `json:"writer_agent_id"`
	Objective     string    `json:"objective"`
	Completed     []string  `json:"completed"`
	OpenItems     []string  `json:"open_items"`
	NextStep      string    `json:"next_step"`
	CreatedAt     time.Time `json:"created_at"`
}

type originRef struct {
	typeName, id, owner string
}

type analysisState struct {
	members, agents, spaces, memories, cards, lines  map[string]bool
	agentOwner, spaceType, cardSpace, positionByLine map[string]string
	personalOwners, usernames, agentSlugs            map[string]bool
	permissionKeys, sourceKeys, triggerOrders        map[string]bool
	labels, links                                    map[string]bool
	origins                                          []originRef
	adminCandidates                                  []string
	teamSpaces, personalSpaces                       int64
}

func Analyze(archive *portableexport.Archive, selectedAdmin string) (Plan, error) {
	if archive == nil {
		return Plan{}, errors.New("team import: archive is required")
	}
	verification := archive.Verification()
	manifest := archive.Manifest()
	if verification.Records > MaxImportRecords {
		return Plan{}, fmt.Errorf("team import: archive has %d records, import preflight limit is %d", verification.Records, MaxImportRecords)
	}
	state := newAnalysisState()
	if err := analyzeMembers(archive, state); err != nil {
		return Plan{}, err
	}
	if err := analyzeAgents(archive, state); err != nil {
		return Plan{}, err
	}
	if err := analyzeSpaces(archive, state); err != nil {
		return Plan{}, err
	}
	if err := analyzePermissions(archive, state); err != nil {
		return Plan{}, err
	}
	if err := analyzeKnowledge(archive, state); err != nil {
		return Plan{}, err
	}
	if err := analyzeContinuity(archive, state); err != nil {
		return Plan{}, err
	}
	if state.teamSpaces != 1 || state.personalSpaces != int64(len(state.members)) {
		return Plan{}, fmt.Errorf("team import: governance shape requires one team space and one personal space per member; got team=%d personal=%d members=%d", state.teamSpaces, state.personalSpaces, len(state.members))
	}
	for _, origin := range state.origins {
		if !entityExists(state, origin.typeName, origin.id) {
			return Plan{}, fmt.Errorf("team import: %s references missing origin %s:%s", origin.owner, origin.typeName, origin.id)
		}
	}
	slices.Sort(state.adminCandidates)
	if len(state.adminCandidates) == 0 {
		return Plan{}, errors.New("team import: archive has no administrator available for takeover")
	}
	selectedAdmin = strings.TrimSpace(selectedAdmin)
	if selectedAdmin != "" && !slices.Contains(state.adminCandidates, selectedAdmin) {
		return Plan{}, fmt.Errorf("team import: selected administrator %q is not an archived admin", selectedAdmin)
	}
	datasets := make([]DatasetPlan, 0, len(manifest.Files)-1)
	for _, receipt := range manifest.Files[1:] {
		datasets = append(datasets, DatasetPlan{Name: receipt.Name, Records: receipt.Records})
	}
	disabled := int64(len(state.members))
	if selectedAdmin != "" {
		disabled--
	}
	return Plan{
		OK: true, FormatVersion: verification.FormatVersion, SchemaVersion: verification.SchemaVersion,
		TenantID: verification.TenantID, TenantName: manifest.TenantName, GeneratedAt: verification.GeneratedAt,
		Datasets: datasets, Records: verification.Records,
		AdminCandidates: append([]string(nil), state.adminCandidates...), SelectedAdmin: selectedAdmin,
		MembersDisabledOnImport: disabled, IDsPreserved: true, RequiresEmptyDeployment: true,
		CredentialsImported: false,
	}, nil
}

func newAnalysisState() *analysisState {
	return &analysisState{
		members: map[string]bool{}, agents: map[string]bool{}, spaces: map[string]bool{}, memories: map[string]bool{}, cards: map[string]bool{}, lines: map[string]bool{},
		agentOwner: map[string]string{}, spaceType: map[string]string{}, cardSpace: map[string]string{}, positionByLine: map[string]string{},
		personalOwners: map[string]bool{}, usernames: map[string]bool{}, agentSlugs: map[string]bool{}, permissionKeys: map[string]bool{}, sourceKeys: map[string]bool{}, triggerOrders: map[string]bool{}, labels: map[string]bool{}, links: map[string]bool{},
	}
}

func analyzeMembers(archive *portableexport.Archive, state *analysisState) error {
	return scanDataset[memberRecord](archive, "members.jsonl", func(r memberRecord) error {
		if err := addID(state.members, r.ID, "member"); err != nil {
			return err
		}
		if !validText(r.Username, 64, true) || !validText(r.DisplayName, 128, true) || !oneOf(r.Role, "admin", "member") || !oneOf(r.Status, "active", "disabled") || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid member %s", r.ID)
		}
		if state.usernames[r.Username] {
			return fmt.Errorf("team import: duplicate username %q", r.Username)
		}
		state.usernames[r.Username] = true
		if r.Role == "admin" {
			state.adminCandidates = append(state.adminCandidates, r.Username)
		}
		return nil
	})
}

func analyzeAgents(archive *portableexport.Archive, state *analysisState) error {
	return scanDataset[agentRecord](archive, "agents.jsonl", func(r agentRecord) error {
		if err := addID(state.agents, r.ID, "agent"); err != nil {
			return err
		}
		if !state.members[r.UserID] || !validText(r.Slug, 64, true) || !validText(r.DisplayName, 128, true) || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid agent %s", r.ID)
		}
		key := r.UserID + "\x00" + r.Slug
		if state.agentSlugs[key] {
			return fmt.Errorf("team import: duplicate agent slug for user %s", r.UserID)
		}
		state.agentSlugs[key], state.agentOwner[r.ID] = true, r.UserID
		return nil
	})
}

func analyzeSpaces(archive *portableexport.Archive, state *analysisState) error {
	return scanDataset[spaceRecord](archive, "spaces.jsonl", func(r spaceRecord) error {
		if err := addID(state.spaces, r.ID, "space"); err != nil {
			return err
		}
		if !validText(r.Name, 128, true) || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid space %s", r.ID)
		}
		switch r.Type {
		case "team":
			if r.OwnerUserID != nil {
				return fmt.Errorf("team import: team space %s must not have an owner", r.ID)
			}
			state.teamSpaces++
		case "personal", "project":
			if r.OwnerUserID == nil || !state.members[*r.OwnerUserID] {
				return fmt.Errorf("team import: space %s has an invalid owner", r.ID)
			}
			if r.Type == "personal" {
				if state.personalOwners[*r.OwnerUserID] {
					return fmt.Errorf("team import: member %s has duplicate personal spaces", *r.OwnerUserID)
				}
				state.personalOwners[*r.OwnerUserID], state.personalSpaces = true, state.personalSpaces+1
			}
		default:
			return fmt.Errorf("team import: space %s has invalid type %q", r.ID, r.Type)
		}
		state.spaceType[r.ID] = r.Type
		return nil
	})
}

func analyzePermissions(archive *portableexport.Archive, state *analysisState) error {
	return scanDataset[permissionRecord](archive, "space_permissions.jsonl", func(r permissionRecord) error {
		key := r.SpaceID + "\x00" + r.UserID
		if state.permissionKeys[key] || !state.spaces[r.SpaceID] || state.spaceType[r.SpaceID] == "personal" || !state.members[r.UserID] || !state.members[r.GrantedBy] || (!r.CanRead && !r.CanWrite) || (r.CanWrite && !r.CanRead) || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid space permission %s/%s", r.SpaceID, r.UserID)
		}
		state.permissionKeys[key] = true
		return nil
	})
}

func analyzeKnowledge(archive *portableexport.Archive, state *analysisState) error {
	if err := scanDataset[memoryRecord](archive, "memories.jsonl", func(r memoryRecord) error {
		if err := addID(state.memories, r.ID, "memory"); err != nil {
			return err
		}
		if !state.spaces[r.SpaceID] || !state.members[r.AuthorUserID] || !validOptionalAgent(state, r.AuthorAgentID, r.AuthorUserID) || !validText(r.Title, 255, true) || strings.TrimSpace(r.Body) == "" || !oneOf(r.Status, "active", "archived", "superseded", "deleted") || r.Version < 1 || !validTimes(r.CreatedAt, r.UpdatedAt) {
			return fmt.Errorf("team import: invalid memory %s", r.ID)
		}
		if err := collectOrigin(state, r.CopiedFromType, r.CopiedFromID, "memory "+r.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if err := scanDataset[cardRecord](archive, "cards.jsonl", func(r cardRecord) error {
		if err := addID(state.cards, r.ID, "card"); err != nil {
			return err
		}
		if !state.spaces[r.SpaceID] || !state.members[r.AuthorUserID] || !validOptionalAgent(state, r.AuthorAgentID, r.AuthorUserID) || !validText(r.Title, 255, true) || !validText(r.CardType, 64, true) || !oneOf(r.Status, "draft", "active", "deprecated") || r.Version < 1 || !validTimes(r.CreatedAt, r.UpdatedAt) || allBlank(r.BodyOriginal, r.BodyDistilled, r.BodyUnderstanding, r.BodyWhyKeep, r.BodyUnresolved) {
			return fmt.Errorf("team import: invalid card %s", r.ID)
		}
		if !validOptionalText(r.SourceName, 120) || !validOptionalHash(r.SourceKey) || !validOptionalHash(r.SourceHash) {
			return fmt.Errorf("team import: invalid card source metadata %s", r.ID)
		}
		if r.SourceKey != nil {
			key := r.SpaceID + "\x00" + *r.SourceKey
			if state.sourceKeys[key] {
				return fmt.Errorf("team import: duplicate card source key in space %s", r.SpaceID)
			}
			state.sourceKeys[key] = true
		}
		if err := collectOrigin(state, r.CopiedFromType, r.CopiedFromID, "card "+r.ID); err != nil {
			return err
		}
		state.cardSpace[r.ID] = r.SpaceID
		return nil
	}); err != nil {
		return err
	}
	if err := scanDataset[triggerRecord](archive, "card_recall_triggers.jsonl", func(r triggerRecord) error {
		if !validID(r.ID) || !state.cards[r.CardID] || state.cardSpace[r.CardID] != r.SpaceID || !validText(r.TriggerText, 512, true) || r.SortOrder < 0 || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid recall trigger %s", r.ID)
		}
		key := r.CardID + fmt.Sprintf("\x00%09d", r.SortOrder)
		if state.triggerOrders[key] {
			return fmt.Errorf("team import: duplicate trigger order for card %s", r.CardID)
		}
		state.triggerOrders[key] = true
		return nil
	}); err != nil {
		return err
	}
	if err := scanDataset[labelRecord](archive, "labels.jsonl", func(r labelRecord) error {
		key := r.EntityType + "\x00" + r.EntityID + "\x00" + r.Label
		if !entityExists(state, r.EntityType, r.EntityID) || !validText(r.Label, 64, true) || state.labels[key] || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid label for %s:%s", r.EntityType, r.EntityID)
		}
		state.labels[key] = true
		return nil
	}); err != nil {
		return err
	}
	return scanDataset[linkRecord](archive, "links.jsonl", func(r linkRecord) error {
		if !validID(r.ID) || !entityExists(state, r.FromType, r.FromID) || !entityExists(state, r.ToType, r.ToID) || !oneOf(r.Kind, "related", "causes", "evolved-from", "contradicts", "part-of") || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid link %s", r.ID)
		}
		key := strings.Join([]string{r.FromType, r.FromID, r.ToType, r.ToID, r.Kind}, "\x00")
		if state.links[key] {
			return fmt.Errorf("team import: duplicate link %s", r.ID)
		}
		state.links[key] = true
		return nil
	})
}

func analyzeContinuity(archive *portableexport.Archive, state *analysisState) error {
	if err := scanDataset[lineRecord](archive, "shared_lines.jsonl", func(r lineRecord) error {
		if err := addID(state.lines, r.ID, "shared line"); err != nil {
			return err
		}
		if !state.members[r.OwnerUserID] || state.agentOwner[r.OwnerAgentID] != r.OwnerUserID || !validText(r.Title, 200, true) || !oneOf(r.Status, "active", "archived") || !validTimes(r.CreatedAt, r.UpdatedAt) {
			return fmt.Errorf("team import: invalid shared line %s", r.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := scanDataset[positionRecord](archive, "shared_line_positions.jsonl", func(r positionRecord) error {
		if !validID(r.ID) || !state.lines[r.LineID] || !state.members[r.WriterUserID] || state.agentOwner[r.WriterAgentID] != r.WriterUserID || !validText(r.Summary, 12000, true) || !oneOf(r.InterpretationStatus, "draft", "confirmed", "active", "needs_review", "stale", "closed") || len([]rune(r.NextStep)) > 4000 || r.Version < 1 || !validTimes(r.CreatedAt, r.UpdatedAt) || !validFacts(state, r.FactsUsed) {
			return fmt.Errorf("team import: invalid shared line position %s", r.ID)
		}
		if state.positionByLine[r.LineID] != "" {
			return fmt.Errorf("team import: shared line %s has multiple current positions", r.LineID)
		}
		state.positionByLine[r.LineID] = r.ID
		return nil
	}); err != nil {
		return err
	}
	if err := scanDataset[historyRecord](archive, "shared_line_position_history.jsonl", func(r historyRecord) error {
		if !validID(r.ID) || !state.lines[r.LineID] || state.positionByLine[r.LineID] == "" || state.positionByLine[r.LineID] != r.PositionID || !state.members[r.WriterUserID] || state.agentOwner[r.WriterAgentID] != r.WriterUserID || !validText(r.Summary, 12000, true) || !oneOf(r.InterpretationStatus, "draft", "confirmed", "active", "needs_review", "stale", "closed") || len([]rune(r.NextStep)) > 4000 || r.PositionVersion < 1 || !validTime(r.CreatedAt) || !validFacts(state, r.FactsUsed) {
			return fmt.Errorf("team import: invalid shared line position history %s", r.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	return scanDataset[handoffRecord](archive, "shared_line_handoffs.jsonl", func(r handoffRecord) error {
		if !validID(r.ID) || !state.lines[r.LineID] || !state.members[r.WriterUserID] || state.agentOwner[r.WriterAgentID] != r.WriterUserID || len([]rune(r.Objective)) > 4000 || len([]rune(r.NextStep)) > 4000 || !validStringList(r.Completed, 100, 1000) || !validStringList(r.OpenItems, 100, 1000) || (strings.TrimSpace(r.Objective+r.NextStep) == "" && len(r.Completed) == 0 && len(r.OpenItems) == 0) || !validTime(r.CreatedAt) {
			return fmt.Errorf("team import: invalid shared line handoff %s", r.ID)
		}
		return nil
	})
}

func scanDataset[T any](archive *portableexport.Archive, name string, accept func(T) error) error {
	r, err := archive.OpenDataset(name)
	if err != nil {
		return err
	}
	defer r.Close()
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	var count int64
	for {
		var record T
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("team import: decode %s record %d: %w", name, count+1, err)
		}
		count++
		if count > MaxImportRecords {
			return fmt.Errorf("team import: %s exceeds record limit", name)
		}
		if err := accept(record); err != nil {
			return fmt.Errorf("%s record %d: %w", name, count, err)
		}
	}
	return nil
}

func addID(set map[string]bool, id, kind string) error {
	if !validID(id) {
		return fmt.Errorf("team import: invalid %s ID %q", kind, id)
	}
	if set[id] {
		return fmt.Errorf("team import: duplicate %s ID %s", kind, id)
	}
	set[id] = true
	return nil
}

func validID(id string) bool { _, ok := platform.ID(id).Timestamp(); return ok }
func validTime(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0
}
func validTimes(created, updated time.Time) bool {
	return validTime(created) && validTime(updated) && !updated.Before(created)
}
func validText(value string, max int, required bool) bool {
	return utf8.ValidString(value) && (!required || strings.TrimSpace(value) != "") && len([]rune(value)) <= max
}
func validOptionalText(value *string, max int) bool {
	return value == nil || validText(*value, max, false)
}
func oneOf(value string, allowed ...string) bool { return slices.Contains(allowed, value) }
func validOptionalAgent(state *analysisState, agent *string, user string) bool {
	return agent == nil || state.agentOwner[*agent] == user
}
func entityExists(state *analysisState, kind, id string) bool {
	if kind == "memory" {
		return state.memories[id]
	}
	if kind == "card" {
		return state.cards[id]
	}
	return false
}
func collectOrigin(state *analysisState, kind, id *string, owner string) error {
	if (kind == nil) != (id == nil) {
		return fmt.Errorf("team import: %s has a partial origin", owner)
	}
	if kind != nil {
		if !oneOf(*kind, "memory", "card") || !validID(*id) {
			return fmt.Errorf("team import: %s has an invalid origin", owner)
		}
		state.origins = append(state.origins, originRef{*kind, *id, owner})
	}
	return nil
}
func validOptionalHash(value *string) bool {
	if value == nil {
		return true
	}
	if len(*value) != 64 || strings.ToLower(*value) != *value {
		return false
	}
	_, err := hex.DecodeString(*value)
	return err == nil
}
func allBlank(values ...*string) bool {
	for _, value := range values {
		if value != nil && strings.TrimSpace(*value) != "" {
			return false
		}
	}
	return true
}
func validFacts(state *analysisState, facts []string) bool {
	if len(facts) > 50 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range facts {
		if seen[id] || strings.TrimSpace(id) == "" || len(id) > 128 {
			return false
		}
		seen[id] = true
	}
	return true
}
func validStringList(values []string, maxItems, maxLen int) bool {
	if len(values) > maxItems {
		return false
	}
	for _, value := range values {
		if !validText(value, maxLen, true) {
			return false
		}
	}
	return true
}
