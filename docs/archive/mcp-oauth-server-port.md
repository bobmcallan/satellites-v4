# MCP OAuth 2.1 Server — Port Plan (V3 → V4)

**Objective.** Bring V4 satellites into compliance with the MCP authorization spec by porting the V3 OAuth 2.1 Authorization Server module so that Claude Code, Claude Desktop, and the MCP SDK can authenticate against `/mcp` via standard discovery + Dynamic Client Registration + PKCE — no operator-pasted bearer tokens, no out-of-band setup.

**Status.** Not started. This document is the porting blueprint.

**Reference implementation:** `/home/bobmc/development/satellites-v3/` (verified in source). V3 satellites already implements the spec; the port is mechanical.

---

## 1. Why port from V3 (decision summary)

The two reachable working OAuth-AS implementations are V3 satellites and vire. Both implement the same RFCs (9728 / 8414 / 7591 + PKCE S256 + `WWW-Authenticate: Bearer resource_metadata=…`). Differences that matter for this port:

| Criterion | V3 satellites | Vire |
|---|---|---|
| Token endpoint auth method | `["none"]` (public client + PKCE) | `["client_secret_post"]` (confidential client) |
| JWT minting | hand-rolled HS256, **stdlib only** | `golang-jwt/v5` dependency |
| Project lineage | same package layout (`internal/auth/`) and same logger (`arbor`) as V4 | different layout (`internal/server/httpd/`) |
| OAuth LOC (just OAuth) | ~830 LOC in `internal/auth/oauth_server.go` + `jwt.go` + `pkce.go` | ~705 LOC in `handlers_oauth.go` (single file) |
| Bonus capability | `agent_token.go` (per-agent JWT tokens, headless callers) | none |

**Decision: port V3.** Same package conventions, zero added dependencies, public-client posture aligned with current MCP guidance.

---

## 2. What V4 has today vs. what the port adds

V4's existing auth surface (`internal/auth/`):
- `handlers.go` — `/api/auth/login`, `/api/auth/logout` (bcrypt + DevMode quick-signin)
- `oauth.go` + `oauth_google.go` + `oauth_github.go` — Google/GitHub login with cookie-session output
- `session.go` + `session_surreal.go` — opaque-UUID cookie sessions backed by SurrealDB
- `store.go` — `UserStore` + `SessionStore` interfaces
- `bearer.go` — `BearerValidator` accepting provider tokens, `sat_` opaque tokens, and `cfg.APIKeys`
- `global_admin.go` — `SATELLITES_GLOBAL_ADMIN_EMAILS` allowlist
- `password.go` — bcrypt verify
- `user.go` + `user_surreal.go` — UserStore implementations

V4's existing `/mcp` middleware (`internal/mcpserver/auth.go`) accepts session cookie + `BearerValidator`-validated bearers. It emits a bare `WWW-Authenticate: Bearer realm="satellites"` header on 401.

What the port adds (no overlap with the above):
- Five new HTTP endpoints: `/.well-known/oauth-authorization-server`, `/.well-known/oauth-protected-resource`, `/oauth/register`, `/oauth/authorize`, `/oauth/token`
- JWT minting + verification (HS256, stdlib only)
- OAuth client / session / code / refresh-token persistence (four new Surreal tables)
- `mcp_session_id` bridge cookie integration into the existing `/api/auth/login` handler
- A new bearer path in `BearerValidator`: validate satellites-issued JWTs locally
- An augmented `WWW-Authenticate` header pointing at the resource-metadata endpoint

The port does **not** replace V4's existing login flow, cookie sessions, or browser OAuth. It sits alongside as a parallel auth path consumed by MCP clients.

---

## 3. Target architecture

```
                         MCP client (Claude Code/Desktop, mcp-remote)
                                       │
                          1. POST /mcp (no auth)
                                       │
                                       ▼
                       /mcp middleware → 401 WWW-Authenticate:
                         Bearer resource_metadata="…/oauth-protected-resource"
                                       │
                          2. GET /.well-known/oauth-protected-resource
                          3. GET /.well-known/oauth-authorization-server
                          4. POST /oauth/register   (RFC 7591 DCR)
                          5. GET  /oauth/authorize  (PKCE S256, browser)
                                       │
                                       ▼
                            Browser → V4 login page (existing)
                                       │
                          6. POST /api/auth/login (existing handler,
                                               extended with mcp_session bridge)
                                       │
                                       ▼
                            Login success → CompleteAuthorization →
                            redirect to client redirect_uri with ?code=…&state=…
                                       │
                          7. POST /oauth/token (code + verifier → JWT access + refresh)
                                       │
                                       ▼
                          8. POST /mcp Authorization: Bearer <JWT>  → 200
```

