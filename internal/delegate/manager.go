package delegate

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/delegate"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// Execute is the registry entry point for the delegate tool: it validates
// arguments, then dispatches, inlines nested calls, or drives control actions.
func (m *Manager) Execute(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
	if m == nil {
		return governance.Result{}, fmt.Errorf("delegate tool is unavailable in this build")
	}
	call, err := delegate.ParseArgs(args)
	if err != nil {
		return governance.Result{}, err
	}
	principal, ok := requestctx.PrincipalFromContext(ctx)
	if !ok || !principal.Authenticated() {
		return governance.Result{}, fmt.Errorf("delegate requires an authenticated principal")
	}
	meta := requestctx.MetadataFromContext(ctx)
	sessionKey := meta.SessionID
	switch call.Action {
	case "list":
		return m.controlList(ctx, m.profile, sessionKey)
	case "steer":
		return m.controlSteer(ctx, m.profile, sessionKey, call.SubagentID, call.Message)
	case "stop":
		return m.controlStop(ctx, m.profile, sessionKey, call.SubagentID)
	}
	spawn := SpawnFromContext(ctx)
	if spawn.Depth > 0 {
		return m.runNested(ctx, spawn, principal, call)
	}
	if call.Background {
		return m.dispatchBackground(ctx, principal, call)
	}
	return m.dispatchSync(ctx, principal, call)
}

// runNested executes an in-turn child for orchestrator nesting. Nested calls
// never touch the durable ledger: the parent turn consumes the summary
// directly, so there is nothing to deliver or recover.
func (m *Manager) runNested(ctx context.Context, spawn Spawn, principal identity.Principal, call delegate.Call) (governance.Result, error) {
	if !m.nestingAllowed(call.Role, spawn.Depth) {
		return governance.Result{}, fmt.Errorf("nested delegation is not permitted here")
	}
	if len(call.Tasks) != 1 {
		return governance.Result{}, fmt.Errorf("nested delegation takes a single task")
	}
	model, provider, billingURL, client, err := m.resolveTarget()
	if err != nil {
		return governance.Result{}, err
	}
	meta := requestctx.MetadataFromContext(ctx)
	task := call.Tasks[0]
	spec := childSpec{
		goal: task.Goal, context: task.Context, outputSchema: task.OutputSchema,
		imageHints: task.Images, model: model, provider: provider, billingURL: billingURL,
		client: client, role: call.Role, depth: spawn.Depth + 1, maxIter: m.maxIterations(),
		principal: principal, sessionKey: meta.SessionID,
		enabledTools: enabledToolsFromContext(ctx), mcpServers: mcpServersFromContext(ctx),
		steer: nil, requestID: config.NewRequestID(), transcript: nil,
	}
	res := m.runChild(WithSpawn(ctx, spawn.Depth+1, call.Role), spec, nil)
	if res.Status == "failed" {
		return governance.Result{}, fmt.Errorf("%s", res.Summary)
	}
	return governance.Result{Content: res.Summary, Outcome: governance.OutcomeProductive}, nil
}

// nestingAllowed reports whether a turn at the given depth and role may
// spawn. Depth 0 (normal turns) runs sync by default or async with
// background:true; deeper turns run inline only for orchestrators with
// headroom under the configured cap.
func (m *Manager) nestingAllowed(role string, depth int) bool {
	if !m.orchestratorOn() || role != "orchestrator" {
		return false
	}
	return depth+1 <= m.maxDepth()
}

