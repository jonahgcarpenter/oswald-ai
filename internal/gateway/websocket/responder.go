package websocket

import (
	"encoding/base64"
	"encoding/json"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
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

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	if err := result.ValidateAttachments(); err != nil {
		return err
	}
	attachments := result.OrderedAttachments()
	if len(attachments) == 0 {
		return r.write(agent.AgentResponse{Response: result.Text})
	}
	encoded := make([]CommandResponseAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		encoded = append(encoded, CommandResponseAttachment{
			Filename: attachment.Filename,
			MIMEType: attachment.MIMEType,
			Data:     base64.StdEncoding.EncodeToString(attachment.Data),
		})
	}
	response := CommandResponse{Type: "command_response", Response: result.Text, Attachments: encoded}
	if len(encoded) == 1 {
		response.Attachment = &response.Attachments[0]
	}
	return r.writeJSON(response)
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
	return r.writeJSON(response)
}

func (r *runtimeResponder) writeJSON(response any) error {
	jsonBytes, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return r.conn.WriteMessage(r.messageType, jsonBytes)
}
