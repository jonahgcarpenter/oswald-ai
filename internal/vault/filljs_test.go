package vault

import (
	"strings"
	"testing"
)

func TestClassifyLoginControls(t *testing.T) {
	controls := []LoginControl{
		{Index: 0, Tag: "input", Type: "text", Name: "user", Visible: true},
		{Index: 1, Tag: "input", Type: "password", Visible: true},
		{Index: 2, Tag: "input", Type: "password", Visible: false},
	}
	pw, id, ok := ClassifyLoginControls(controls)
	if !ok || pw != 1 || id != 0 {
		t.Fatalf("classify = %d %d %v", pw, id, ok)
	}
	if _, _, ok := ClassifyLoginControls([]LoginControl{{Index: 0, Tag: "input", Type: "text", Visible: true}}); ok {
		t.Fatal("classified without a password field")
	}
	hidden := []LoginControl{{Index: 0, Tag: "input", Type: "password", Visible: false}}
	if _, _, ok := ClassifyLoginControls(hidden); ok {
		t.Fatal("classified a hidden password field")
	}
}

func TestClassifyOTPControls(t *testing.T) {
	controls := []LoginControl{
		{Index: 0, Tag: "input", Type: "text", Autocomplete: "one-time-code", Visible: true},
		{Index: 1, Tag: "input", Type: "text", Name: "username", Visible: true},
		{Index: 2, Tag: "input", Type: "tel", Name: "otp_code", Visible: true},
	}
	slots := ClassifyOTPControls(controls)
	if len(slots) != 2 || slots[0] != 0 || slots[1] != 2 {
		t.Fatalf("otp slots = %v", slots)
	}
	if slots := ClassifyOTPControls([]LoginControl{{Index: 0, Tag: "input", Type: "text", Name: "q", Visible: true}}); len(slots) != 0 {
		t.Fatalf("otp false positive = %v", slots)
	}
}

func TestFillJSAssertsOriginAndFills(t *testing.T) {
	js := BuildFillJS(1, "https://example.com", `p"a\\ss`, "abc123")
	if !strings.Contains(js, `"https://example.com"`) {
		t.Fatal("fill script lost the bound origin")
	}
	if !strings.Contains(js, "origin_changed") {
		t.Fatal("fill script lost the origin assertion")
	}
	if strings.Contains(js, `p"a\\ss`) {
		t.Fatal("fill script leaked the raw secret")
	}
	report := ParseFillReport(`{"filled":1}`)
	if report.Filled != 1 {
		t.Fatalf("report = %+v", report)
	}
	report = ParseFillReport(`"{\"refused\":\"origin_changed\",\"found\":\"https://evil.example\"}"`)
	if report.Refused != "origin_changed" {
		t.Fatalf("double-encoded report = %+v", report)
	}
	controls := ParseLoginControls(`"[{\"index\":1,\"tag\":\"input\",\"type\":\"password\",\"visible\":true}]"`)
	if len(controls) != 1 || controls[0].Type != "password" {
		t.Fatalf("controls = %+v", controls)
	}
	if got := BuildInspectionJS("n"); !strings.Contains(got, `"n"`) {
		t.Fatal("inspection script lost the nonce")
	}
}
