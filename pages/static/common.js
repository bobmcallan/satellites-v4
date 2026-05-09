/* SATELLITES common.js — Alpine.js components */

// Navigation menu: desktop dropdown + mobile slide-out
function navMenu() {
    return {
        dropdownOpen: false,
        mobileOpen: false,
        isMobile() { return window.innerWidth <= 768; },
        toggle() {
            if (this.isMobile()) { this.mobileOpen = true; this.dropdownOpen = false; }
            else { this.dropdownOpen = !this.dropdownOpen; this.mobileOpen = false; }
        },
        closeDropdown() { this.dropdownOpen = false; },
        closeMobile() { this.mobileOpen = false; },
    };
}

// Toast notifications
function toasts() {
    return {
        list: [],
        add(detail) {
            const t = { id: Date.now(), msg: detail.msg || detail, dark: detail.dark || false };
            this.list.push(t);
            setTimeout(() => { this.list = this.list.filter(x => x.id !== t.id); }, 3500);
        }
    };
}

// Panel keys recognised by the project page. Anything in `?expand=`
// not listed here is treated as an entity id (sty_*, doc_*, ci_*, etc.)
// and routed to its owning panel via PANEL_KEYS_BY_ID_PREFIX.
const PANEL_KEYS = [
    'meta', 'stories', 'documents', 'contracts',
    'configuration', 'repo', 'changelog', 'ledger',
];
const PANEL_KEYS_SET = (function () {
    const m = {};
    for (let i = 0; i < PANEL_KEYS.length; i++) { m[PANEL_KEYS[i]] = true; }
    return m;
})();
const PANEL_KEYS_BY_ID_PREFIX = {
    'sty_': 'stories',
    'doc_': 'documents',
    'ci_':  'contracts',
    'ldg_': 'ledger',
    'cl_':  'changelog',
};
function panelForEntityID(token) {
    for (const pfx in PANEL_KEYS_BY_ID_PREFIX) {
        if (token.indexOf(pfx) === 0) { return PANEL_KEYS_BY_ID_PREFIX[pfx]; }
    }
    return '';
}

// panels store (sty_70c0f7a3, sty_43d72112) — URL is the source of truth
// for which panels are open on /projects/{id}. `?expand=` is a
// comma-separated set whose tokens are EITHER panel keys (stories,
// documents, ...) OR entity ids (sty_*, doc_*, ci_*). Entity-id tokens
// implicitly open their owning panel; default-open panels (per
// `data-default-expanded`) are layered on top, never erased by a
// non-empty URL list. Toggling rewrites the URL via history.replaceState
// (defaults are not written — the bare URL stays bare).
function panelsStoreInit(store) {
    const params = new URLSearchParams(window.location.search);
    if (!params.has('expand')) { return; }
    const tokens = params.get('expand').split(',').map(s => s.trim()).filter(Boolean);
    const next = Object.assign({}, store._expanded);
    for (let i = 0; i < tokens.length; i++) {
        const t = tokens[i];
        if (PANEL_KEYS_SET[t]) {
            next[t] = true;
        } else {
            const owner = panelForEntityID(t);
            if (owner) { next[owner] = true; }
        }
    }
    store._expanded = next;
}

// sectionToggle (story_25695308; URL-state for sty_70c0f7a3) —
// collapsible panel-headed section. Open state lives in the `panels`
// Alpine store, which mirrors the `?expand=...` URL param. The stable
// per-panel identifier is read from `data-toggle-key` on the host
// element; `data-default-expanded` ("true"/"false") is the fallback
// when the URL has no `expand` param.
function sectionToggle() {
    return {
        _key: '',
        init() {
            const ds = (this.$el && this.$el.dataset) || {};
            this._key = ds.toggleKey || '';
            const def = ds.defaultExpanded === 'true';
            const store = Alpine.store('panels');
            if (!this._key || !store) { return; }
            // sty_43d72112 — defaults always layer on top of URL state.
            // Track default-open keys so _writeURL can skip them and the
            // bare-URL invariant survives the user toggling other panels.
            if (def) {
                store._defaults = Object.assign({}, store._defaults, { [this._key]: true });
                if (!store._expanded[this._key]) {
                    store._expanded = Object.assign({}, store._expanded, { [this._key]: true });
                }
            }
        },
        toggle() {
            const store = Alpine.store('panels');
            if (!this._key || !store) { return; }
            store.toggle(this._key);
        },
        get open() {
            const store = Alpine.store('panels');
            return !!(store && store._expanded[this._key]);
        },
        get caret() { return this.open ? '▾' : '▸'; },
        get collapsed() { return !this.open; },
        get hiddenWhenOpen() { return this.open ? '' : 'is-hidden'; },
    };
}

