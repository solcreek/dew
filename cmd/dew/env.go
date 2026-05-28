//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func cmdEnv(args []string) error {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: dew env <set|list|remove> <target> [args]\n")
		return nil
	}

	sub := args[0]
	target := args[1]

	token, err := loadDeployToken(target)
	if err != nil {
		return err
	}
	endpoint := resolveEndpoint(target)

	switch sub {
	case "set":
		return cmdEnvSet(endpoint, token, args[2:])
	case "list":
		return cmdEnvList(endpoint, token, args[2:])
	case "remove":
		return cmdEnvRemove(endpoint, token, args[2:])
	default:
		return fmt.Errorf("unknown env subcommand %q (use: set, list, remove)", sub)
	}
}

func cmdEnvSet(endpoint, token string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: dew env set <target> <APP> KEY=VALUE [KEY=VALUE...]")
	}
	app := args[0]
	vars := make(map[string]string)
	for _, kv := range args[1:] {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid env var %q (use KEY=VALUE)", kv)
		}
		vars[parts[0]] = parts[1]
	}

	body, _ := json.Marshal(vars)
	url := fmt.Sprintf("%s/v1/apps/%s/env", endpoint, app)
	req, _ := http.NewRequest("PUT", url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("env set: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("env set failed: %d %s", resp.StatusCode, respBody)
	}

	for k := range vars {
		fmt.Fprintf(os.Stderr, "  %s = <set>\n", k)
	}
	return nil
}

func cmdEnvList(endpoint, token string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: dew env list <target> <APP>")
	}
	app := args[0]

	url := fmt.Sprintf("%s/v1/apps/%s/env", endpoint, app)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("env list: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("env list failed: %d %s", resp.StatusCode, respBody)
	}

	var keys []string
	json.NewDecoder(resp.Body).Decode(&keys)
	if len(keys) == 0 {
		fmt.Println("No environment variables set.")
		return nil
	}
	for _, k := range keys {
		fmt.Printf("  %s = <set>\n", k)
	}
	return nil
}

func cmdEnvRemove(endpoint, token string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: dew env remove <target> <APP> <KEY>")
	}
	app := args[0]
	key := args[1]

	url := fmt.Sprintf("%s/v1/apps/%s/env/%s", endpoint, app, key)
	req, _ := http.NewRequest("DELETE", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("env remove: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("env remove failed: %d %s", resp.StatusCode, respBody)
	}

	fmt.Fprintf(os.Stderr, "  Removed: %s\n", key)
	return nil
}
