// chain.go — sty_4fb2d985 `chain` CLI subcommand.
//
// `satellites-client chain {status,advance,run}` is the operator-side
// replacement for `.satellites/route_epic.sh`. The substrate's
// `chain_*` MCP verbs return the snapshot + next dispatchable id;
// this CLI wraps `postEnqueue` (the local daemon's unix-socket
// helper) as the dispatch hook so a single command — `chain run` —
// drives a story end-to-end without operator polling.
//
// Layering: CLI talks to the substrate over MCP/HTTP (callRemote) AND
// to the local daemon over unix-socket (postEnqueue). The two
// transports cannot be collapsed: substrate is shared and does not
// own the daemon socket; the daemon is local and does not own the
// substrate's data plane.
//
// AC3 / R3 surface contract:
//
//   - `status` is a thin pass-through to `chain_status`.
//   - `advance` calls `chain_advance` for the next dispatchable id,
//     then `postEnqueue` to dispatch. Daemon absent → exit 5 with
//     `daemonNotRunningMsg`.
//   - `run` loops the above; one JSON heartbeat per iteration on
//     stdout; exit 0 on terminal, exit 7 on timeout.
//   - Daemon is NOT auto-spawned. Same operator-decision contract as
//     `task run` (sty_5aa20f1b AC3 / sty_ad40584f C4).

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/clientdaemon"
	"github.com/bobmcallan/satellites/internal/cliexit"
)

// chainExitTimeout is the cliexit code `chain run --timeout` uses
// when the deadline elapses. AC3 binds the value to 7 so operator
// scripts can branch on `$?` without parsing stdout.
const chainExitTimeout = 7

// defaultChainPoll is the loop cadence when --poll is not supplied.
// Mirrors client.DefaultChainPollInterval; kept as a local constant
// so cobra can format the flag default without importing the typed
// surface's constant directly into the flag table.
const defaultChainPoll = 30 * time.Second

// registerChainNoun attaches the `chain` cobra group + verbs.
func registerChainNoun(root *cobra.Command) {
	noun := nounStub("chain", "Chain — substrate-side router (replaces .satellites/route_epic.sh).")
	noun.AddCommand(
		newChainStatusCmd(),
		newChainAdvanceCmd(),
		newChainRunCmd(),
	)
	root.AddCommand(noun)
}

// newChainStatusCmd registers `chain status --story-id <id>`. Thin
// pass-through to the `chain_status` MCP verb.
func newChainStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Return the chain snapshot (story header, per-phase live task, next dispatchable id, terminal flag, anomalies).",
		RunE: readHandler("chain_status", func(cmd *cobra.Command, args []string) (any, error) {
			storyID, _ := cmd.Flags().GetString("story-id")
			if storyID == "" {
				return nil, cliexit.Newf(cliexit.Usage, "chain status: --story-id is required")
			}
			return map[string]any{"story_id": storyID}, nil
		}),
	}
	c.Flags().String("story-id", "", "Story to inspect (required).")
	_ = c.MarkFlagRequired("story-id")
	return c
}

// newChainAdvanceCmd registers `chain advance --story-id <id>`. One
// shot: read the snapshot, dispatch the next dispatchable id via the
// local daemon, print the combined ChainAdvanceOutput payload.
func newChainAdvanceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "advance",
		Short: "Compute the next dispatchable task on the chain + push-enqueue it via the local daemon.",
		RunE:  runChainAdvanceCmd,
	}
	c.Flags().String("story-id", "", "Story whose chain to advance (required).")
	c.Flags().String("socket", clientdaemon.DefaultSocketPath(), "Daemon unix socket path.")
	c.Flags().Bool("dry-run", false, "Print the next dispatchable id without contacting the daemon.")
	_ = c.MarkFlagRequired("story-id")
	return c
}

// newChainRunCmd registers `chain run --story-id <id> [--poll N]
// [--timeout N] [--socket <path>]`. Loops `chain advance` + sleep
// until terminal or timeout; emits one JSON heartbeat per iteration
// on stdout.
func newChainRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "run",
		Short: "Loop chain advance + poll until the chain reaches a terminal state.",
		RunE:  runChainRunCmd,
	}
	c.Flags().String("story-id", "", "Story whose chain to run (required).")
	c.Flags().String("socket", clientdaemon.DefaultSocketPath(), "Daemon unix socket path.")
	c.Flags().Duration("poll", defaultChainPoll, "Loop poll cadence.")
	c.Flags().Duration("timeout", 0, "Hard deadline; 0 = no timeout.")
	_ = c.MarkFlagRequired("story-id")
	return c
}

// runChainAdvanceCmd is the `chain advance` body. Calls MCP
// `chain_advance` for the next dispatchable id, then `postEnqueue`
// to dispatch. Returns a single ChainAdvanceOutput on stdout.
func runChainAdvanceCmd(cmd *cobra.Command, args []string) error {
	storyID, _ := cmd.Flags().GetString("story-id")
	if storyID == "" {
		return cliexit.Newf(cliexit.Usage, "chain advance: --story-id is required")
	}
	socketPath, _ := cmd.Flags().GetString("socket")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	adv, err := fetchChainAdvance(cmd, storyID)
	if err != nil {
		return err
	}
	if dryRun || adv.Terminal || adv.NextTaskID == "" || adv.Detail != "" {
		return emitChainAdvance(cmd, adv)
	}
	resp, err := postEnqueue(cmd.Context(), socketPath, adv.NextTaskID)
	if err != nil {
		return err
	}
	adv.Dispatched = true
	adv.Ack = client.DispatchAck{
		Dispatched:    true,
		DaemonPID:     resp.DaemonPID,
		QueuePosition: resp.QueuePosition,
	}
	return emitChainAdvance(cmd, adv)
}

