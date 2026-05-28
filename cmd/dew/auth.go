//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	path := filepath.Join(dewConfigDir(), "credentials")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No saved credentials.")
			return nil
		}
		return err
	}

	fmt.Printf("%-30s %s\n", "HOST", "TOKEN")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			token := fields[1]
			masked := token[:min(10, len(token))] + "..." + token[max(0, len(token)-4):]
			fmt.Printf("%-30s %s\n", fields[0], masked)
		}
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
