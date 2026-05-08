// placeholderClient is the satellites-agent's MCP-backed worker
// Client used by the iter-2 boot path. It issues JSON-RPC tools/call
// requests over HTTP against the configured MCP endpoint and writes a
// kind:agent-noop ledger row in Execute (the placeholder marker for
// the noop execution surface). order:03 (sty_a6250f92) replaces the
// Execute body with a real `claude --print` subprocess run.
//
// The placeholder does not spawn subprocesses. The file deliberately
// has no os/exec import so a sentinel test can assert that constraint.

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/ternarybob/arbor"
)

// placeholderClient implements the worker.Client interface against a
// remote MCP server.
type placeholderClient struct {
	cfg    config.AgentConfig
	logger arbor.ILogger
	http   *http.Client

	// rpcID generates monotonic JSON-RPC request ids.
	rpcID atomic.Int64
}

// NewPlaceholderClient constructs a placeholder Client bound to the
// MCP endpoint named in cfg.MCPURL. The returned client uses an
// http.Client with cfg.ExecuteTimeout/2 as its per-request bound (the
// outer worker contexts already enforce per-call timeouts; this is a
// belt-and-braces transport-level cap).
func NewPlaceholderClient(cfg config.AgentConfig, logger arbor.ILogger) Client {
	timeout := cfg.ExecuteTimeout / 2
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &placeholderClient{
		cfg:    cfg,
		logger: logger,
		http:   &http.Client{Timeout: timeout},
	}
}

// jsonrpcReq is the wire shape sent to the MCP server.
type jsonrpcReq struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Method  string         `json:"method"`
	Params  jsonrpcCallReq `json:"params"`
}

type jsonrpcCallReq struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// jsonrpcResp is the wire shape parsed back. The MCP tools/call shape
// returns a `result.content[].text` payload that the placeholder
// surfaces as raw bytes for caller-specific decoding.
type jsonrpcResp struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Result  *jsonrpcResult `json:"result,omitempty"`
	Error   *jsonrpcErr    `json:"error,omitempty"`
}

type jsonrpcResult struct {
	Content []jsonrpcContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

type jsonrpcContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type jsonrpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// callTool issues an MCP tools/call request and returns the first
// content text on success. A non-nil error covers transport, HTTP-
// status, and JSON-RPC error envelopes.
func (c *placeholderClient) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	id := c.rpcID.Add(1)
	body, err := json.Marshal(jsonrpcReq{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/call",
		Params:  jsonrpcCallReq{Name: name, Arguments: args},
	})
	if err != nil {
		return "", fmt.Errorf("placeholder: marshal %s: %w", name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.MCPURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("placeholder: new request %s: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if tok := c.cfg.AuthToken; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("placeholder: %s: %w", name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("placeholder: read %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("placeholder: %s: status %d body %s", name, resp.StatusCode, string(raw))
	}
	var parsed jsonrpcResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("placeholder: decode %s: %w", name, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("placeholder: %s: rpc error %d: %s", name, parsed.Error.Code, parsed.Error.Message)
	}
	if parsed.Result == nil || len(parsed.Result.Content) == 0 {
		return "", nil
	}
	return parsed.Result.Content[0].Text, nil
}

// Claim implements Client by calling the task_claim MCP verb. A "no
// task" response (server returns null or an empty object) is reported
// as (nil, nil); a transport error is surfaced.
func (c *placeholderClient) Claim(ctx context.Context, workerID string, workspaceIDs []string) (*TaskEnvelope, error) {
	args := map[string]any{}
	if workerID != "" {
		args["worker_id"] = workerID
	}
	// task_claim accepts a single workspace_id; pass the first when
	// supplied. Multi-workspace polling is the WS subscriber's job;
	// the placeholder only narrows when the operator scoped it.
	if len(workspaceIDs) > 0 {
		args["workspace_id"] = workspaceIDs[0]
	}
	text, err := c.callTool(ctx, "task_claim", args)
	if err != nil {
		return nil, err
	}
	if text == "" || text == "null" {
		return nil, nil
	}
	var env struct {
		ID           string `json:"id"`
		WorkspaceID  string `json:"workspace_id"`
		ProjectID    string `json:"project_id"`
		Origin       string `json:"origin"`
		LedgerRootID string `json:"ledger_root_id"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return nil, fmt.Errorf("placeholder: decode task_claim envelope: %w", err)
	}
	if env.ID == "" {
		return nil, nil
	}
	return &TaskEnvelope{
		ID:           env.ID,
		WorkspaceID:  env.WorkspaceID,
		ProjectID:    env.ProjectID,
		Origin:       env.Origin,
		LedgerRootID: env.LedgerRootID,
	}, nil
}

// Execute writes a single kind:agent-noop ledger row tagged with the
// task id and returns OutcomeSuccess. The real `claude --print`
// implementation lands in order:03 alongside NewClaudeClient.
func (c *placeholderClient) Execute(ctx context.Context, task TaskEnvelope) (Outcome, error) {
	args := map[string]any{
		"project_id": task.ProjectID,
		"type":       "evidence",
		"tags":       []string{"kind:agent-noop", "task_id:" + task.ID},
		"content":    fmt.Sprintf("# agent-noop\n\nPlaceholder execution for task %s on workspace %s.\n", task.ID, task.WorkspaceID),
	}
	if _, err := c.callTool(ctx, "ledger_append", args); err != nil {
		return OutcomeFailure, err
	}
	return OutcomeSuccess, nil
}

// Close transitions the task to the supplied outcome via task_update.
func (c *placeholderClient) Close(ctx context.Context, taskID, workerID string, outcome Outcome) error {
	if taskID == "" {
		return errors.New("placeholder: close: empty task_id")
	}
	args := map[string]any{
		"id":      taskID,
		"status":  "closed",
		"outcome": string(outcome),
	}
	_, err := c.callTool(ctx, "task_update", args)
	return err
}

// Heartbeat writes one kind:worker-heartbeat ledger row.
func (c *placeholderClient) Heartbeat(ctx context.Context, workerID, taskID, workspaceID string) error {
	args := map[string]any{
		"type":    "evidence",
		"tags":    []string{"kind:worker-heartbeat", "task_id:" + taskID, "worker_id:" + workerID},
		"content": fmt.Sprintf("# worker-heartbeat\n\nworker=%s task=%s workspace=%s\n", workerID, taskID, workspaceID),
	}
	_, err := c.callTool(ctx, "ledger_append", args)
	return err
}

// Shutdown writes one kind:worker-shutdown ledger row.
func (c *placeholderClient) Shutdown(ctx context.Context, workerID, reason string) error {
	args := map[string]any{
		"type":    "evidence",
		"tags":    []string{"kind:worker-shutdown", "worker_id:" + workerID},
		"content": fmt.Sprintf("# worker-shutdown\n\nworker=%s reason=%s\n", workerID, reason),
	}
	_, err := c.callTool(ctx, "ledger_append", args)
	return err
}
