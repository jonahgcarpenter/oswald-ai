package builtin

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	sessioncommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/session"
	stopcommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/stop"
)

// NewProfileService registers only the profile runtime's public commands.
func NewProfileService(store sessioncommands.Starter, canceler stopcommands.Canceler) (*commands.Service, error) {
	help := &helpHandler{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: help}, commands.Command{Handler: sessioncommands.New(store)}, commands.Command{Handler: stopcommands.New(canceler)})
	if err != nil {
		return nil, err
	}
	help.commands = service
	return service, nil
}
