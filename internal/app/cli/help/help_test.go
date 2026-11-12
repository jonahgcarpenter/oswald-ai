package help

import "testing"

func TestWantsHelp(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"empty", nil, false},
		{"help", []string{"help"}, true},
		{"short", []string{"-h"}, true},
		{"long", []string{"--help"}, true},
		{"help with rest", []string{"--help", "extra"}, true},
		{"command", []string{"show"}, false},
		{"flag", []string{"--profile", "x"}, false},
		{"later help is a value", []string{"set", "KEY", "--help"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WantsHelp(tc.args); got != tc.want {
				t.Fatalf("WantsHelp(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
