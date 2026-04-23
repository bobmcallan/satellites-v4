# Satellites v4 — Portal UI Design

Authoritative UX reference for the v4 portal. Scopes what the portal renders and how the developer moves through it. **Aesthetic, component library, copy rules, and accessibility floor live in the governing design brief `doc_087842fa` (satellites_design.md) on this project** — this document does not re-specify them; it references them and names the v4 views that consume them.

Principle IDs referenced throughout follow the seed in story_18d1646a. Architecture primitives and schemas are in `docs/architecture.md` (doc_adfa7adf).

**Business objective:** developers should not be distanced from the code. Every view earns its place by resolving a question the developer would otherwise have to answer by trawling git, SQL, logs, or an agent's self-report.

---

## 1. Positioning

The portal is a terminal for an engineering pipeline. It renders the data, respects the lifecycle, stays out of the way (`doc_087842fa`, §1). Users are engineers and operators, not casual browsers — the portal feels like `htop`, not a SaaS dashboard.

**v4 portal's job:** surface the five project primitives (documents, stories, tasks, ledger, repo) and the one enclosing primitive (workspace) as live, inspectable views. Read-mostly. Write affordances are narrow: file a story, amend a principle/document, override a verdict with justification, claim/close in dev workflows. No admin-override that bypasses the lifecycle (`pr_20440c77`).

**Aesthetic governance:** all component/layout/palette/copy decisions resolve to `doc_087842fa`. Search the brief before inventing a new primitive.

**Principles:** `pr_0c11b762` (Evidence is the primary trust leverage), `pr_20440c77` (Process order is load-bearing).

---

## 2. Core views

Six views. Each specifies **purpose**, **data shown**, **interaction affordances**, **live-update behaviour**, and **principle citations**. All views are workspace-scoped (`pr_0779e5af`) — the workspace switcher is global chrome; no view displays data from a workspace the session is not a member of.

### 2.1 Project view (landing)

- **Purpose:** top-level navigation into the five primitives. Answer "what's happening in this project?" at a glance.
- **Data shown:** project header (id, name, workspace breadcrumb, status); principle count (active); story counts per status; open task count; last-commit on repo; recent activity strip (last 10 ledger rows across the project). Links to story list, task queue, ledger, document browser, repo view.
- **Interaction affordances:** navigate to any of the five primitive views. Focus project ("make this the session default"). Open project settings. No destructive actions on this page.
- **Live-update behaviour:** recent activity strip subscribes to workspace ledger events; new rows prepend on insert. Counts in panel headers (`doc_087842fa` §6 rule 7) re-compute on each ledger event.
- **Principles:** `pr_7ade92ae` (Project is top-level), `pr_c25cc661` (Five primitives shown directly), `pr_0779e5af` (workspace-scoped).

### 2.2 Story view

- **Purpose:** end-to-end audit surface for one story. The single answer for "what has the agent touched?", "has delivery actually shipped?", and "why are we doing this?".
- **Data shown:**
  - Header: story id, title, status, priority, category, tags (incl. `epic:*` and `feature-order:*`), estimated/actual points.
  - **Scope** panel: description + acceptance criteria (rendered markdown).
  - **Source documents** panel: linked `document` ids (per `pr_10c48b6c`) with titles — the "why are we doing this?" anchor.
  - **Contract instances** timeline: one row per CI in sequence order with status, claimed-by, close-row link.
  - **Ledger excerpts** panel: rows filtered to `story_id`, default sort newest-first, filterable by kind (plan / evidence / decision / verdict). Live.
  - **Repo provenance** panel: branch, commit SHA(s) that touched this story, diff-stat against main, CI status. Answers "what has the agent touched?".
  - **Reviewer verdict** panel: story-review row(s) with score + reasoning.
  - **Delivery status** strip: "in_progress / done / blocked", plus for done stories: *commits visible on main?* — the answer to "has delivery actually shipped?" beyond the mere `status=done` flag.
