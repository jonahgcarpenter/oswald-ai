// Package browser vault tools: model-blind autofill from the encrypted
// local vault. The agent sees opaque handles plus metadata; passwords and
// codes are resolved server-side, injected over the existing CDP session,
// and never enter tool results, logs, or history.
package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/vault"
)

// Model-facing vault tool names.
const (
	VaultListName        = "browser_vault_list"
	VaultFillName        = "browser_vault_fill"
	VaultSaveLoginName   = "browser_vault_save_login"
	VaultSaveAddressName = "browser_vault_save_address"
	VaultEnterCodeName   = "browser_vault_enter_code"
	VaultRemoveName      = "browser_vault_rm"
)

// VaultNames returns the vault tool names in registration order.
func VaultNames() []string {
	return []string{VaultListName, VaultFillName, VaultSaveLoginName, VaultSaveAddressName, VaultEnterCodeName, VaultRemoveName}
}

// VaultDefinitions returns the model-facing vault contracts.
func VaultDefinitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		VaultListDefinition(), VaultFillDefinition(), VaultSaveLoginDefinition(), VaultSaveAddressDefinition(), VaultEnterCodeDefinition(), VaultRemoveDefinition(),
	}
}

// VaultListDefinition lists saved login handles and metadata.
func VaultListDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VaultListName,
		Description: "ALWAYS call this first when a page asks for a password. Lists saved logins as handles with the identifiers you type yourself via browser_type. Passwords are never returned. A login fills only on the exact origin it was saved for.",
		Parameters: llm.ToolParameters{
			Type:                 "object",
			Properties:           map[string]llm.ToolParameterProperty{},
			Required:             []string{},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// VaultFillDefinition fills the current page from a vault handle.
func VaultFillDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VaultFillName,
		Description: "Fill the CURRENT browser page from a vault handle (see browser_vault_list): the password for a login, or address fields for an address item. Card items are never filled. Secrets are resolved locally and injected into the page; they never enter the conversation. Refused unless the page origin exactly matches the item's bound origin. For logins, type the identifier yourself with browser_type first, then submit. Requires browser_navigate first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"handle": {Type: "string", Description: "A handle from browser_vault_list."},
			},
			Required:             []string{"handle"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// VaultSaveLoginDefinition saves a login for the current page.
func VaultSaveLoginDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VaultSaveLoginName,
		Description: "Save a login for the CURRENT page. Ask the user for the identifier and password in chat (and the authenticator seed if they want automatic 2FA codes), then save: secrets are sealed on write and never returned. The item binds to the page origin automatically; fill it later with browser_vault_fill. Requires browser_navigate first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"label":           {Type: "string", Description: "Short site name for the saved login (default: the page host)."},
				"identifier":      {Type: "string", Description: "Login identifier the user typed (email, phone, or username)."},
				"identifier_type": {Type: "string", Description: "Identifier kind (default: email).", Enum: []string{"email", "phone", "username"}},
				"password":        {Type: "string", Description: "Login password; sealed on write, never returned."},
				"otp":             {Type: "string", Description: "Optional authenticator seed or otpauth://totp URI for automatic 2FA codes."},
			},
			Required:             []string{"identifier", "password"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// VaultSaveAddressDefinition saves an address for the current page.
func VaultSaveAddressDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VaultSaveAddressName,
		Description: "Save an address for the CURRENT page. Ask the user for the address fields in chat, then save: values are sealed on write and never returned. The item binds to the page origin automatically; fill it later with browser_vault_fill. Requires browser_navigate first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"label":         {Type: "string", Description: "Short name for the saved address (default: the page host)."},
				"address_line1": {Type: "string", Description: "Street address (required)."},
				"address_line2": {Type: "string", Description: "Apartment, suite, or unit."},
				"city":          {Type: "string", Description: "City (required)."},
				"state":         {Type: "string", Description: "State or province."},
				"postal_code":   {Type: "string", Description: "Postal or ZIP code (required)."},
				"country":       {Type: "string", Description: "Country (required)."},
			},
			Required:             []string{"address_line1", "city", "postal_code", "country"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// VaultRemoveDefinition deletes one vault item by handle.
func VaultRemoveDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VaultRemoveName,
		Description: "Delete one saved vault item by its exact handle from browser_vault_list. This cannot be undone; double-check the handle belongs to the item the user wants gone.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"handle": {Type: "string", Description: "The exact vault handle to delete."},
			},
			Required:             []string{"handle"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// VaultEnterCodeDefinition fills a one-time verification code.
func VaultEnterCodeDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VaultEnterCodeName,
		Description: "The page asks for a one-time / verification / 2FA code after the password. Fills it from the login's saved authenticator seed over the browser session; the code never enters the conversation. When no seed is saved, the result explains how to add one or complete the step on-device. Never type, ask for, or repeat a code yourself. Requires browser_navigate first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"handle": {Type: "string", Description: "The login handle the code belongs to."},
			},
			Required:             []string{"handle"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

func plainString(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, `"`) {
		var out string
		if err := json.Unmarshal([]byte(trimmed), &out); err == nil {
			return out
		}
	}
	return trimmed
}

