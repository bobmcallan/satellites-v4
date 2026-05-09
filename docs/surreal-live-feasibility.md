# Surreal LIVE migration — feasibility + hub-consumer inventory

`epic:surreal-live-migration` (`sty_9a320134`), order:01 (`sty_a84b9c64`).

## TL;DR — recommendation

**AMBER. Proceed to order:02** building `internal/surreallive` as a typed
subscriber alongside the existing in-process hub, with three caveats
that must be designed in from the first commit:

1. **Caller-side reconnect.** The `surrealdb.go` SDK does not
   auto-reconnect a LIVE notification stream when the underlying
   connection drops. `internal/surreallive` owns the dial loop +
   exponential backoff, mirroring what `internal/agent/worker/wsclient.go`
   already does for the WS hub today.
2. **Replay strategy.** LIVE has no built-in replay — events that
   arrive while the subscriber is disconnected are lost. The
   substrate already has two compensating mechanisms: the agent's
   `idle_backoff` polling Claim (worker.go:152) and the per-row
   timestamp on every store table. Order:02's reconnect path
   should issue a catch-up `SELECT … FROM <table> WHERE
   updated_at > <last_seen>` immediately after the LIVE
   re-subscribe, then accept the live stream from there.
3. **Workspace scoping.** The bare `surrealdb.Live(ctx, db, table,
   diff)` API subscribes to the whole table. A multi-tenant
   substrate cannot afford that — the subscriber would receive
   rows from workspaces the caller has no membership in. Order:02
   uses raw `LIVE SELECT … FROM <table> WHERE workspace_id IN $ids`
   queries (the wire form `surrealdb.go` exposes via `Query`), and
   layers a client-side membership filter as belt-and-braces.

Each phase from order:02 onwards keeps the existing hub running, so
the migration stays revertible until the order:07 cutover.

## Hub-consumer inventory

Source-grep at HEAD (`commit 2c16212`).

### Server-side publishers (every store that calls `Publisher.Publish`)

| Source | File | Topic | Kinds | Payload shape |
|---|---|---|---|---|
| ledger | `internal/ledger/emit.go:75-131` | `ws:<workspace_id>` | `ledger.append`, `ledger.dereference`, `story.activity.append` | row id, workspace_id, project_id, story_id, type, tags, created_at; story-activity flavour adds the activity sub-fields |
| task | `internal/task/emit.go:46` | `ws:<workspace_id>` | `task.<status>` (every status transition: planned, published, claimed, in_flight, closed, archived) | task_id, workspace_id, project_id, story_id, kind, action, origin, priority, outcome (when closed) |
| story | `internal/story/emit.go:38` | `ws:<workspace_id>` | `story.<status>` (every status transition: backlog, ready, in_progress, blocked, done, cancelled) | story_id, workspace_id, project_id, title, status, priority, category, tags[], updated_at |
| repo | `internal/repo/emit.go:32` | `ws:<workspace_id>` | `repo.reindex.<phase>` | repo metadata + phase-specific extras |

`internal/repo/emit.go` defines `emitReindex` but **no production call
site** exists at HEAD — the function is plumbed for a future story and
contributes zero events today. Treat as dormant; revisit when reindex
emits are wired.

### Server-side subscribers

| Source | File | Topics | Replay | Notes |
|---|---|---|---|---|
| `internal/wshandler/wshandler.go` | `wshandler.go:257` (`SubscribeSince`) | one per portal client; format `ws:<workspace_id>` | hub ring buffer (cap 500) replays from `since_id` cursor | The only first-class consumer. Forwards every event to the connected gorilla/websocket client. Fail modes: `ErrNotMember`, `ErrInvalidTopic` produce typed JSON error frames. |
| `internal/storystatus/reconciler.go` | (registered via `task.Store.AddListener` at `cmd/satellites/main.go`) | n/a — listener fan-out, not topic-keyed | n/a | sty_051bd266 — task.Listener, not a hub subscriber. Out of scope for this migration. |

