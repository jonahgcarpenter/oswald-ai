package websocket

import (
	"encoding/json"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
)

type runtimeResponder struct {
	conn        *gorilla.Conn
	messageType int
}

func (r *runtimeResponder) StartProcessing() (func(), error) {
	return nil, nil
}

func (r *runtimeResponder) SendFallback(text string) error {
	return r.write(agent.AgentResponse{Response: text})
}

func (r *runtimeResponder) SendCommandResponse(text string) error {
	return r.write(agent.AgentResponse{Response: text})
}

func (r *runtimeResponder) SendAgentError(text string) error {
	return r.write(agent.AgentResponse{Error: text})
}

func (r *runtimeResponder) SendAgentResponse(response *agent.AgentResponse) error {
	if response == nil {
		return nil
	}
	return r.write(*response)
}

func (r *runtimeResponder) write(response agent.AgentResponse) error {
	jsonBytes, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return r.conn.WriteMessage(r.messageType, jsonBytes)
}
