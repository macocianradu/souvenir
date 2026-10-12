package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/db/users"
)

const usage = `usage:
  souvenir                          chat in this terminal, as the owner
  souvenir serve                    serve the chat over SSH
  souvenir users                    list users
  souvenir users add NAME [admin]   add a user
  souvenir users remove NAME        remove a user and everything they own
  souvenir keys                     list registered SSH keys
  souvenir keys add NAME [KEY]      register a public key for a user; reads stdin without KEY
  souvenir keys remove FINGERPRINT  remove a key
`

func manage(ctx context.Context, accounts *users.Store, args []string) int {
	if err := runManage(ctx, accounts, args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runManage(ctx context.Context, accounts *users.Store, args []string) error {
	switch strings.Join(args[:min(len(args), 2)], " ") {
	case "users":
		all, err := accounts.List(ctx)
		if err != nil {
			return err
		}
		for _, u := range all {
			role := ""
			if u.IsAdmin {
				role = " (admin)"
			}
			fmt.Printf("%s%s\n", u.Name, role)
		}
		return nil
	case "users add":
		if len(args) < 3 || len(args) > 4 || (len(args) == 4 && args[3] != "admin") {
			return fmt.Errorf("%s", usage)
		}
		u, err := accounts.Add(ctx, args[2], len(args) == 4)
		if err == nil {
			fmt.Printf("Added %s\n", u.Name)
		}
		return err
	case "users remove":
		if len(args) != 3 {
			return fmt.Errorf("%s", usage)
		}
		return accounts.Remove(ctx, args[2])
	case "keys":
		keys, err := accounts.Keys(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			fmt.Printf("%s  %s  %s\n", k.User, k.Fingerprint, k.Comment)
		}
		return nil
	case "keys add":
		if len(args) < 3 {
			return fmt.Errorf("%s", usage)
		}
		line := strings.Join(args[3:], " ")
		if line == "" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			line = string(data)
		}
		k, err := accounts.AddKey(ctx, args[2], line)
		if err == nil {
			fmt.Printf("Registered %s for %s\n", k.Fingerprint, k.User)
		}
		return err
	case "keys remove":
		if len(args) != 3 {
			return fmt.Errorf("%s", usage)
		}
		return accounts.RemoveKey(ctx, args[2])
	}
	return fmt.Errorf("%s", usage)
}