### Client-side subscribers

| Source | File | Subscription | Replay | Notes |
|---|---|---|---|---|
| portal `pages/static/ws.js` | `ws.js:130-149` | `ws:<workspace_id>` | `since_id` cursor preserved across reconnects | Standard `_applyEvent` path in `common.js:450-486` consumes `task.<status>`, `story.<status>`, `ledger.append`, `story.activity.append`. |
| satellites-agent | `internal/agent/worker/wsclient.go:99-265` | one connection per workspace listed in `subscribe_workspace_ids`; topic `ws:<workspace_id>` | `since_id` cursor + per-workspace ring | Filters to `task.published` only and emits `WakeEvent` per match. Polling Claim is the correctness backstop; wake is an optimization. |

### Cross-cutting

- `internal/hub/hub.go` is a single in-process primitive — one
  `*hub.Hub` per server process. The 500-event ring buffer is
  workspace-keyed (one ring per topic).
- `internal/hub/authhub.go` validates that the topic's workspace
  matches the subscriber's memberships before the channel is
  attached (`SubscribeSince` path) and that publish-time event's
  `WorkspaceID` matches the topic suffix (`Publish` path). Mismatches
  produce a `kind:hub-mismatch` ledger row.
- `internal/hubemit.Publisher` is the typed contract; today only
  `cmd/satellites/main.go`'s `hubPublisher` (a thin adapter onto
  `*hub.AuthHub`) implements it.

## Migration target — what each consumer becomes

| Consumer | Future shape under LIVE |
|---|---|
| ledger emit | `LIVE SELECT … FROM ledger WHERE workspace_id IN $ids` — surrealStore writes + LIVE notification fire on the same Surreal mutation. |
| task emit | `LIVE SELECT … FROM tasks WHERE workspace_id IN $ids`. Order:04 wires the agent's wake source onto this. |
| story emit | `LIVE SELECT … FROM stories WHERE workspace_id IN $ids`. Used by the portal's row-update path and the storystatus reconciler. |
| repo emit | Wired only when the dormant `emitReindex` call lands; same shape. |
| `wshandler` | Order:06 swaps its upstream from `*hub.AuthHub` to a `*surreallive.Subscriber`. Outbound WS frame shape unchanged. |
| portal `ws.js` | Unchanged. The portal connects to `/ws` exactly as today; the upstream of that endpoint is what moves. |
| agent `wsclient.go` | Order:04 adds a parallel `livewake.go` that emits onto the same `WakeChan`. Order:07 deletes `wsclient.go`. |

## Surreal LIVE — what we know from the SDK + docs

surrealdb.go versions in the module cache: `v1.1.0` (current
`go.mod`), `v1.3.0`, `v1.4.0`. The SDK API surface for live queries:

- `surrealdb.Live(ctx, db, table models.Table, diff bool) (*models.UUID, error)`
  — opens a per-table live query, returns the liveQueryID.
- `db.LiveNotifications(liveQueryID string) (chan connection.Notification, error)`
  — returns the receive channel.
- `db.CloseLiveNotifications(liveQueryID)` — releases the channel.
- For filtered live queries, the wire form is `LIVE SELECT … FROM
  <table> WHERE …` issued via `surrealdb.Query` rather than the
  typed `Live` helper — the SDK helper is table-only.

The example at `surrealdb.go@v1.4.0/example_livequery_test.go`
demonstrates the canonical loop:

```go
live, err := surrealdb.Live(ctx, db, "users", false)
// liveQueryID = live.String()
notifications, err := db.LiveNotifications(live.String())
for notif := range notifications {
    // notif.Action: "CREATE" | "UPDATE" | "DELETE"
    // notif.Result: row payload (or JSON-patch when diff=true)
}
```

### Behavioural questions — answered + blocked

