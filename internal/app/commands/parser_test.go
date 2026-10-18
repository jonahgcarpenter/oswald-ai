package commands

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantOK   bool
		wantName string
		wantArgs []string
		wantText string
	}{
		{name: "plain command", in: "/connect", wantOK: true, wantName: "connect"},
		{name: "command with args", in: " /connect 1 abc ", wantOK: true, wantName: "connect", wantArgs: []string{"1", "abc"}, wantText: "1 abc"},
		{name: "only slash", in: "/", wantOK: true, wantName: ""},
		{name: "slash not first", in: "what does /connect do?", wantOK: false},
		{name: "empty", in: "", wantOK: false},
		{name: "space only", in: "   ", wantOK: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := Parse(test.in)
			if ok != test.wantOK {
				t.Fatalf("Parse(%q) ok = %t, want %t", test.in, ok, test.wantOK)
			}
			if !ok {
				return
			}
			if got.Name != test.wantName || got.ArgsText != test.wantText {
				t.Fatalf("Parse(%q) = %+v, want name=%q argsText=%q", test.in, got, test.wantName, test.wantText)
			}
			if len(got.Args) != len(test.wantArgs) {
				t.Fatalf("args = %#v, want %#v", got.Args, test.wantArgs)
			}
			for i := range got.Args {
				if got.Args[i] != test.wantArgs[i] {
					t.Fatalf("args = %#v, want %#v", got.Args, test.wantArgs)
				}
			}
		})
	}
}

func TestIsAttempt(t *testing.T) {
	if !IsAttempt(" /connect ") {
		t.Fatal("expected slash-prefixed input to be an attempt")
	}
	if IsAttempt("what does /connect do?") {
		t.Fatal("expected embedded slash command text not to be an attempt")
	}
}
