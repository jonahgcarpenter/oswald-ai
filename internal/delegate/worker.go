package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	tooldelegate "github.com/jonahgcarpenter/oswald-ai/internal/tools/delegate"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// dispatchEnvelope is the durable record of one unit: everything needed to
// run, deliver, and audit it without re-reading operator configuration.
type dispatchEnvelope struct {
	CallID       string          `json:"call_id"`
	UnitID       string          `json:"unit_id"`
	UnitIndex    int             `json:"unit_index"`
	UnitCount    int             `json:"unit_count"`
	Goal         string          `json:"goal"`
	Context      string          `json:"context"`
	HasSchema    bool            `json:"has_schema"`
	Schema       map[string]any  `json:"output_schema,omitempty"`
	Images       []string        `json:"images,omitempty"`
	Role         string          `json:"role"`
	Depth        int             `json:"depth"`
	Model        string          `json:"model"`
	Provider     string          `json:"provider"`
	BaseURL      string          `json:"base_url"`
	MaxIter      int             `json:"max_iterations"`
	Principal    storedPrincipal `json:"principal"`
	SessionKey   string          `json:"session_key"`
	DisplayName  string          `json:"display_name,omitempty"`
	EnabledTools []string        `json:"enabled_toolsets,omitempty"`
	MCPServers   []string        `json:"mcp_servers,omitempty"`
	Stateless    bool            `json:"stateless_origin"`
	ChatID       string          `json:"chat_id,omitempty"`
	ChatLabel    string          `json:"chat_label,omitempty"`
	ChatName     string          `json:"chat_display_name,omitempty"`
	Transport    string          `json:"transport_profile,omitempty"`
	PlatformMsg  string          `json:"platform_message_id,omitempty"`
	IsDirect     bool            `json:"is_direct,omitempty"`
}

type storedPrincipal struct {
	UserID     string `json:"user_id"`
	Gateway    string `json:"gateway"`
	ExternalID string `json:"external_id"`
	Assurance  string `json:"assurance"`
}

// registerLive tracks a running unit for control and stall monitoring.
func (m *Manager) registerLive(child *liveChild) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live[child.id] = child
}

// forgetLive drops a terminal unit from the live set. Late worker results
// check membership first and are discarded once forgotten.
func (m *Manager) forgetLive(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, id)
}

// liveForSession returns live children owned by one origin session, newest
// first. Used by control actions and /new cancellation.
func (m *Manager) liveForSession(profile, sessionKey string) []*liveChild {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*liveChild
	for _, child := range m.live {
		if child.done || child.origin.profile != profile || child.origin.sessionKey != sessionKey {
			continue
		}
		out = append(out, child)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].started.After(out[j].started) })
	return out
}

// serveUnit runs one dispatched unit to a terminal state: claim, execute,
// record. Abandoned units (stopped, crashed, or force-finalized) are dropped
// by the generation check so only the owning worker records the outcome.
func (m *Manager) serveUnit(workerCtx context.Context, callID, unitID string, task tooldelegate.Task, index, total int, origin originRef, model, provider, billingURL string, client llm.Chatter) {
	defer m.wg.Done()
	select {
	case m.pool <- struct{}{}:
	case <-workerCtx.Done():
		return
	}
	defer func() { <-m.pool }()
	claimed, err := m.store.ClaimDelegation(workerCtx, unitID, float64(os.Getpid()), nowUnix())
	if err != nil || !claimed {
		m.log.Debug("delegate.worker.claim_failed", "delegation worker lost its claim", config.F("delegation_id", unitID), config.F("status", "degraded"))
		return
	}
	res := m.runUnit(workerCtx, callID, unitID, task, index, total, origin, model, provider, billingURL, client, "leaf", 1)
	m.finishUnit(workerCtx, unitID, res)
}