// dispatchSync runs one batch inline and returns the consolidated result as
// this call's tool result. The main agent loop stays blocked inside Execute
// until every task finishes, so no intermediate final is emitted. Sync units
// never touch the durable ledger: the parent consumes the summary directly,
// like nested runs. They are still registered live while running so the
// stall monitor can interrupt wedged work.
func (m *Manager) dispatchSync(ctx context.Context, principal identity.Principal, call delegate.Call) (governance.Result, error) {
	meta := requestctx.MetadataFromContext(ctx)
	maxTasks := m.maxConcurrent()
	if len(call.Tasks) > maxTasks {
		return governance.Result{}, fmt.Errorf("delegate: at most %d tasks per call", maxTasks)
	}
	tasks, err := validatedTasks(call.Tasks)
	if err != nil {
		return governance.Result{}, err
	}
	model, provider, billingURL, client, err := m.resolveTarget()
	if err != nil {
		return governance.Result{}, err
	}
	origin := m.originFromContext(ctx, principal, meta.SessionID)
	callID := config.NewRequestID()
	results := make([]childResult, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		unitID := callID
		if len(tasks) > 1 {
			unitID = fmt.Sprintf("%s-%d", callID, i+1)
		}
		wg.Add(1)
		go func(index int, id string, t delegate.Task) {
			defer wg.Done()
			if m.pool != nil {
				select {
				case m.pool <- struct{}{}:
					defer func() { <-m.pool }()
				case <-ctx.Done():
					results[index] = childResult{Status: "interrupted", Summary: "Subagent interrupted: parent turn ended."}
					return
				}
			}
			res := m.runUnit(ctx, callID, id, t, index, len(tasks), origin, model, provider, billingURL, client, call.Role, 1)
			m.cleanupSyncUnit(id)
			results[index] = res
		}(i, unitID, task)
	}
	wg.Wait()
	if ctx.Err() != nil && allInterrupted(results) {
		return governance.Result{}, ctx.Err()
	}
	if len(results) == 1 && results[0].Status == "failed" {
		return governance.Result{}, fmt.Errorf("%s", results[0].Summary)
	}
	return governance.Result{Content: formatSyncResults(tasks, results), Outcome: governance.OutcomeProductive}, nil
}

// cleanupSyncUnit releases a finished sync unit's live registration. Sync
// units never reach finishUnit (no ledger write), so drop the live entry and
// cancel its context directly.
func (m *Manager) cleanupSyncUnit(unitID string) {
	m.mu.Lock()
	child, ok := m.live[unitID]
	if ok {
		delete(m.live, unitID)
	}
	m.mu.Unlock()
	if ok && child != nil && child.cancel != nil {
		child.cancel()
	}
}

func allInterrupted(results []childResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, res := range results {
		if res.Status != "interrupted" {
			return false
		}
	}
	return true
}

// formatSyncResults renders one consolidated tool result in input order. A
// single task returns its summary plus terminal notes; a batch prefixes each
// unit with a TASK i/N header carrying its goal.
func formatSyncResults(tasks []delegate.Task, results []childResult) string {
	if len(tasks) == 1 && len(results) == 1 {
		return formatSingleSync(tasks[0], results[0])
	}
	var sb strings.Builder
	for i := range tasks {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "TASK %d/%d\nGoal: %s\n", i+1, len(tasks), strings.TrimSpace(tasks[i].Goal))
		sb.WriteString("Result:\n")
		sb.WriteString(strings.TrimSpace(syncResultBody(results[i])))
	}
	return strings.TrimSpace(sb.String())
}

func formatSingleSync(task delegate.Task, res childResult) string {
	return strings.TrimSpace(syncResultBody(res))
}

