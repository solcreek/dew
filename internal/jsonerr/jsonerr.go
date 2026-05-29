// Package jsonerr provides structured error output for --json mode.
// When --json is set, errors are written to stdout as JSON instead of
// unstructured stderr text. This lets agents parse one stream.
package jsonerr

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type ErrorResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

func Exit(err error, jsonMode bool) {
	if jsonMode {
		code := classifyError(err)
		json.NewEncoder(os.Stdout).Encode(ErrorResponse{
			OK:    false,
			Error: err.Error(),
			Code:  code,
		})
	} else {
		fmt.Fprintf(os.Stderr, "dew: %v\n", err)
	}
	os.Exit(1)
}

func classifyError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid token"):
		return "auth_error"
	case strings.Contains(msg, "not found"):
		return "not_found"
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host"):
		return "network_error"
	case strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "deploy in progress"):
		return "conflict"
	case strings.Contains(msg, "invalid") || strings.Contains(msg, "required"):
		return "validation_error"
	default:
		return "error"
	}
}
