package homeassistant

import (
	"fmt"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

type runtimeResponder struct {
	connection *trackedConnection
	requestID  string
	mu         sync.Mutex
	terminal   bool
}

func (r *runtimeResponder) StartProcessing() (func(), error) { return nil, nil }

func (r *runtimeResponder) SendFallback(text string) error { return r.sendResult(text, "", nil) }

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	if err := result.ValidateAttachments(); err != nil {
		return err
	}
	if len(result.OrderedAttachments()) > 0 {
		return r.sendError("command_attachments_unsupported", "Home Assistant does not support command attachments.")
	}
	return r.sendResult(result.Text, "", nil)
}

func (r *runtimeResponder) SendAgentError(_ string) error {
	return r.sendError("request_failed", "Oswald could not process the request.")
}

func (r *runtimeResponder) CancelAgentResponse() error {
	return r.sendError("request_canceled", "The request was canceled.")
}

func (r *runtimeResponder) SendAgentResponse(response *agent.AgentResponse) error {
	if response == nil {
		return r.sendError("request_failed", "Oswald returned no response.")
	}
	if response.Error != "" {
		return r.sendError("request_failed", "Oswald could not process the request.")
	}
	return r.sendResult(response.Response, response.Model, response.Metrics)
}

func (r *runtimeResponder) sendResult(response, model string, metrics *agent.ModelMetrics) error {
	return r.sendTerminal(protocolMessage{Type: "result", RequestID: r.requestID, Response: response, Model: model, Metrics: metrics})
}

func (r *runtimeResponder) sendError(code, message string) error {
	return r.sendTerminal(protocolMessage{Type: "error", RequestID: r.requestID, Code: code, Message: message})
}

func (r *runtimeResponder) sendTerminal(message protocolMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminal {
		return fmt.Errorf("home assistant terminal response already sent")
	}
	r.terminal = true
	return r.connection.writeJSON(message)
}