Only steps 2, 3, 4, 5, 7 are new endpoints. Step 6 is V4's existing login handler with one ~10-line addition. Step 8 reaches V4's existing `/mcp` middleware via a new JWT-validation branch in `BearerValidator`.

---

## 4. File-by-file porting map

V3 source paths are absolute under `/home/bobmc/development/satellites-v3/`. V4 destinations are relative to the satellites repo root.

| V3 source | LOC | V4 destination | Notes |
|---|---:|---|---|
| `internal/auth/oauth_server.go` | 667 | `internal/auth/oauth_server.go` (new) | Drop-in port. Adapter: replace V3's `models.OAuthStore` with a V4-package `OAuthStore` interface; replace `*config.OAuth2Config` with the V4 `*config.Config` (pulling new TTL fields added in §6). |
| `internal/auth/jwt.go` | 105 | `internal/auth/jwt.go` (new) | Drop-in port. Hand-rolled HS256 — no deps. The `IsLoggedIn` cookie helper at the bottom is unused by the OAuth-AS path; carry it for parity with V4's `auth.Handlers` if desired or drop it. |
| `internal/auth/pkce.go` | 14 | `internal/auth/pkce.go` (new) | Pure function. Drop-in port. |
| `internal/storage/oauth_store.go` | 203 | `internal/auth/oauth_store_surreal.go` (new) | Adapter: replace V3's `dbConn` wrapper with V4's direct `*surrealdb.DB` (pattern from `auth/session_surreal.go`). Add `DEFINE TABLE` calls in the constructor (V4 convention from `NewSurrealSessionStore`). |
| `internal/models/oauth.go` | 48 | `internal/auth/oauth_types.go` (new — folded into `auth` package) | V4 has no separate `models` package. Move the four struct types into `auth`. |
| `internal/handlers/auth.go:505-525` (mcp_session_id bridge block) | ~20 | `internal/auth/handlers.go` (existing — extend `Login` handler) | After successful login + WriteCookie, check for `mcp_session_id` cookie. If present, call `OAuthServer.CompleteAuthorization`, clear the bridge cookie, redirect to the returned URL. Otherwise fall through to existing redirect-to-`/`. |
| `internal/auth/middleware.go` (JWT validation block, lines 95-122) | ~30 | `internal/auth/bearer.go` (existing — extend `Validate`) | Add a new branch before the existing Google/GitHub fallback: if the token is three-part dot-separated, try `ValidateJWT`. On success, return `BearerInfo{UserID: claims.Sub, Provider: "satellites"}`. |
| (none — new code) | ~10 | `internal/mcpserver/auth.go` (existing — augment `WWW-Authenticate`) | Replace bare `Bearer realm="satellites"` with `Bearer resource_metadata="<base>/.well-known/oauth-protected-resource"`. Compute base from the request (`X-Forwarded-Proto` aware). |
| `internal/auth/agent_token.go` | 125 | **Skip in v1.** Open question §11. | Optional V3 capability; can land in a follow-up if headless agents need per-agent JWTs. |

Tests (port alongside the matching source):

| V3 test | V4 destination |
|---|---|
| `internal/auth/oauth_server_test.go` | `internal/auth/oauth_server_test.go` |
| `internal/auth/jwt_test.go` | `internal/auth/jwt_test.go` |
| `internal/auth/pkce_test.go` | `internal/auth/pkce_test.go` |
| (new) | `tests/integration/mcp_oauth_e2e_test.go` — full chain: discovery → DCR → authorize (with scripted login) → token → /mcp call |

---

## 5. Adapter changes (the non-mechanical bits)

The port is mostly verbatim. These are the places where V3 code can't move unchanged:

**5.1 OAuthStore interface lives in `auth`, not `models`.** V4 has no `internal/models` package; types and store interfaces co-locate in their domain package. Define `OAuthStore` in `internal/auth/oauth_types.go`:

