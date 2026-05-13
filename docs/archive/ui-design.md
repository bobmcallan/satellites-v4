# Satellites v4 — Portal UI Design

Authoritative UX reference for the v4 portal. Scopes what the portal renders and how the developer moves through it. **Aesthetic, component library, copy rules, and accessibility floor live in the governing design brief `satellites_design.md` on this project** — this document does not re-specify them; it references them and names the v4 views that consume them.

Principles are cited by short name throughout; the authoritative full names live on the project's principle documents. Architecture primitives and schemas are in `docs/architecture.md`.

**Business objective:** developers should not be distanced from the code. Every view earns its place by resolving a question the developer would otherwise have to answer by trawling git, SQL, logs, or an agent's self-report.

---

## 1. Positioning

The portal is a terminal for an engineering pipeline. It renders the data, respects the lifecycle, stays out of the way (`satellites_design.md` §1). Users are engineers and operators, not casual browsers — the portal feels like `htop`, not a SaaS dashboard.

**v4 portal's job:** surface the five project primitives (documents, stories, tasks, ledger, repo) and the one enclosing primitive (workspace) as live, inspectable views. Read-mostly. Write affordances are narrow: file a story, amend a principle/document, override a verdict with justification, claim/close in dev workflows. No admin-override that bypasses the lifecycle — "Process order and evidence are first-class" is enforced server-side.

**Aesthetic governance:** all component/layout/palette/copy decisions resolve to `satellites_design.md`. Search the brief before inventing a new primitive.

**Principles:** Evidence is the primary trust leverage, Process order and evidence are first-class.

---

## 2. Core views

Six views. Each specifies **purpose**, **data shown**, **interaction affordances**, **live-update behaviour**, and **principle citations**. All views are workspace-scoped — the workspace switcher is global chrome; no view displays data from a workspace the session is not a member of.

### 2.1 Project view (landing)

- **Purpose:** top-level navigation into the five primitives. Answer "what's happening in this project?" at a glance.
- **Data shown:** project header (id, name, workspace breadcrumb, status); principle count (active); story counts per status; open task count; last-commit on repo; recent activity strip (last 10 ledger rows across the project). Links to story list, task queue, ledger, document browser, repo view.
- **Interaction affordances:** navigate to any of the five primitive views. Focus project ("make this the session default"). Open project settings. No destructive actions on this page.
- **Live-update behaviour:** recent activity strip subscribes to workspace ledger events; new rows prepend on insert. Counts in panel headers (`satellites_design.md` §6 rule 7) re-compute on each ledger event.
- **Principles:** Project is top-level primitive, Five primitives per project, Workspace is the multi-tenant primitive.

### 2.2 Story view

- **Purpose:** end-to-end audit surface for one story. The single answer for "what has the agent touched?", "has delivery actually shipped?", and "why are we doing this?".
- **Data shown:**
  - Header: story id, title, status, priority, category, tags (incl. `epic:*` and `feature-order:*`), estimated/actual points.
  - **Scope** panel: description + acceptance criteria (rendered markdown).
  - **Source documents** panel: linked `document` ids (per "Documents drive feature stories") with titles — the "why are we doing this?" anchor.
  - **Contract instances** timeline: one row per CI in sequence order with status, claimed-by, close-row link.
  - **Ledger excerpts** panel: rows filtered to `story_id`, default sort newest-first, filterable by kind (plan / evidence / decision / verdict). Live.
  - **Repo provenance** panel: branch, commit SHA(s) that touched this story, diff-stat against main, CI status. Answers "what has the agent touched?".
  - **Reviewer verdict** panel: story-review row(s) with score + reasoning.
  - **Delivery status** strip: "in_progress / done / blocked", plus for done stories: *commits visible on main?* — the answer to "has delivery actually shipped?" beyond the mere `status=done` flag.
