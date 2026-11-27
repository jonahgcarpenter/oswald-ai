package tools

import (
	"crypto/sha256"
	"encoding/hex"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
	"strings"
)

// registerHandlers wires configured builtin handlers and policies into the registry.

func normalizeBrowserNavigateArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"url": normalizedString(args, "url", false)}
}

func normalizeBrowserSnapshotArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"full": args["full"]}
}

func normalizeBrowserRefArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"ref", "key"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	return normalized
}

func normalizeBrowserTypeArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["ref"].(string); ok {
		normalized["ref"] = strings.Join(strings.Fields(value), " ")
	}
	if text, ok := args["text"].(string); ok {
		sum := sha256.Sum256([]byte(text))
		normalized["text_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

func normalizeBrowserScrollArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["direction"].(string); ok {
		normalized["direction"] = strings.Join(strings.Fields(value), " ")
	}
	return normalized
}

func normalizeBrowserEmptyArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{}
}

func normalizeBrowserConsoleArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{"clear": args["clear"]}
	if expression, ok := args["expression"].(string); ok {
		sum := sha256.Sum256([]byte(expression))
		normalized["expression_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

func normalizeVaultHandleArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["handle"].(string); ok {
		normalized["handle"] = strings.Join(strings.Fields(value), " ")
	}
	return normalized
}

func normalizeVaultSaveArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"label", "identifier", "identifier_type"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"password", "otp"} {
		if value, ok := args[key].(string); ok {
			sum := sha256.Sum256([]byte(value))
			normalized[key+"_hash"] = hex.EncodeToString(sum[:])
		}
	}
	return normalized
}

func normalizeVaultSaveAddressArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["label"].(string); ok {
		normalized["label"] = strings.Join(strings.Fields(value), " ")
	}
	for _, key := range []string{"address_line1", "address_line2", "city", "state", "postal_code", "country"} {
		if value, ok := args[key].(string); ok {
			sum := sha256.Sum256([]byte(value))
			normalized[key+"_hash"] = hex.EncodeToString(sum[:])
		}
	}
	return normalized
}

func normalizeBrowserDialogArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["action"].(string); ok {
		normalized["action"] = strings.Join(strings.Fields(value), " ")
	}
	if text, ok := args["prompt_text"].(string); ok {
		sum := sha256.Sum256([]byte(text))
		normalized["prompt_text_hash"] = hex.EncodeToString(sum[:])
	}
	if value, ok := args["dialog_id"].(string); ok {
		normalized["dialog_id"] = strings.Join(strings.Fields(value), " ")
	}
	return normalized
}

func normalizeBrowserVisionArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{"annotate": args["annotate"]}
	if question, ok := args["question"].(string); ok {
		sum := sha256.Sum256([]byte(question))
		normalized["question_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

func normalizeWebSearchArgs(args map[string]interface{}) interface{} {
	limit, err := websearch.WebResultLimit(args)
	var normalized interface{} = limit
	if err != nil {
		normalized = map[string]interface{}{"invalid": args["limit"]}
	}
	return map[string]interface{}{"query": normalizedString(args, "query", true), "limit": normalized}
}

func normalizeImageGenerateArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{"prompt": normalizedString(args, "prompt", false), "aspect_ratio": "landscape"}
	if aspect, exists := args["aspect_ratio"]; exists {
		normalized["aspect_ratio"] = aspect
	}
	if source, exists := args["image_url"]; exists {
		normalized["image_url"] = source
	}
	return normalized
}

// normalizeSessionSearchArgs canonicalizes shape-defining selectors so repeated
// identical reads collapse, while distinct queries remain distinct.
func normalizeSessionSearchArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"query", "session_id", "sort", "detail", "after", "before", "role_filter"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"limit", "around_message_id", "window"} {
		if value, ok := args[key]; ok {
			normalized[key] = value
		}
	}
	if raw, ok := args["exclude_session_ids"].([]interface{}); ok {
		normalized["exclude_session_ids_count"] = len(raw)
	}
	return normalized
}

// normalizeSkillManageArgs canonicalizes skill selectors so repeated
// identical reads collapse while distinct skills stay distinct. Bodies and
// descriptions never survive normalization, only names and hashes.
func normalizeSkillManageArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"action", "name", "file_path"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"description", "content", "old_text"} {
		if value, ok := args[key].(string); ok {
			sum := sha256.Sum256([]byte(value))
			normalized[key+"_hash"] = hex.EncodeToString(sum[:])
		}
	}
	return normalized
}

func normalizedString(args map[string]interface{}, key string, lower bool) string {
	value, _ := args[key].(string)
	value = strings.Join(strings.Fields(value), " ")
	if lower {
		value = strings.ToLower(value)
	}
	return value
}

func normalizeReadFileArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{
		"path":   normalizedString(args, "path", false),
		"offset": args["offset"],
		"limit":  args["limit"],
	}
}