- **Interaction affordances:** claim next CI (if session is owner), close CI (with evidence), file a follow-up story, override a verdict (justification required, recorded on ledger), amend AC (subject to preplan/plan rules).
- **Live-update behaviour:** ledger excerpts, CI timeline, and repo provenance all stream live. Websocket disconnect shows a hairline "reconnecting…" strip in the panel header; on reconnect, the panel refetches the range it missed (see §3 reconnect policy).
- **Principles:** `pr_a9ccecfb` (Story is the unit), `pr_0c11b762` (Evidence-first), `pr_10c48b6c` (Documents drive features), `pr_20440c77` (Process order visible), `pr_0779e5af` (workspace-scoped).

### 2.3 Task queue view

- **Purpose:** live monitor of satellites-agent work. The single answer for "what is the agent doing right now?".
- **Data shown:** table with columns `ID` (muted), `ORIGIN` (story_stage / scheduled / …), `PAYLOAD SUMMARY`, `STATUS` (enqueued / claimed / in_flight / closed), `WORKER`, `ENQUEUED_AT`, `CLAIMED_AT`, `DURATION`. Grouped or filter-toggled into sections: *in_flight*, *enqueued*, *recently closed*.
- **Interaction affordances:** filter by workspace (default = session-member set), origin, status, worker. Click a task row's ID to drill into the task detail (payload + ledger trail). Re-enqueue a timed-out task (admin only). No "mark done from UI" — that violates the lifecycle.
- **Live-update behaviour:** the *in_flight* section is purely live. Each task mutation (dispatched → in_flight → close) flips the row in place with a 1-second highlight. New enqueues appear at the bottom of *enqueued*; closes move the row to *recently closed* with outcome badge. Disconnect: show the "reconnecting…" strip; on reconnect, refetch the entire visible filter set (tasks are small rows; a full refetch is cheaper than replaying events).
- **Principles:** `pr_75826278` (Tasks are the queue), `pr_f81f60ca` (Worker separation — the queue is the work monitor), `pr_0779e5af` (workspace-scoped).

### 2.4 Ledger inspection view

- **Purpose:** the auditable record. Filter by scope (story_id, contract_id, project) and kind (evidence / plan / verdict / action_claim / decision). Read every trail end-to-end.
- **Data shown:** table of ledger rows — `CREATED_AT` (relative + absolute on hover), `TYPE`, `TAGS` (chips, one per tag), `SCOPE` (project/story/contract ids, muted), `CONTENT` (truncated with expand-in-place). Each row links to its parent story and CI.
- **Interaction affordances:** full-text search across content; filter by tag (chips clickable); filter by source (agent / user / system); time range. Copy row id. Export filter result as markdown (for offline review).
- **Live-update behaviour:** ledger page streams new rows into the top of the table when they match the active filter. A "N new rows" pill appears if the user has scrolled away from top; click to jump. Dereferenced rows are hidden by default; a toggle shows them greyed out (consistent with v3 behaviour; `doc_087842fa` §4 `.badge-muted`).
- **Principles:** `pr_0c11b762` (Evidence is the primary trust leverage), `pr_20440c77` (Audit chain reads end-to-end), `pr_0779e5af` (workspace-scoped).

### 2.5 Document browser

- **Purpose:** reach any document in the project, filtered by type (`artifact` / `contract` / `skill` / `principle` / `reviewer` / `design` / `architecture`). See version history. Discover what stories cite each doc.
- **Data shown:** type filter tabs (one per document type). List table: `ID` (muted), `TITLE`, `TYPE` (chip), `CATEGORIES` (chips), `UPDATED_AT`, `VERSION`. Principles filter adds a status chip (active / archived) and scope (system / project).
- **Interaction affordances:** open a document → detail view with rendered markdown body + version history + linked stories (stories that name this doc in `source_documents`). Create (for agents/admins). Archive. Amend — creates a new version, doesn't mutate the current.
- **Live-update behaviour:** list refreshes on document create/update/archive events. Detail view re-renders the body when a new version is ingested.
- **Principles:** `pr_93835b29` (Documents share one schema, type-discriminated), `pr_10c48b6c` (Documents drive features — the browser surfaces this), `pr_0779e5af` (workspace-scoped).

