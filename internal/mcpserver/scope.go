package mcpserver

import "context"

// ctxKey is the package-local context-key type. The auth-pipeline ctx
// key (CallerIdentity) lives in internal/auth (sty_f2d342e2 relocation);
// scopes here cover transport-state values that never crossed into auth.
type ctxKey int

// scopedProjectKey is the context key under which Server.ServeHTTP stores
// the URL-scoped project_id (when the MCP request was made against a URL
// like `/mcp?project_id=proj_xxx`). Per-request transport state, not
// caller identity (which lives on auth's ctx key).
const scopedProjectKey ctxKey = 1

// requestBaseURLKey is the context key under which Server.ServeHTTP
// stores the externally-visible base URL the caller used to reach this
// MCP endpoint (`<scheme>://<host>`). Tool handlers read it via
// requestBaseURLFrom so derived MCP URLs in responses match the URL
// the user is already connected to — V3 parity, no env var required.
const requestBaseURLKey ctxKey = 2

// withRequestBaseURL stores the inbound request's reconstructed base
// URL on the context. Empty input is a no-op.
func withRequestBaseURL(ctx context.Context, baseURL string) context.Context {
	if baseURL == "" {
		return ctx
	}
	return context.WithValue(ctx, requestBaseURLKey, baseURL)
}

// requestBaseURLFrom returns the base URL attached by ServeHTTP, or ""
// when the call did not originate from an HTTP transport (e.g. unit
// tests calling handlers directly).
func requestBaseURLFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestBaseURLKey).(string)
	return v
}

// withScopedProjectID returns a child context carrying id as the
// URL-scoped project. Empty id is a no-op.
func withScopedProjectID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, scopedProjectKey, id)
}

// ScopedProjectIDFrom returns the URL-scoped project_id attached by
// Server.ServeHTTP, or "" when the request was made against the
// unscoped `/mcp` URL. Tools that take project_id as a parameter must
// reject mismatches via enforceScopedProject.
func ScopedProjectIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(scopedProjectKey).(string)
	return v
}

// enforceScopedProject checks that candidate is compatible with the
// URL-scoped project_id. Returns the effective project_id to use:
//   - "" scope, "" candidate → "" (caller must supply or fall back)
//   - "" scope, "X" candidate → "X" (no scoping; passthrough)
//   - "X" scope, "" candidate → "X" (scope wins; tool didn't supply one)
//   - "X" scope, "X" candidate → "X" (match)
//   - "X" scope, "Y" candidate → "", false (mismatch — caller should reject)
func enforceScopedProject(ctx context.Context, candidate string) (string, bool) {
	scoped := ScopedProjectIDFrom(ctx)
	if scoped == "" {
		return candidate, true
	}
	if candidate == "" || candidate == scoped {
		return scoped, true
	}
	return "", false
}
