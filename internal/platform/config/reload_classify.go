package config

import "reflect"

// ClassifyReload diffs effective configs by section and reports whether the
// change applies live plus the differing classes from a fixed vocabulary for
// logging and CLI effect reporting. Hot classes are runtime (log level),
// admission (allow/ban/mention rules), providers (the default profile's
// selected transport), model (the default model plus named-profile routing
// overrides: model, provider, URL, or key), and channel_overrides (the global
// platform channel routing map plus per-profile overlays). Every other class
// forces a restart: model_window (context windows), profiles (any other
// named-profile change, including profiles added to or removed from the map),
// and api, gateway, platforms, tools, mcp_servers, plugins, and delegation.
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
	routingChanged, windowChanged, overridesChanged, otherChanged := profileRoutingDiff(old.Profiles, next.Profiles)
	add("model", old.LLMGatewayModel == next.LLMGatewayModel && !routingChanged)
	add("model_window", old.ModelContextWindow == next.ModelContextWindow && !windowChanged)
	add("api", old.OpenAIAPIKey == next.OpenAIAPIKey && old.OpenAIListenHost == next.OpenAIListenHost && old.OpenAIListenPort == next.OpenAIListenPort)
	add("gateway", reflect.DeepEqual(old.ProfileRoutes, next.ProfileRoutes))
	add("platforms", old.DiscordToken == next.DiscordToken &&
		old.BlueBubblesURL == next.BlueBubblesURL && old.BlueBubblesPassword == next.BlueBubblesPassword &&
		old.BlueBubblesListenPort == next.BlueBubblesListenPort && old.BlueBubblesWebhookPath == next.BlueBubblesWebhookPath &&
		old.BlueBubblesWebhookCallbackHost == next.BlueBubblesWebhookCallbackHost &&
		reflect.DeepEqual(old.BlueBubblesMentionPatterns, next.BlueBubblesMentionPatterns))
	add("channel_overrides", reflect.DeepEqual(old.ChannelOverrides, next.ChannelOverrides) && !overridesChanged)
	add("tools", old.BraveAPIKey == next.BraveAPIKey && old.SearxngURL == next.SearxngURL &&
		old.ComfyUIURL == next.ComfyUIURL && old.ComfyUIGenerationTimeout == next.ComfyUIGenerationTimeout &&
		old.BrowserLaunchCommand == next.BrowserLaunchCommand && old.BrowserEndpointURL == next.BrowserEndpointURL &&
		old.BrowserCommandTimeout == next.BrowserCommandTimeout && old.BrowserSnapshotThreshold == next.BrowserSnapshotThreshold &&
		old.BrowserInactivityTimeout == next.BrowserInactivityTimeout && old.BrowserRecordSessions == next.BrowserRecordSessions &&
		old.BrowserAllowPrivateURLs == next.BrowserAllowPrivateURLs && old.BrowserAllowInsecureURLs == next.BrowserAllowInsecureURLs &&
		old.BrowserRestrictEvaluate == next.BrowserRestrictEvaluate &&
		reflect.DeepEqual(old.TerminalEnvPassthrough, next.TerminalEnvPassthrough))
	add("mcp_servers", reflect.DeepEqual(old.MCPServers, next.MCPServers))
	add("plugins", reflect.DeepEqual(old.PluginEntries, next.PluginEntries))
	add("delegation", old.DelegationModel == next.DelegationModel && old.DelegationProvider == next.DelegationProvider &&
		old.DelegationBaseURL == next.DelegationBaseURL && old.DelegationMaxIterations == next.DelegationMaxIterations &&
		old.DelegationMaxConcurrentChildren == next.DelegationMaxConcurrentChildren &&
		old.DelegationMaxSpawnDepth == next.DelegationMaxSpawnDepth &&
		old.DelegationOrchestratorEnabled == next.DelegationOrchestratorEnabled)
	add("profiles", !otherChanged)
	if len(classes) == 0 {
		return true, nil
	}
	for _, class := range classes {
		switch class {
		case "runtime", "admission", "providers", "model", "channel_overrides":
		default:
			return false, classes
		}
	}
	return true, classes
}

// profileRoutingDiff reports how named-profile configs differ. A profile
// absent from either map counts as an other change: a new profile has no
// running router to hot-apply to, so additions and removals never classify
// hot. Routing fields bill to model, context windows to model_window,
// per-profile channel overrides to channel_overrides, and any remaining field
// to profiles.
func profileRoutingDiff(old, next map[string]*Config) (routingChanged, windowChanged, overridesChanged, otherChanged bool) {
	for name, oldProfile := range old {
		nextProfile, ok := next[name]
		if !ok || oldProfile == nil || nextProfile == nil {
			otherChanged = true
			continue
		}
		if oldProfile.LLMGatewayModel != nextProfile.LLMGatewayModel ||
			oldProfile.LLMGatewayProvider != nextProfile.LLMGatewayProvider ||
			oldProfile.LLMGatewayURL != nextProfile.LLMGatewayURL ||
			oldProfile.LLMGatewayAPIKey != nextProfile.LLMGatewayAPIKey {
			routingChanged = true
		}
		if oldProfile.ModelContextWindow != nextProfile.ModelContextWindow {
			windowChanged = true
		}
		if !reflect.DeepEqual(oldProfile.ChannelOverrides, nextProfile.ChannelOverrides) {
			overridesChanged = true
		}
		if !reflect.DeepEqual(withoutModelRouting(oldProfile), withoutModelRouting(nextProfile)) {
			otherChanged = true
		}
	}
	for name := range next {
		if _, ok := old[name]; !ok {
			otherChanged = true
		}
	}
	return routingChanged, windowChanged, overridesChanged, otherChanged
}

// withoutModelRouting copies cfg with the hot-applied routing and window
// fields blanked, plus the channel-override map (billed separately as
// channel_overrides, global and per-profile alike). The struct copy aliases
// shared maps and slices, which is fine for the equality check in
// profileRoutingDiff.
func withoutModelRouting(cfg *Config) *Config {
	snapshot := *cfg
	snapshot.LLMGatewayModel = ""
	snapshot.LLMGatewayProvider = ""
	snapshot.LLMGatewayURL = ""
	snapshot.LLMGatewayAPIKey = ""
	snapshot.ModelContextWindow = 0
	snapshot.ChannelOverrides = nil
	return &snapshot
}

// ReloadEffect reports whether a validated change applies live or needs a
// restart. It is the CLI-facing wrapper over ClassifyReload: "live" when every
// differing class is hot (routing and channel overrides included), "restart"
// for anything else (context windows and named-profile changes included).
func ReloadEffect(old, next *Config) string {
	if hot, _ := ClassifyReload(old, next); hot {
		return "live"
	}
	return "restart"
}
