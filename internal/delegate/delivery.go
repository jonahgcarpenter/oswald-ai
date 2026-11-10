package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
)

// deliveryTurn formats a terminal child result as a new-turn prompt in the
// origin session. Bodies stay bounded: the summary is the deliverable, not
// the child's whole workspace.
const maxDeliveryRunes = 16000

func deliveryTurn(unitID, goal string, res childResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Subagent %s finished (%s).\n", unitID, res.Status)
	sb.WriteString("Goal: " + strings.TrimSpace(goal) + "\n")
	sb.WriteString("Result:\n" + truncateRunes(res.Summary, maxDeliveryRunes) + "\n")
	if res.Truncated {
		sb.WriteString("Note: the subagent hit its iteration budget; results above may be partial.\n")
	}
	if res.SchemaValid != nil && !*res.SchemaValid {
		sb.WriteString("Note: the final answer did not validate against the requested schema")
		if len(res.SchemaErrors) > 0 {
			sb.WriteString(": " + strings.Join(res.SchemaErrors, "; "))
		}
		sb.WriteString(". Treat it as unvalidated text.\n")
	}
	if res.Timeout != nil {
		fmt.Fprintf(&sb, "Note: the subagent stopped making progress (no activity for %ds, %s); its partial work was abandoned.\n", res.Timeout.Elapsed, res.Timeout.Phase)
	}
	for _, orphan := range res.Orphans {
		sb.WriteString("Note: background process terminated with its subagent: " + orphan + ".\n")
	}
	for _, unread := range res.Unread {
		sb.WriteString("Note: unread child process output:\n" + unread + "\n")
	}
	for _, missed := range res.MissedSteer {
		sb.WriteString("Note: steer did not land — the subagent finished before it could be delivered: " + missed + "\n")
	}
	return strings.TrimSpace(sb.String()) + "\n"
}

func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "\n[…truncated]"
	}
	return s
}

// deliveryLoop picks up terminal-but-undelivered rows and enqueues one lane
// turn per unit. It runs until the manager stops.
func (m *Manager) deliveryLoop(ctx context.Context, tick time.Duration) {
	defer m.wg.Done()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.deliverDue(ctx)
		}
	}
}

func (m *Manager) deliverDue(ctx context.Context) {
	due, err := m.store.ListDueDeliveries(ctx, m.maxConcurrent()+16)
	if err != nil {
		m.log.Debug("delegate.delivery.list_failed", "delegation delivery scan failed", config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	for _, row := range due {
		var env dispatchEnvelope
		if err := json.Unmarshal([]byte(row.TaskJSON), &env); err != nil || env.UnitID == "" {
			_ = m.store.FailDelivery(ctx, row.ID, row.DeliveryAttempts+1, nowUnix())
			continue
		}
		token := "delivery-" + row.ID + "-" + fmt.Sprint(row.DeliveryAttempts)
		claimed, err := m.store.ClaimDelivery(ctx, row.ID, token, nowUnix())
		if err != nil || !claimed {
			continue
		}
		if env.Stateless {
			_ = m.store.MarkDelivered(ctx, row.ID, nowUnix())
			continue
		}
		var result childResult
		if err := json.Unmarshal([]byte(row.ResultJSON), &result); err != nil {
			_ = m.store.FailDelivery(ctx, row.ID, row.DeliveryAttempts+1, nowUnix())
			continue
		}
		text := deliveryTurn(row.ID, firstLine(env.Goal), result)
		err = m.broker.Submit(&broker.Request{
			RequestID:   config.NewRequestID(),
			Principal:   originPrincipal(env),
			DisplayName: env.DisplayName,
			SessionKey:  env.SessionKey,
			ForceQueue:  true,
			Prompt:      text,
		})
		if err != nil {
			_ = m.store.NoteDeliveryAttempt(ctx, row.ID, row.DeliveryAttempts+1, nowUnix())
			if row.DeliveryAttempts+1 >= maxDeliveryAttempts {
				_ = m.store.FailDelivery(ctx, row.ID, row.DeliveryAttempts+1, nowUnix())
				m.log.Warn("delegate.delivery.abandoned", "delegation handoff failed too many times", config.F("delegation_id", row.ID), config.F("status", "degraded"))
			}
			continue
		}
		_ = m.store.MarkDelivered(ctx, row.ID, nowUnix())
	}
}

func originPrincipal(env dispatchEnvelope) identity.Principal {
	return identity.Principal{
		CanonicalUserID: env.Principal.UserID,
		Gateway:         env.Principal.Gateway,
		ExternalID:      env.Principal.ExternalID,
		Assurance:       identity.Assurance(env.Principal.Assurance),
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// pruneLoop bounds the durable ledger: completed work older than the
// retention window goes first, then the oldest terminal rows past the cap.
// Live and pending rows are never touched.
func (m *Manager) pruneLoop(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := m.store.PruneDelegations(ctx, time.Now().Add(-retentionDays*24*time.Hour).Unix(), maxRowsRetained)
			if err != nil {
				m.log.Debug("delegate.prune.failed", "delegation prune failed", config.F("status", "degraded"), config.ErrorField(err))
				continue
			}
			if deleted > 0 {
				m.log.Debug("delegate.prune.completed", "pruned delegation records", config.F("count", deleted))
			}
			m.pruneTranscripts()
		}
	}
}

// pruneTranscripts removes live transcript files whose unit is no longer
// live. Terminal transcript retention is a debugging convenience, not a
// persisted contract: the ledger in state.db is the record.
func (m *Manager) pruneTranscripts() {
	dir := filepath.Join(m.profileRoot, "cache", "delegation", "live")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	m.mu.Lock()
	live := make(map[string]bool, len(m.live))
	for id := range m.live {
		live[sanitizedUnitName(id)] = true
	}
	callLive := make(map[string]bool)
	for _, child := range m.live {
		callLive[sanitizedUnitName(child.callID)] = true
	}
	m.mu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() || !callLive[entry.Name()] {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			keep := false
			for id := range live {
				if strings.HasPrefix(f.Name(), "task-"+id) {
					keep = true
					break
				}
			}
			if !keep {
				_ = os.Remove(filepath.Join(dir, entry.Name(), f.Name()))
			}
		}
	}
}

// recover reaps rows whose owner died with the previous process and queues a
// scan for work that completed but never delivered.
func (m *Manager) recover(ctx context.Context) {
	reaped, err := m.store.ReapOrphanedDelegations(ctx, alivePID, float64(time.Now().UnixNano())/1e9)
	if err != nil {
		m.log.Warn("delegate.recover.failed", "delegation recovery scan failed", config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	if reaped > 0 {
		m.log.Warn("delegate.recover.orphaned", "marked orphaned delegations unknown", config.F("count", reaped))
	}
}
