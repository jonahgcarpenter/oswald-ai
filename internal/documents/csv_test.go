package documents

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestCSVPreflightQuotingAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		separator   byte
		want        error
	}{
		{"quoted separators", `"` + strings.Repeat(",", 2000) + `",tail`, ',', nil},
		{"escaped quotes and multiline", "\"one\"\"two,\nthree\r\nfour\",tail\r\nnext,row", ',', nil},
		{"quoted TSV", "\"one\t\"\"two\nthree\"\ttail", '\t', nil},
		{"field limit", strings.Repeat(",", maxCSVFields-1), ',', nil},
		{"too many fields", strings.Repeat(",", maxCSVFields), ',', ErrLimit},
		{"too many TSV fields", strings.Repeat("\t", maxCSVFields), '\t', ErrLimit},
		{"byte limit", strings.Repeat("x", maxCSVRecordBytes), ',', nil},
		{"too many bytes", strings.Repeat("x", maxCSVRecordBytes+1), ',', ErrLimit},
		{"multiline byte limit", `"` + strings.Repeat("x\n", maxCSVRecordBytes/2) + `"`, ',', ErrLimit},
		{"record reset", strings.Repeat("x", maxCSVRecordBytes-1) + "\n" + strings.Repeat("y", maxCSVRecordBytes), ',', nil},
		{"field reset", strings.Repeat(",", maxCSVFields-1) + "\n" + strings.Repeat(",", maxCSVFields-1), ',', nil},
		{"bare quote", "x\"y", ',', ErrInvalid},
		{"unclosed quote", "\"x\"\"", ',', ErrInvalid},
		{"text after quote", "\"x\"y", ',', ErrInvalid},
		{"bare CR after quote", "\"x\"\ry", ',', ErrInvalid},
		{"final CR", "\"x\"\r", ',', nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := preflightCSV(context.Background(), []byte(tc.input), tc.separator); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
	r, err := NewExtractor(nil).Extract(context.Background(), "multiline.csv", "", []byte("\"one\"\"two,\nthree\r\nfour\",tail\r\nnext,row"))
	if err != nil || len(r.Chunks) != 2 || r.Chunks[0].Text != "one\"two,\nthree\nfour\ttail" || r.Chunks[1].Text != "next\trow" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestCSVDelimiterHeavyInputAllocationBound(t *testing.T) {
	for _, separator := range []byte{',', '\t'} {
		// Allocate the admitted 20 MiB input before measuring extraction overhead.
		data := bytes.Repeat([]byte{separator}, maxInput)
		ext := ".csv"
		if separator == '\t' {
			ext = ".tsv"
		}
		e := NewExtractor(nil)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r, err := e.Extract(context.Background(), "heavy"+ext, "", data)
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(data)
		if !errors.Is(err, ErrLimit) || len(r.Chunks) != 0 {
			t.Fatalf("expected pre-parse rejection, got %v", err)
		}
		// The old csv.Reader path allocated hundreds of MiB in field metadata.
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
			t.Fatalf("extraction allocated %d bytes", allocated)
		}
	}
}

// Cancel deterministically during scanning, without sleeps or huge allocations.
type scanCancellation struct {
	context.Context
	calls int
}

func (c *scanCancellation) Err() error {
	c.calls++
	if c.calls >= 3 {
		return context.Canceled
	}
	return nil
}

func TestCSVPreflightCancellation(t *testing.T) {
	ctx := &scanCancellation{Context: context.Background()}
	data := []byte(strings.Repeat("\"a,b\"\n", 10000))
	if err := preflightCSV(ctx, data, ','); !errors.Is(err, context.Canceled) || ctx.calls != 3 {
		t.Fatalf("calls=%d err=%v", ctx.calls, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := preflightCSV(canceled, nil, ','); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := NewExtractor(nil).Extract(canceled, "heavy.csv", "", bytes.Repeat([]byte(","), maxInput)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