```go
type OAuthStore interface {
    SaveClient(ctx context.Context, c *OAuthClient) error
    GetClient(ctx context.Context, clientID string) (*OAuthClient, error)
    SaveSession(ctx context.Context, s *OAuthSession) error
    GetSession(ctx context.Context, sessionID string) (*OAuthSession, error)
    GetSessionByClientID(ctx context.Context, clientID string) (*OAuthSession, error)
    DeleteSession(ctx context.Context, sessionID string) error
    SaveCode(ctx context.Context, c *OAuthCode) error
    GetCode(ctx context.Context, code string) (*OAuthCode, error)
    MarkCodeUsed(ctx context.Context, code string) error
    SaveRefreshToken(ctx context.Context, t *OAuthRefreshToken) error
    GetRefreshToken(ctx context.Context, token string) (*OAuthRefreshToken, error)
    DeleteRefreshToken(ctx context.Context, token string) error
}
```

**5.2 Config wiring.** V3's `OAuthServer` constructor takes `*config.OAuth2Config`. V4 has one flat `*config.Config`. Either pass the whole `*config.Config` and read TTL fields off it, or define a small `OAuthSettings` struct in the auth package that the V4 main wires up from `cfg`. The latter keeps the auth package config-package-free; pick to taste.

**5.3 SurrealDB connection shape.** V3's `OAuthStoreImpl` uses an internal `*dbConn` wrapper with `.get()` indirection. V4's pattern (per `auth/session_surreal.go`) is to take `*surrealdb.DB` directly. Mechanical replacement:
```go
// V3
results, err := surrealdb.Query[…](ctx, s.conn.get(), sql, vars)
// V4
results, err := surrealdb.Query[…](ctx, s.db, sql, vars)
```

**5.4 SurrealDB schema bootstrap.** Add `DEFINE TABLE IF NOT EXISTS …` calls in the `NewSurrealOAuthStore` constructor for `oauth_client`, `oauth_session`, `oauth_code`, `oauth_refresh_token`. Pattern at `internal/auth/session_surreal.go:36-39`.

**5.5 The `mcp_session_id` bridge.** V3's flow (verified in `oauth_server.go:344-353` and `handlers/auth.go:510-525`):

1. `/oauth/authorize` creates an `OAuthSession`, sets `mcp_session_id` cookie (10-min, HttpOnly, SameSite=Lax, Secure off in dev), redirects browser to `/?mcp_session=<id>`.
2. Login page handles its normal flow.
3. On successful login (post-cookie), the login handler reads the `mcp_session_id` cookie. If present, calls `OAuthServer.CompleteAuthorization(sessionID, userID)` which mints an authorization code, deletes the OAuthSession, and returns the redirect URL with `?code=…&state=…` for the MCP client.
4. Login handler clears the `mcp_session_id` cookie (MaxAge -1) and redirects to the returned URL instead of `/`.

V4 integration point: `internal/auth/handlers.go::Handlers.Login`, immediately after `WriteCookie(w, sess, cookieOpts(h.Cfg))`. ~20 LOC patch. The `OAuthServer` reference needs to be wired into `Handlers` (or passed via a callback in `Handlers` to keep the auth package internally cohesive).

**5.6 BearerValidator extension.** V4's `bearer.go::Validate` currently dispatches: `sat_` prefix → registry; otherwise → cache → Google → GitHub. Add a new branch before the Google/GitHub fallback: if the token has the JWT shape (three dot-separated base64 segments) and `ValidateJWT(token, cfg.JWTSecret)` succeeds, return `BearerInfo{UserID: claims.Sub, Provider: "satellites"}` and cache it. Order matters: JWT before Google because a malformed JWT bouncing through the Google userinfo endpoint is wasted latency.

**5.7 WWW-Authenticate header.** Update `internal/mcpserver/auth.go::AuthMiddleware`'s 401 path:
```go
base := scheme(r) + "://" + r.Host
w.Header().Set("WWW-Authenticate",
    fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, base))
```
where `scheme(r)` honours `X-Forwarded-Proto` (Fly terminates TLS at the edge).

---

## 6. Config additions

New fields on `*config.Config` (added to `internal/config/config.go::Config` struct, `defaults()`, `describeTable`, and `applyEnvOverrides`). Per the warn-not-fatal contract, every new field has a code default.