// runChainRunCmd is the `chain run` loop body. Emits one JSON
// heartbeat per iteration on stdout, then a final ChainRunOutput on
// terminal / timeout. Exit codes: 0 terminal, 5 daemon absent, 7
// timeout.
func runChainRunCmd(cmd *cobra.Command, args []string) error {
	storyID, _ := cmd.Flags().GetString("story-id")
	if storyID == "" {
		return cliexit.Newf(cliexit.Usage, "chain run: --story-id is required")
	}
	socketPath, _ := cmd.Flags().GetString("socket")
	poll, _ := cmd.Flags().GetDuration("poll")
	if poll <= 0 {
		poll = defaultChainPoll
	}
	timeout, _ := cmd.Flags().GetDuration("timeout")

	out := cmd.OutOrStdout()
	if out == nil {
		out = os.Stdout
	}

	runOut := client.ChainRunOutput{StoryID: storyID}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			runOut.TerminalState = client.TerminalStateTimeout
			_ = emitChainRunFinal(out, runOut)
			return cliexit.Newf(chainExitTimeout, "chain run %s: timeout after %s", storyID, timeout)
		}
		runOut.Iterations++

		adv, err := fetchChainAdvance(cmd, storyID)
		if err != nil {
			return err
		}
		// Heartbeat — one JSON line per iteration on stdout. AC3 binds
		// the shape so `chain run &` shows up in tail.
		_ = emitChainAdvanceTo(out, adv)

		dispatched := false
		if !adv.Terminal && adv.NextTaskID != "" && adv.Detail == "" {
			resp, perr := postEnqueue(cmd.Context(), socketPath, adv.NextTaskID)
			if perr != nil {
				return perr
			}
			dispatched = true
			adv.Dispatched = true
			adv.Ack = client.DispatchAck{
				Dispatched:    true,
				DaemonPID:     resp.DaemonPID,
				QueuePosition: resp.QueuePosition,
			}
			runOut.Dispatched = append(runOut.Dispatched, adv.NextTaskID)
		}
		_ = dispatched

		if adv.Terminal {
			// Distinguish story-closed from no-dispatchable by
			// re-reading the snapshot.
			st, serr := fetchChainStatus(cmd, storyID)
			if serr != nil {
				return serr
			}
			if st.StoryStatus == "done" {
				runOut.TerminalState = client.TerminalStateStoryClosed
			} else {
				runOut.TerminalState = client.TerminalStateNoDispatchable
			}
			return emitChainRunFinal(out, runOut)
		}

		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(poll):
		}
	}
}

// fetchChainAdvance issues one `chain_advance` MCP call and returns
// the typed payload.
func fetchChainAdvance(cmd *cobra.Command, storyID string) (client.ChainAdvanceOutput, error) {
	raw, err := callRemote(cmd, "chain_advance", map[string]any{"story_id": storyID})
	if err != nil {
		return client.ChainAdvanceOutput{}, err
	}
	var out client.ChainAdvanceOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return client.ChainAdvanceOutput{}, cliexit.Wrap(cliexit.Server, fmt.Errorf("chain advance: decode response: %w", err))
	}
	return out, nil
}

// fetchChainStatus issues one `chain_status` MCP call and returns
// the typed payload. Used by `chain run` on the terminal iteration
// to distinguish story-closed from no-dispatchable.
func fetchChainStatus(cmd *cobra.Command, storyID string) (client.ChainStatusOutput, error) {
	raw, err := callRemote(cmd, "chain_status", map[string]any{"story_id": storyID})
	if err != nil {
		return client.ChainStatusOutput{}, err
	}
	var out client.ChainStatusOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return client.ChainStatusOutput{}, cliexit.Wrap(cliexit.Server, fmt.Errorf("chain run: decode chain_status: %w", err))
	}
	return out, nil
}

// emitChainAdvance writes the payload to cmd.OutOrStdout(); auto-JSON
// per the noun-table contract.
func emitChainAdvance(cmd *cobra.Command, adv client.ChainAdvanceOutput) error {
	out := cmd.OutOrStdout()
	if out == nil {
		out = os.Stdout
	}
	return emitChainAdvanceTo(out, adv)
}

// emitChainAdvanceTo is the io.Writer-bound variant used by `chain
// run`'s heartbeat loop.
func emitChainAdvanceTo(w io.Writer, adv client.ChainAdvanceOutput) error {
	body, err := json.Marshal(adv)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("chain advance: marshal: %w", err))
	}
	if _, err := w.Write(append(body, '\n')); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("chain advance: write stdout: %w", err))
	}
	return nil
}

// emitChainRunFinal writes the terminal ChainRunOutput payload.
func emitChainRunFinal(w io.Writer, out client.ChainRunOutput) error {
	body, err := json.Marshal(out)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("chain run: marshal terminal: %w", err))
	}
	if _, err := w.Write(append(body, '\n')); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("chain run: write terminal: %w", err))
	}
	return nil
}
