package builtin

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	approvecommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/approve"
	compactcommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/compact"
	modelcommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/model"
	queuecommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/queue"
	sessioncommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/session"
	stopcommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/stop"
)

// NewProfileService registers only the profile runtime's public commands.
// approvalRoot binds /approve execution to one profile working directory.
func NewProfileService(store sessioncommands.Starter, canceler stopcommands.Canceler, approvalRoot string, compacter compactcommands.Summarizer, compactStore compactcommands.Store, queuer queuecommands.Submitter, modelRouter modelcommands.Router, modelPins modelcommands.Pins) (*commands.Service, error) {
	help := &helpHandler{}
	cmds := []commands.Command{
		{Handler: help},
		{Handler: sessioncommands.New(store)},
		{Handler: stopcommands.New(canceler)},
		{Handler: approvecommands.NewApprove(approvalRoot)},
		{Handler: approvecommands.NewDeny()},
		{Handler: queuecommands.New(queuer)},
	}
	if compacter != nil && compactStore != nil {
		cmds = append(cmds, commands.Command{Handler: compactcommands.New(compactStore, compacter)})
	}
	if modelRouter != nil && modelPins != nil {
		cmds = append(cmds, commands.Command{Handler: modelcommands.New(modelRouter, modelPins)})
	}
	service, err := commands.NewServiceWithCommands(cmds...)
	if err != nil {
		return nil, err
	}
	help.commands = service
	return service, nil
}
