// Command apikey issues and revokes operator-managed client credentials.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func main() {
	if err := run(context.Background(), config.DefaultDatabasePath, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, path string, args []string, out io.Writer) error {
	if len(args) != 2 || (args[0] != "create" && args[0] != "revoke") {
		return fmt.Errorf("usage: apikey create <canonical_user_id> | apikey revoke <key_id>")
	}
	svc := accounts.NewService(path, nil, nil, nil)
	defer svc.Close()
	switch args[0] {
	case "create":
		id, token, err := svc.CreateAPIKey(ctx, args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "key_id: %s\ntoken: %s\n", id, token)
		return err
	case "revoke":
		if err := svc.RevokeAPIKey(ctx, args[1]); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "revoked")
		return err
	}
	return nil
}