### 2.6 Repo + index view

- **Purpose:** expose the project's indexed repo so developers can answer "where does X live?" without cloning. Surface "what has the agent touched across this branch" at the repo level.
- **Data shown:** repo header (git_remote, default_branch, head_sha, last_indexed_at, index_version, symbol/file counts). Semantic search input (symbol-first, text second). Recent commits on main with touched-files-count + linked stories (commits cite story ids in messages). Branch diff panel — select a branch → see the diff-stat against main + linked story.
- **Interaction affordances:** semantic search runs against the MCP `repo_search` / `repo_search_text` verbs. Click a symbol → source view (via `repo_get_symbol_source`). Click a file → raw content via `repo_get_file`. Trigger a manual re-index (admin only, via `repo_scan`).
- **Live-update behaviour:** the index state chip (*up-to-date / stale / indexing*) reflects live. A push/commit triggers a reindex task; the chip flips to *indexing* during it and back to *up-to-date* on completion. Recent commits list streams new commits on push.
- **Principles:** `pr_c52ba6e8` (Repo is a first-class primitive with a semantic index), `pr_0779e5af` (workspace-scoped).

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

**Principles:** `pr_0c11b762` (Evidence — live state is audit-visible state), `pr_0779e5af` (Workspace-scoped topics), `pr_20440c77` (Lifecycle transitions are first-class events).

---

## 4. Visibility guarantees

The four questions the portal *must* answer, and the single view that resolves each:

| Question | Resolving view | How it resolves |
|----------|----------------|-----------------|
| **What has the agent touched?** | Story view (§2.2) — Repo provenance panel | Branch + commits + diff-stat against main + CI status; the story's full code surface is in one panel. Also usable at repo level (§2.6 "branch diff") for cross-story audits. |
| **What is the agent doing right now?** | Task queue view (§2.3) | *in_flight* section is the live monitor. Filtered by workspace. One row per active task, with origin + payload summary + worker + elapsed duration. No other view exposes "right now" as its primary question. |
| **Has delivery actually shipped?** | Story view (§2.2) — Delivery status strip | For `status=done` stories, the strip explicitly checks *commits visible on main?* — not just the lifecycle flag. Echoes story_3aa15ae7's v3 lesson: a story closed as delivered is not shipped if the commits aren't on the branch that counts. |
| **Why are we doing this?** | Story view (§2.2) — Source documents panel | Lists the `document` rows the story cites (per `pr_10c48b6c`). Click through to the document body. If source_documents is empty, the panel displays the muted copy "No source documents" and flags it — bugs/infra/ops stories are exempt from the document requirement, but feature stories without a document source are a smell. |

Each question has exactly one primary-resolving view; no guarantee is split across multiple primary views. This avoids the v3 pattern where the same question had three partial answers scattered across pages.

**Principles:** `pr_a9ccecfb` (Story is the audit unit), `pr_c52ba6e8` (Repo visibility is non-ceremonial), `pr_75826278` (Task queue is the work monitor), `pr_10c48b6c` (Documents drive features).

---

## 5. Interaction model

- **Alpine.js only** for client interactivity (`doc_087842fa` §5). Each `<section class="panel-headed">` that needs state gets a scoped `x-data`. No React, Vue, or htmx.
- **Server-rendered pages.** Go templates produce the HTML; Alpine hydrates local state and fetches via `/api/…`. No SPA routing.
- **Read-mostly.** Write affordances:
  - *Story:* file, amend AC (subject to lifecycle), claim/close CI (owners), override verdict (with justification), close story (via MCP flow).
  - *Document:* create, amend (new version), archive.
  - *Principle:* create (project-scope), update, archive.
  - *Task:* enqueue (free preplan or manual cron trigger, admin only), re-enqueue a timeout (admin only).
  - *Repo:* trigger manual reindex (admin only).
