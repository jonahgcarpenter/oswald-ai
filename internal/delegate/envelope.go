package delegate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	tooldelegate "github.com/jonahgcarpenter/oswald-ai/internal/tools/delegate"
)

// validatedTasks re-checks the batch the tool schema already validated:
// non-empty goals, image-count bounds, and schema shapes. Defense in depth:
// the registry normalizer never mutates arguments, so a second parse here
// costs nothing and keeps dispatch honest if the entry point ever changes.
func validatedTasks(tasks []tooldelegate.Task) ([]tooldelegate.Task, error) {
	if len(tasks) == 0 {
		return nil, fmt.Errorf("delegate: no tasks to dispatch")
	}
	out := make([]tooldelegate.Task, 0, len(tasks))
	for i, task := range tasks {
		if strings.TrimSpace(task.Goal) == "" {
			return nil, fmt.Errorf("delegate: tasks[%d] has an empty goal", i)
		}
		if len(task.Images) > tooldelegate.MaxImagesPerTask {
			return nil, fmt.Errorf("delegate: tasks[%d] exceeds %d images", i, tooldelegate.MaxImagesPerTask)
		}
		out = append(out, task)
	}
	return out, nil
}

// marshalTask serializes one unit's dispatch envelope: everything the worker
// needs to run and everything the delivery loop needs to report, without
// re-reading operator configuration.
func marshalTask(task tooldelegate.Task, role, unitID, callID string, index, total int, origin originRef, model, provider, billingURL string, maxIter int) (string, error) {
	env := dispatchEnvelope{
		CallID:    callID,
		UnitID:    unitID,
		UnitIndex: index,
		UnitCount: total,
		Goal:      strings.TrimSpace(task.Goal),
		Context:   task.Context,
		HasSchema: len(task.OutputSchema) > 0,
		Schema:    task.OutputSchema,
		Images:    append([]string(nil), task.Images...),
		Role:      role,
		Depth:     1,
		Model:     model,
		Provider:  provider,
		BaseURL:   billingURL,
		MaxIter:   maxIter,
		Principal: storedPrincipal{
			UserID:     origin.principal.CanonicalUserID,
			Gateway:    origin.principal.Gateway,
			ExternalID: origin.principal.ExternalID,
			Assurance:  string(origin.principal.Assurance),
		},
		SessionKey:   origin.sessionKey,
		DisplayName:  origin.displayName,
		EnabledTools: append([]string(nil), origin.enabledTools...),
		MCPServers:   append([]string(nil), origin.mcpServers...),
		Stateless:    origin.stateless,
		ChatID:       origin.chatID,
		ChatLabel:    origin.chatLabel,
		ChatName:     origin.chatDisplayName,
		Transport:    origin.transportProfile,
		PlatformMsg:  origin.platformMessage,
		IsDirect:     origin.isDirect,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("delegate: failed to encode task: %w", err)
	}
	return string(raw), nil
}

// delegationRow builds the durable ledger row for one unit. The unit starts
// running and undelivered; the worker claims it and FinishDelegation closes
// it, so a crash between insert and claim reaps as unknown, never as new.
func delegationRow(unitID, callID string, origin originRef, envelope string, now float64) memory.DelegationRow {
	return memory.DelegationRow{
		ID:              unitID,
		OriginSession:   origin.sessionKey,
		OriginUISession: origin.sessionKey,
		ParentSessionID: callID,
		State:           memory.DelegationRunning,
		DeliveryState:   memory.DelegationDeliveryPending,
		TaskJSON:        envelope,
		OriginSessionID: origin.sessionKey,
	}
}

// marshalResult serializes a terminal child outcome into result_json.
func marshalResult(res childResult) (string, error) {
	raw, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
