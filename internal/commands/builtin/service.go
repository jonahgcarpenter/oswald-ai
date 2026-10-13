package builtin

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	approvecommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/approve"
	sessioncommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/session"
	stopcommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/stop"
)

// NewProfileService registers only the profile runtime's public commands.
// approvalRoot binds /approve execution to one profile working directory.
func NewProfileService(store sessioncommands.Starter, canceler stopcommands.Canceler, approvalRoot string) (*commands.Service, error) {
	help := &helpHandler{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: help}, commands.Command{Handler: sessioncommands.New(store)}, commands.Command{Handler: stopcommands.New(canceler)}, commands.Command{Handler: approvecommands.NewApprove(approvalRoot)}, commands.Command{Handler: approvecommands.NewDeny()})
	if err != nil {
		return nil, err
	}
	help.commands = service
	return service, nil
}
