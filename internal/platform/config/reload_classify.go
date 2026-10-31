package config

import "reflect"

// ClassifyReload diffs effective configs by section. Only the runtime log
// level and gateway admission policy are hot; every other differing section
// forces a restart. The returned classes name the differing sections from a
// fixed vocabulary for logging and CLI effect reporting.
func ClassifyReload(old, next *Config) (bool, []string) {
	if old == nil || next == nil {
		return false, []string{"unavailable"}
	}
	var classes []string
	add := func(name string, same bool) {
		if !same {
			classes = append(classes, name)
		}
	}
	add("runtime", old.LogLevel == next.LogLevel)
	add("admission", reflect.DeepEqual(old.DiscordPolicy, next.DiscordPolicy) &&
		reflect.DeepEqual(old.BlueBubblesPolicy, next.BlueBubblesPolicy) &&
		old.DiscordGroupRequireMention == next.DiscordGroupRequireMention &&
		old.BlueBubblesGroupRequireMention == next.BlueBubblesGroupRequireMention)
	add("providers", reflect.DeepEqual(old.LLMGatewayURL, next.LLMGatewayURL) &&
		reflect.DeepEqual(old.LLMGatewayAPIKey, next.LLMGatewayAPIKey) &&
		old.LLMGatewayProvider == next.LLMGatewayProvider)
	add("model", old.LLMGatewayModel == next.LLMGatewayModel && old.ModelContextWindow == next.ModelContextWindow)
	add("api", old.OpenAIAPIKey == next.OpenAIAPIKey && old.OpenAIListenHost == next.OpenAIListenHost && old.OpenAIListenPort == next.OpenAIListenPort)
	add("gateway", reflect.DeepEqual(old.ProfileRoutes, next.ProfileRoutes))
	add("platforms", old.DiscordToken == next.DiscordToken &&
		old.BlueBubblesURL == next.BlueBubblesURL && old.BlueBubblesPassword == next.BlueBubblesPassword &&
		old.BlueBubblesListenPort == next.BlueBubblesListenPort && old.BlueBubblesWebhookPath == next.BlueBubblesWebhookPath &&
		old.BlueBubblesWebhookCallbackHost == next.BlueBubblesWebhookCallbackHost &&
		reflect.DeepEqual(old.BlueBubblesMentionPatterns, next.BlueBubblesMentionPatterns))
	add("tools", old.BraveAPIKey == next.BraveAPIKey && old.SearxngURL == next.SearxngURL &&
		old.ComfyUIURL == next.ComfyUIURL && old.ComfyUIGenerationTimeout == next.ComfyUIGenerationTimeout &&
		old.BrowserLaunchCommand == next.BrowserLaunchCommand && old.BrowserEndpointURL == next.BrowserEndpointURL &&
		old.BrowserCommandTimeout == next.BrowserCommandTimeout && old.BrowserSnapshotThreshold == next.BrowserSnapshotThreshold &&
		old.BrowserInactivityTimeout == next.BrowserInactivityTimeout && old.BrowserRecordSessions == next.BrowserRecordSessions &&
		old.BrowserAllowPrivateURLs == next.BrowserAllowPrivateURLs && old.BrowserAllowInsecureURLs == next.BrowserAllowInsecureURLs &&
		old.BrowserRestrictEvaluate == next.BrowserRestrictEvaluate)
	add("mcp_servers", reflect.DeepEqual(old.MCPServers, next.MCPServers))
	add("profiles", reflect.DeepEqual(old.Profiles, next.Profiles))
	if len(classes) == 0 {
		return true, nil
	}
	for _, class := range classes {
		if class != "runtime" && class != "admission" {
			return false, classes
		}
	}
	return true, classes
}

// ReloadEffect reports whether a validated change applies live or needs a
// restart. It is the CLI-facing wrapper over ClassifyReload: "live" for hot
// sections, "restart" for anything else.
func ReloadEffect(old, next *Config) string {
	if hot, _ := ClassifyReload(old, next); hot {
		return "live"
	}
	return "restart"
}
