package requestctx

import (
	"context"
	"reflect"
	"testing"
)

func TestImageSearchCatalogInspectionAndBounds(t *testing.T) {
	s := NewImageSearchState()
	ctx := WithImageSearchState(context.Background(), s)
	if ImageSearchStateFromContext(ctx) != s || ImageSearchStateFromContext(context.Background()) != nil {
		t.Fatal("context ownership")
	}
	ref := ImageSearchReference{Title: "synthetic", MIMEType: "image/png", Data: "cHJldmlldw=="}
	first, err := s.AddReferences([]ImageSearchReference{ref, ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectedReference(first[0].ID); err == nil {
		t.Fatal("same batch selectable")
	}
	s.MarkInspected(first[0], first[0])
	if s.MarkSelected("unknown") || !s.MarkSelected(first[0].ID) || s.MarkSelected(first[0].ID) {
		t.Fatal("selection fencing")
	}
	second, err := s.AddReferences([]ImageSearchReference{ref, ref})
	if err != nil {
		t.Fatal(err)
	}
	active := s.ActiveReferences()
	if len(first) != 1 || len(second) != 1 || len(active) != 1 || active[0].ID != first[0].ID || second[0].ID != first[0].ID || s.MarkSelected(second[0].ID) {
		t.Fatal("active window")
	}
	active[0].ID = "mutated"
	if s.ActiveReferences()[0].ID != second[0].ID {
		t.Fatal("aliased catalog")
	}
	if len(s.SelectedReferences()) != 1 || s.SelectedReferences()[0].ID != first[0].ID {
		t.Fatal("lost selection")
	}
	if s.ReserveSearch() != nil || s.ReserveSearch() != nil || s.ReserveSearch() == nil {
		t.Fatal("execution bound")
	}
	if _, err := NewImageSearchState().InspectedReference(first[0].ID); err == nil {
		t.Fatal("cross-request selection")
	}
}

func TestImageSearchDedupRecencyAtomicValidationAndOriginalIdentity(t *testing.T) {
	s := NewImageSearchState()
	refs := []ImageSearchReference{
		{MIMEType: "image/png", Data: "a", Title: "first"},
		{MIMEType: "image/png", Data: "b"},
		{MIMEType: "image/png", Data: "c"},
		{MIMEType: "image/jpeg", Data: "c"},
	}
	if _, err := s.AddReferences(refs[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddReferences(refs[2:]); err != nil {
		t.Fatal(err)
	}
	before := append([]ImageSearchReference(nil), s.refs...)
	for _, invalid := range [][]ImageSearchReference{
		{refs[0], {MIMEType: "image/png"}},
		{refs[0], {MIMEType: "image/png", Data: "overflow"}},
		{refs[0], refs[1], refs[2]},
	} {
		if _, err := s.AddReferences(invalid); err == nil || !reflect.DeepEqual(s.refs, before) || s.nextID != 4 || len(s.originalIDs) != 4 {
			t.Fatal("invalid batch mutated catalog")
		}
	}
	// Model retry inspection may replace bytes; dedup must use original identity.
	resized := s.refs[0]
	resized.Data = "resized"
	s.MarkInspected(s.refs[0], resized)
	if !s.MarkSelected("search-1") {
		t.Fatal("selection")
	}
	refs[0].Title = "later provenance"
	got, err := s.AddReferences([]ImageSearchReference{refs[0], refs[2]})
	if err != nil || len(got) != 2 || got[0].ID != "search-1" || got[0].Data != "resized" || got[0].Title != "first" || got[1].ID != "search-3" {
		t.Fatalf("repeated references: %v %v", got, err)
	}
	if !reflect.DeepEqual(s.ActiveReferences(), got) || s.MarkSelected(got[0].ID) || s.SelectedReferences()[0].ID != "search-1" {
		t.Fatal("recency changed selection")
	}
	if _, err := s.AddReferences(nil); err != nil || !reflect.DeepEqual(s.ActiveReferences(), got) {
		t.Fatal("empty search changed active previews")
	}
}

func TestImageSearchDuplicateBatchAllocatesSequentialIDs(t *testing.T) {
	s := NewImageSearchState()
	a := ImageSearchReference{MIMEType: "image/png", Data: "a"}
	b := ImageSearchReference{MIMEType: "image/png", Data: "b"}
	first, err := s.AddReferences([]ImageSearchReference{a, a})
	if err != nil || len(first) != 1 || first[0].ID != "search-1" {
		t.Fatal("duplicate batch")
	}
	second, err := s.AddReferences([]ImageSearchReference{a, b})
	if err != nil || len(second) != 2 || second[0].ID != "search-1" || second[1].ID != "search-2" {
		t.Fatal("sequential allocation")
	}
}
