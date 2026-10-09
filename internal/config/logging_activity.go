package config

// Activity events intentionally omit development telemetry inherited from
// scoped loggers. DEBUG diagnostics retain that telemetry with the same privacy
// filtering. This catalog is also enforced by the production source guard.
var activityLogEvents = map[string]bool{
	"app.started": true, "app.stopped": true, "gateway.connected": true,
	"chat.requested": true, "chat.completed": true, "command.completed": true,
	"tool.completed": true, "tool.blocked": true,
}

var activityLogFields = map[string]bool{
	"ts": true, "level": true, "event": true, "component": true, "msg": true,
	"service": true, "log_type": true, "instance_id": true, "record_kind": true,
	"user_id": true, "gateway": true, "request_id": true, "operation_id": true, "parent_operation_id": true,
	"input_type": true, "command_name": true, "tool_name": true, "scope": true,
	"status": true, "outcome": true, "execution_outcome": true, "delivery_outcome": true,
	"reason_code": true, "error_code": true, "duration_ms": true, "is_execution_complete": true,
	"tool_execution_count": true, "profile_count": true, "gateway_count": true, "cleanup_reason": true,
}

func compactActivityLog(payload map[string]any, event string) {
	if !activityLogEvents[event] {
		return
	}
	for key := range payload {
		if !activityLogFields[key] {
			delete(payload, key)
		}
	}
}
