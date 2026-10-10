package config

// Activity events intentionally omit development telemetry inherited from
// scoped loggers. DEBUG diagnostics retain that telemetry with the same privacy
// filtering. This catalog is also enforced by the production source guard.
var activityLogEvents = map[string]bool{
	"app.started": true, "app.stopped": true, "gateway.connected": true,
	"chat.requested": true, "chat.completed": true, "command.completed": true,
	"tool.completed": true, "tool.blocked": true,
	"compaction.profile.started": true, "compaction.profile.complete": true,
}

// activityLogFields allowlists the details keys retained on INFO activity
// records. Everything else in details is dropped for these events.
var activityLogFields = map[string]bool{
	"msg":     true,
	"profile": true, "user_id": true, "gateway": true, "request_id": true,
	"operation_id": true, "parent_operation_id": true,
	"input_type": true, "command_name": true, "tool_name": true, "scope": true,
	"status": true, "outcome": true, "execution_outcome": true, "delivery_outcome": true,
	"reason_code": true, "error_code": true, "duration_ms": true, "is_execution_complete": true,
	"tool_execution_count": true, "tool_count": true, "profile_count": true, "gateway_count": true, "cleanup_reason": true,
	"input_tokens": true, "output_tokens": true,
	"turn_count": true, "is_submitted": true, "is_artifact_reused": true,
	"bot_id": true, "bot_name": true, "private_api": true, "webhook_path": true, "listen_addr": true, "port": true,
}

func compactActivityLog(details map[string]any, event string) {
	if !activityLogEvents[event] {
		return
	}
	for key := range details {
		if !activityLogFields[key] {
			delete(details, key)
		}
	}
}