| Field | Env var | Default | Notes |
|---|---|---|---|
| `JWTSecret` | `SATELLITES_JWT_SECRET` | random 32 bytes generated at boot + warning emitted | Volatile default invalidates all minted tokens on restart. Operators set a stable value in prod. **This is the key the infra MIGRATION_V4.md said was retired — it's needed again, in a different role: signing satellites-issued OAuth access tokens, not browser sessions.** |
| `OAuthIssuer` | `SATELLITES_OAUTH_ISSUER` | `""` (derive from request host) | Set in prod when the server is behind a proxy with an opaque host header. |
| `OAuthAccessTokenTTL` | `SATELLITES_OAUTH_ACCESS_TOKEN_TTL` | `1h` | JWT bearer lifetime. |
| `OAuthRefreshTokenTTL` | `SATELLITES_OAUTH_REFRESH_TOKEN_TTL` | `168h` (7d) | Matches V3 / vire. |
| `OAuthCodeTTL` | `SATELLITES_OAUTH_CODE_TTL` | `10m` | Auth-code lifetime; spec says ≤10m. |

`Describe()` table updates for each. Config tests add coverage for the new defaults + env overrides + range validation (degrade-to-default per the warn-not-fatal contract).

---

## 7. Database schema

Four new SurrealDB tables, defined idempotently in `NewSurrealOAuthStore`:

```sql
DEFINE TABLE IF NOT EXISTS oauth_client      SCHEMALESS;
DEFINE TABLE IF NOT EXISTS oauth_session     SCHEMALESS;
DEFINE TABLE IF NOT EXISTS oauth_code        SCHEMALESS;
DEFINE TABLE IF NOT EXISTS oauth_refresh_token SCHEMALESS;
DEFINE INDEX IF NOT EXISTS oauth_session_client ON TABLE oauth_session FIELDS client_id;
DEFINE INDEX IF NOT EXISTS oauth_code_expires  ON TABLE oauth_code     FIELDS expires_at;
DEFINE INDEX IF NOT EXISTS oauth_rt_expires    ON TABLE oauth_refresh_token FIELDS expires_at;
```

Sweep cron (mirrors `SurrealSessionStore.Sweep`): delete expired `oauth_code` and `oauth_refresh_token` rows on a tick. Optional for v1; the lazy `expires_at > $now` filters in `GetCode` / `GetRefreshToken` keep correctness without sweeping. Add when row count becomes a concern.

---

## 8. Wiring (cmd/satellites-server/main.go)

After the existing `auth.NewBearerValidator` call:

```go
oauthStore := auth.NewSurrealOAuthStore(conn, logger)
oauthServer := auth.NewOAuthServer(auth.OAuthServerConfig{
    JWTSecret:           cfg.JWTSecret,
    Issuer:              cfg.OAuthIssuer,
    AccessTokenTTL:      cfg.OAuthAccessTokenTTL,
    RefreshTokenTTL:     cfg.OAuthRefreshTokenTTL,
    CodeTTL:             cfg.OAuthCodeTTL,
    Store:               oauthStore,
    Logger:              logger,
    DevMode:             cfg.DevMode,
})
authHandlers.OAuthServer = oauthServer  // for the mcp_session bridge
bearerValidator.WithJWTSecret(cfg.JWTSecret)  // enable the JWT branch
```

Routes are registered via the `RouteRegistrar` pattern V4 already uses:

```go
type oauthRoutes struct{ srv *auth.OAuthServer }

func (o *oauthRoutes) RegisterRoutes(mux *http.ServeMux) {
    mux.HandleFunc("GET /.well-known/oauth-authorization-server", o.srv.HandleAuthorizationServer)
    mux.HandleFunc("GET /.well-known/oauth-protected-resource",  o.srv.HandleProtectedResource)
    mux.HandleFunc("POST /oauth/register",                       o.srv.HandleRegister)
    mux.HandleFunc("/oauth/authorize",                           o.srv.HandleAuthorize)
    mux.HandleFunc("POST /oauth/token",                          o.srv.HandleToken)
}
```

Append `&oauthRoutes{srv: oauthServer}` to the `registrars` slice that's passed to `httpserver.New`.

---

## 9. Test plan