- **No lifecycle bypass.** No button flips a story to `done` without a `story_close` pass. No button marks a CI `passed` outside the close flow. No button sets a ledger row to `archived` without a reason. `pr_20440c77` is enforced server-side (§ architecture.md §5 process-order gate); the UI doesn't expose bypass affordances.
- **Keyboard:** `/` focuses the page's primary search/filter. Table rows link via the ID cell (a plain `<a>`) — the whole row is not a click target (preserves text selection). Focus-visible outlines per `doc_087842fa` §9.
- **Copy-to-clipboard** on IDs and commit SHAs (pattern from `doc_087842fa` §5 + existing `common.js`). Always toasts the confirmation.
- **Destructive confirmations** via `common.js` `confirm()` dialog (never `window.confirm`).

**Principles:** `pr_20440c77` (No bypass), `pr_f81f60ca` (Worker separation — UI doesn't orchestrate; it renders).

---

## 6. Sketches

Low-fidelity. Shape, not pixels. Each follows the `.panel-headed` primitive from `doc_087842fa` §3.

> Notation: `●` = live indicator (green when connected, muted when disconnected). `▾` = filter/select. `[btn]` = button. `#id` = muted monospace id. `(n)` = count in panel header.

### Sketch 1 — Project view (§2.1)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ WORKSPACE / wksp_xxxx   PROJECT / satellites-v4  #proj_fb8fb884  [FOCUS]    ●│
├──────────────────────────────────────────────────────────────────────────────┤
│ OVERVIEW                                                                     │
│ ┌────────────┬────────────┬────────────┬────────────┬────────────┐          │
│ │ STORIES    │ TASKS      │ LEDGER     │ DOCUMENTS  │ REPO       │          │
│ │ 14 open    │ 0 in_flight│ 1,284 rows │ 17 active  │ on main    │          │
│ │ 9 done     │ 1 queued   │ 3 today    │ 12 princ.  │ head a91… ●│          │
│ └────────────┴────────────┴────────────┴────────────┴────────────┘          │
├──────────────────────────────────────────────────────────────────────────────┤
│ RECENT ACTIVITY (last 10)                                                  ● │
│ 29m ago · kind:story-review   story_18d1646a   verdict approved score 5    │
│ 1h ago  · kind:plan           story_eb2a2a88   plan submitted              │
│ …                                                                           │
├──────────────────────────────────────────────────────────────────────────────┤
│ PRINCIPLES (12 active)                                                       │
│ · pr_7ade92ae  Project is the top-level primitive within a workspace        │
│ · pr_c25cc661  Five primitives per project                                  │
│ …                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sketch 2 — Story view (§2.2)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ STORY  #story_eb2a2a88    feature-order:6                              ● live│
│ Write architecture document — v4 data model, services, protocols, queue…    │
│ [done] [high] [documentation] [8 pts]  epic:v4-foundation                   │
├──────────────────────────────────────────────────────────────────────────────┤
│ SCOPE                                                                        │
│ Description + acceptance criteria rendered markdown…                        │
├──────────────────────────────────────────────────────────────────────────────┤
│ SOURCE DOCUMENTS                                                             │
│ · doc_adfa7adf  satellites_design.md (cite)                                 │
├──────────────────────────────────────────────────────────────────────────────┤
│ CONTRACTS                                                                    │
│ ▸ preplan        #ci_d7806a6a   passed   claimed 21:26   closed 21:27        │
│ ▸ plan           #ci_a70a149d   passed   ...                                 │
│ ▸ develop        #ci_15ce5898   passed   ...                                 │
│ ▸ story_close    #ci_1ba66487   passed   ...                                 │
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
│ #tsk_ab12  story_stage   story_739ae7ef develop   agent_x   00:47 elapsed   │
├──────────────────────────────────────────────────────────────────────────────┤
│ ENQUEUED (0)                                                                 │
│ No tasks enqueued.                                                           │
├──────────────────────────────────────────────────────────────────────────────┤
│ RECENTLY CLOSED (20)                                                         │
│ #tsk_9f3a  story_stage   story_eb2a2a88 story_close   —      success 1m ago │
│ #tsk_1c0d  scheduled     backlog_sweep                —      success 5m ago │
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
│ 29m ago       evidence    phase:story-close       story_18d1646a            │
│               kind:story-review                   ci_1904c6dc    ▸ expand   │
│ 1h ago        plan        phase:plan              story_eb2a2a88            │
│               artifact:plan.md                    ci_a70a149d    ▸ expand   │
│ …                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sketch 5 — Document browser (§2.5)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ DOCUMENTS    (17)                                                            │
│ [ all ] [artifact] [contract] [skill] [principle] [reviewer] [architecture] │
├──────────────────────────────────────────────────────────────────────────────┤
│ ID            TITLE                          TYPE         CATEGORIES         │
│ doc_adfa7adf  Satellites v4 — Architecture   architecture architecture,…     │
│ doc_087842fa  Satellites — Portal Design …   general      general            │
│ pr_7ade92ae   Project is top-level primitive principle    v4, primitive      │
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
│ a91295c7  docs: add v4 architecture document            story_eb2a2a88      │
│ 58a29fd   feat(bootstrap): scaffold satellites-v4 …     story_3de8ac95      │
│ 6e7e345   Initial commit                                —                    │
├──────────────────────────────────────────────────────────────────────────────┤
│ BRANCH DIFF   ▾ select branch                                                │
│ (select a branch to see diff-stat + linked story)                            │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## 7. Out of scope

- **Theming + branding** beyond dark-first / light alternate (`doc_087842fa` §7).
- **Mobile-specific layouts** — the narrow-viewport fallback is in `doc_087842fa`; no separate mobile spec.
- **Internationalisation** — English only for v1.
- **Portal implementation code** — AC7 explicit. This story ships a doc.
- **Onboarding tours / celebratory animations** — would violate `doc_087842fa` §1 ("power tool, not marketing surface").
- **Workflow editor UI** — workflows are the list of CIs on a story; editing is expressed through story_update + contract CRUD on the API surface, not a graph editor.

---

## 8. Principle traceability matrix

| Principle | Primary section(s) | Role |
|-----------|---------------------|------|
| `pr_7ade92ae` Project is top-level | §2.1 Project view chrome | Workspace → Project breadcrumb in every view. |
| `pr_c25cc661` Five primitives | §2 (six views: five primitives + repo) | Views mirror the primitive set; no sibling views for nonexistent primitives. |
| `pr_93835b29` Documents share one schema | §2.5 Document browser | Single browser with type-filter tabs rather than separate browsers per document kind. |
| `pr_a9ccecfb` Story is the unit | §2.2 Story view | The story is the audit unit; three of four visibility guarantees resolve there. |
| `pr_75826278` Tasks are the queue | §2.3 Task queue view | Single live monitor surface; origin diversity hidden behind one table. |
| `pr_f81f60ca` Agent is the worker | §2.3, §5 Interaction model | UI doesn't orchestrate; it renders what the worker did. |
| `pr_441a9fa9` Preplan tight | §2.2 (preplan CI row is one line in the CIs table) | Preplan surfaces as a short CI; not promoted to a dedicated tab. |
| `pr_10c48b6c` Documents drive features | §2.2 Source documents panel + §2.5 "linked stories" | Feature-story → document bidirectional link visible from both sides. |
| `pr_c52ba6e8` Repo is first-class | §2.6 Repo view + §2.2 Repo provenance panel | Repo has its own top-level view and shows up inside the story view. |
| `pr_0c11b762` Evidence is trust leverage | §2.4 Ledger view; §2.2 Ledger excerpts; §3 Live state | Ledger is first-class, live, filterable. |
| `pr_20440c77` Process order invariant | §5 Interaction model (no bypass); §2.2 CI timeline | UI does not expose lifecycle-bypass buttons. |
| `pr_0779e5af` Workspace is multi-tenant primitive | §2 (every view header), §3 (ws topics) | Workspace switcher is global chrome; all views filtered by membership. |

All twelve principles referenced. No orphan principles; no orphan views.

---

## 9. Changelog

| version | date | change |
|---------|------|--------|
| 0.1.0 | 2026-04-23 | Initial UI design document (story_187782e7). Cites doc_087842fa as governing aesthetic brief; cites doc_adfa7adf for architecture primitives. |