- **Interaction affordances:** claim next CI (if session is owner), close CI (with evidence), file a follow-up story, override a verdict (justification required, recorded on ledger), amend AC (subject to preplan/plan rules).
- **Live-update behaviour:** ledger excerpts, CI timeline, and repo provenance all stream live. Websocket disconnect shows a hairline "reconnecting…" strip in the panel header; on reconnect, the panel refetches the range it missed (see §3 reconnect policy).
- **Principles:** Story is the unit, Evidence is the primary trust leverage, Documents drive feature stories, Process order and evidence are first-class, Workspace is the multi-tenant primitive.

### 2.3 Task queue view

- **Purpose:** live monitor of satellites-agent work. The single answer for "what is the agent doing right now?".
- **Data shown:** table with columns `ID` (muted), `ORIGIN` (story_stage / scheduled / …), `PAYLOAD SUMMARY`, `STATUS` (enqueued / claimed / in_flight / closed), `WORKER`, `ENQUEUED_AT`, `CLAIMED_AT`, `DURATION`. Grouped or filter-toggled into sections: *in_flight*, *enqueued*, *recently closed*.
- **Interaction affordances:** filter by workspace (default = session-member set), origin, status, worker. Click a task row's ID to drill into the task detail (payload + ledger trail). Re-enqueue a timed-out task (admin only). No "mark done from UI" — that violates the lifecycle.
- **Live-update behaviour:** the *in_flight* section is purely live. Each task mutation (dispatched → in_flight → close) flips the row in place with a 1-second highlight. New enqueues appear at the bottom of *enqueued*; closes move the row to *recently closed* with outcome badge. Disconnect: show the "reconnecting…" strip; on reconnect, refetch the entire visible filter set (tasks are small rows; a full refetch is cheaper than replaying events).
- **Principles:** Tasks are the queue, Satellites-agent is the worker (the queue is the work monitor), Workspace is the multi-tenant primitive.

### 2.4 Ledger inspection view

- **Purpose:** the auditable record. Filter by scope (story_id, contract_id, project) and kind (evidence / plan / verdict / action_claim / decision). Read every trail end-to-end.
- **Data shown:** table of ledger rows — `CREATED_AT` (relative + absolute on hover), `TYPE`, `TAGS` (chips, one per tag), `SCOPE` (project/story/contract ids, muted), `CONTENT` (truncated with expand-in-place). Each row links to its parent story and CI.
- **Interaction affordances:** full-text search across content; filter by tag (chips clickable); filter by source (agent / user / system); time range. Copy row id. Export filter result as markdown (for offline review).
- **Live-update behaviour:** ledger page streams new rows into the top of the table when they match the active filter. A "N new rows" pill appears if the user has scrolled away from top; click to jump. Dereferenced rows are hidden by default; a toggle shows them greyed out (consistent with v3 behaviour; `satellites_design.md` §4 `.badge-muted`).
- **Principles:** Evidence is the primary trust leverage, Process order and evidence are first-class, Workspace is the multi-tenant primitive.

### 2.5 Document browser

- **Purpose:** reach any document in the project, filtered by type (`artifact` / `contract` / `skill` / `principle` / `reviewer` / `design` / `architecture`). See version history. Discover what stories cite each doc.
- **Data shown:** type filter tabs (one per document type). List table: `ID` (muted), `TITLE`, `TYPE` (chip), `CATEGORIES` (chips), `UPDATED_AT`, `VERSION`. Principles filter adds a status chip (active / archived) and scope (system / project).
- **Interaction affordances:** open a document → detail view with rendered markdown body + version history + linked stories (stories that name this doc in `source_documents`). Create (for agents/admins). Archive. Amend — creates a new version, doesn't mutate the current.
- **Live-update behaviour:** list refreshes on document create/update/archive events. Detail view re-renders the body when a new version is ingested.
- **Principles:** Documents share one schema, Documents drive feature stories, Workspace is the multi-tenant primitive.

### 2.6 Repo + index view

