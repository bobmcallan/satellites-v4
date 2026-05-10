// Command satellites-client is the operator + agent CLI for the
// satellites substrate. Cobra-based; one binary that surfaces every
// noun×verb in docs/cli-primary-design.md §2 as a subcommand.
//
// This file is the order:03 scaffold (sty_bfd1dd92) — the binary
// boots, prints --help / --version, and every verb stub returns
// "not yet implemented" with exit code 5. Orders 04+05 fill the
// stubs in.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/clicred"
	"github.com/bobmcallan/satellites/internal/cliio"
	"github.com/bobmcallan/satellites/internal/config"
)

// PersistentFlags carries the cli-printing-press subset adopted in
// docs/cli-primary-design.md §3. All flags are persistent on the
// root command — every subcommand inherits them.
type PersistentFlags struct {
	JSON     bool
	Compact  bool
	Quiet    bool
	DryRun   bool
	Stdin    bool
	Yes      bool
	NoInput  bool
	NoCache  bool
	NoColor  bool
	Select   string
	Server   string
	Token    string
}

// pf is the package-level holder Cobra binds the persistent-flag
// values into. Subcommands access it directly; PreRunE wires the
// auto-JSON resolution + credential resolution against it.
var pf PersistentFlags

// resolvedToken is populated by PreRunE from the credential chain.
// Subcommands consume it (orders 04+05) when constructing the HTTP
// client.
var resolvedToken string

// resolvedMode is the auto-JSON-aware output mode for this
// invocation. Subcommands consume it when rendering output.
var resolvedMode cliio.Mode

// newRootCmd constructs the satellites-client root command + the
// noun-grouped subcommand tree. Factored out so smoke tests can
// exercise it without process re-entry.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "satellites-client",
		Short:         "Operator + agent CLI for the satellites substrate.",
		Long:          longRootDescription,
		Version:       config.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Auto-JSON resolution: --json flag OR stdout-not-tty
			// flips Mode to ModeJSON. Subcommands read resolvedMode.
			resolvedMode = cliio.Resolve(pf.JSON, os.Stdout)
			return nil
		},
	}

	root.PersistentFlags().BoolVar(&pf.JSON, "json", false, "Force JSON output (auto-on when stdout is not a tty).")
	root.PersistentFlags().BoolVar(&pf.Compact, "compact", false, "Drop output to high-gravity fields per noun.")
	root.PersistentFlags().BoolVar(&pf.Quiet, "quiet", false, "Suppress non-error output.")
	root.PersistentFlags().BoolVar(&pf.DryRun, "dry-run", false, "Print payload that would be sent and exit 0.")
	root.PersistentFlags().BoolVar(&pf.Stdin, "stdin", false, "Read body from stdin where the verb supports it.")
	root.PersistentFlags().BoolVar(&pf.Yes, "yes", false, "Skip confirmation prompts on destructive verbs.")
	root.PersistentFlags().BoolVar(&pf.NoInput, "no-input", false, "Refuse interactive prompts; fail fast.")
	root.PersistentFlags().BoolVar(&pf.NoCache, "no-cache", false, "Bypass any client-side cache (reserved).")
	root.PersistentFlags().BoolVar(&pf.NoColor, "no-color", false, "Strip ANSI escape codes from output.")
	root.PersistentFlags().StringVar(&pf.Select, "select", "", "Project the output down to one field name.")
	root.PersistentFlags().StringVar(&pf.Server, "server", "", "Override the canonical satellites-server URL.")
	root.PersistentFlags().StringVar(&pf.Token, "token", "", "Override the bearer (otherwise SATELLITES_TOKEN / credentials.json).")

	// Per-noun subcommand registration. Each function in the
	// satelitesClientNouns slice attaches its noun group to root.
	for _, register := range satellitesClientNouns {
		register(root)
	}

	// `info` is the surviving advisory verb; group on root for
	// parity with `satellites_info`.
	root.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Return server identity + version metadata.",
		RunE:  func(cmd *cobra.Command, args []string) error { return cliexit.NotImplemented("info") },
	})

	return root
}

// longRootDescription is the prose displayed under
// `satellites-client --help`. Kept short — the catalogue browses
// by `<noun> --help`.
const longRootDescription = `satellites-client is the primary operator and agent surface for the
satellites substrate. Group commands by noun (story, task, ledger,
…); each noun groups its CRUD verbs (get / list / create / …).

Persistent flags follow the cli-printing-press convention:
--json, --compact, --quiet, --dry-run, --stdin, --yes, --no-input,
--no-cache, --no-color, --select, --server, --token.

Auto-JSON: when stdout is not a tty, every verb emits JSON regardless
of --json. Pipe to jq with confidence.

Exit codes: 0 success, 2 usage, 3 not found, 4 auth, 5 server, 7
rate limited.`

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		// Resolve typed exit code; print the error with the standard
		// "error: <verb>: <reason>" shape per docs/cli-primary-design.md §3.
		// Cobra has already written usage info when SilenceUsage is
		// false; we keep silent and surface the error explicitly.
		emitErr(err)
		os.Exit(int(cliexit.Resolve(err)))
	}
}

// emitErr writes the error to stderr in the documented format. Path
// names the noun + verb tuple when the error comes from a Cobra
// RunE handler; otherwise the verb is empty.
func emitErr(err error) {
	if err == nil {
		return
	}
	// Distinguish Cobra usage errors (flag parse failures) from
	// verb-emitted errors. Cobra wraps usage errors in a plain
	// errors.New / pflag.Error; verb errors carry *cliexit.Error.
	var typed *cliexit.Error
	if errors.As(err, &typed) {
		cliio.PrintError("", typed)
		return
	}
	// Cobra usage errors land here; surface them as Usage.
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
}

// resolveCredentials is the helper subcommands call (orders 04+05)
// before making an HTTP call. Returns the bearer or an error keyed
// to cliexit.Auth on failure.
func resolveCredentials() (string, error) {
	if resolvedToken != "" {
		return resolvedToken, nil
	}
	tok, err := clicred.Resolve(pf.Token, pf.Server)
	if err != nil {
		return "", cliexit.Wrap(cliexit.Auth, err)
	}
	resolvedToken = tok
	return tok, nil
}