// finishUnit records one terminal outcome exactly once. The worker and the
// stall monitor race here; whoever wins the live-set transition owns the
// write and the loser drops its result. Units dropped by /new cancellation
// are recorded honestly but marked delivered immediately: the owning
// conversation is gone, so there is nowhere to deliver to.
func (m *Manager) finishUnit(ctx context.Context, unitID string, res childResult) {
	m.mu.Lock()
	child, ok := m.live[unitID]
	if !ok || child.done {
		m.mu.Unlock()
		return
	}
	child.done = true
	child.finishedAt = time.Now()
	drop := child.drop
	delete(m.live, unitID)
	m.mu.Unlock()

	data, err := marshalResult(res)
	if err != nil {
		data = "{}"
	}
	owner := int64(os.Getpid())
	if err := m.store.FinishDelegation(ctx, unitID, owner, delegationState(res.Status), data); err != nil {
		m.log.Debug("delegate.worker.record_failed", "failed to record delegation outcome", config.F("delegation_id", unitID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	if drop {
		if err := m.store.MarkDelivered(ctx, unitID, nowUnix()); err != nil {
			m.log.Debug("delegate.worker.drop_failed", "failed to mark cancelled delegation delivered", config.F("delegation_id", unitID), config.F("status", "degraded"), config.ErrorField(err))
		}
		return
	}
	m.log.Debug("delegate.worker.complete", "recorded delegation outcome", config.F("delegation_id", unitID), config.F("status", res.Status))
}

// delegationState maps a terminal child status onto the ledger vocabulary.
// Anything unrecognized fails closed as failed, never as completed.
func delegationState(status string) string {
	switch status {
	case "completed":
		return memory.DelegationCompleted
	case "interrupted":
		return memory.DelegationInterrupted
	case "stalled":
		return memory.DelegationStalled
	case "failed":
		return memory.DelegationFailed
	default:
		return memory.DelegationFailed
	}
}

// runUnit executes one unit synchronously and records the terminal outcome.
// Nested orchestrator calls share this path; only the worker and delivery
// loop touch the ledger for top-level dispatches.
func (m *Manager) runUnit(workerCtx context.Context, callID, unitID string, task tooldelegate.Task, index, total int, origin originRef, model, provider, billingURL string, client llm.Chatter, role string, depth int) childResult {
	trans, err := newTranscript(m.transcriptPath(callID, unitID, depth))
	if err != nil {
		return childResult{Status: "failed", Summary: "Subagent failed to start: transcript unavailable."}
	}
	steer := agent.NewSteerInbox()
	childCtx, cancel := context.WithCancel(workerCtx)
	transAct := trans.act
	rec := &liveChild{
		id: unitID, callID: callID, goal: task.Goal, role: role, depth: depth,
		origin: origin, started: time.Now(), cancel: cancel, steer: steer,
		activity: transAct, routeModel: model, routeProvider: provider,
	}
	m.registerLive(rec)

	spec := childSpec{
		goal: task.Goal, context: task.Context, outputSchema: task.OutputSchema,
		imageHints: task.Images, model: model, provider: provider, billingURL: billingURL,
		client: client, role: role, depth: depth, maxIter: m.maxIterations(),
		principal: origin.principal, sessionKey: origin.sessionKey, displayName: origin.displayName,
		enabledTools: origin.enabledTools, mcpServers: origin.mcpServers,
		steer: steer, requestID: unitID, transcript: trans,
	}
	withSpawn := WithSpawn(childCtx, depth, role)
	res := m.runChild(withSpawn, spec, rec)
	trans.close("terminal " + res.Status)
	return res
}

// transcriptPath names the per-unit live log. Nested runs share the call
// tree directory and gain a depth suffix.
func (m *Manager) transcriptPath(callID, unitID string, depth int) string {
	name := sanitizedUnitName(unitID)
	if depth > 1 {
		name = fmt.Sprintf("%s-d%d", sanitizedUnitName(unitID), depth)
	}
	return filepath.Join(m.profileRoot, "cache", "delegation", "live", sanitizedUnitName(callID), "task-"+name+".log")
}

func sanitizedUnitName(id string) string {
	var sb strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	out := sb.String()
	if out == "" || out == "." || out == ".." {
		return "task"
	}
	return out
}

// childBlockedTools lists builtins withheld from a child catalog. The
// delegate tool itself is appended unless nesting explicitly allows it.
func childBlockedTools(includeDelegate bool) []string {
	out := append([]string(nil), BlockedTools...)
	if includeDelegate {
		kept := out[:0]
		for _, name := range out {
			if name != "delegate" {
				kept = append(kept, name)
			}
		}
		out = kept
	}
	return out
}

// collectOrphans kills still-running child-owned background processes and
// attaches unread output from completed ones. Handoff-detached processes
// (Owner "handoff") are left alone: the child explicitly handed them off.
func (m *Manager) collectOrphans(childReqID string) (orphans, unread []string) {
	if m == nil || m.procs == nil {
		return nil, nil
	}
	procs, err := m.procs.List()
	if err != nil {
		return nil, nil
	}
	for _, info := range procs {
		if info.Owner != childReqID {
			continue
		}
		if info.Status == process.StatusRunning {
			if _, err := m.procs.Kill(info.ID); err == nil {
				orphans = append(orphans, info.ID+": "+info.CommandPreview)
			}
			continue
		}
		tail, tailErr := m.procs.Tail(info.ID, 5)
		entry := info.ID + " (exit " + strconv.Itoa(info.ExitCode) + ")"
		if tailErr == nil && strings.TrimSpace(tail) != "" {
			entry += ":\n" + tail
		}
		unread = append(unread, entry)
	}
	return orphans, unread
}