func normalizeWriteFileArgs(args map[string]interface{}) interface{} {
	content, _ := args["content"].(string)
	sum := sha256.Sum256([]byte(content))
	return map[string]interface{}{
		"path":         normalizedString(args, "path", false),
		"content_hash": hex.EncodeToString(sum[:]),
	}
}

func normalizePatchArgs(args map[string]interface{}) interface{} {
	oldString, _ := args["old_string"].(string)
	newString, _ := args["new_string"].(string)
	oldSum := sha256.Sum256([]byte(oldString))
	newSum := sha256.Sum256([]byte(newString))
	return map[string]interface{}{
		"path":        normalizedString(args, "path", false),
		"old_hash":    hex.EncodeToString(oldSum[:]),
		"new_hash":    hex.EncodeToString(newSum[:]),
		"replace_all": args["replace_all"],
	}
}

func normalizeSearchFilesArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"pattern", "target", "path", "file_glob", "order", "output_mode"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"limit", "offset", "context"} {
		if value, ok := args[key]; ok {
			normalized[key] = value
		}
	}
	return normalized
}

func normalizeTerminalArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{
		"command": normalizedString(args, "command", false),
		"workdir": normalizedString(args, "workdir", false),
		"timeout": args["timeout"],
	}
}

func normalizeExecuteCodeArgs(args map[string]interface{}) interface{} {
	code, _ := args["code"].(string)
	sum := sha256.Sum256([]byte(code))
	return map[string]interface{}{
		"code_hash": hex.EncodeToString(sum[:]),
		"reset":     args["reset"],
	}
}

func normalizeProcessManageArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["action"].(string); ok {
		normalized["action"] = strings.Join(strings.Fields(value), " ")
	}
	if value, ok := args["session_id"].(string); ok {
		normalized["session_id"] = strings.TrimSpace(value)
	}
	for _, key := range []string{"timeout", "offset", "limit"} {
		if value, ok := args[key]; ok {
			normalized[key] = value
		}
	}
	if data, ok := args["data"].(string); ok {
		sum := sha256.Sum256([]byte(data))
		normalized["data_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

// normalizeDelegateArgs canonicalizes delegate calls to counts and labels so
// repeated identical dispatches collapse. Prompts, contexts, messages, and
// schemas never survive normalization — only counts and names.
func normalizeDelegateArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["action"].(string); ok {
		normalized["action"] = strings.Join(strings.Fields(value), " ")
	}
	if value, ok := args["role"].(string); ok {
		normalized["role"] = strings.Join(strings.Fields(value), " ")
	}
	if value, ok := args["subagent_id"].(string); ok {
		normalized["subagent_id"] = strings.TrimSpace(value)
	}
	if tasks, ok := args["tasks"].([]interface{}); ok {
		normalized["task_count"] = len(tasks)
		imageCount := 0
		hasSchema := false
		for _, raw := range tasks {
			entry, _ := raw.(map[string]interface{})
			if entry == nil {
				continue
			}
			if images, ok := entry["images"].([]interface{}); ok {
				imageCount += len(images)
			}
			if _, ok := entry["output_schema"]; ok {
				hasSchema = true
			}
		}
		normalized["image_count"] = imageCount
		normalized["has_output_schema"] = hasSchema
	} else {
		if _, ok := args["goal"]; ok {
			normalized["task_count"] = 1
		}
		if images, ok := args["images"].([]interface{}); ok {
			normalized["image_count"] = len(images)
		}
		if _, ok := args["output_schema"]; ok {
			normalized["has_output_schema"] = true
		}
	}
	return normalized
}

func normalizeCronjobManageArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"action", "job_id", "schedule", "script", "deliver"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	if prompt, ok := args["prompt"].(string); ok {
		sum := sha256.Sum256([]byte(prompt))
		normalized["prompt_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

// normalizeClarifyArgs canonicalizes the question form so repeated identical
// asks collapse while distinct forms stay distinct. Raw question text never
// survives normalization, only counts and a hash.
func normalizeClarifyArgs(args map[string]interface{}) interface{} {
	questions, _ := args["questions"].([]interface{})
	normalized := map[string]interface{}{"question_count": len(questions)}
	choiceCount := 0
	var sb strings.Builder
	for _, raw := range questions {
		entry, _ := raw.(map[string]interface{})
		if entry == nil {
			continue
		}
		if text, ok := entry["question"].(string); ok {
			sb.WriteString(strings.Join(strings.Fields(text), " "))
			sb.WriteString("\x00")
		}
		if multi, ok := entry["multi_select"].(bool); ok && multi {
			sb.WriteString("multi\x00")
		}
		if rawChoices, ok := entry["choices"].([]interface{}); ok {
			for _, rawChoice := range rawChoices {
				if choice, ok := rawChoice.(string); ok {
					choiceCount++
					sb.WriteString(strings.Join(strings.Fields(choice), " "))
					sb.WriteString("\x00")
				}
			}
		}
	}
	normalized["choice_count"] = choiceCount
	sum := sha256.Sum256([]byte(sb.String()))
	normalized["form_hash"] = hex.EncodeToString(sum[:])
	return normalized
}
