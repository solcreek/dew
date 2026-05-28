//go:build darwin

package main

import (
	"fmt"
	"os"
)

func cmdAuth(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: dew auth <set|list|remove> [args]\n")
		return nil
	}

	switch args[0] {
	case "set":
		return cmdAuthSet(args[1:])
	case "list":
		return cmdAuthList()
	case "remove":
		return cmdAuthRemove(args[1:])
	default:
		return fmt.Errorf("unknown auth subcommand %q (use: set, list, remove)", args[0])
	}
}

func cmdAuthSet(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: dew auth set <host> <token>")
	}
	host := args[0]
	token := args[1]

	if err := saveCredentials(host, token); err != nil {
		return err
	}
	masked := token[:min(10, len(token))] + "..." + token[max(0, len(token)-4):]
	fmt.Fprintf(os.Stderr, "  Saved: %s → %s\n", host, masked)
	return nil
}

func cmdAuthList() error {
	store := loadCredentialStore()
	if len(store.Credentials) == 0 {
		fmt.Println("No saved credentials.")
		return nil
	}
	fmt.Printf("%-30s %s\n", "HOST", "TOKEN")
	for host, token := range store.Credentials {
		masked := token[:min(10, len(token))] + "..." + token[max(0, len(token)-4):]
		fmt.Printf("%-30s %s\n", host, masked)
	}
	return nil
}

func cmdAuthRemove(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew auth remove <host>")
	}
	host := args[0]
	removeCredentials(host)
	fmt.Fprintf(os.Stderr, "  Removed: %s\n", host)
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
