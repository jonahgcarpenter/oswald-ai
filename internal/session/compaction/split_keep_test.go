package compaction

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

func bigKeepTurn(id int64) memory.SessionTurn {
	return memory.SessionTurn{ID: id, UserText: "user " + strings.Repeat("u", 2000), AssistantText: "assistant " + strings.Repeat("a", 2000)}
}

func TestSplitKeepTailDividesNewestFirst(t *testing.T) {
	var turns []memory.SessionTurn
	for i := 10; i >= 1; i-- {
		turns = append(turns, bigKeepTurn(int64(i)))
	}
	// Each turn is ~1K tokens; keep 2500 fits the 2 newest, candidates hold the rest.
	kept, candidates := SplitKeepTail(turns, 2500)
	if len(kept) != 2 || kept[0].ID != 10 || kept[1].ID != 9 {
		t.Fatalf("kept=%v, want newest [10 9]", turnIDsForTest(kept))
	}
	if len(candidates) != 8 || candidates[0].ID != 8 || candidates[len(candidates)-1].ID != 1 {
		t.Fatalf("candidates=%v, want older [8..1]", turnIDsForTest(candidates))
	}
}

func TestSplitKeepTailKeepsSingleNewestWhenOversize(t *testing.T) {
	turns := []memory.SessionTurn{bigKeepTurn(2), bigKeepTurn(1)}
	kept, candidates := SplitKeepTail(turns, 10)
	if len(kept) != 1 || kept[0].ID != 2 {
		t.Fatalf("kept=%v, want single newest [2]", turnIDsForTest(kept))
	}
	if len(candidates) != 1 || candidates[0].ID != 1 {
		t.Fatalf("candidates=%v, want [1]", turnIDsForTest(candidates))
	}
}

func TestSplitKeepTailEmptyAndFitting(t *testing.T) {
	if kept, candidates := SplitKeepTail(nil, 15000); kept != nil || candidates != nil {
		t.Fatalf("nil yielded kept=%v candidates=%v", kept, candidates)
	}
	turns := []memory.SessionTurn{{ID: 2, UserText: "b", AssistantText: "B"}, {ID: 1, UserText: "a", AssistantText: "A"}}
	kept, candidates := SplitKeepTail(turns, 15000)
	if len(kept) != 2 || candidates != nil {
		t.Fatalf("fitting history yielded kept=%d candidates=%v", len(kept), candidates)
	}
}

func turnIDsForTest(turns []memory.SessionTurn) string {
	var out []string
	for _, turn := range turns {
		out = append(out, fmt.Sprint(turn.ID))
	}
	return strings.Join(out, " ")
}
