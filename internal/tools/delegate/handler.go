package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// Task is one validated unit of delegated work.
type Task struct {
	Goal         string
	Context      string
	OutputSchema map[string]interface{}
	Images       []string
}

// Call is a validated delegate invocation: either a spawn (single task or
// batch) or a control action against live subagents.
type Call struct {
	Action     string
	Tasks      []Task
	Role       string
	SubagentID string
	Message    string
	// Background, when true, dispatches asynchronously and returns a handle
	// immediately; results arrive later as new turns. The default (false)
	// blocks until every task finishes and returns the consolidated result
	// as this call's tool result.
	Background bool
}

// ParseArgs validates raw tool arguments into a Call. Spawn takes either a
// single goal or a tasks batch, never both; control actions take a
// subagent_id (plus message for steer) and ignore spawn fields.
func ParseArgs(args map[string]interface{}) (Call, error) {
	var call Call
	action, _ := args["action"].(string)
	call.Action = strings.TrimSpace(action)
	if call.Action == "" {
		call.Action = "spawn"
	}
	switch call.Action {
	case "spawn", "list", "steer", "stop":
	default:
		return Call{}, fmt.Errorf("delegate: action must be spawn, list, steer, or stop")
	}
	role, _ := args["role"].(string)
	call.Role = strings.TrimSpace(role)
	if call.Role == "" {
		call.Role = "leaf"
	}
	if call.Role != "leaf" && call.Role != "orchestrator" {
		return Call{}, fmt.Errorf("delegate: role must be leaf or orchestrator")
	}
	subagent, _ := args["subagent_id"].(string)
	call.SubagentID = strings.TrimSpace(subagent)
	message, _ := args["message"].(string)
	call.Message = strings.TrimSpace(message)
	if raw, ok := args["background"]; ok && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return Call{}, fmt.Errorf("delegate: background must be a boolean")
		}
		call.Background = b
	}
	if call.Action == "list" {
		return call, nil
	}
	if call.Action == "steer" || call.Action == "stop" {
		if strings.TrimSpace(call.SubagentID) == "" {
			return Call{}, fmt.Errorf("delegate: subagent_id is required to %s a subagent", call.Action)
		}
		if call.Action == "steer" && strings.TrimSpace(call.Message) == "" {
			return Call{}, fmt.Errorf("delegate: message is required to steer a subagent")
		}
		return call, nil
	}
	tasks, hasTasks := args["tasks"]
	goal, _ := args["goal"].(string)
	contextText, _ := args["context"].(string)
	schema, hasSchema := args["output_schema"]
	images, hasImages := args["images"]
	if hasTasks && (strings.TrimSpace(goal) != "" || strings.TrimSpace(contextText) != "" || hasSchema || hasImages) {
		return Call{}, fmt.Errorf("delegate: tasks is mutually exclusive with goal, context, output_schema, and images")
	}
	if !hasTasks {
		if strings.TrimSpace(goal) == "" {
			return Call{}, fmt.Errorf("delegate: goal is required")
		}
		spec, err := parseTask(map[string]interface{}{"goal": goal, "context": contextText, "output_schema": schema, "images": images})
		if err != nil {
			return Call{}, err
		}
		call.Tasks = []Task{spec}
		return call, nil
	}
	list, ok := tasks.([]interface{})
	if !ok || len(list) == 0 {
		return Call{}, fmt.Errorf("delegate: tasks must be a non-empty array")
	}
	if len(list) > MaxTasksPerCall {
		return Call{}, fmt.Errorf("delegate: at most %d tasks per call", MaxTasksPerCall)
	}
	for i, raw := range list {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			return Call{}, fmt.Errorf("delegate: tasks[%d] must be an object", i)
		}
		spec, err := parseTask(entry)
		if err != nil {
			return Call{}, fmt.Errorf("delegate: tasks[%d]: %w", i, err)
		}
		call.Tasks = append(call.Tasks, spec)
	}
	return call, nil
}

func parseTask(entry map[string]interface{}) (Task, error) {
	goal, _ := entry["goal"].(string)
	if strings.TrimSpace(goal) == "" {
		return Task{}, errors.New("goal is required")
	}
	contextText, _ := entry["context"].(string)
	task := Task{Goal: strings.TrimSpace(goal), Context: strings.TrimSpace(contextText)}
	if raw, ok := entry["output_schema"]; ok && raw != nil {
		spec, ok := raw.(map[string]interface{})
		if !ok {
			return Task{}, errors.New("output_schema must be a JSON object")
		}
		task.OutputSchema = spec
	}
	if raw, ok := entry["images"]; ok && raw != nil {
		list, ok := raw.([]interface{})
		if !ok {
			return Task{}, errors.New("images must be an array of strings")
		}
		if len(list) > MaxImagesPerTask {
			return Task{}, fmt.Errorf("at most %d images per task", MaxImagesPerTask)
		}
		for _, item := range list {
			path, ok := item.(string)
			if !ok || strings.TrimSpace(path) == "" {
				return Task{}, errors.New("images must be an array of strings")
			}
			task.Images = append(task.Images, strings.TrimSpace(path))
		}
	}
	return task, nil
}

// PlaceholderHandler fails closed until startup installs the live delegate
// manager. It keeps the tool visible in every catalog while guaranteeing no
// dispatch runs without the full runtime (pool, store, delivery) behind it.
func PlaceholderHandler() func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		return governance.Result{}, errors.New("delegate tool is unavailable in this build")
	}
}
