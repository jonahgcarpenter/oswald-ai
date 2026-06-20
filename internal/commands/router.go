package commands

// Handler processes a command for the current canonical user.
type Handler interface {
	CanHandle(input string) bool
	Handle(canonicalUserID, input string) (response string, handled bool, err error)
}

// Router dispatches command input to registered handlers in order.
type Router struct {
	handlers []Handler
}

// NewRouter creates a command router from the provided handlers.
func NewRouter(handlers ...Handler) *Router {
	return &Router{handlers: append([]Handler(nil), handlers...)}
}

// IsCommand reports whether any registered handler recognizes input as a command.
func (r *Router) IsCommand(input string) bool {
	if r == nil {
		return false
	}
	for _, handler := range r.handlers {
		if handler != nil && handler.CanHandle(input) {
			return true
		}
	}
	return false
}

// Handle dispatches input to the first handler that recognizes it.
func (r *Router) Handle(canonicalUserID, input string) (string, bool, error) {
	if r == nil {
		return "", false, nil
	}
	for _, handler := range r.handlers {
		if handler == nil {
			continue
		}
		if !handler.CanHandle(input) {
			continue
		}
		response, handled, err := handler.Handle(canonicalUserID, input)
		if handled || err != nil {
			return response, handled, err
		}
	}
	return "", false, nil
}
