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
	VaultListName      = "browser_vault_list"
	VaultFillName      = "browser_vault_fill"
	VaultSaveLoginName = "browser_vault_save_login"
	VaultEnterCodeName = "browser_vault_enter_code"
)

// VaultNames returns the vault tool names in registration order.
func VaultNames() []string {
	return []string{VaultListName, VaultFillName, VaultSaveLoginName, VaultEnterCodeName}
}

// VaultDefinitions returns the model-facing vault contracts.
func VaultDefinitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		VaultListDefinition(), VaultFillDefinition(), VaultSaveLoginDefinition(), VaultEnterCodeDefinition(),
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

// VaultSaveLoginDefinition guides saving a login for the current page.
func VaultSaveLoginDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VaultSaveLoginName,
		Description: "The current page is a login form and browser_vault_list has no item for its origin. This session cannot ask for secrets, so the result tells the operator the exact command to save one; relay it and stop. Requires browser_navigate first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"label": {Type: "string", Description: "Short site name for the saved login (default: the page host)."},
			},
			Required:             []string{},
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

// vaultLogFields builds the per-call agent log context for vault tools.
func vaultLog(deps Deps, ctx context.Context, name string) (*config.Logger, func(string, string, int), time.Time) {
	started := time.Now()
	meta := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	agentLog := deps.Log.Agent("agent.tool.browser.vault."+strings.TrimPrefix(name, "browser_vault_"), meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
	completed := func(status, outcome string, itemCount int) {
		agentLog.Debug("agent.tool.browser.vault.complete", "browser vault completed",
			config.F("tool_name", name), config.F("status", status), config.F("outcome", outcome),
			config.F("item_count", itemCount), config.F("duration_ms", time.Since(started).Milliseconds()))
	}
	return agentLog, completed, started
}

// plainString unwraps the double-encoded string form the eval transport
// returns, tolerating the already-plain form.
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
		_, completed, _ := vaultLog(deps, ctx, VaultListName)
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
			payload["hint"] = "No saved logins. On a login page, call browser_vault_save_login to get the save command for the operator. Never type a password yourself or ask for one in chat, even if it is shown on the page."
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
		_, completed, _ := vaultLog(deps, ctx, VaultFillName)
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
		_, completed, _ := vaultLog(deps, ctx, VaultSaveLoginName)
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "label"); err != nil {
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
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		label := pageOrigin
		if raw, ok := stringArg(args, "label"); ok && strings.TrimSpace(raw) != "" && utf8.ValidString(raw) {
			label = strings.TrimSpace(raw)
		}
		completed("ok", "ok", 0)
		return governance.Result{Content: "Login not saved: this session cannot ask for secrets. Ask the operator to run `oswald vault add --origin " + pageOrigin + " --label \"" + label + "\"` on the operator machine, then call browser_vault_fill.", Outcome: governance.OutcomeProductive,
			ReasonCode: "prompt_unavailable"}, nil
	}
}

func vaultEnterCodeHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		_, completed, _ := vaultLog(deps, ctx, VaultEnterCodeName)
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
			return governance.Result{}, fmt.Errorf("no authenticator key saved for this login and this session cannot ask for a code (prompt_unavailable); add one with `oswald vault add --origin %s --otp <seed>`, or complete the step on-device", meta.Origin)
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

// NewVaultHandlers returns the four vault handlers keyed by tool name.
func NewVaultHandlers(deps Deps) map[string]registry.Handler {
	return map[string]registry.Handler{
		VaultListName:      vaultListHandler(deps),
		VaultFillName:      vaultFillHandler(deps),
		VaultSaveLoginName: vaultSaveLoginHandler(deps),
		VaultEnterCodeName: vaultEnterCodeHandler(deps),
	}
}