- **Purpose:** expose the project's indexed repo so developers can answer "where does X live?" without cloning. Surface "what has the agent touched across this branch" at the repo level.
- **Data shown:** repo header (git_remote, default_branch, head_sha, last_indexed_at, index_version, symbol/file counts). Semantic search input (symbol-first, text second). Recent commits on main with touched-files-count + linked stories (commits cite story ids in messages). Branch diff panel — select a branch → see the diff-stat against main + linked story.
- **Interaction affordances:** semantic search runs against the MCP `repo_search` / `repo_search_text` verbs. Click a symbol → source view (via `repo_get_symbol_source`). Click a file → raw content via `repo_get_file`. Trigger a manual re-index (admin only, via `repo_scan`).
- **Live-update behaviour:** the index state chip (*up-to-date / stale / indexing*) reflects live. A push/commit triggers a reindex task; the chip flips to *indexing* during it and back to *up-to-date* on completion. Recent commits list streams new commits on push.
- **Principles:** Repo is a first-class primitive, Workspace is the multi-tenant primitive.

---

## 3. Live-state surfaces

Three mutable surfaces stream via the workspace-scoped websocket hub (see `docs/architecture.md` §6). Every surface has a defined event trigger and reconnect policy.

| Surface | Event trigger | Reconnect / replay |
|---------|--------------|--------------------|
| **Task queue** (§2.3) | Task `status` transition (enqueued → claimed → in_flight → closed). Task row insert/update in the server's write path fans to `ws:<workspace_id>` with a `task.*` event type. | On disconnect: show "reconnecting…" strip in panel header. On reconnect: refetch the active filter's full visible set via `/api/tasks?...`. Tasks are small rows; full-set refetch is cheaper than event replay and simpler to reason about. |
| **Ledger inspection** (§2.4) + story-view **ledger excerpts** (§2.2) | Ledger row insert / status change (active → dereferenced → archived). Fan-out carries `ledger.created` / `ledger.status_changed` events tagged with `story_id` and `contract_id` so subscribers can filter client-side. | On disconnect: "reconnecting…" strip. On reconnect: fetch rows created after the last `created_at` the client saw (`/api/ledger?after=<ts>`). Event replay by timestamp — no deduplication needed because the client keyed by row `id`. |
| **Story view** (§2.2) — CI timeline + repo provenance + reviewer verdict | CI status change, verdict row insert, new commit linked to the story. Fan-out events: `contract_instance.updated`, `ledger.created{kind:story-review}`, `repo.commit.linked{story_id}`. | On disconnect: "reconnecting…" strip at top of page. On reconnect: refetch the story composite via `/api/stories/<id>/composite` — returns the story + CIs + latest ledger window + repo provenance in one round trip. Cheaper than replaying three independent event streams. |

**Connection indicator (global chrome):** a small strip in the top nav shows *live* (green, `#4ade80`), *reconnecting…* (warning, `#d4a017`), or *disconnected — retry* (danger, `#ef4444`) with a manual retry button. No view is trusted to render stale data as live — each live panel dims its "live" chip when the workspace websocket is not connected.

**Replay cap:** the server sends up to N=500 replayed events on reconnect; beyond that, clients refetch the visible composite.

**Principles:** Evidence is the primary trust leverage (live state is audit-visible state), Workspace is the multi-tenant primitive (workspace-scoped topics), Process order and evidence are first-class (lifecycle transitions are first-class events).

---

## 4. Visibility guarantees

The four questions the portal *must* answer, and the single view that resolves each:

