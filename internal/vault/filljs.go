// Login form inspection and fill scripts for vault autofill.
//
// The inspection script enumerates page inputs and stamps them with a
// per-call nonce. Go classifies the controls; the fill script asserts the
// page origin matches the vault item's bound origin synchronously inside
// the page before writing anything.
package vault

import (
	"encoding/json"
	"fmt"
	"strings"
)

// inspectionSlotAttr stamps inspected inputs so the fill script targets the
// exact controls this inspection classified.
const inspectionSlotAttr = "data-vault-slot"

// LoginControl is one classified page input.
type LoginControl struct {
	Index        int    `json:"index"`
	Tag          string `json:"tag"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	ID           string `json:"id"`
	Autocomplete string `json:"autocomplete"`
	Placeholder  string `json:"placeholder"`
	Visible      bool   `json:"visible"`
}

// BuildInspectionJS enumerates up to 100 inputs and stamps each with the
// nonce. The result is a JSON array of LoginControl.
func BuildInspectionJS(nonce string) string {
	stamp, _ := json.Marshal(nonce)
	return `(() => { const n=` + string(stamp) + `; const els=[...document.querySelectorAll('input,select,textarea')]; ` +
		`const out=els.slice(0,100).map((e,i)=>{ try { e.setAttribute('` + inspectionSlotAttr + `',n+':'+i); ` +
		`const r=e.getBoundingClientRect(); const cs=getComputedStyle(e); ` +
		`return {index:i,tag:(e.tagName||'').toLowerCase(),type:(e.type||'').toLowerCase(),name:e.name||'',id:e.id||'',` +
		`autocomplete:e.autocomplete||'',placeholder:(e.placeholder||'').slice(0,120),` +
		`visible:!!(r&&(r.width>0||r.height>0))&&cs.visibility!=='hidden'&&cs.display!=='none'; } ` +
		`catch { return {index:i,tag:'',type:'',name:'',id:'',autocomplete:'',placeholder:'',visible:false}; } }); ` +
		`return JSON.stringify(out); })()`
}

// ClassifyLoginControls picks the password field and the identifier field.
// The password control is a visible password input; the identifier is the
// first visible text-like input (email, tel, or text).
func ClassifyLoginControls(controls []LoginControl) (passwordIdx, identifierIdx int, ok bool) {
	passwordIdx, identifierIdx = -1, -1
	for _, c := range controls {
		if !c.Visible {
			continue
		}
		if c.Tag != "input" {
			continue
		}
		switch c.Type {
		case "password":
			if passwordIdx < 0 {
				passwordIdx = c.Index
			}
		case "email", "tel", "text", "":
			if identifierIdx < 0 {
				identifierIdx = c.Index
			}
		}
	}
	if passwordIdx < 0 {
		return -1, -1, false
	}
	return passwordIdx, identifierIdx, true
}

// ClassifyOTPControls returns indexes of visible one-time-code inputs.
func ClassifyOTPControls(controls []LoginControl) []int {
	var out []int
	for _, c := range controls {
		if !c.Visible || c.Tag != "input" {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{c.Autocomplete, c.Name, c.ID, c.Placeholder}, " "))
		switch {
		case strings.Contains(haystack, "one-time-code"):
			out = append(out, c.Index)
		case strings.Contains(haystack, "otp") || strings.Contains(haystack, "totp"):
			out = append(out, c.Index)
		case (strings.Contains(haystack, "code") || strings.Contains(haystack, "passcode")) &&
			(c.Type == "text" || c.Type == "tel" || c.Type == "" || strings.Contains(haystack, "verif")):
			out = append(out, c.Index)
		}
	}
	return out
}

// ClassifyAddressControls maps input indexes to canonical address field
// names, preferring WHATWG autocomplete tokens and falling back to
// name/id/placeholder heuristics.
func ClassifyAddressControls(controls []LoginControl) map[int]string {
	tokens := map[string]string{}
	for field, token := range AddressFields {
		tokens[token] = field
	}
	nameHints := map[string]string{
		"address": "address_line1", "address1": "address_line1", "street": "address_line1",
		"address2": "address_line2", "apt": "address_line2", "suite": "address_line2",
		"city": "city", "town": "city",
		"state": "state", "province": "state", "region": "state",
		"zip": "postal_code", "postal": "postal_code",
		"country": "country",
	}
	out := map[int]string{}
	claimed := map[string]bool{}
	claim := func(index int, field string) {
		if field == "" || claimed[field] {
			return
		}
		if _, dup := out[index]; dup {
			return
		}
		out[index] = field
		claimed[field] = true
	}
	for _, c := range controls {
		if !c.Visible {
			continue
		}
		if field, ok := tokens[strings.ToLower(strings.TrimSpace(c.Autocomplete))]; ok {
			claim(c.Index, field)
		}
	}
	for _, c := range controls {
		if !c.Visible {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{c.Name, c.ID, c.Placeholder}, " "))
		bestHint, bestField := "", ""
		for hint, field := range nameHints {
			if strings.Contains(haystack, hint) && len(hint) > len(bestHint) {
				bestHint, bestField = hint, field
			}
		}
		if bestField != "" {
			claim(c.Index, bestField)
		}
	}
	return out
}

// BuildFillJS writes value into the stamped slot and returns a JSON fill
// report. The origin assertion runs first: a page that navigated away
// from the bound origin refuses with origin_changed and writes nothing.
func BuildFillJS(slot int, expectedOrigin, value, nonce string) string {
	origin, _ := json.Marshal(expectedOrigin)
	secret, _ := json.Marshal(value)
	stamp, _ := json.Marshal(nonce)
	return `(() => { const expected=` + string(origin) + `; const n=` + string(stamp) + `; ` +
		`if (location.origin!==expected) { return JSON.stringify({refused:'origin_changed',found:location.origin}); } ` +
		`const e=document.querySelector('[` + inspectionSlotAttr + `="'+n+':` + fmt.Sprintf("%d", slot) + `'"]'); ` +
		`if (!e) { return JSON.stringify({filled:0}); } ` +
		`const v=` + string(secret) + `; e.focus(); ` +
		`try { const proto=e.constructor&&e.constructor.prototype; ` +
		`const desc=(proto&&Object.getOwnPropertyDescriptor(proto,'value'))||Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value'); ` +
		`if (desc&&desc.set) { desc.set.call(e,v); } else { e.value=v; } } ` +
		`catch { try { e.value=v; } catch { return JSON.stringify({filled:0}); } } ` +
		`e.dispatchEvent(new Event('input',{bubbles:true})); e.dispatchEvent(new Event('change',{bubbles:true})); ` +
		`try { e.blur(); } catch {} ` +
		`try { e.removeAttribute('` + inspectionSlotAttr + `'); } catch {} ` +
		`return JSON.stringify({filled:1}); })()`
}

// FillReport is the parsed result of a fill script run.
type FillReport struct {
	Filled  int    `json:"filled"`
	Refused string `json:"refused"`
	Found   string `json:"found"`
}

// ParseFillReport decodes a fill result, tolerating the double-encoded
// string form the eval transport returns.
func ParseFillReport(raw string) FillReport {
	var report FillReport
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
			trimmed = inner
		}
	}
	_ = json.Unmarshal([]byte(trimmed), &report)
	return report
}

// ParseLoginControls decodes an inspection result the same way.
func ParseLoginControls(raw string) []LoginControl {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
			trimmed = inner
		}
	}
	var controls []LoginControl
	_ = json.Unmarshal([]byte(trimmed), &controls)
	return controls
}
