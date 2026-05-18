// task_run_async.go — sty_57210d2a (C2) async dispatch wire.
//
// runTaskAsync POSTs /v1/enqueue over the local serve-daemon unix
// socket and prints the daemon's JSON response to stdout. The
// dispatched subprocess lives in the daemon process; this CLI
// process exits as soon as the enqueue ack arrives.
//
// Per pr_mcp_cli_shared_path the wire is unix-socket only — no MCP
// verb, no substrate-domain import (the import-graph layering test in
// task_run_async_test.go enforces this). The request/response shapes
// are imported from internal/clientdaemon so the daemon-side schema
// is the single source of truth.
//
// Per pr_no_unrequested_compat C4 (sty_ad40584f) flips async to the
// CLI default and removes the `--async` flag; the unix-socket wire
// shape is unchanged. Operator surface: bare `task run <id>` is async
// (push-enqueue into the daemon); `--sync` re-opts into the blocking
// shape. The `--socket` override is retained. No --mode selector, no
// env-var indirection, no auto-spawn of the daemon (AC3 is explicit:
// the operator decides whether the daemon runs).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bobmcallan/satellites/internal/clientdaemon"
	"github.com/bobmcallan/satellites/internal/cliexit"
)

// asyncEnqueueTimeout caps the total round-trip to the daemon. The
// daemon's Enqueue is an in-memory mutex + append (sub-100ms on a
// healthy host); 5s leaves slack for cold-start sockets while still
// keeping AC1's `elapsed < 1s` assertion comfortable.
const asyncEnqueueTimeout = 5 * time.Second

// daemonNotRunningMsg is the exact stderr string AC3 binds the
// reviewer to. The em-dash is U+2014; the surrounding text is the
// operator's next-step prompt. Reviewers byte-compare this string —
// do not edit casually.
const daemonNotRunningMsg = "daemon not running — start it with: satellites-client serve start"

// runTaskAsync is the default branch task_run.go takes when `--sync`
// is NOT set (post-C4 default-flip). POSTs /v1/enqueue, prints
// {task_id, daemon_pid, queue_position} to stdout, returns nil on
// 200 → exit 0.
func runTaskAsync(cmd *cobra.Command, taskID string) error {
	socketPath, _ := cmd.Flags().GetString("socket")

	resp, err := postEnqueue(cmd.Context(), socketPath, taskID)
	if err != nil {
		return err
	}

	body, mErr := json.Marshal(resp)
	if mErr != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: marshal response: %w", taskID, mErr))
	}
	out := cmd.OutOrStdout()
	if out == nil {
		out = os.Stdout
	}
	if _, wErr := out.Write(append(body, '\n')); wErr != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: write stdout: %w", taskID, wErr))
	}
	return nil
}

// postEnqueue is the unix-socket POST helper. Returns the decoded
// EnqueueResponse on 200; maps connect-refused / file-not-found to
// the AC3 daemon-not-running cliexit error.
func postEnqueue(ctx context.Context, socketPath, taskID string) (clientdaemon.EnqueueResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, asyncEnqueueTimeout)
	defer dialCancel()

	cli := &http.Client{
		Transport: &http.Transport{
			DialContext: func(dctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(dctx, "unix", socketPath)
			},
		},
		Timeout: asyncEnqueueTimeout,
	}

	reqBody, _ := json.Marshal(clientdaemon.EnqueueRequest{TaskID: taskID})
	req, rErr := http.NewRequestWithContext(dialCtx, http.MethodPost, "http://daemon/v1/enqueue", bytes.NewReader(reqBody))
	if rErr != nil {
		return clientdaemon.EnqueueResponse{}, cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: build request: %w", taskID, rErr))
	}
	req.Header.Set("Content-Type", "application/json")

	httpResp, doErr := cli.Do(req)
	if doErr != nil {
		if isDaemonAbsent(doErr) {
			return clientdaemon.EnqueueResponse{}, cliexit.Newf(cliexit.Server, "%s", daemonNotRunningMsg)
		}
		return clientdaemon.EnqueueResponse{}, cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: POST /v1/enqueue: %w", taskID, doErr))
	}
	defer httpResp.Body.Close()

	respBody, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode != http.StatusOK {
		return clientdaemon.EnqueueResponse{}, cliexit.Newf(cliexit.Server, "task run %s: daemon returned HTTP %d: %s", taskID, httpResp.StatusCode, bytes.TrimSpace(respBody))
	}
	var out clientdaemon.EnqueueResponse
	if uErr := json.Unmarshal(respBody, &out); uErr != nil {
		return clientdaemon.EnqueueResponse{}, cliexit.Wrap(cliexit.Server, fmt.Errorf("task run %s: decode response: %w", taskID, uErr))
	}
	return out, nil
}

// isDaemonAbsent reports whether err indicates the daemon's socket
// is not listening — either the socket file is absent (fs.ErrNotExist)
// or the kernel refused the connect (ECONNREFUSED). Both surface as
// the operator-facing "daemon not running" message.
func isDaemonAbsent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return false
}
