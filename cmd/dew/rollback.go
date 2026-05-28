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

	"github.com/solcreek/dew/internal/progress"
)

func cmdRollback(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: dew rollback <target> <app>")
	}

	target := args[0]
	app := args[1]

	token, err := loadDeployToken(target)
	if err != nil {
		return err
	}
	endpoint := resolveEndpoint(target)

	sp := progress.New()
	sp.Step(fmt.Sprintf("Rolling back %s", app))

	url := fmt.Sprintf("%s/v1/apps/%s/rollback", endpoint, app)
	req, _ := http.NewRequest("POST", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sp.Fail("request failed")
		return fmt.Errorf("rollback: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		sp.Fail(fmt.Sprintf("HTTP %d", resp.StatusCode))
		return fmt.Errorf("rollback failed: %d %s", resp.StatusCode, body)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	sp.Done(fmt.Sprintf("%s rolled back", app))

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(result)
	}
	return nil
}

func init() {
	_ = strings.TrimSpace // avoid unused import
}
