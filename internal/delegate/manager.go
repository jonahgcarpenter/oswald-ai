package delegate

import (
	"context"
	"fmt"
	"strings"

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
	return m.dispatch(ctx, principal, call)
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
// spawn. Depth 0 (normal turns) always dispatches async; deeper turns run
// inline only for orchestrators with headroom under the configured cap.
func (m *Manager) nestingAllowed(role string, depth int) bool {
	if !m.orchestratorOn() || role != "orchestrator" {
		return false
	}
	return depth+1 <= m.maxDepth()
}

// dispatch records one batch and returns a handle immediately. Workers pick
// units up asynchronously; results arrive later as new turns.
func (m *Manager) dispatch(ctx context.Context, principal identity.Principal, call delegate.Call) (governance.Result, error) {
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
	origin := originRef{
		profile:      m.profile,
		sessionKey:   meta.SessionID,
		principal:    principal,
		gateway:      principal.Gateway,
		enabledTools: enabledToolsFromContext(ctx),
		mcpServers:   mcpServersFromContext(ctx),
	}
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
	if len(tasks) == 1 {
		sb.WriteString(" running in the background.")
	} else {
		sb.WriteString(" running in the background.")
	}
	sb.WriteString(" Results will arrive as new messages in this session. Track them with delegate(action=list).")
	return governance.Result{Content: sb.String(), Outcome: governance.OutcomeProductive}, nil
}