| Question | Resolving view | How it resolves |
|----------|----------------|-----------------|
| **What has the agent touched?** | Story view (§2.2) — Repo provenance panel | Branch + commits + diff-stat against main + CI status; the story's full code surface is in one panel. Also usable at repo level (§2.6 "branch diff") for cross-story audits. |
| **What is the agent doing right now?** | Task queue view (§2.3) | *in_flight* section is the live monitor. Filtered by workspace. One row per active task, with origin + payload summary + worker + elapsed duration. No other view exposes "right now" as its primary question. |
| **Has delivery actually shipped?** | Story view (§2.2) — Delivery status strip | For `status=done` stories, the strip explicitly checks *commits visible on main?* — not just the lifecycle flag. Echoes a v3 delivery-vs-shipped lesson: a story closed as delivered is not shipped if the commits aren't on the branch that counts. |
| **Why are we doing this?** | Story view (§2.2) — Source documents panel | Lists the `document` rows the story cites (per "Documents drive feature stories"). Click through to the document body. If source_documents is empty, the panel displays the muted copy "No source documents" and flags it — bugs/infra/ops stories are exempt from the document requirement, but feature stories without a document source are a smell. |

Each question has exactly one primary-resolving view; no guarantee is split across multiple primary views. This avoids the v3 pattern where the same question had three partial answers scattered across pages.

**Principles:** Story is the unit (audit unit), Repo is a first-class primitive (repo visibility is non-ceremonial), Tasks are the queue (task queue is the work monitor), Documents drive feature stories.

---

## 5. Interaction model

- **Alpine.js only** for client interactivity (`satellites_design.md` §5). Each `<section class="panel-headed">` that needs state gets a scoped `x-data`. No React, Vue, or htmx.
- **Server-rendered pages.** Go templates produce the HTML; Alpine hydrates local state and fetches via `/api/…`. No SPA routing.
- **Read-mostly.** Write affordances:
  - *Story:* file, amend AC (subject to lifecycle), claim/close CI (owners), override verdict (with justification), close story (via MCP flow).
  - *Document:* create, amend (new version), archive.
  - *Principle:* create (project-scope), update, archive.
  - *Task:* enqueue (free preplan or manual cron trigger, admin only), re-enqueue a timeout (admin only).
  - *Repo:* trigger manual reindex (admin only).
- **No lifecycle bypass.** No button flips a story to `done` without a `story_close` pass. No button marks a CI `passed` outside the close flow. No button sets a ledger row to `archived` without a reason. "Process order and evidence are first-class" is enforced server-side (see `docs/architecture.md` §5 process-order gate); the UI doesn't expose bypass affordances.
- **Keyboard:** `/` focuses the page's primary search/filter. Table rows link via the ID cell (a plain `<a>`) — the whole row is not a click target (preserves text selection). Focus-visible outlines per `satellites_design.md` §9.
- **Copy-to-clipboard** on IDs and commit SHAs (pattern from `satellites_design.md` §5 + existing `common.js`). Always toasts the confirmation.
- **Destructive confirmations** via `common.js` `confirm()` dialog (never `window.confirm`).

