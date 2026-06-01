// Package dewerr is dew's machine-readable error contract.
//
// Errors that cross the dew↔caller boundary (CLI exit code, --json
// output, daemon IPC) carry a stable Code that lets agents and shell
// scripts make consistent retry decisions without parsing English.
//
// The exported codes are a public contract — see docs/exit-codes.md
// for the policy (codes never get re-mapped; new categories get a
// new code from the reserved range).
//
// This package lives under pkg/ rather than internal/ so the future
// agent SDK can `import dewerr` and pattern-match on errors.As
// without depending on the dew binary.
package dewerr

import (
	"errors"
	"fmt"
	"time"
)

// Code is the typed exit code carried by Error. Values double as the
// process exit code dew returns to the shell — so 0–127 are usable
// (POSIX leaves 128+ for signal deaths), 124–127 are claimed by
// `timeout(1)` and the shell's chroot tradition (don't use), and 1
// is the unclassified fallback.
//
// Layout:
//
//	0       success (never an Error)
//	1       generic            (untyped failure, fallback)
//	2       usage              (invalid flag, missing arg)
//	100     auth               (token expired, unauthorized)
//	101     network            (DNS, connection refused, transient)
//	102     not_found          (resource doesn't exist)
//	103     conflict           (state mismatch, precondition failed)
//	104     timeout            (operation exceeded its deadline)
//	105     unavailable        (rate-limited, disk full, resource exhausted)
//	106–119 RESERVED — append-only, never re-map
//	120–127 FORBIDDEN — timeout(1) + chroot tradition
//	128–255 POSIX signal range
type Code int

const (
	CodeGeneric     Code = 1
	CodeUsage       Code = 2
	CodeAuth        Code = 100
	CodeNetwork     Code = 101
	CodeNotFound    Code = 102
	CodeConflict    Code = 103
	CodeTimeout     Code = 104
	CodeUnavailable Code = 105
)

// Slug returns the stable string identifier used in JSON output.
// Slugs are part of the contract: they only get added, never renamed.
func (c Code) Slug() string {
	switch c {
	case CodeGeneric:
		return "error"
	case CodeUsage:
		return "usage"
	case CodeAuth:
		return "auth"
	case CodeNetwork:
		return "network"
	case CodeNotFound:
		return "not_found"
	case CodeConflict:
		return "conflict"
	case CodeTimeout:
		return "timeout"
	case CodeUnavailable:
		return "unavailable"
	default:
		return "error"
	}
}

// RetryableByDefault reports whether a code typically indicates a
// transient failure an agent can retry. Callers can override per
// error via Error.Retryable.
func (c Code) RetryableByDefault() bool {
	switch c {
	case CodeNetwork, CodeTimeout, CodeUnavailable:
		return true
	default:
		return false
	}
}

// Error is the typed error dew uses internally and emits in --json
// mode. Wrap a cause via Cause; the chain participates in errors.Is
// / errors.As through the Unwrap method below.
type Error struct {
	Code       Code
	Message    string
	Cause      error
	Retryable  bool          // override for Code.RetryableByDefault()
	RetryAfter time.Duration // 0 = no specific delay suggested
	Hint       map[string]string
}

// New constructs a typed error.
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg, Retryable: code.RetryableByDefault()}
}

// Newf constructs a typed error with fmt.Sprintf.
func Newf(code Code, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// Wrap wraps an underlying error with a code. If err is already a
// *Error, the inner code is preserved (the outer is treated as
// additional context); use Annotate if you want to override.
func Wrap(err error, code Code, msg string) *Error {
	if err == nil {
		return nil
	}
	var inner *Error
	if errors.As(err, &inner) {
		// Preserve the inner classification; layer msg on top.
		return &Error{
			Code:       inner.Code,
			Message:    msg + ": " + inner.Message,
			Cause:      inner.Cause,
			Retryable:  inner.Retryable,
			RetryAfter: inner.RetryAfter,
			Hint:       inner.Hint,
		}
	}
	return &Error{Code: code, Message: msg, Cause: err, Retryable: code.RetryableByDefault()}
}

// Wrapf is Wrap with fmt.Sprintf.
func Wrapf(err error, code Code, format string, args ...any) *Error {
	return Wrap(err, code, fmt.Sprintf(format, args...))
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

// Unwrap participates in errors.Is / errors.As chains.
func (e *Error) Unwrap() error { return e.Cause }

// CodeOf returns the Code associated with err, walking the chain via
// errors.As. Untyped errors return CodeGeneric (1) so callers always
// have a usable code.
func CodeOf(err error) Code {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeGeneric
}

// Retryable reports whether err signals a transient condition the
// caller can retry. Falls back to CodeOf(err).RetryableByDefault().
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return false
}
