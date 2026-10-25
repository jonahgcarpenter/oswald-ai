package clarify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// Notice carries one parked clarify question to the transport for display.
// Expired marks the follow-up sent when the wait lapses with no answer.
type Notice struct {
	Question    Question
	MinutesLeft int
	Expired     bool
}

// Notifier displays a parked clarify ask while the tool call blocks.
// Installers render per transport: transports without live progress send a
// real message, streaming transports show a status chunk. A nil error means
// the ask reached the user; any error finalizes the wait as undelivered.
type Notifier func(notice Notice) error

type notifierKey struct{}

// WithNotifier attaches the parked-ask reporter to the request.
func WithNotifier(ctx context.Context, fn Notifier) context.Context {
	return context.WithValue(ctx, notifierKey{}, fn)
}

// NotifierFromContext returns the parked-ask reporter, if any.
func NotifierFromContext(ctx context.Context) Notifier {
	fn, _ := ctx.Value(notifierKey{}).(Notifier)
	return fn
}

// isInteractiveGateway reports whether a transport has a user who can answer
// a parked ask. Unattended transports fail closed instead of blocking a turn
// that nobody will ever resolve.
func isInteractiveGateway(gateway string) bool {
	switch strings.ToLower(strings.TrimSpace(gateway)) {
	case "discord", "imessage":
		return true
	default:
		return false
	}
}

// NewHandler parks one single-flight clarify ask per conversation and walks
// the questions one at a time: each goes out as its own text, the user's
// reply answers it, and the next follows. The wait ends at the shared
// deadline, on cancellation, or when every question is answered. Collected
// answers return as this call's tool result so the agent continues in the
// same loop instead of ending the turn.
func NewHandler() func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, fmt.Errorf("clarify requires an authenticated request")
		}
		if !isInteractiveGateway(principal.Gateway) {
			return governance.Result{}, fmt.Errorf("clarify requires an interactive transport with a user who can answer")
		}
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		for key := range args {
			if key != "questions" {
				return governance.Result{}, fmt.Errorf("unsupported clarify argument")
			}
		}
		questions, err := parseQuestions(args["questions"])
		if err != nil {
			return governance.Result{}, err
		}
		meta := requestctx.MetadataFromContext(ctx)
		session := strings.TrimSpace(meta.SessionID)
		if session == "" {
			session = strings.TrimSpace(meta.RequestID)
		}
		notify := NotifierFromContext(ctx)
		if notify == nil {
			return governance.Result{}, fmt.Errorf("clarify requires an interactive transport with a user who can answer")
		}
		// The answer channel buffers the whole form so rapid replies arrive
		// in order even when the handler has not reached the next question.
		answer := make(chan Answer, len(questions))
		pending := Pending{
			RequestID:  strings.TrimSpace(meta.RequestID),
			Profile:    principal.CanonicalUserID,
			SessionKey: session,
			Questions:  questions,
			Answer:     answer,
		}
		if !Request(pending) {
			return governance.Result{}, fmt.Errorf("a clarify ask is already pending in this conversation; wait for the user's answer")
		}
		cleanup := func() {
			if _, ok := Take(pending.Profile, pending.SessionKey); ok {
				// Parked ask withdrawn before resolution.
			}
		}
		deadline := time.Now().Add(ClarifyWindow)
		timeout := time.After(time.Until(deadline))
		minutesLeft := func() int {
			if mins := int(time.Until(deadline).Minutes()); mins > 1 {
				return mins
			}
			return 1
		}
		responses := make([]Response, 0, len(questions))
		// consume resolves one reply, racing the shared deadline and the
		// request context. A non-empty outcome ends the wait; a non-nil
		// error aborts the turn.
		consume := func(question Question) (Response, string, error) {
			select {
			case ans, ok := <-answer:
				if !ok {
					return Response{Question: question.Question}, outcomeCancelled, nil
				}
				return ParseAnswer(question, ans.Text), "", nil
			case <-timeout:
				// A reply racing the deadline still wins: consume it first.
				select {
				case ans, ok := <-answer:
					if ok {
						return ParseAnswer(question, ans.Text), "", nil
					}
				default:
				}
				return Response{Question: question.Question}, outcomeTimedOut, nil
			case <-ctx.Done():
				// A reply racing cancellation still wins: consume it first.
				select {
				case ans, ok := <-answer:
					if ok {
						return ParseAnswer(question, ans.Text), "", nil
					}
				default:
				}
				return Response{}, "", ctx.Err()
			}
		}
		for i, question := range questions {
			if err := notify(Notice{Question: question, MinutesLeft: minutesLeft()}); err != nil {
				cleanup()
				return governance.Result{
					Content: ResultContent(append(responses, unansweredAll(questions[i:])...), outcomeUndelivered, "a question could not be delivered; continue with the answers collected so far"),
					Outcome: governance.OutcomeProductive,
				}, nil
			}
			response, outcome, err := consume(question)
			if err != nil {
				cleanup()
				return governance.Result{}, err
			}
			if outcome != "" {
				cleanup()
				notice := "the request ended before every question was answered; continue with the answers collected so far"
				if outcome == outcomeTimedOut {
					_ = notify(Notice{Expired: true})
					notice = "the user did not answer in time; continue with the answers collected so far instead of re-asking verbatim"
				}
				return governance.Result{
					Content: ResultContent(append(responses, unansweredAll(questions[i:])...), outcome, notice),
					Outcome: governance.OutcomeProductive,
				}, nil
			}
			responses = append(responses, response)
		}
		cleanup()
		return governance.Result{
			Content: ResultContent(responses, outcomeSubmitted, ""),
			Outcome: governance.OutcomeProductive,
		}, nil
	}
}

// unansweredAll marks every question unanswered for non-submitted outcomes.
func unansweredAll(questions []Question) []Response {
	responses := make([]Response, 0, len(questions))
	for _, question := range questions {
		responses = append(responses, Response{Question: question.Question, Status: statusUnanswered})
	}
	return responses
}
