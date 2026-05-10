// Package cliio holds output-shaping helpers for the
// `satellites-client` CLI: auto-JSON detection (when stdout is not a
// tty, every verb emits JSON) and a typed Render helper that picks
// JSON vs human-readable per the resolved mode. Per the convention
// adopted in docs/cli-primary-design.md §3.
package cliio

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Mode names the resolved output mode for a single CLI invocation.
type Mode int

const (
	// ModeAuto means the CLI inspects stdout: tty → human-readable,
	// pipe → JSON. This is the default; --json flips it forcibly to
	// ModeJSON even on a tty.
	ModeAuto Mode = iota
	// ModeJSON forces JSON output regardless of stdout's nature.
	ModeJSON
)

// IsTerminal reports whether the supplied file descriptor is a tty.
// Wraps golang.org/x/term so callers don't pull the dep directly.
func IsTerminal(fd uintptr) bool {
	return term.IsTerminal(int(fd))
}

// Resolve picks the effective Mode given the user's --json flag and
// the stdout-is-tty signal. The contract: if --json is set OR stdout
// is not a tty, emit JSON; otherwise emit the human-readable form.
func Resolve(jsonFlag bool, stdout *os.File) Mode {
	if jsonFlag {
		return ModeJSON
	}
	if !IsTerminal(stdout.Fd()) {
		return ModeJSON
	}
	return ModeAuto
}

// IsJSON is a convenience: a Mode that should emit JSON.
func (m Mode) IsJSON() bool {
	return m == ModeJSON
}

// RenderJSON marshals payload to w. Uses encoding/json's compact
// form by default; --compact (per the CLI's persistent flag) is
// applied at the noun level since field selection is per-noun.
func RenderJSON(w io.Writer, payload any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(payload)
}

// RenderJSONIndent marshals payload to w with two-space indent.
// Used by ModeAuto on a tty when no human-form template exists for
// the noun yet.
func RenderJSONIndent(w io.Writer, payload any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// PrintError writes the supplied error to stderr in the style
// adopted by docs/cli-primary-design.md §3:
//
//	error: <verb>: <message>
//
// `verb` is the noun + verb tuple ("story get", "ledger append", …).
// Empty verb omits that segment.
func PrintError(verb string, err error) {
	if err == nil {
		return
	}
	if verb == "" {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "error: %s: %s\n", verb, err)
}
