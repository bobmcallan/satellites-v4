// mcp_transport.go holds the residual MCP `tools/call` JSON-RPC
// transport that hotpath.go's runners (fetchTaskWalk, fetchStoryRaw,
// appendEvidence, closeTaskSuccess) still depend on. Splitting it out
// of client_claude.go lets sty_74e67353 work#1 migrate the dispatcher's
// pre-spawn fetches + appendExecuteEvidence onto internal/cliremote
// (the shared /api/v1 surface — pr_mcp_cli_shared_path) without
// touching hotpath.go. work#2 finishes the migration by rewriting
// the hot-path runners onto cliremote and deleting this file
// entirely (pr_no_unrequested_compat — delete + migrate, no alias).
//
// The fields claudeClient.http + claudeClient.rpcID stay on the struct
// (declared in client_claude.go) because callTool is a method on
// *claudeClient that reads them; their lifetime ends with this file.

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

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
// returns a `result.content[].text` payload that callTool surfaces as
// raw bytes for caller-specific decoding.
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
// status, and JSON-RPC error envelopes. Retained for hotpath.go's
// runners until work#2 migrates them onto internal/cliremote.
func (c *claudeClient) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	id := c.rpcID.Add(1)
	body, err := json.Marshal(jsonrpcReq{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/call",
		Params:  jsonrpcCallReq{Name: name, Arguments: args},
	})
	if err != nil {
		return "", fmt.Errorf("callTool: marshal %s: %w", name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.SpawnMCPURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("callTool: new request %s: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if tok := c.cfg.AuthToken; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("callTool: %s: %w", name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("callTool: read %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("callTool: %s: status %d body %s", name, resp.StatusCode, string(raw))
	}
	var parsed jsonrpcResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("callTool: decode %s: %w", name, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("callTool: %s: rpc error %d: %s", name, parsed.Error.Code, parsed.Error.Message)
	}
	if parsed.Result == nil || len(parsed.Result.Content) == 0 {
		return "", nil
	}
	return parsed.Result.Content[0].Text, nil
}