**Principles:** Process order and evidence are first-class (no bypass), Satellites-agent is the worker (UI doesn't orchestrate; it renders).

---

## 6. Sketches

Low-fidelity. Shape, not pixels. Each follows the `.panel-headed` primitive from `satellites_design.md` §3.

> Notation: `●` = live indicator (green when connected, muted when disconnected). `▾` = filter/select. `[btn]` = button. `#id` = muted monospace id. `(n)` = count in panel header.

### Sketch 1 — Project view (§2.1)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ WORKSPACE / wksp_xxxx   PROJECT / satellites  #proj_xxxxxxxx  [FOCUS]       ●│
├──────────────────────────────────────────────────────────────────────────────┤
│ OVERVIEW                                                                     │
│ ┌────────────┬────────────┬────────────┬────────────┬────────────┐          │
│ │ STORIES    │ TASKS      │ LEDGER     │ DOCUMENTS  │ REPO       │          │
│ │ 14 open    │ 0 in_flight│ 1,284 rows │ 17 active  │ on main    │          │
│ │ 9 done     │ 1 queued   │ 3 today    │ 12 princ.  │ head a91… ●│          │
│ └────────────┴────────────┴────────────┴────────────┴────────────┘          │
├──────────────────────────────────────────────────────────────────────────────┤
│ RECENT ACTIVITY (last 10)                                                  ● │
│ 29m ago · kind:story-review   #story_…   verdict approved score 5           │
│ 1h ago  · kind:plan           #story_…   plan submitted                     │
│ …                                                                           │
├──────────────────────────────────────────────────────────────────────────────┤
│ PRINCIPLES (12 active)                                                       │
│ · Project is the top-level primitive within a workspace                     │
│ · Five primitives per project                                               │
│ …                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sketch 2 — Story view (§2.2)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ STORY  #story_…    feature-order:6                                    ● live│
│ Write architecture document — v4 data model, services, protocols, queue…    │
│ [done] [high] [documentation] [8 pts]  epic:v4-foundation                   │
├──────────────────────────────────────────────────────────────────────────────┤
│ SCOPE                                                                        │
│ Description + acceptance criteria rendered markdown…                        │
├──────────────────────────────────────────────────────────────────────────────┤
│ SOURCE DOCUMENTS                                                             │
│ · satellites_design.md   (cite)                                             │
├──────────────────────────────────────────────────────────────────────────────┤
│ CONTRACTS                                                                    │
│ ▸ preplan        #ci_…   passed   claimed 21:26   closed 21:27               │
│ ▸ plan           #ci_…   passed   ...                                        │
│ ▸ develop        #ci_…   passed   ...                                        │
│ ▸ story_close    #ci_…   passed   ...                                        │
├──────────────────────────────────────────────────────────────────────────────┤
│ LEDGER EXCERPTS   (14)                                                     ● │
│ ▾ kind: all                                                                 │
│ 22:01 · story-review     approved score 5                                   │
│ 22:00 · evidence         develop close evidence                             │
│ …                                                                           │
├──────────────────────────────────────────────────────────────────────────────┤
│ REPO PROVENANCE                                                            ● │
│ branch main      commit a91295c7 "docs: add v4 architecture document"       │
│ diff-stat  1 file changed, 609 insertions(+)                                │
│ on main? YES   CI? —                                                        │
├──────────────────────────────────────────────────────────────────────────────┤
│ DELIVERY STATUS   [done]                      commits on main? YES          │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sketch 3 — Task queue view (§2.3)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ TASK QUEUE    workspace: wksp_xxxx    ▾ all origins    ▾ all statuses     ● │
├──────────────────────────────────────────────────────────────────────────────┤
│ IN_FLIGHT (1)                                                              ● │
│ #tsk_…   story_stage   #story_… develop   agent_x   00:47 elapsed           │
├──────────────────────────────────────────────────────────────────────────────┤
│ ENQUEUED (0)                                                                 │
│ No tasks enqueued.                                                           │
├──────────────────────────────────────────────────────────────────────────────┤
│ RECENTLY CLOSED (20)                                                         │
│ #tsk_…   story_stage   #story_… story_close   —      success 1m ago         │
│ #tsk_…   scheduled     backlog_sweep           —      success 5m ago        │
│ …                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sketch 4 — Ledger inspection (§2.4)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ LEDGER    (1,284)                                               ▾ tags    ● │
├──────────────────────────────────────────────────────────────────────────────┤
│ [search across content_____________________] ▾ type  ▾ source  ▾ time       │
├──────────────────────────────────────────────────────────────────────────────┤
│ ▲ 3 new rows — jump to top                                                   │
├──────────────────────────────────────────────────────────────────────────────┤
│ CREATED_AT    TYPE        TAGS                    SCOPE                      │
│ 29m ago       evidence    phase:story-close       #story_…                  │
│               kind:story-review                   #ci_…          ▸ expand   │
│ 1h ago        plan        phase:plan              #story_…                  │
│               artifact:plan.md                    #ci_…          ▸ expand   │
│ …                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sketch 5 — Document browser (§2.5)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ DOCUMENTS    (17)                                                            │
│ [ all ] [artifact] [contract] [skill] [principle] [reviewer] [architecture] │
├──────────────────────────────────────────────────────────────────────────────┤
│ TITLE                                         TYPE          CATEGORIES       │
│ Satellites v4 — Architecture                  architecture  architecture,…   │
│ Satellites — Portal Design (governing brief)  general       general          │
│ Project is top-level primitive                principle     v4, primitive    │
│ …                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sketch 6 — Repo + index view (§2.6)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ REPO   git@github.com:bobmcallan/satellites.git        branch main    ● up-to-date │
│ head a91295c7   last-indexed 22:00    18 symbols · 5 files                   │
├──────────────────────────────────────────────────────────────────────────────┤
│ SEARCH  [semantic search over symbols + text_______________________]  [go]   │
├──────────────────────────────────────────────────────────────────────────────┤
│ RECENT COMMITS                                                               │
│ a91295c7  docs: add v4 architecture document            #story_…            │
│ 58a29fd   feat(bootstrap): scaffold satellites repo …   #story_…            │
│ 6e7e345   Initial commit                                —                    │
├──────────────────────────────────────────────────────────────────────────────┤
│ BRANCH DIFF   ▾ select branch                                                │
│ (select a branch to see diff-stat + linked story)                            │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## 7. Out of scope

- **Theming + branding** beyond dark-first / light alternate (`satellites_design.md` §7).
- **Mobile-specific layouts** — the narrow-viewport fallback is in `satellites_design.md`; no separate mobile spec.
- **Internationalisation** — English only for v1.
- **Portal implementation code** — AC7 explicit. This story ships a doc.
- **Onboarding tours / celebratory animations** — would violate `satellites_design.md` §1 ("power tool, not marketing surface").
- **Workflow editor UI** — workflows are the list of CIs on a story; editing is expressed through story_update + contract CRUD on the API surface, not a graph editor.

---

## 8. Principle traceability matrix

| Principle | Primary section(s) | Role |
|-----------|---------------------|------|
| Project is top-level primitive | §2.1 Project view chrome | Workspace → Project breadcrumb in every view. |
| Five primitives per project | §2 (six views: five primitives + repo) | Views mirror the primitive set; no sibling views for nonexistent primitives. |
| Documents share one schema | §2.5 Document browser | Single browser with type-filter tabs rather than separate browsers per document kind. |
| Story is the unit | §2.2 Story view | The story is the audit unit; three of four visibility guarantees resolve there. |
| Tasks are the queue | §2.3 Task queue view | Single live monitor surface; origin diversity hidden behind one table. |
| Satellites-agent is the worker | §2.3, §5 Interaction model | UI doesn't orchestrate; it renders what the worker did. |
| Preplan scope is tight | §2.2 (preplan CI row is one line in the CIs table) | Preplan surfaces as a short CI; not promoted to a dedicated tab. |
| Documents drive feature stories | §2.2 Source documents panel + §2.5 "linked stories" | Feature-story → document bidirectional link visible from both sides. |
| Repo is a first-class primitive | §2.6 Repo view + §2.2 Repo provenance panel | Repo has its own top-level view and shows up inside the story view. |
| Evidence is the primary trust leverage | §2.4 Ledger view; §2.2 Ledger excerpts; §3 Live state | Ledger is first-class, live, filterable. |
| Process order and evidence are first-class | §5 Interaction model (no bypass); §2.2 CI timeline | UI does not expose lifecycle-bypass buttons. |
| Workspace is the multi-tenant primitive | §2 (every view header), §3 (ws topics) | Workspace switcher is global chrome; all views filtered by membership. |

All twelve principles referenced. No orphan principles; no orphan views.

---

## 9. Changelog

| version | date | change |
|---------|------|--------|
| 0.1.0 | 2026-04-23 | Initial UI design document. Cites `satellites_design.md` as governing aesthetic brief; cites `docs/architecture.md` for architecture primitives. |
| 0.1.1 | 2026-04-23 | Scrubbed external v3 IDs (principles cited by short name; doc filenames in place of doc IDs). |
