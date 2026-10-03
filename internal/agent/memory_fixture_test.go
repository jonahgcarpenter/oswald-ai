package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// Each synthetic owner has a separate approved profile database. The fixture
// normalizes short test conversation labels into real transport session keys.
type agentMemoryFixture struct {
	root, defaultSoul string
	stores            map[string]*memory.ProfileStore
	sql               *sql.DB
	readers           map[string]*sql.DB
}

func fixtureSession(key string) string {
	if strings.Contains(key, ":") {
		return key
	}
	return "homeassistant:" + key
}
func (s *agentMemoryFixture) Close() error {
	var errs []error
	for _, store := range s.stores {
		errs = append(errs, store.Close())
	}
	return errors.Join(errs...)
}
func (s *agentMemoryFixture) ResolveSessionContext(ctx context.Context, owner, key string, ttl time.Duration) (memory.SessionContext, error) {
	return s.stores[owner].ResolveSessionContext(ctx, owner, fixtureSession(key), ttl)
}
func (s *agentMemoryFixture) SessionFileMemory(ctx context.Context, owner, key string, g int) (string, string, bool, error) {
	return s.stores[owner].SessionFileMemory(ctx, owner, fixtureSession(key), g)
}
func (s *agentMemoryFixture) BindSessionFileMemory(ctx context.Context, owner, key string, g int, user, notes string) (string, string, error) {
	return s.stores[owner].BindSessionFileMemory(ctx, owner, fixtureSession(key), g, user, notes)
}
func (s *agentMemoryFixture) SessionImages(ctx context.Context, owner, key string, g int) ([]requestctx.InputImage, error) {
	return s.stores[owner].SessionImages(ctx, owner, fixtureSession(key), g)
}
func (s *agentMemoryFixture) ReserveImageVersion(ctx context.Context, owner, key string, g int, id string, v int) (int, error) {
	return s.stores[owner].ReserveImageVersion(ctx, owner, fixtureSession(key), g, id, v)
}
func (s *agentMemoryFixture) LatestSessionSummary(ctx context.Context, owner, key string, g int) (memory.SessionSummary, error) {
	return s.stores[owner].LatestSessionSummary(ctx, owner, fixtureSession(key), g)
}
func (s *agentMemoryFixture) RecentCompletedExchangesAfter(ctx context.Context, owner, key string, g int, after int64, limit int) ([]memory.SessionTurn, error) {
	return s.stores[owner].RecentCompletedExchangesAfter(ctx, owner, fixtureSession(key), g, after, limit)
}
func (s *agentMemoryFixture) PageDeliveredSessionTurnsAfter(ctx context.Context, owner, key string, g int, after int64, limit int) ([]memory.SessionTurn, error) {
	return s.stores[owner].PageDeliveredSessionTurnsAfter(ctx, owner, fixtureSession(key), g, after, limit)
}
func (s *agentMemoryFixture) AppendPendingSessionTurn(ctx context.Context, write memory.SessionTurnWrite) (memory.StoredSessionTurn, error) {
	write.SessionID = fixtureSession(write.SessionID)
	if write.Pressure.Limit == 0 {
		write.Pressure = memory.SessionPromptPressure{Tokens: 1, Limit: 100000, Version: "synthetic-v1"}
	}
	return s.stores[write.UserID].AppendPendingSessionTurn(ctx, write)
}
func (s *agentMemoryFixture) MarkSessionTurnDelivered(ctx context.Context, owner string, id int64) error {
	return s.stores[owner].MarkSessionTurnDelivered(ctx, owner, id)
}
func (s *agentMemoryFixture) NewSessionContext(ctx context.Context, owner, key string) error {
	return s.stores[owner].NewSessionContext(ctx, owner, fixtureSession(key))
}

// Pending inspection is deliberately distinct from production delivered reads.
func (s *agentMemoryFixture) RecentSessionTurns(owner, key string, offset, count int) ([]memory.SessionTurn, error) {
	rows, err := s.readers[owner].Query(`SELECT a.id,u.content,a.content,e.value FROM sessions s JOIN messages a ON a.session_id=s.id AND a.role='assistant' JOIN state_meta e ON e.key='oswald:v1:turn:'||s.id||':'||a.id JOIN messages u ON u.id=json_extract(e.value,'$.user_message_id') WHERE s.profile_name=? AND s.session_key=? ORDER BY a.id DESC LIMIT ? OFFSET ?`, owner, fixtureSession(key), count, offset-1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []memory.SessionTurn
	for rows.Next() {
		turn := memory.SessionTurn{UserID: owner, SessionID: fixtureSession(key)}
		var encoded string
		if err := rows.Scan(&turn.ID, &turn.UserText, &turn.AssistantText, &encoded); err != nil {
			return nil, err
		}
		var state struct {
			History   string   `json:"history"`
			ToolNames []string `json:"tool_names"`
		}
		if err := json.Unmarshal([]byte(encoded), &state); err != nil {
			return nil, err
		}
		turn.ToolNames = state.ToolNames
		turn.ToolHistory, err = memory.DecodeToolHistory(state.History)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

func (s *agentMemoryFixture) AppendSessionTurn(ctx context.Context, key, owner, user, answer string, names []string, ttl time.Duration) error {
	scope, err := s.ResolveSessionContext(ctx, owner, key, ttl)
	if err != nil {
		return err
	}
	return s.AppendSessionTurnForGeneration(ctx, key, owner, scope.Generation, user, answer, names, ttl)
}
func (s *agentMemoryFixture) AppendSessionTurnForGeneration(ctx context.Context, key, owner string, g int, user, answer string, names []string, ttl time.Duration) error {
	turn, err := s.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{UserID: owner, SessionID: key, Generation: g, UserText: user, AssistantText: answer, ToolNames: names, TTL: ttl})
	if err != nil {
		return err
	}
	return s.MarkSessionTurnDelivered(ctx, owner, turn.ID)
}

func (s *agentMemoryFixture) publishSummary(ctx context.Context, owner, key string, g, count int, artifact memory.SummaryArtifact) error {
	store := s.stores[owner]
	scope := memory.ActiveSessionScope{UserID: owner, SessionID: fixtureSession(key), Generation: g}
	turns, _, err := store.CompressionCandidates(ctx, scope, 0)
	if err != nil {
		return err
	}
	if len(turns) < count {
		return errors.New("insufficient synthetic summary sources")
	}
	turns = turns[:count]
	claim, err := store.ClaimCompression(ctx, scope, turns, memory.CompressionContract("test-model", "test-v1", 100000))
	if err != nil {
		return err
	}
	if err := store.ReserveCompressionSubmission(ctx, claim); err != nil {
		return err
	}
	if err := store.SaveCompressionArtifact(ctx, claim, artifact); err != nil {
		return err
	}
	return store.PublishProfileSummary(ctx, claim, turns, artifact)
}
