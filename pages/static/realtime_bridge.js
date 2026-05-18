/* SATELLITES realtime_bridge.js — shared client-side hub-event bridge.
 *
 * sty_7667c9bc. Owns ONE SatellitesWS connection per page and dispatches
 * a CustomEvent on `document` for each incoming hub event, scoped by
 * the page's data-project-id host attribute. Panels subscribe via
 * `document.addEventListener('satellites:realtime:<entity>', …)` and
 * patch their own DOM. The router's job ends at dispatch; the panel
 * owns its render.
 *
 * Bootstrap inputs (head.html, inside the {{if .WSConfig.WorkspaceID}}
 * guard):
 *   window.SATELLITES_WS                = { workspaceId, debug }
 *   window.SATELLITES_REALTIME_ROUTES   = [{kind_prefix, entity}, …]
 *
 * CustomEvent contract:
 *   new CustomEvent('satellites:realtime:' + entity, {
 *     detail: { kind, payload, eventID }
 *   })
 *
 * Project-id scoping runs HERE — single point of truth. A panel's
 * listener does not re-check payload.project_id. Hub events whose
 * payload.project_id is non-empty AND non-equal to the page's
 * data-project-id never dispatch. The wsIndicator (nav.html) keeps its
 * own SatellitesWS connection for now; collapsing that into this
 * bridge is a separate story (sty_7667c9bc plan §2.7).
 */

function realtimeBridge() {
    return {
        _ws: null,
        _projectID: '',
        _routes: [],
        _debug: false,

        init() {
            const cfg = window.SATELLITES_WS;
            if (!cfg || !cfg.workspaceId) { return; }
            if (!window.SatellitesWS) { return; }
            this._debug = !!cfg.debug;
            const routes = window.SATELLITES_REALTIME_ROUTES;
            this._routes = Array.isArray(routes) ? routes : [];
            const host = document.querySelector('[data-project-id]');
            this._projectID = host && host.dataset ? (host.dataset.projectId || '') : '';
            const self = this;
            this._ws = new window.SatellitesWS({
                workspaceId: cfg.workspaceId,
                projectId: this._projectID,
                debug: this._debug,
                onEvent: function (ev) { self._dispatch(ev); },
            });
            this._ws.connect();
        },

        destroy() {
            if (this._ws && typeof this._ws.close === 'function') { this._ws.close(); }
            this._ws = null;
        },

        // _dispatch resolves the incoming event's kind to an entity via
        // the route table, applies the project-id scope check, then
        // fires a `satellites:realtime:<entity>` CustomEvent on
        // document. Unrouted kinds drop silently (logged when debug is
        // on); off-project events drop without a log.
        _dispatch(ev) {
            if (!ev || !ev.Kind) { return; }
            const entity = this._entityFor(ev.Kind);
            if (!entity) {
                if (this._debug && typeof console !== 'undefined' && console.debug) {
                    console.debug('realtime_bridge: unrouted kind', ev.Kind);
                }
                return;
            }
            const payload = ev.Data || ev.data || {};
            const eventProject = payload && payload.project_id;
            if (this._projectID && eventProject && eventProject !== this._projectID) {
                return;
            }
            document.dispatchEvent(new CustomEvent('satellites:realtime:' + entity, {
                detail: { kind: ev.Kind, payload: payload, eventID: ev.ID }
            }));
        },

        // _entityFor walks the route table and returns the entity for
        // the first kind_prefix that the kind starts with. Empty
        // string when no rule matches.
        _entityFor(kind) {
            for (let i = 0; i < this._routes.length; i++) {
                const r = this._routes[i];
                if (r && r.kind_prefix && kind.indexOf(r.kind_prefix) === 0) {
                    return r.entity || '';
                }
            }
            return '';
        },
    };
}

document.addEventListener('alpine:init', function () {
    if (window.Alpine && window.Alpine.data) {
        window.Alpine.data('realtimeBridge', realtimeBridge);
    }
});

window.realtimeBridge = realtimeBridge;