| Question | Status | Answer |
|---|---|---|
| Does LIVE replay events that occurred during a brief disconnect? | **Answered (no, by SDK design).** | The notification channel is bound to the open Surreal session. When the session drops, the notification channel closes. There is no replay; missed events are lost. Caller must resubscribe and back-fill via a `SELECT … WHERE updated_at > $cursor` query. |
| Notification payload shape? | **Answered.** | `connection.Notification{ID, Action, Record, Result}` where `Action` is `"CREATE"|"UPDATE"|"DELETE"` and `Result` is the post-mutation row (full row for `diff=false`; JSON-patch ops for `diff=true`). |
| Does LIVE fire on UPDATE/CREATE/DELETE? Payload per kind? | **Answered.** | All three. CREATE returns the full new row; UPDATE returns the full post-mutation row (no diff unless requested); DELETE returns the row id only. With `diff=true` UPDATE returns JSON-patch ops. |
| Does Surreal's RBAC scope LIVE notifications to rows the connected user can see? | **Blocked (needs cluster verification).** | Docs imply yes via Surreal's per-record permissions, but the satellites server connects with a single privileged credential, so RBAC scoping at the DB layer does not apply to multi-tenant data. The WHERE clause must enforce workspace isolation; client-side filter on the resulting notification gives belt-and-braces. Order:02 must validate this against pprod's actual Surreal config. |
| Auto-reconnect by the client library? | **Answered (no).** | The SDK does not auto-reconnect. Caller owns the dial loop, exponential backoff, and the catch-up SELECT after re-subscribe. |
| Multi-node consistency in cluster mode? | **Blocked (needs pprod-topology test).** | Surreal docs note that LIVE notifications in a multi-node cluster are scoped to the node the live query was registered against; cross-node delivery depends on the cluster mode (foundation cluster vs. standalone). Order:02 confirms against pprod's actual topology before order:08's two-machine canary. |

The two BLOCKED rows do not block order:02 — the implementation
proceeds with the conservative assumption (no replay, no cluster
guarantee, client enforces workspace filter). Order:02 ends with
the resolved answers captured in the same ledger row that closes
the develop task; if either answer makes the model unsafe, that
becomes the BLOCK gate for order:03.

## What ships in order:02

The next story (`sty_c7c3850f`) builds `internal/surreallive` against
the model this doc describes:

- `Subscriber` opens a `LIVE SELECT id, workspace_id, project_id,
  story_id, status, … FROM <table> WHERE workspace_id IN $ids`
  query.
- On notification, the result is decoded into a typed `Event`
  matching the existing `hub.Event` shape (Kind = `<table>.<status>`
  for parity with the in-process hub's emit kinds).
- On disconnect, the dial loop reconnects with exponential backoff
  (mirrors `internal/agent/worker/wsclient.go`); on reconnect, a
  `SELECT … FROM <table> WHERE updated_at > $cursor AND
  workspace_id IN $ids` runs first, replaying any events missed
  during the outage; then the live stream resumes.
- The subscriber is wired in `cmd/satellites/main.go` as a SECOND
  emit path alongside the hub. No consumer reads from it yet.

## Out of scope for this story

- Building `internal/surreallive` (order:02).
- Switching any consumer onto LIVE (order:04 onwards).
- Reducing the MCP catalogue (orthogonal — `epic:cli-primary`).
- The `epic:portal-stories-cleanup` chip strip changes (already shipped).

## References

- `internal/hub/hub.go`, `internal/hub/authhub.go` — current bus.
- `internal/{ledger,task,story,repo}/emit.go` — current publishers.
- `internal/wshandler/wshandler.go` — current server-side subscriber.
- `pages/static/ws.js`, `pages/static/common.js` — current client-side subscriber.
- `internal/agent/worker/wsclient.go` — agent's WS subscriber (the reconnect/backoff template).
- `surrealdb.go@v1.4.0/example_livequery_test.go` — SDK live query example.
- `epic:surreal-live-migration` (`sty_9a320134`) — parent epic.