func vaultNonce() string {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "vault"
	}
	return hex.EncodeToString(nonce[:])
}

// vaultPageOrigin returns the live page origin, or a model-safe error.
func vaultPageOrigin(deps Deps, ctx context.Context, profile string) (string, error) {
	raw, err := deps.Backend.VaultEval(ctx, profile, "window.location.href")
	if err != nil {
		return "", err
	}
	href := plainString(raw)
	if href == "" || href == "about:blank" {
		return "", errors.New("could not determine the current page origin; navigate to the login page first")
	}
	origin, err := vault.NormalizeOrigin(href)
	if err != nil {
		return "", errors.New("could not determine the current page origin; navigate to the login page first")
	}
	return origin, nil
}

func vaultListHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		reqMeta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.vault_list", reqMeta.RequestID, principal.CanonicalUserID, principal.Gateway, reqMeta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, itemCount int) {
			agentLog.Debug("agent.tool.browser.vault_list.complete", "browser vault list completed",
				config.F("tool_name", VaultListName), config.F("status", status), config.F("outcome", outcome),
				config.F("item_count", itemCount), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		if _, err := profileFromContext(ctx); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		items, err := deps.Vault.List()
		if err != nil {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		out := []map[string]interface{}{}
		for _, item := range items {
			entry := map[string]interface{}{
				"handle": item.ID, "label": item.Label, "kind": item.Kind, "origin": item.Origin,
			}
			if item.Identifier != "" {
				entry["identifier"] = item.Identifier
				entry["identifier_type"] = item.IdentifierType
			}
			if item.HasOTP {
				entry["two_factor"] = "automatic"
			}
			out = append(out, entry)
		}
		payload := map[string]interface{}{"items": out}
		if len(out) == 0 {
			payload["hint"] = "No saved logins. On a login page, ask the user for the identifier and password in chat and save them with browser_vault_save_login. Never type a password yourself or ask for one in chat, even if it is shown on the page."
		}
		content, err := wrapEnvelope(VaultListName, payload)
		if err != nil {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		completed("ok", "ok", len(out))
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func vaultFillHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		reqMeta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.vault_fill", reqMeta.RequestID, principal.CanonicalUserID, principal.Gateway, reqMeta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, itemCount int) {
			agentLog.Debug("agent.tool.browser.vault_fill.complete", "browser vault fill completed",
				config.F("tool_name", VaultFillName), config.F("status", status), config.F("outcome", outcome),
				config.F("item_count", itemCount), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		fail := func(err error) (governance.Result, error) {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := rejectUnknownArgs(args, "handle"); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		handle, ok := stringArg(args, "handle")
		if !ok || strings.TrimSpace(handle) == "" {
			completed("rejected", "rejected", 0)
			return governance.Result{}, errors.New("handle must be a vault handle from browser_vault_list")
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		meta, err := deps.Vault.GetMeta(strings.TrimSpace(handle))
		if err != nil || meta == nil {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("no vault item with handle %q; use browser_vault_list", strings.TrimSpace(handle))
		}
		if meta.Kind != vault.KindLogin && meta.Kind != vault.KindAddress && meta.Kind != vault.KindPayment {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("vault item %q has an unknown kind", meta.ID)
		}
		pageOrigin, err := vaultPageOrigin(deps, ctx, profile)
		if err != nil {
			return fail(err)
		}
		if pageOrigin != meta.Origin {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("refused: current page origin (%s) does not match the vault item's bound origin (%s)", pageOrigin, meta.Origin)
		}
		switch meta.Kind {
		case vault.KindPayment:
			completed("error", "error", 0)
			return governance.Result{}, errors.New("payment card fill is disabled: card entry needs an interactive confirmation this session cannot provide, so nothing was written; enter the card manually")
		case vault.KindAddress:
			return vaultFillAddress(deps, ctx, profile, pageOrigin, meta, completed, fail)
		default:
			return vaultFillLogin(deps, ctx, profile, pageOrigin, meta, completed, fail)
		}
	}
}

func vaultFillLogin(deps Deps, ctx context.Context, profile, pageOrigin string, meta *vault.VaultItemMeta, completed func(string, string, int), fail func(error) (governance.Result, error)) (governance.Result, error) {
	nonce := vaultNonce()
	inspect, err := deps.Backend.VaultEval(ctx, profile, vault.BuildInspectionJS(nonce))
	if err != nil {
		return fail(fmt.Errorf("could not inspect page inputs: %v", err))
	}
	passwordIdx, _, ok := vault.ClassifyLoginControls(vault.ParseLoginControls(inspect))
	if !ok {
		completed("error", "error", 0)
		return governance.Result{}, errors.New("no login form fields were found on the current page")
	}
	password, _, err := deps.Vault.ResolvePassword(meta.ID)
	if err != nil {
		return fail(err)
	}
	vault.RegisterSecretValues(password)
	fill, err := deps.Backend.VaultEval(ctx, profile, vault.BuildFillJS(passwordIdx, meta.Origin, password, nonce))
	password = ""
	if err != nil {
		return fail(fmt.Errorf("page fill failed: %v", err))
	}
	report := vault.ParseFillReport(fill)
	if report.Refused == "origin_changed" {
		completed("error", "error", 0)
		return governance.Result{}, fmt.Errorf("refused: the page navigated away from the bound origin (%s); nothing was written", meta.Origin)
	}
	if report.Filled == 0 {
		completed("error", "error", 0)
		return governance.Result{}, errors.New("no fillable password field matched the saved login on this page")
	}
	content, err := wrapEnvelope(VaultFillName, map[string]interface{}{
		"filled_fields": 1, "kind": meta.Kind, "origin": pageOrigin,
		"next": "Type the identifier into the username field with browser_type if the form has one, then submit. If the site then asks for a verification code, call browser_vault_enter_code with this handle.",
	})
	if err != nil {
		return fail(err)
	}
	completed("ok", "ok", 1)
	return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
}

func vaultFillAddress(deps Deps, ctx context.Context, profile, pageOrigin string, meta *vault.VaultItemMeta, completed func(string, string, int), fail func(error) (governance.Result, error)) (governance.Result, error) {
	fields, err := deps.Vault.ResolveFields(meta.ID)
	if err != nil {
		return fail(err)
	}
	nonce := vaultNonce()
	inspect, err := deps.Backend.VaultEval(ctx, profile, vault.BuildInspectionJS(nonce))
	if err != nil {
		return fail(fmt.Errorf("could not inspect page inputs: %v", err))
	}
	slots := vault.ClassifyAddressControls(vault.ParseLoginControls(inspect))
	filled := []string{}
	for slot, field := range slots {
		value := fields[field]
		if value == "" {
			continue
		}
		raw, err := deps.Backend.VaultEval(ctx, profile, vault.BuildFillJS(slot, meta.Origin, value, vaultNonce()))
		if err != nil {
			return fail(fmt.Errorf("address fill failed: %v", err))
		}
		if report := vault.ParseFillReport(raw); report.Refused == "origin_changed" {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("refused: the page navigated away from the bound origin (%s); nothing further was written", meta.Origin)
		} else if report.Filled > 0 {
			filled = append(filled, vault.AddressFields[field])
		}
	}
	fields = nil
	if len(filled) == 0 {
		completed("error", "error", 0)
		return governance.Result{}, errors.New("no fillable address field matched the saved item on this page")
	}
	content, err := wrapEnvelope(VaultFillName, map[string]interface{}{
		"filled_fields": len(filled), "kind": meta.Kind, "origin": pageOrigin, "fields": filled,
	})
	if err != nil {
		return fail(err)
	}
	completed("ok", "ok", len(filled))
	return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
}

func vaultSaveLoginHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		reqMeta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.vault_save_login", reqMeta.RequestID, principal.CanonicalUserID, principal.Gateway, reqMeta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, itemCount int) {
			agentLog.Debug("agent.tool.browser.vault_save_login.complete", "browser vault save login completed",
				config.F("tool_name", VaultSaveLoginName), config.F("status", status), config.F("outcome", outcome),
				config.F("item_count", itemCount), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		fail := func(err error) (governance.Result, error) {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := rejectUnknownArgs(args, "label", "identifier", "identifier_type", "password", "otp"); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		pageOrigin, err := vaultPageOrigin(deps, ctx, profile)
		if err != nil {
			return fail(err)
		}
		identifier, ok := stringArg(args, "identifier")
		if !ok || strings.TrimSpace(identifier) == "" {
			completed("rejected", "rejected", 0)
			return governance.Result{}, errors.New("identifier is required; ask the user for the login identifier in chat")
		}
		idType := "email"
		if raw, ok := stringArg(args, "identifier_type"); ok && strings.TrimSpace(raw) != "" {
			idType = strings.TrimSpace(raw)
		}
		password, ok := stringArg(args, "password")
		if !ok || password == "" {
			completed("rejected", "rejected", 0)
			return governance.Result{}, errors.New("password is required; ask the user for the password in chat")
		}
		otp, _ := stringArg(args, "otp")
		label := pageOrigin
		if raw, ok := stringArg(args, "label"); ok && strings.TrimSpace(raw) != "" && utf8.ValidString(raw) {
			label = strings.TrimSpace(raw)
		}
		meta, err := deps.Vault.AddLogin(label, idType, strings.TrimSpace(identifier), password, strings.TrimSpace(otp), pageOrigin)
		password = ""
		otp = ""
		if err != nil {
			return fail(err)
		}
		payload := map[string]interface{}{
			"saved": true, "handle": meta.ID, "label": meta.Label, "kind": meta.Kind, "origin": pageOrigin,
			"next": "Saved. Fill it on this page with browser_vault_fill using the handle.",
		}
		if meta.HasOTP {
			payload["two_factor"] = "automatic"
		}
		content, err := wrapEnvelope(VaultSaveLoginName, payload)
		if err != nil {
			return fail(err)
		}
		completed("ok", "ok", 1)
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func vaultSaveAddressHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		reqMeta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.vault_save_address", reqMeta.RequestID, principal.CanonicalUserID, principal.Gateway, reqMeta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, itemCount int) {
			agentLog.Debug("agent.tool.browser.vault_save_address.complete", "browser vault save address completed",
				config.F("tool_name", VaultSaveAddressName), config.F("status", status), config.F("outcome", outcome),
				config.F("item_count", itemCount), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		fail := func(err error) (governance.Result, error) {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		fields := []string{"address_line1", "address_line2", "city", "state", "postal_code", "country"}
		if err := rejectUnknownArgs(args, append([]string{"label"}, fields...)...); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		pageOrigin, err := vaultPageOrigin(deps, ctx, profile)
		if err != nil {
			return fail(err)
		}
		values := map[string]string{}
		for _, key := range fields {
			if raw, ok := stringArg(args, key); ok && strings.TrimSpace(raw) != "" {
				values[key] = strings.TrimSpace(raw)
			}
		}
		for _, key := range []string{"address_line1", "city", "postal_code", "country"} {
			if values[key] == "" {
				completed("rejected", "rejected", 0)
				return governance.Result{}, fmt.Errorf("%s is required; ask the user for the address fields in chat", key)
			}
		}
		label := pageOrigin
		if raw, ok := stringArg(args, "label"); ok && strings.TrimSpace(raw) != "" && utf8.ValidString(raw) {
			label = strings.TrimSpace(raw)
		}
		meta, err := deps.Vault.AddAddress(label, values, pageOrigin)
		for k := range values {
			values[k] = ""
		}
		if err != nil {
			return fail(err)
		}
		content, err := wrapEnvelope(VaultSaveAddressName, map[string]interface{}{
			"saved": true, "handle": meta.ID, "label": meta.Label, "kind": meta.Kind, "origin": pageOrigin,
			"next": "Saved. Fill it on this page with browser_vault_fill using the handle.",
		})
		if err != nil {
			return fail(err)
		}
		completed("ok", "ok", 1)
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func vaultRemoveHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		reqMeta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.vault_remove", reqMeta.RequestID, principal.CanonicalUserID, principal.Gateway, reqMeta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, itemCount int) {
			agentLog.Debug("agent.tool.browser.vault_remove.complete", "browser vault remove completed",
				config.F("tool_name", VaultRemoveName), config.F("status", status), config.F("outcome", outcome),
				config.F("item_count", itemCount), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		fail := func(err error) (governance.Result, error) {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := rejectUnknownArgs(args, "handle"); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		handle, ok := stringArg(args, "handle")
		if !ok || strings.TrimSpace(handle) == "" {
			completed("rejected", "rejected", 0)
			return governance.Result{}, errors.New("handle must be an exact vault handle from browser_vault_list")
		}
		if _, err := profileFromContext(ctx); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		removed, err := deps.Vault.Remove(strings.TrimSpace(handle))
		if err != nil {
			return fail(err)
		}
		if !removed {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("no vault item with handle %q; use browser_vault_list", strings.TrimSpace(handle))
		}
		content, err := wrapEnvelope(VaultRemoveName, map[string]interface{}{"removed": strings.TrimSpace(handle)})
		if err != nil {
			return fail(err)
		}
		completed("ok", "ok", 1)
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func vaultEnterCodeHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		reqMeta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.vault_enter_code", reqMeta.RequestID, principal.CanonicalUserID, principal.Gateway, reqMeta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, itemCount int) {
			agentLog.Debug("agent.tool.browser.vault_enter_code.complete", "browser vault enter code completed",
				config.F("tool_name", VaultEnterCodeName), config.F("status", status), config.F("outcome", outcome),
				config.F("item_count", itemCount), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		fail := func(err error) (governance.Result, error) {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := rejectUnknownArgs(args, "handle"); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		handle, ok := stringArg(args, "handle")
		if !ok || strings.TrimSpace(handle) == "" {
			completed("rejected", "rejected", 0)
			return governance.Result{}, errors.New("handle must be a vault handle from browser_vault_list")
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		meta, err := deps.Vault.GetMeta(strings.TrimSpace(handle))
		if err != nil || meta == nil {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("no vault item with handle %q; use browser_vault_list", strings.TrimSpace(handle))
		}
		if meta.Kind != vault.KindLogin {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("vault item %q is not a login", meta.ID)
		}
		pageOrigin, err := vaultPageOrigin(deps, ctx, profile)
		if err != nil {
			return fail(err)
		}
		if pageOrigin != meta.Origin {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("refused: current page origin (%s) does not match the vault item's bound origin (%s)", pageOrigin, meta.Origin)
		}
		_, otpSeed, err := deps.Vault.ResolvePassword(meta.ID)
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(otpSeed) == "" {
			completed("error", "error", 0)
			return governance.Result{}, fmt.Errorf("no authenticator key saved for this login; ask the user for the login password and authenticator seed in chat and save them with browser_vault_save_login, or complete the step on-device")
		}
		code, err := vault.TOTPNow(otpSeed, time.Now())
		if err != nil {
			return fail(err)
		}
		vault.RegisterSecretValues(code)
		nonce := vaultNonce()
		inspect, err := deps.Backend.VaultEval(ctx, profile, vault.BuildInspectionJS(nonce))
		if err != nil {
			code = ""
			return fail(fmt.Errorf("could not inspect page inputs: %v", err))
		}
		slots := vault.ClassifyOTPControls(vault.ParseLoginControls(inspect))
		if len(slots) == 0 {
			code = ""
			completed("error", "error", 0)
			return governance.Result{}, errors.New("no one-time-code field on the current page; tell the user to complete the step on their device and wait for the page to move on")
		}
		filled := 0
		digits := code
		code = ""
		for i, slot := range slots {
			value := digits
			if len(slots) > 1 {
				if i >= len(digits) {
					break
				}
				value = digits[i : i+1]
			}
			raw, err := deps.Backend.VaultEval(ctx, profile, vault.BuildFillJS(slot, meta.Origin, value, nonce))
			if err != nil {
				return fail(fmt.Errorf("code fill failed: %v", vault.ScrubValues(err.Error(), digits)))
			}
			if report := vault.ParseFillReport(raw); report.Refused == "origin_changed" {
				completed("error", "error", 0)
				return governance.Result{}, fmt.Errorf("refused: the page navigated away from the bound origin (%s); nothing was written", meta.Origin)
			} else if report.Filled > 0 {
				filled++
			}
		}
		if filled == 0 {
			completed("error", "error", 0)
			return governance.Result{}, errors.New("the code could not be entered on this page")
		}
		content, err := wrapEnvelope(VaultEnterCodeName, map[string]interface{}{
			"filled_fields": filled, "origin": pageOrigin,
			"next": "Submit the form (many sites auto-submit when the last digit lands).",
		})
		if err != nil {
			return fail(err)
		}
		completed("ok", "ok", filled)
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

// NewVaultHandlers returns the six vault handlers keyed by tool name.
func NewVaultHandlers(deps Deps) map[string]registry.Handler {
	return map[string]registry.Handler{
		VaultListName:        vaultListHandler(deps),
		VaultFillName:        vaultFillHandler(deps),
		VaultSaveLoginName:   vaultSaveLoginHandler(deps),
		VaultSaveAddressName: vaultSaveAddressHandler(deps),
		VaultEnterCodeName:   vaultEnterCodeHandler(deps),
		VaultRemoveName:      vaultRemoveHandler(deps),
	}
}
