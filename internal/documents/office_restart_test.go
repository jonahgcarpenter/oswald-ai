package documents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type officeExit int

func (e officeExit) Error() string { return "synthetic office exit" }
func (e officeExit) ExitCode() int { return int(e) }

func TestOfficeProfileRestartProtocol(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second error
		cancel        bool
		calls         int
	}{
		{"restart succeeds", officeExit(81), nil, false, 2},
		{"restart bounded", officeExit(81), officeExit(81), false, 2},
		{"other failure", officeExit(1), nil, false, 1},
		{"canceled", officeExit(81), nil, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExtractor(nil)
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			var firstArgs []string
			e.run = func(callCtx context.Context, callDir, program string, args ...string) ([]byte, error) {
				calls++
				if callCtx != ctx || callDir != dir || program != libreoffice {
					t.Fatal("restart changed execution scope")
				}
				if calls == 1 {
					firstArgs = append([]string(nil), args...)
					if tc.cancel {
						cancel()
					}
					return nil, tc.first
				}
				if !reflect.DeepEqual(firstArgs, args) {
					t.Fatal("restart changed arguments/profile")
				}
				if tc.second != nil {
					return nil, tc.second
				}
				return nil, os.WriteFile(filepath.Join(dir, "input.pdf"), []byte("synthetic PDF"), 0600)
			}
			_, err := e.convert(ctx, dir, filepath.Join(dir, "input.rtf"), ".rtf")
			if calls != tc.calls {
				t.Fatalf("calls=%d", calls)
			}
			want := tc.first
			if tc.calls == 2 {
				want = tc.second
			}
			if !errors.Is(err, want) {
				t.Fatalf("want %v, got %v", want, err)
			}
		})
	}
}
