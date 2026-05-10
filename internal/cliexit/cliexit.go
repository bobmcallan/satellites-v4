// Package cliexit holds the typed exit-code mapping the
// `satellites-client` CLI emits and the agent worker (order:06)
// reads when shelling out to the CLI. One source of truth so
// agent-side code can branch on a typed value rather than
// string-matching exit messages.
//
// Codes match the cli-printing-press convention adopted in
// docs/cli-primary-design.md §3:
//
//	0 OK         — success
//	2 Usage      — argument / flag error
//	3 NotFound   — id resolution failed
//	4 Auth       — bearer expired / scope mismatch
//	5 Server     — server-side / API failure (incl. not-implemented stubs)
//	7 RateLimit  — 429 from satellites-server (reserved; no rate limiter today)
//
// Code 1 is intentionally NOT used. Go's runtime returns 1 on a
// panic; reserving 1 for that case makes a CLI panic distinguishable
// from any deliberate error path.
package cliexit

import (
	"errors"
	"fmt"
)

// Code is the typed exit code surfaced to the OS.
type Code int

const (
	OK        Code = 0
	Usage     Code = 2
	NotFound  Code = 3
	Auth      Code = 4
	Server    Code = 5
	RateLimit Code = 7
)

// Error wraps an underlying error with a typed exit code. Cobra
// RunE handlers return *Error; the root command's exit hook
// extracts the code via errors.As.
type Error struct {
	Code Code
	Err  error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap returns the underlying error so errors.Is / errors.As work
// across Code-bearing wrappers.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Wrap returns an Error carrying the given code. nil err returns nil.
func Wrap(code Code, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Err: err}
}

// Newf constructs a new Error with the given code and formatted
// message. Use for the typical "error: <verb>: <field>: <reason>"
// shape adopted in docs/cli-primary-design.md §3.
func Newf(code Code, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// NotImplemented returns a Server-coded error for stub subcommands
// that order:04+ will fill in. The verb name is included so the
// error message is self-describing.
func NotImplemented(verb string) error {
	return &Error{
		Code: Server,
		Err:  fmt.Errorf("not yet implemented: %s (orders 04-05 deliver)", verb),
	}
}

// Resolve extracts the typed exit code from err. nil err returns
// OK; err with no embedded *Error returns Server (the catch-all
// for server-side / unexpected errors).
func Resolve(err error) Code {
	if err == nil {
		return OK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Server
}