// syncResultBody renders a terminal child outcome inline: summary plus the
// same terminal notes deliveryTurn attaches (truncation, schema, timeout,
// orphans, unread, missed steer), so sync parents lose nothing by skipping
// the broker handoff.
func syncResultBody(res childResult) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(res.Summary))
	if res.Truncated {
		sb.WriteString("\nNote: the subagent hit its iteration budget; results above may be partial.")
	}
	if res.SchemaValid != nil && !*res.SchemaValid {
		sb.WriteString("\nNote: the final answer did not validate against the requested schema")
		if len(res.SchemaErrors) > 0 {
			sb.WriteString(": " + strings.Join(res.SchemaErrors, "; "))
		}
		sb.WriteString(". Treat it as unvalidated text.")
	}
	if res.Timeout != nil {
		fmt.Fprintf(&sb, "\nNote: the subagent stopped making progress (no activity for %ds, %s); its partial work was abandoned.", res.Timeout.Elapsed, res.Timeout.Phase)
	}
	for _, orphan := range res.Orphans {
		sb.WriteString("\nNote: background process terminated with its subagent: " + orphan + ".")
	}
	for _, unread := range res.Unread {
		sb.WriteString("\nNote: unread child process output:\n" + unread)
	}
	for _, missed := range res.MissedSteer {
		sb.WriteString("\nNote: steer did not land — the subagent finished before it could be delivered: " + missed)
	}
	return strings.TrimSpace(sb.String()) + "\n"
}

// dispatchBackground records one batch and returns a handle immediately.
// Workers pick units up asynchronously; results arrive later as new turns
// pushed to the origin conversation. End your turn after dispatching: do not
// call list to poll unless the user asks for status.
func (m *Manager) dispatchBackground(ctx context.Context, principal identity.Principal, call delegate.Call) (governance.Result, error) {
	meta := requestctx.MetadataFromContext(ctx)
	maxTasks := m.maxConcurrent()
	if len(call.Tasks) > maxTasks {
		return governance.Result{}, fmt.Errorf("delegate: at most %d tasks per call", maxTasks)
	}
	tasks, err := validatedTasks(call.Tasks)
	if err != nil {
		return governance.Result{}, err
	}
	model, provider, billingURL, client, err := m.resolveTarget()
	if err != nil {
		return governance.Result{}, err
	}
	origin := m.originFromContext(ctx, principal, meta.SessionID)
	callID := config.NewRequestID()
	now := nowUnix()
	subIDs := make([]string, 0, len(tasks))
	for i, task := range tasks {
		unitID := callID
		if len(tasks) > 1 {
			unitID = fmt.Sprintf("%s-%d", callID, i+1)
		}
		envelope, err := marshalTask(task, call.Role, unitID, callID, i, len(tasks), origin, model, provider, billingURL, m.maxIterations())
		if err != nil {
			return governance.Result{}, err
		}
		row := delegationRow(unitID, callID, origin, envelope, now)
		if err := m.store.InsertDelegation(ctx, row); err != nil {
			return governance.Result{}, fmt.Errorf("delegate: failed to record dispatch: %w", err)
		}
		m.wg.Add(1)
		go m.serveUnit(context.WithoutCancel(ctx), callID, unitID, task, i, len(tasks), origin, model, provider, billingURL, client)
		subIDs = append(subIDs, unitID)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Delegated %d task(s) as subagent %s", len(tasks), strings.Join(subIDs, ", "))
	sb.WriteString(" running in the background.")
	sb.WriteString(" End your turn with a one-line acknowledgment; the result will arrive as a new message in this session. Do not call list unless the user asks for status.")
	return governance.Result{Content: sb.String(), Outcome: governance.OutcomeProductive}, nil
}

// originFromContext snapshots the calling turn's ownership plus its transport
// destination so background delivery can route home. Ownership still comes
// from the principal; routing never affects it.
func (m *Manager) originFromContext(ctx context.Context, principal identity.Principal, sessionKey string) originRef {
	routing, _ := requestctx.TransportRoutingFromContext(ctx)
	return originRef{
		profile:          m.profile,
		sessionKey:       sessionKey,
		principal:        principal,
		gateway:          principal.Gateway,
		enabledTools:     enabledToolsFromContext(ctx),
		mcpServers:       mcpServersFromContext(ctx),
		chatID:           routing.ChatID,
		chatLabel:        routing.ChatLabel,
		chatDisplayName:  routing.ChatDisplayName,
		transportProfile: routing.TransportProfile,
		platformMessage:  routing.PlatformMessage,
		isDirect:         routing.IsDirect,
	}
}