**Unit tests (port from V3, run under V4 package paths):**
- `oauth_server_test.go` — table-driven: discovery payloads, DCR auto-registration, authorize param validation, PKCE happy/sad, token grant happy/sad, refresh rotation
- `jwt_test.go` — mint+validate roundtrip, expiry, signature mismatch, malformed input
- `pkce_test.go` — verifier→challenge equivalence, constant-time compare

**Integration test (new):** `tests/integration/mcp_oauth_e2e_test.go`. Containerised satellites + surrealdb + DevMode signin. Steps:
1. `POST /mcp` no-auth → assert 401 + `WWW-Authenticate` header contains `resource_metadata="…"`.
2. `GET` the resource_metadata URL → assert it points to `authorization_servers: [<base>]`.
3. `GET /.well-known/oauth-authorization-server` → assert metadata payload matches RFC 8414.
4. `POST /oauth/register` with a redirect_uri → assert 201 and a client_id is returned.
5. `GET /oauth/authorize?…` with PKCE → assert 302 to `/?mcp_session=…` + `mcp_session_id` cookie set.
6. `POST /api/auth/login` carrying the cookie → assert 302 to client redirect_uri with `?code=…&state=…`.
7. `POST /oauth/token` with the code + verifier → assert 200 with `access_token`, `refresh_token`, `expires_in`.
8. `POST /mcp` with `Authorization: Bearer <access_token>` → assert 200 + tools/list response.
9. `POST /oauth/token` with the refresh token → assert rotated refresh + new access token.

**Failure tests:** wrong client_id, wrong redirect_uri, wrong PKCE verifier, expired code, reused code, expired refresh token, JWT signature tamper, wrong issuer.

---

## 10. Acceptance criteria

- [ ] `POST /mcp` with no auth returns `401` with `WWW-Authenticate: Bearer resource_metadata="<base>/.well-known/oauth-protected-resource"`.
- [ ] `GET /.well-known/oauth-protected-resource` returns RFC 9728 metadata referencing the local AS.
- [ ] `GET /.well-known/oauth-authorization-server` returns RFC 8414 metadata with `code_challenge_methods_supported: ["S256"]` and `token_endpoint_auth_methods_supported: ["none"]`.
- [ ] `POST /oauth/register` (RFC 7591 DCR) accepts a JSON body, persists an `oauth_client` row, returns the client document.
- [ ] `GET /oauth/authorize` with valid PKCE params creates an `oauth_session` row, sets `mcp_session_id` cookie, redirects to `/?mcp_session=<id>`.
- [ ] `POST /api/auth/login` (existing handler) detects the `mcp_session_id` cookie on success, completes the OAuth flow, redirects to the client's `redirect_uri` with `?code=…&state=…`.
- [ ] `POST /oauth/token` with grant_type=authorization_code + code + verifier verifies PKCE, mints a JWT access token (HS256, signed with `SATELLITES_JWT_SECRET`) + refresh token, returns standard JSON.
- [ ] `POST /oauth/token` with grant_type=refresh_token rotates the refresh token (deletes old, issues new) and mints a fresh access token.
- [ ] `POST /mcp` with `Authorization: Bearer <JWT>` succeeds and the MCP tool calls receive the resolved user identity from the JWT's `sub` claim.
- [ ] `POST /mcp` with an expired JWT returns `401` with the same `WWW-Authenticate` header.
- [ ] `SATELLITES_JWT_SECRET` unset emits a startup warning ("JWT secret unset — generated random per-boot, all MCP tokens invalidate on restart") and the binary still boots (per warn-not-fatal contract).
- [ ] `claude --add-mcp https://satellites-pprod.fly.dev/mcp` from a fresh Claude Code install completes the OAuth flow end-to-end via browser, with the user clicking only the "Sign in" button.
- [ ] All ported V3 unit tests pass under V4 package paths.
- [ ] The new `tests/integration/mcp_oauth_e2e_test.go` passes against a real Surreal container.

---

## 11. Open questions

