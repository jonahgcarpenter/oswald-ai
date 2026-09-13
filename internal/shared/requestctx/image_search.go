package requestctx

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

// ImageSearchReference is a normalized preview snapshot owned by one request.
// Data is base64, not original image bytes. These fields avoid an llm import cycle.
type ImageSearchReference struct {
	ID, Title, SourceURL string
	MIMEType, Data       string
}

// ImageSearchState owns the bounded search catalog, inspection receipts, and
// selected attribution for one Process invocation; it must never be reused.
type ImageSearchState struct {
	mu        sync.Mutex
	refs      []ImageSearchReference
	inspected map[string]bool
	selected  []string
	searches  int
	// Original identities survive agent replacement of inspected preview bytes.
	originalIDs map[[sha256.Size]byte]string
	nextID      int
}

// NewImageSearchState creates an empty request-owned catalog.
func NewImageSearchState() *ImageSearchState {
	return &ImageSearchState{inspected: make(map[string]bool)}
}

// WithImageSearchState attaches the agent-owned catalog without copying it.
func WithImageSearchState(ctx context.Context, state *ImageSearchState) context.Context {
	return context.WithValue(ctx, contextKey("image_search"), state)
}

// ImageSearchStateFromContext returns the current request's catalog, or nil.
func ImageSearchStateFromContext(ctx context.Context) *ImageSearchState {
	state, _ := ctx.Value(contextKey("image_search")).(*ImageSearchState)
	return state
}

// ReserveSearch consumes one of two handler executions, including failed searches.
func (s *ImageSearchState) ReserveSearch() error {
	if s == nil {
		return errors.New("image search state unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.searches >= 2 {
		return errors.New("image search request limit reached")
	}
	s.searches++
	return nil
}

// AddReferences validates and admits at most four tool-normalized previews, assigns
// server-owned IDs, and returns unique value copies in search order. Repeated
// original MIME/data pairs reuse IDs and refresh recency, preserving provenance.
// New previews beyond the four-entry catalog are omitted; existing IDs remain
// reusable even at capacity. Invalid batches never mutate the catalog.
func (s *ImageSearchState) AddReferences(refs []ImageSearchReference) ([]ImageSearchReference, error) {
	if s == nil {
		return nil, errors.New("image search state unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(refs) > 4 {
		return nil, errors.New("image search catalog limit reached")
	}
	keys := make([][sha256.Size]byte, len(refs))
	for i, ref := range refs {
		if ref.Data == "" || (ref.MIMEType != "image/jpeg" && ref.MIMEType != "image/png") {
			return nil, errors.New("invalid normalized preview")
		}
		keys[i] = sha256.Sum256([]byte(ref.MIMEType + "\x00" + ref.Data))
	}
	if s.originalIDs == nil {
		s.originalIDs = make(map[[sha256.Size]byte]string)
	}
	var out []ImageSearchReference
	seen := make(map[string]bool)
	for i, ref := range refs {
		id := s.originalIDs[keys[i]]
		if seen[id] {
			continue
		}
		if id == "" {
			if len(s.refs) == 4 {
				continue
			}
			s.nextID++
			ref.ID = fmt.Sprintf("search-%d", s.nextID)
			s.originalIDs[keys[i]] = ref.ID
		} else {
			for j, existing := range s.refs {
				if existing.ID == id {
					ref = existing
					s.refs = append(s.refs[:j], s.refs[j+1:]...)
					break
				}
			}
		}
		s.refs = append(s.refs, ref)
		seen[ref.ID] = true
		out = append(out, ref)
	}
	return out, nil
}

// ActiveReferences returns copies of all admitted previews, oldest first, with
// a maximum of four. The agent may omit whole previews to fit its input budget.
func (s *ImageSearchState) ActiveReferences() []ImageSearchReference {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ImageSearchReference(nil), s.refs...)
}

// MarkInspected verifies the original request image against the catalog and
// retains the representation submitted in a successful call, including resize
// retries. Selected attachments remain frozen at their inspected representation.
func (s *ImageSearchState) MarkInspected(original, submitted ImageSearchReference) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inspected == nil {
		s.inspected = make(map[string]bool)
	}
	for i, ref := range s.refs {
		if ref.ID == original.ID && ref.MIMEType == original.MIMEType && ref.Data == original.Data && submitted.Data != "" && (submitted.MIMEType == "image/jpeg" || submitted.MIMEType == "image/png") {
			for _, selected := range s.selected {
				if selected == ref.ID {
					return true
				}
			}
			s.refs[i].Data, s.refs[i].MIMEType = submitted.Data, submitted.MIMEType
			s.inspected[ref.ID] = true
			return true
		}
	}
	return false
}

// InspectedReference returns a value copy only for a current, inspected ID.
func (s *ImageSearchState) InspectedReference(id string) (ImageSearchReference, error) {
	if s == nil {
		return ImageSearchReference{}, errors.New("image search state unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range s.refs {
		if ref.ID == id && s.inspected[id] {
			return ref, nil
		}
	}
	return ImageSearchReference{}, errors.New("select an image result inspected in a prior successful model call")
}

// MarkSelected records attribution only after the tool validates its attachment.
// False means already selected; callers must not return a duplicate attachment.
func (s *ImageSearchState) MarkSelected(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inspected[id] {
		return false
	}
	for _, selected := range s.selected {
		if selected == id {
			return false
		}
	}
	s.selected = append(s.selected, id)
	return true
}

// Unselect rolls back a selection whose attachment the agent could not admit.
func (s *ImageSearchState) Unselect(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, selected := range s.selected {
		if selected == id {
			s.selected = append(s.selected[:i], s.selected[i+1:]...)
			return
		}
	}
}

// SelectedReferences returns attribution copies in selection order, independently
// of the active vision window. The agent owns final delivery and attribution.
func (s *ImageSearchState) SelectedReferences() []ImageSearchReference {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ImageSearchReference, 0, len(s.selected))
	for _, id := range s.selected {
		for _, ref := range s.refs {
			if ref.ID == id {
				out = append(out, ref)
			}
		}
	}
	return out
}
