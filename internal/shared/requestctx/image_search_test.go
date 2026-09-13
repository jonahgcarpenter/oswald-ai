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
		{refs[0], {MIMEType: "image/gif", Data: "invalid"}},
		{refs[0], refs[1], refs[2], refs[3], refs[0]},
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
	wantActive := []ImageSearchReference{before[1], before[3], got[0], got[1]}
	if !reflect.DeepEqual(s.ActiveReferences(), wantActive) || s.MarkSelected(got[0].ID) || s.SelectedReferences()[0].ID != "search-1" {
		t.Fatal("recency changed selection")
	}
	if _, err := s.AddReferences(nil); err != nil || !reflect.DeepEqual(s.ActiveReferences(), wantActive) {
		t.Fatal("empty search changed active previews")
	}
}

func TestImageSearchPartialAndFullCatalogAdmission(t *testing.T) {
	s := NewImageSearchState()
	refs := []ImageSearchReference{
		{MIMEType: "image/png", Data: "a"},
		{MIMEType: "image/png", Data: "b"},
		{MIMEType: "image/png", Data: "c"},
		{MIMEType: "image/png", Data: "d"},
		{MIMEType: "image/png", Data: "e"},
		{MIMEType: "image/png", Data: "f"},
		{MIMEType: "image/png", Data: "g"},
	}
	first, err := s.AddReferences(refs[:3])
	if err != nil || len(first) != 3 {
		t.Fatalf("first admission: %v %v", first, err)
	}
	s.MarkInspected(first[0], first[0])
	s.MarkSelected(first[0].ID)
	second, err := s.AddReferences(refs[3:])
	if err != nil || len(second) != 1 || second[0].ID != "search-4" {
		t.Fatalf("partial admission: %v %v", second, err)
	}
	for i := 0; i < 10; i++ {
		got, err := s.AddReferences([]ImageSearchReference{refs[4], refs[0], refs[5], refs[3]})
		if err != nil || len(got) != 2 || got[0].ID != "search-1" || got[1].ID != "search-4" {
			t.Fatalf("full catalog reuse: %v %v", got, err)
		}
		if len(s.refs) != 4 || len(s.originalIDs) != 4 || s.nextID != 4 || len(s.SelectedReferences()) != 1 {
			t.Fatal("omitted previews grew catalog state or lost selection")
		}
	}
	active := s.ActiveReferences()
	if active[0].ID != "search-2" || active[1].ID != "search-3" || active[2].ID != "search-1" || active[3].ID != "search-4" {
		t.Fatalf("oldest-first recency: %v", active)
	}
	for _, ref := range active {
		s.MarkInspected(ref, ref)
		s.MarkSelected(ref.ID)
	}
	if len(s.SelectedReferences()) != 4 || s.MarkSelected("search-5") || len(s.inspected) != 4 {
		t.Fatal("selection exceeded catalog capacity")
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