1. **Username/password vs. OAuth provider login on the V4 login page.** V4 currently has both. Either is fine downstream of the OAuth dance — pick which to expose to MCP-driven users. Recommend keeping both; the user picks at the login form.
2. **Should `agent_token.go` ride along?** V3's per-agent JWT capability serves headless agents (CI, CLI tools) without going through the browser OAuth dance. V4's existing `cfg.APIKeys` static bearers cover the same use case more bluntly. Defer to a follow-up unless the immediate dogfood needs per-agent attribution.
3. **Issuer config.** Recommend deriving from `X-Forwarded-Proto` + `Host` (V3's pattern, `oauth_server.go:603-609`) rather than mandating `SATELLITES_OAUTH_ISSUER`. The env var stays as an override for setups behind opaque proxies.
4. **`mcp_session_id` cookie cleanup.** What happens if a user lands on `/?mcp_session=…` but doesn't log in (closes the tab)? V3 leaves the `oauth_session` row to expire naturally. Confirm V4 has (or adds) a sweep similar to `auth.SurrealSessionStore.Sweep` for `oauth_session` rows.
5. **JWT secret rotation.** Not designed in V3. A rotation event invalidates every in-flight access token. Acceptable for v1; a follow-up could support a key set with `kid` claims.
6. **Constant-time compare for client_secret.** V3 uses string equality on registered clients (`oauth_server.go:298` `containsString`). For DCR public clients with `token_endpoint_auth_methods: ["none"]` there's no client_secret to compare; V3's choice is correct for the public-client path. Document this so it isn't "fixed" by mistake.
7. **CORS on the discovery and token endpoints.** The MCP SDK fetches `/.well-known/*` from the user's browser via the running Claude client; CORS-permissive headers may be needed depending on the client implementation. Verify against an actual `mcp-remote` run before declaring done.

---

## 12. Suggested PR sequencing

For reviewability, split the port into focused PRs. Each is independently testable.

| # | Scope | Files touched | Risk |
|---|---|---|---|
| 1 | JWT + PKCE + types (pure functions, no wiring) | `internal/auth/jwt.go`, `pkce.go`, `oauth_types.go` + tests | very low |
| 2 | OAuthStore interface + SurrealDB implementation | `internal/auth/oauth_store_surreal.go` + tests against a Surreal test container | low |
| 3 | OAuthServer handlers (no wiring yet) | `internal/auth/oauth_server.go` + tests with stub store | low |
| 4 | Config additions (fields, defaults, env, warnings) | `internal/config/config.go` + tests | very low |
| 5 | BearerValidator JWT branch + WWW-Authenticate augmentation | `internal/auth/bearer.go`, `internal/mcpserver/auth.go` + tests | medium (touches existing /mcp middleware) |
| 6 | mcp_session_id bridge in login handler + main.go wiring + route registration | `internal/auth/handlers.go`, `cmd/satellites-server/main.go` | medium (touches existing login flow) |
| 7 | Integration test for full OAuth chain | `tests/integration/mcp_oauth_e2e_test.go` | adds confidence |
| 8 | Infra repo update: re-add `SATELLITES_JWT_SECRET` to `.env.pprod` + `fly/secrets/deploy.sh` | (separate repo: `satellites-infra`) | low |

PRs 1–4 can land in parallel (no inter-deps). PRs 5 and 6 depend on 1–4. PR 7 depends on everything else. PR 8 lands when 6 ships.

---

## 13. Out of scope for v1

- Token revocation endpoint (RFC 7009). Lazy expiry suffices for v1.
- Token introspection (RFC 7662). MCP clients don't need it; resource server validates JWTs locally.
- Pushed Authorization Requests (PAR, RFC 9126). Not required by the MCP spec.
- Multi-tenancy — issuer/audience scoping per workspace. Single issuer for v1.
- JWKS endpoint (RFC 7517). HS256 doesn't need one; if the move to RS256/ES256 happens later, add `/.well-known/jwks.json` then.
- `agent_token.go` port — see open question §11.2.

---

## 14. Reference reading

- V3 source root: `/home/bobmc/development/satellites-v3/internal/auth/`
- V3 routes wiring: `/home/bobmc/development/satellites-v3/internal/server/routes.go:105-111`
- V3 mcp_session bridge: `/home/bobmc/development/satellites-v3/internal/handlers/auth.go:510-525`
- MCP authorization spec: <https://modelcontextprotocol.io/specification/draft/basic/authorization> (verify the current revision before starting; spec was at 2025-06-18 at last check)
- RFCs: 6749 (OAuth 2.0), 7591 (DCR), 7636 (PKCE), 8414 (AS metadata), 9728 (resource metadata)
