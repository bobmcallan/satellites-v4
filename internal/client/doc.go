// Package client is the typed, transport-free seam between satellites'
// substrate stores and any caller surface — the MCP wire layer in
// internal/mcpserver, the CLI in cmd/satellites-client (order:03+),
// and any future caller (HTTP, Go SDK, embedded test harness).
//
// # Architectural contract
//
//  1. No mcpgo imports. The package MUST NOT depend on
//     github.com/mark3labs/mcp-go. Any wire-format concern (JSON-RPC,
//     CallToolRequest, CallToolResult) lives in the caller. This is
//     enforced by code review; CI may add a grep gate.
//
//  2. No HTTP, no JSON-RPC. Each method takes typed Go inputs and
//     returns typed Go outputs. Errors are Go errors, not wire
//     envelopes — the caller maps them to its preferred shape.
//
//  3. Per-method shape: func (c *Client) <Verb>(ctx context.Context,
//     caller Caller, in <Verb>Input) (<Verb>Output, error). The
//     Caller carries the user identity + workspace memberships; the
//     Input carries the verb's positional/optional arguments.
//
//  4. Tenancy resolution at the wire boundary. Membership lookup,
//     bearer parsing, and session resolution stay in the caller (which
//     knows about request headers); the typed methods receive a
//     fully-populated Caller and apply membership-driven filtering
//     inside the typed methods.
//
// # Migration sequence (cli-primary order:02)
//
// The package is being filled in noun-by-noun per the order:02 plan
// (sty_66c02002). Each slice carves business logic out of the
// matching internal/mcpserver/*.go handler and leaves the handler as
// a thin adapter (≤25 lines) that unmarshals the request, calls the
// typed method, and marshals the response.
//
// Refactor invariant: tests in internal/mcpserver/*_test.go pass
// unmodified. They are the proof of behaviour preservation.
//
// # See also
//
// - docs/cli-primary-design.md — verb inventory + CLI catalogue.
// - epic sty_c01b23b5 — the full migration arc.
package client