// storyPanel (sty_70c0f7a3 + sty_6300fb27 + sty_48198f3e) — V3-style
// story panel.
//
// Parses these tokens out of the query (plus free-text against
// data-search):
//   - `order:<field>` — updated|created|priority|status|title — re-orders
//     the visible rows client-side.
//   - `status:<value>` — all|open|done|cancelled|... — comma-separated
//     values OR. Default chip is `status:open`, which matches any row
//     whose status is NOT done/cancelled.
//   - `priority:<value>` — critical|high|medium|low|all — comma-separated.
//   - `category:<value>` — feature|bug|improvement|... — comma-separated.
//   - `tags:<value>` — single tag; multiple tags compose with AND.
//   - free text — matched against data-search (id + title + tags haystack).
//
// Clicking a tag-chip appends `tags:<tag>` to the query (V3 parity).
// `_filterTick` is a reactive counter the realtime bridge bumps when a
// `story.<status>` WS frame patches a row's data-status, so x-show
// re-evaluates against the new status (sty_48198f3e bug fix — Alpine
// doesn't track dataset mutations natively).
function storyPanel() {
    return {
        query: '',
        expanded: '',
        // Reactive bump counter for realtime row patches. Read from
        // matchesRow so x-show re-runs when bumped.
        _filterTick: 0,
        // sty_1d6751e9 + sty_01f75142 — bulk-select state for the
        // operator-status-override surface. selectedIDs is a Set keyed
        // by story id; bulkTarget holds the target enum; bulkBusy
        // disables apply during an in-flight Promise.all;
        // bulkResultText carries the "applied N / failed M" strip.
        selectedIDs: new Set(),
        bulkTarget: 'ready',
        bulkBusy: false,
        bulkResultText: '',
        get tokens() { return parseStoryQuery(this.query); },
        matchesRow(el) {
            // Touch the reactive counter so dataset mutations applied
            // by _applyStoryEvent re-trigger x-show evaluation.
            void this._filterTick;
            const ds = (el && el.dataset) || {};
            // sty_a34cd88f — pinned-expand bypass. When a story is
            // expanded, its row stays visible regardless of any filter.
            // Prevents the orphan-expand artefact when status flips to
            // a value the chip filter excludes (e.g. operator marks it
            // done while the default status:open chip is active).
            if (ds.id && ds.id === this.expanded) { return true; }
            const t = this.tokens;
            // status: chip default is `open` (= not done, not cancelled).
            // `status:all` lifts any status filtering. Multi-value lists
            // OR (any-of).
            if (t.status.length === 0) {
                if (ds.status === 'done' || ds.status === 'cancelled') { return false; }
            } else if (t.status.indexOf('all') === -1) {
                let ok = false;
                for (let i = 0; i < t.status.length; i++) {
                    const s = t.status[i];
                    if (s === 'open') {
                        if (ds.status !== 'done' && ds.status !== 'cancelled') { ok = true; break; }
                    } else if (ds.status === s) { ok = true; break; }
                }
                if (!ok) { return false; }
            }
            // priority: any-of (or `all` lifts).
            if (t.priority.length > 0 && t.priority.indexOf('all') === -1) {
                if (t.priority.indexOf((ds.priority || '').toLowerCase()) === -1) { return false; }
            }
            // category: any-of (or `all` lifts).
            if (t.category.length > 0 && t.category.indexOf('all') === -1) {
                if (t.category.indexOf((ds.category || '').toLowerCase()) === -1) { return false; }
            }
            // tags: AND across the chip list. data-tags is a
            // space-delimited string ("foo bar baz ").
            if (t.tags.length > 0) {
                const rowTags = ' ' + (ds.tags || '').toLowerCase() + ' ';
                for (let i = 0; i < t.tags.length; i++) {
                    if (rowTags.indexOf(' ' + t.tags[i] + ' ') === -1) { return false; }
                }
            }
            if (!t.text) { return true; }
            const hay = (ds.search || '').toLowerCase();
            return hay.indexOf(t.text) !== -1;
        },
        isExpanded(el) {
            const id = (el && el.dataset && el.dataset.detailFor) || '';
            return !!id && this.expanded === id;
        },
        rowClass(el) {
            const id = (el && el.dataset && el.dataset.id) || '';
            return id && this.expanded === id ? 'is-expanded' : '';
        },
        toggleRow(ev) {
            const target = ev && ev.currentTarget;
            const id = (target && target.dataset && target.dataset.id) || '';
            if (!id) { return; }
            this.expanded = this.expanded === id ? '' : id;
            this._syncExpandToURL();
        },
        // sty_a34cd88f + sty_43d72112 — mirror this.expanded into the
        // URL via history.replaceState so reloads + bookmarks keep the
        // expansion. The `?expand=` slot is shared with the panels
        // store: storyPanel owns the sty_* token only and preserves
        // every other token (panel keys + non-sty entity ids).
        _syncExpandToURL() {
            if (typeof window === 'undefined' || !window.history || typeof window.history.replaceState !== 'function') { return; }
            try {
                const url = new URL(window.location.href);
                const current = (url.searchParams.get('expand') || '')
                    .split(',').map(s => s.trim()).filter(Boolean);
                const others = current.filter(t => t.indexOf('sty_') !== 0);
                const next = this.expanded ? others.concat([this.expanded]) : others;
                if (next.length) { url.searchParams.set('expand', next.join(',')); }
                else { url.searchParams.delete('expand'); }
                window.history.replaceState(window.history.state, '', url.toString());
            } catch (err) { /* URL ctor or replaceState unavailable — best effort */ }
        },
        addTagToQuery(ev) {
            const target = ev && ev.currentTarget;
            const tag = (target && target.dataset && target.dataset.tag) || '';
            if (!tag) { return; }
            const token = 'tags:' + tag;
            const q = (this.query || '').trim();
            // Avoid duplicate tokens — both the tags:<tag> form and the
            // bare-tag form V3 used to write.
            const parts = q.length ? q.split(/\s+/) : [];
            if (parts.indexOf(token) !== -1 || parts.indexOf(tag) !== -1) { return; }
            parts.push(token);
            this.query = parts.join(' ');
        },
        // Effective chip list for the chip strip beneath the search
        // input. Defaults (status:open, priority:all, category:all)
        // appear when the user hasn't overridden that key. User-entered
        // key:value tokens appear after the defaults; free text gets a
        // single `search:<text>` chip. V3 parity (sty_48198f3e).
        getEffectiveChips() {
            const t = this.tokens;
            const chips = [];
            if (t.status.length === 0) {
                chips.push({ key: 'status', value: 'open', isDefault: true });
            } else {
                for (let i = 0; i < t.status.length; i++) {
                    chips.push({ key: 'status', value: t.status[i], isDefault: false });
                }
            }
            if (t.priority.length === 0) {
                chips.push({ key: 'priority', value: 'all', isDefault: true });
            } else {
                for (let i = 0; i < t.priority.length; i++) {
                    chips.push({ key: 'priority', value: t.priority[i], isDefault: false });
                }
            }
            if (t.category.length === 0) {
                chips.push({ key: 'category', value: 'all', isDefault: true });
            } else {
                for (let i = 0; i < t.category.length; i++) {
                    chips.push({ key: 'category', value: t.category[i], isDefault: false });
                }
            }
            for (let i = 0; i < t.tags.length; i++) {
                chips.push({ key: 'tags', value: t.tags[i], isDefault: false });
            }
            if (t.order) {
                chips.push({ key: 'order', value: t.order, isDefault: false });
            }
            if (t.text) {
                chips.push({ key: 'search', value: t.text, isDefault: false });
            }
            return chips;
        },
        // Strip a key:value (or free-text) token from the query. Default
        // chips are no-ops — a `status:open` default chip becomes a
        // user-set chip the moment the user types `status:done`, so
        // dismissing the default has no token to remove.
        removeChip(key, value) {
            if (!key) { return; }
            if (key === 'search') {
                // Drop free-text by stripping all non-key:value tokens.
                const parts = (this.query || '').trim().split(/\s+/).filter(Boolean);
                const kept = [];
                for (let i = 0; i < parts.length; i++) {
                    if (parts[i].indexOf(':') > 0) { kept.push(parts[i]); }
                }
                this.query = kept.join(' ');
                return;
            }
            const parts = (this.query || '').trim().split(/\s+/).filter(Boolean);
            const kept = [];
            for (let i = 0; i < parts.length; i++) {
                const p = parts[i];
                const idx = p.indexOf(':');
                if (idx <= 0) { kept.push(p); continue; }
                const k = p.slice(0, idx).toLowerCase();
                const v = p.slice(idx + 1).toLowerCase();
                if (k !== key) { kept.push(p); continue; }
                if (k === 'tags' || k === 'order') {
                    if (v !== String(value).toLowerCase()) { kept.push(p); }
                    continue;
                }
                // Comma-separated values: drop matching entry, keep rest.
                const vals = v.split(',').filter(s => s !== String(value).toLowerCase());
                if (vals.length > 0) { kept.push(k + ':' + vals.join(',')); }
            }
            this.query = kept.join(' ');
        },
        // Reset to defaults — drops every token, including free text.
        clearAllFilters() { this.query = ''; },
        // Apply `order:<field>` by physically reordering the table's
        // tbody after a query change. Triggered via a watcher Alpine
        // sets up in init(); kept side-effect-y rather than reactive
        // because re-sorting visible DOM rows is the simplest path.
        init() {
            this._readExpandFromURL();
            this.$watch('query', () => { applyStoryOrder(this.$el, this.tokens.order); });
            // Apply once on mount so any initial query (e.g. via #hash) takes effect.
            this.$nextTick(() => { applyStoryOrder(this.$el, this.tokens.order); });
            this._attachRealtimeBridge();
        },
        // sty_a34cd88f + sty_43d72112 — seed this.expanded from the
        // first sty_ token in the comma-separated `?expand=` list so
        // reloads + bookmarks restore the expansion regardless of
        // which other tokens (panel keys, doc ids, etc.) ride along.
        _readExpandFromURL() {
            if (typeof window === 'undefined' || !window.location) { return; }
            try {
                const url = new URL(window.location.href);
                const tokens = (url.searchParams.get('expand') || '')
                    .split(',').map(s => s.trim()).filter(Boolean);
                for (let i = 0; i < tokens.length; i++) {
                    if (tokens[i].indexOf('sty_') === 0) {
                        this.expanded = tokens[i];
                        return;
                    }
                }
            } catch (err) { /* best effort */ }
        },
        destroy() {
            if (this._ws && typeof this._ws.close === 'function') { this._ws.close(); }
        },
        // sty_1d6751e9 — operator status override (per-row + bulk). The
        // POST endpoint /api/stories/{id}/status takes {status, reason}
        // and delegates to stores.UpdateStatus, which emits the canonical
        // story.status_change event. The realtime bridge above patches
        // the row in place on that event — no extra plumbing here.
        isSelected(id) {
            void this._filterTick;
            return this.selectedIDs.has(id);
        },
        toggleRowSelection(id, ev) {
            if (ev) { ev.stopPropagation(); }
            if (this.selectedIDs.has(id)) {
                this.selectedIDs.delete(id);
            } else {
                this.selectedIDs.add(id);
            }
            // Clone the Set so Alpine sees a reactive write — `Set` mutations
            // don't trigger reactivity on Alpine's proxy.
            this.selectedIDs = new Set(this.selectedIDs);
            this.bulkResultText = '';
        },
        clearSelection() {
            this.selectedIDs = new Set();
            this.bulkResultText = '';
        },
        async applyRowStatus(id, target) {
            const res = await this._postStatus(id, target);
            if (!res.ok) {
                console.warn('storyPanel: row status failed', id, res.status, res.body);
            }
            return res;
        },
        async applySelectionStatus() {
            if (this.bulkBusy || this.selectedIDs.size === 0) { return; }
            this.bulkBusy = true;
            this.bulkResultText = '';
            const ids = Array.from(this.selectedIDs);
            const target = this.bulkTarget;
            const results = await Promise.all(ids.map(id => this._postStatus(id, target)));
            let applied = 0, failed = 0;
            for (const r of results) {
                if (r.ok) { applied++; } else { failed++; }
            }
            this.bulkResultText = 'applied ' + applied + ' / failed ' + failed;
            this.bulkBusy = false;
        },
        async _postStatus(id, target) {
            try {
                const resp = await fetch('/api/stories/' + encodeURIComponent(id) + '/status', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    credentials: 'same-origin',
                    body: JSON.stringify({ status: target }),
                });
                const text = await resp.text();
                return { ok: resp.ok, status: resp.status, body: text };
            } catch (err) {
                return { ok: false, status: 0, body: String(err) };
            }
        },
        // sty_af303c26 (focused slice) — open a workspace-scoped WebSocket
        // and apply `story.<status>` events to the matching story row in
        // place. Existing `internal/story/emit.go` already publishes the
        // event on every UpdateStatus; we just patch the DOM in
        // < 500 ms. Document/contract/ledger panels are not yet wired —
        // they ship in the realtime epic.
        _attachRealtimeBridge() {
            if (!window.SATELLITES_WS || !window.SATELLITES_WS.workspaceId) { return; }
            if (!window.SatellitesWS) { return; }
            const projectID = this._readProjectID();
            if (!projectID) { return; }
            const self = this;
            this._ws = new window.SatellitesWS({
                workspaceId: window.SATELLITES_WS.workspaceId,
                debug: !!window.SATELLITES_WS.debug,
                onEvent: function (ev) { self._applyEvent(ev, projectID); },
            });
            this._ws.connect();
        },
        _readProjectID() {
            const host = document.querySelector('[data-project-id]');
            return host ? (host.dataset.projectId || '') : '';
        },
        // sty_f4b87ea3 — single dispatch for the workspace WS frame.
        // sty_a03449d1 retired contract_instance.* events (the rows
        // are gone) and consumes task.<status> events instead — the
        // task chain IS the workflow, so each task transition patches
        // the matching <tr> in place. Successor tasks minted with
        // prior_task_id (the retry chain) are appended in created_at
        // order.
        _applyEvent(ev, projectID) {
            if (!ev || !ev.Kind) { return; }
            if (ev.Kind.indexOf('story.') === 0) {
                this._applyStoryEvent(ev, projectID);
                return;
            }
            if (ev.Kind.indexOf('task.') === 0) {
                this._applyTaskEvent(ev, projectID);
                return;
            }
        },
        _applyStoryEvent(ev, projectID) {
            const data = ev.Data || ev.data || {};
            if (data.project_id && data.project_id !== projectID) { return; }
            const storyID = data.story_id;
            if (!storyID) { return; }
            const newStatus = ev.Kind.substring('story.'.length);
            if (!newStatus) { return; }
            const row = this.$el.querySelector('tr.story-row[data-id="' + storyID + '"]');
            if (!row) {
                // sty_1ff1065a: cold row — substrate just emitted a
                // story.<status> event for a story we haven't rendered.
                // Synthesise a row from the payload + bump _filterTick.
                this._appendStoryRow(storyID, newStatus, data);
                return;
            }
            row.dataset.status = newStatus;
            const pill = row.querySelector('.col-status .status-pill');
            if (pill) { pill.textContent = newStatus; }
            row.setAttribute('data-realtime-updated-at', String(Date.now()));
            // sty_48198f3e: bump the reactive counter so x-show
            // re-evaluates against the new dataset.status. Alpine
            // doesn't track DOM dataset mutations as reactive deps;
            // this counter is the explicit signal that filters need
            // to re-run.
            this._filterTick++;
        },
        // sty_1ff1065a — append a freshly-created story row to the
        // panel without a refetch. The substrate's story.Create emit
        // (internal/story/emit.go) carries every data-* attribute the
        // chip filter reads: status, priority, category, tags, plus
        // title for the visible cell and updated_at for ordering.
        // When the panel is in its empty state (no <table>), we swap
        // the "No matches" placeholder for a fresh skeleton table.
        _appendStoryRow(storyID, status, data) {
            const root = this.$el;
            if (!root) { return; }
            let table = root.querySelector('table.panel-table-stories');
            if (!table) {
                const empty = root.querySelector('p[data-testid="panel-stories-empty"]');
                if (empty) { empty.remove(); }
                table = document.createElement('table');
                table.className = 'panel-table panel-table-stories';
                table.setAttribute('data-testid', 'panel-stories-table');
                table.innerHTML =
                    '<thead><tr>' +
                    '<th class="col-select"><span class="visually-hidden">select</span></th>' +
                    '<th class="col-id">id</th>' +
                    '<th class="col-title">title</th>' +
                    '<th class="col-status">status</th>' +
                    '<th class="col-priority">priority</th>' +
                    '<th class="col-updated">updated</th>' +
                    '</tr></thead><tbody></tbody>';
                root.appendChild(table);
            }
            const tbody = table.querySelector('tbody');
            if (!tbody) { return; }
            const title = (data && data.title) ? String(data.title) : '';
            const priority = (data && data.priority) ? String(data.priority) : '';
            const category = (data && data.category) ? String(data.category) : '';
            const updated = (data && data.updated_at) ? String(data.updated_at) : '';
            const tagsArr = (data && Array.isArray(data.tags)) ? data.tags : [];
            const tagsStr = tagsArr.join(' ') + (tagsArr.length ? ' ' : '');
            const search = (storyID + ' ' + title + ' ' + tagsStr).toLowerCase();

            const row = document.createElement('tr');
            row.className = 'story-row';
            row.dataset.id = storyID;
            row.dataset.status = status;
            row.dataset.priority = priority;
            row.dataset.category = category;
            row.dataset.title = title;
            row.dataset.updated = updated;
            row.dataset.tags = tagsStr;
            row.dataset.search = search;
            row.setAttribute('data-testid', 'story-row-' + storyID);
            row.setAttribute('data-realtime-updated-at', String(Date.now()));
            row.setAttribute('x-show', 'matchesRow($el)');
            row.setAttribute(':class', 'rowClass($el)');
            row.setAttribute('@click', 'toggleRow');
            const tagChips = tagsArr.map(t =>
                '<button type="button" class="tag-chip is-clickable" data-tag="' + this._escape(t) + '" @click.stop="addTagToQuery" title="Click to filter by this tag">' + this._escape(t) + '</button>'
            ).join('');
            row.innerHTML =
                '<td class="col-select" @click.stop>' +
                '<input type="checkbox"' +
                ' data-testid="story-row-select-' + this._escape(storyID) + '"' +
                ' :checked="isSelected(\'' + this._escape(storyID) + '\')"' +
                ' @change="toggleRowSelection(\'' + this._escape(storyID) + '\', $event)" />' +
                '</td>' +
                '<td class="col-id"><code>' + this._escape(storyID) + '</code></td>' +
                '<td class="col-title">' +
                '<div class="story-row-title">' + this._escape(title) + '</div>' +
                (tagsArr.length ? '<div class="story-row-tags" data-testid="story-row-tags-' + this._escape(storyID) + '">' + tagChips + '</div>' : '') +
                '</td>' +
                '<td class="col-status"><code class="status-pill">' + this._escape(status) + '</code></td>' +
                '<td class="col-priority">' + (priority ? '<code class="priority-pill">' + this._escape(priority) + '</code>' : '') + '</td>' +
                '<td class="col-updated muted">' + this._escape(updated) + '</td>';

            const detail = document.createElement('tr');
            detail.className = 'story-detail';
            detail.dataset.detailFor = storyID;
            detail.setAttribute('data-testid', 'story-detail-' + storyID);
            detail.setAttribute('x-show', 'isExpanded($el)');
            detail.innerHTML =
                '<td colspan="6">' +
                '<div class="story-detail-flat">' +
                '<section class="story-detail-block"><h4>description</h4><p class="muted">—</p></section>' +
                '<section class="story-detail-block"><h4>acceptance criteria</h4><p class="muted">—</p></section>' +
                '<section class="story-detail-block" data-story-tasks="' + this._escape(storyID) + '"><h4>tasks</h4><p class="muted" data-testid="story-tasks-empty-' + this._escape(storyID) + '">No tasks</p></section>' +
                '</div></td>';

            tbody.insertBefore(row, tbody.firstChild);
            tbody.insertBefore(detail, row.nextSibling);
            this._filterTick++;
        },
        // sty_a03449d1 — patch a task row inside the expanded
        // story detail. The task event payload (task/emit.go) carries
        // workspace_id, project_id, story_id, task_id, origin,
        // priority, and outcome (closed-only). We locate the tasks
        // sub-table by data-story-tasks=<story_id> and update the row
        // keyed by data-task-id; new task ids append a skeleton row.
        // Events for unrendered stories (panel filtered out, story not
        // expanded) find no host and silently drop.
        _applyTaskEvent(ev, projectID) {
            const data = ev.Data || ev.data || {};
            const storyID = data.story_id;
            const taskID = data.task_id;
            if (!storyID || !taskID) { return; }
            if (data.project_id && projectID && data.project_id !== projectID) { return; }
            const host = this.$el.querySelector('section[data-story-tasks="' + storyID + '"]');
            if (!host) { return; }
            const newStatus = ev.Kind.substring('task.'.length);
            if (!newStatus) { return; }
            const row = host.querySelector('tr.story-task-row[data-task-id="' + taskID + '"]');
            if (row) {
                row.dataset.status = newStatus;
                const pill = row.querySelector('.status-pill');
                if (pill) {
                    pill.textContent = newStatus;
                    pill.className = 'status-pill status-' + newStatus;
                }
                if (data.outcome) {
                    const outcomeCell = row.querySelector('.col-outcome');
                    if (outcomeCell) {
                        outcomeCell.innerHTML = '<code class="outcome-' + this._escape(data.outcome) + '" data-testid="story-task-outcome-' + this._escape(taskID) + '">' + this._escape(data.outcome) + '</code>';
                    }
                }
                row.setAttribute('data-realtime-updated-at', String(Date.now()));
                return;
            }
            this._appendTaskRow(host, storyID, taskID, newStatus, data);
        },
        _appendTaskRow(host, storyID, taskID, status, data) {
            let table = host.querySelector('table.panel-table-tasks');
            if (!table) {
                const empty = host.querySelector('p.muted');
                if (empty) { empty.remove(); }
                table = document.createElement('table');
                table.className = 'panel-table panel-table-tasks';
                table.setAttribute('data-testid', 'story-tasks-' + storyID);
                table.innerHTML = '<thead><tr><th class="col-seq">#</th><th class="col-kind">kind</th><th class="col-name">action</th><th class="col-status">status</th><th class="col-iter">iter</th><th class="col-outcome">outcome</th><th class="col-claimed">claimed by</th><th class="col-verdict">verdict</th></tr></thead><tbody></tbody>';
                host.appendChild(table);
            }
            const tbody = table.querySelector('tbody');
            if (!tbody) { return; }
            const seq = tbody.querySelectorAll('tr.story-task-row').length + 1;
            const kind = (data && data.kind) ? String(data.kind) : 'work';
            const action = (data && data.action) ? String(data.action) : '';
            const contractName = action.indexOf('contract:') === 0 ? action.substring('contract:'.length) : action;
            const tr = document.createElement('tr');
            tr.className = 'story-task-row' + (status === 'planned' ? ' status-planned' : '');
            tr.dataset.taskId = taskID;
            tr.dataset.status = status;
            tr.dataset.kind = kind;
            tr.setAttribute('data-testid', 'story-task-row-' + taskID);
            tr.setAttribute('data-realtime-updated-at', String(Date.now()));
            tr.innerHTML =
                '<td class="col-seq"><code>#' + seq + '</code></td>' +
                '<td class="col-kind"><code class="task-kind-' + this._escape(kind) + '">' + this._escape(kind) + '</code></td>' +
                '<td class="col-name"><code class="ci-name">' + this._escape(contractName) + '</code></td>' +
                '<td class="col-status"><code class="status-pill status-' + this._escape(status) + '" data-testid="story-task-status-' + this._escape(taskID) + '">' + this._escape(status) + '</code></td>' +
                '<td class="col-iter"><span class="muted">—</span></td>' +
                '<td class="col-outcome"><span class="muted">—</span></td>' +
                '<td class="col-claimed"><span class="muted">—</span></td>' +
                '<td class="col-verdict"><span class="muted">—</span></td>';
            tbody.appendChild(tr);
        },
        _escape(s) {
            return String(s == null ? '' : s)
                .replace(/&/g, '&amp;')
                .replace(/</g, '&lt;')
                .replace(/>/g, '&gt;')
                .replace(/"/g, '&quot;')
                .replace(/'/g, '&#39;');
        },
    };
}

// parseStoryQuery splits a story-panel query string into structured
// tokens. V3 parity (sty_48198f3e):
//   - `order:<field>` (single) — updated|created|priority|status|title.
//   - `status:<value>` — comma-separated list (`status:open,done`).
//   - `priority:<value>` — comma-separated list.
//   - `category:<value>` — comma-separated list.
//   - `tags:<value>` — single tag per token; multiple `tags:` tokens
//     compose. The `tag:` alias maps to `tags:` for V3 input parity.
//   - free text — anything else, lower-cased, joined with spaces.
//
// Unknown colon-tokens flow through as free text so a bare `epic:foo`
// (the V3 wire-format that pre-dates the `tags:` prefix) still
// matches the data-search haystack via free-text path.
function parseStoryQuery(q) {
    const out = {
        order: '',
        status: [],
        priority: [],
        category: [],
        tags: [],
        text: '',
        // statusOverride retained for back-compat with any external
        // caller; equivalent to status.length > 0 today.
        statusOverride: false,
    };
    const free = [];
    const parts = (q || '').trim().split(/\s+/).filter(Boolean);
    const orderFields = { updated: 1, created: 1, priority: 1, status: 1, title: 1 };
    for (let i = 0; i < parts.length; i++) {
        const p = parts[i];
        const idx = p.indexOf(':');
        if (idx > 0) {
            const k = p.slice(0, idx).toLowerCase();
            const v = p.slice(idx + 1).toLowerCase();
            if (k === 'order' && orderFields[v]) { out.order = v; continue; }
            if (k === 'status') {
                const vals = v.split(',').filter(Boolean);
                for (let j = 0; j < vals.length; j++) { out.status.push(vals[j]); }
                out.statusOverride = true;
                continue;
            }
            if (k === 'priority') {
                const vals = v.split(',').filter(Boolean);
                for (let j = 0; j < vals.length; j++) { out.priority.push(vals[j]); }
                continue;
            }
            if (k === 'category') {
                const vals = v.split(',').filter(Boolean);
                for (let j = 0; j < vals.length; j++) { out.category.push(vals[j]); }
                continue;
            }
            if (k === 'tags' || k === 'tag') {
                if (v) { out.tags.push(v); }
                continue;
            }
        }
        free.push(p.toLowerCase());
    }
    out.text = free.join(' ');
    return out;
}

// applyStoryOrder physically sorts the tbody rows of the story-panel
// table. Each story has TWO rows (the row itself + the detail row);
// pairs are kept together so expand-on-click still targets the right
// detail. host is the panel root element; field is the parsed
// `order:<field>` value (empty = leave default order).
function applyStoryOrder(host, field) {
    if (!host || !field) { return; }
    const tbody = host.querySelector('tbody');
    if (!tbody) { return; }
    const rows = tbody.querySelectorAll('tr.story-row');
    // Build pairs: [row, detail] keyed by data-id; preserve original index for stable sort.
    const pairs = [];
    rows.forEach((row, idx) => {
        const detail = tbody.querySelector('tr.story-detail[data-detail-for="' + row.dataset.id + '"]');
        pairs.push({ row, detail, idx });
    });
    pairs.sort((a, b) => {
        const aval = (a.row.dataset[field] || '').toLowerCase();
        const bval = (b.row.dataset[field] || '').toLowerCase();
        if (aval === bval) { return a.idx - b.idx; }
        // Most fields read better newest-first; title sorts ascending.
        if (field === 'title') { return aval < bval ? -1 : 1; }
        return aval < bval ? 1 : -1;
    });
    for (let i = 0; i < pairs.length; i++) {
        tbody.appendChild(pairs[i].row);
        if (pairs[i].detail) { tbody.appendChild(pairs[i].detail); }
    }
}

// footerStatus (sty_558c0431) — three-slot footer Alpine factory.
// Owns the local uptime tick and the /api/health poll cycle. The right
// slot is data-driven from `footerStatusItems` — adding a status badge
// is one entry there + one corresponding field on /api/health.
function footerStatusItems() {
    return [
        { id: 'gemini', label: 'gemini', field: 'gemini', testid: 'footer-gemini' },
    ];
}

function footerStatus() {
    return {
        items: footerStatusItems(),
        status: {},
        uptimeSeconds: 0,
        startedAtMs: 0,
        _tickHandle: null,
        _pollHandle: null,
        _visHandler: null,
        async init() {
            // Drive the visible counter off the server's `started_at`
            // when we have it, otherwise off the page-load instant. The
            // first /api/health response replaces this anchor.
            this.startedAtMs = Date.now();
            this._tickHandle = setInterval(() => { this.uptimeSeconds = Math.max(0, Math.floor((Date.now() - this.startedAtMs) / 1000)); }, 1000);
            this._visHandler = () => { if (!document.hidden) { this.poll(); } };
            document.addEventListener('visibilitychange', this._visHandler);
            await this.poll();
            this.schedulePoll();
        },
        destroy() {
            if (this._tickHandle) { clearInterval(this._tickHandle); }
            if (this._pollHandle) { clearTimeout(this._pollHandle); }
            if (this._visHandler) { document.removeEventListener('visibilitychange', this._visHandler); }
        },
        schedulePoll() {
            if (this._pollHandle) { clearTimeout(this._pollHandle); }
            // 30 s while visible; pause while hidden — visibilitychange resumes.
            this._pollHandle = setTimeout(async () => {
                if (!document.hidden) { await this.poll(); }
                this.schedulePoll();
            }, 30000);
        },
        async poll() {
            try {
                const r = await fetch('/api/health', { credentials: 'same-origin' });
                if (!r.ok && r.status !== 503) { return; }
                const j = await r.json();
                this.status = j || {};
                if (typeof j.started_at === 'string') {
                    const t = Date.parse(j.started_at);
                    if (!isNaN(t)) { this.startedAtMs = t; }
                }
            } catch (e) { /* swallow — next poll retries */ }
        },
        get uptimeLabel() {
            const s = this.uptimeSeconds;
            const h = Math.floor(s / 3600);
            const m = Math.floor((s % 3600) / 60);
            const ss = s % 60;
            if (h > 0) { return 'agent up ' + h + 'h ' + m + 'm ' + ss + 's'; }
            return 'agent up ' + m + 'm ' + ss + 's';
        },
        badgeClass(item) {
            const v = (this.status && this.status[item.field]) || '';
            if (v === 'ok') { return 'is-ok'; }
            if (v === 'configured') { return 'is-amber'; }
            if (v === 'unreachable') { return 'is-error'; }
            return 'is-muted';
        },
    };
}

// Delegated click handler for .mcp-copy-btn (project meta panel
// sty_0495f550). The button carries data-copy-source=<url>; clicking
// it copies that string to the clipboard and briefly swaps the
// button label to "[copied]" as feedback. Falls back to a hidden-
// textarea selection when navigator.clipboard is unavailable
// (older browsers / non-secure contexts).
function copyToClipboard(text) {
    if (navigator && navigator.clipboard && navigator.clipboard.writeText) {
        return navigator.clipboard.writeText(text);
    }
    return new Promise((resolve, reject) => {
        try {
            const ta = document.createElement('textarea');
            ta.value = text;
            ta.style.position = 'fixed';
            ta.style.left = '-9999px';
            document.body.appendChild(ta);
            ta.select();
            const ok = document.execCommand('copy');
            document.body.removeChild(ta);
            if (ok) { resolve(); } else { reject(new Error('copy command failed')); }
        } catch (e) { reject(e); }
    });
}

document.addEventListener('click', function (ev) {
    const target = ev.target;
    if (!target || !target.classList || !target.classList.contains('mcp-copy-btn')) { return; }
    const source = target.getAttribute('data-copy-source') || '';
    if (!source) { return; }
    ev.preventDefault();
    const original = target.textContent;
    copyToClipboard(source).then(() => {
        target.textContent = '[copied]';
        target.setAttribute('data-copied', 'true');
    }).catch(() => {
        target.textContent = '[copy failed]';
    }).finally(() => {
        setTimeout(() => {
            target.textContent = original;
            target.removeAttribute('data-copied');
        }, 1500);
    });
});

document.addEventListener('alpine:init', () => {
    Alpine.store('panels', {
        _expanded: {},
        _defaults: {},
        init() { panelsStoreInit(this); },
        isOpen(key) { return !!this._expanded[key]; },
        toggle(key) {
            const next = Object.assign({}, this._expanded);
            if (next[key]) { delete next[key]; } else { next[key] = true; }
            this._expanded = next;
            this._writeURL();
        },
        // sty_43d72112 — write only the user-set delta (open panels that
        // are NOT default-open) to the URL, and preserve any entity-id
        // tokens (sty_*, doc_*, ci_*) the storyPanel and friends own.
        // Two writers (panels + storyPanel) share the `?expand=` slot;
        // each owns its own token class and merges with the other.
        _writeURL() {
            try {
                const url = new URL(window.location.href);
                const current = (url.searchParams.get('expand') || '')
                    .split(',').map(s => s.trim()).filter(Boolean);
                const entityTokens = current.filter(t => !PANEL_KEYS_SET[t]);
                const panelTokens = Object.keys(this._expanded)
                    .filter(k => PANEL_KEYS_SET[k] && !this._defaults[k])
                    .sort();
                const list = panelTokens.concat(entityTokens);
                if (list.length) { url.searchParams.set('expand', list.join(',')); }
                else { url.searchParams.delete('expand'); }
                window.history.replaceState({}, '', url);
            } catch (e) { /* history API unavailable */ }
        },
    });
    Alpine.data('sectionToggle', sectionToggle);
    Alpine.data('storyPanel', storyPanel);
    Alpine.data('footerStatus', footerStatus);
});
