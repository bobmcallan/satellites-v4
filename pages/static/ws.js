/*
 * SATELLITES ws.js — portal-side websocket connection manager (slice 10.4).
 *
 * Owns the state machine rendered by the nav indicator widget.
 * Bootstrap input (from head.html):   window.SATELLITES_WS = { workspaceId, debug }
 * Output surface:                      global `SatellitesWS` class + `wsIndicator()` Alpine factory
 *
 * State machine:
 *   idle -> connect() -> connecting
 *   connecting -> first event received -> live
 *   connecting -> onerror|onclose -> reconnecting (schedule backoff)
 *   live -> onclose -> reconnecting
 *   reconnecting -> connect succeeds -> live
 *   reconnecting -> max retries at cap -> disconnected
 *   disconnected -> retry() -> connecting
 *
 * Backoff: base 1000ms, double each attempt, cap 30000ms.
 * Zero-flicker: a reconnect completing within ZERO_FLICKER_MS keeps the
 * visible status at 'live' (the transient 'reconnecting' flip is skipped).
 */

// Backoff constants — exported on the class so tests can assert them
// without executing the state machine.
//
// __SATELLITES_WS_FAST: when set to true on `window` BEFORE this script
// loads, the indicator runs with compressed timings so chromedp E2E tests
// (tests/portalui) can observe state transitions in seconds. Production
// behaviour is unchanged when the flag is absent or false. The flag is
// strictly === true to prevent accidental truthy values from speeding up
// real users.
const __FAST = (typeof window !== 'undefined' && window.__SATELLITES_WS_FAST === true);
const BACKOFF_BASE_MS = __FAST ? 50 : 1000;
const BACKOFF_MAX_MS = __FAST ? 200 : 30000;
const ZERO_FLICKER_MS = __FAST ? 30 : 500;
// MAX_CAP_RETRIES bounds the number of retry attempts at the
// BACKOFF_MAX_MS ceiling before giving up and transitioning to
// DISCONNECTED. A small cap (3 → ~2 minutes) loses the connection
// across deploy windows that take longer than that to re-accept WS
// upgrades; the client then never reconnects and the page goes
// stale until the user refreshes. Pushed to a much higher value so
// the client keeps trying through long deploy / network-outage
// windows; visibilitychange + online listeners (below) reset the
// backoff for instant recovery when the user returns to the tab.
const MAX_CAP_RETRIES = __FAST ? 3 : 1000;
const DEBUG_BUFFER_CAP = 10;

// State enum — five fixed names. The state-table test greps for these.
const STATE_IDLE = 'idle';
const STATE_CONNECTING = 'connecting';
const STATE_LIVE = 'live';
const STATE_RECONNECTING = 'reconnecting';
const STATE_DISCONNECTED = 'disconnected';

class SatellitesWS {
    constructor(opts) {
        opts = opts || {};
        this.workspaceId = opts.workspaceId || '';
        this.projectId = opts.projectId || '';
        this.debug = !!opts.debug;
        this.onStatusChange = opts.onStatusChange || function () {};
        this.onEvent = opts.onEvent || function () {};
        this.url = opts.url || this._defaultURL();
        this.status = STATE_IDLE;
        this.conn = null;
        this.backoffMs = BACKOFF_BASE_MS;
        this.capRetries = 0;
        this.lastEventID = '';
        this.recentEvents = [];       // ring-capped DEBUG_BUFFER_CAP
        this._reconnectTimer = null;
        this._connectStartedAt = 0;
    }

    _defaultURL() {
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        return proto + '//' + window.location.host + '/ws';
    }

    // transition is the central state-change sink. All status flips route
    // through it so onStatusChange fires consistently.
    transition(next) {
        if (this.status === next) { return; }
        this.status = next;
        this.onStatusChange(next);
    }

    connect() {
        if (this.status === STATE_CONNECTING || this.status === STATE_LIVE) {
            return;
        }
        this._connectStartedAt = Date.now();
        // If we're already reconnecting, leave the label visible; otherwise flip.
        if (this.status !== STATE_RECONNECTING) {
            this.transition(STATE_CONNECTING);
        }
        try {
            this.conn = new WebSocket(this.url);
        } catch (e) {
            this._scheduleReconnect();
            return;
        }
        this.conn.addEventListener('open', () => this._onOpen());
        this.conn.addEventListener('message', (ev) => this._onMessage(ev));
        this.conn.addEventListener('close', () => this._onClose());
        this.conn.addEventListener('error', () => this._onClose());
    }

    retry() {
        // Manual retry — reset backoff + cap counter, transition straight
        // into connecting.
        this.backoffMs = BACKOFF_BASE_MS;
        this.capRetries = 0;
        if (this._reconnectTimer) {
            clearTimeout(this._reconnectTimer);
            this._reconnectTimer = null;
        }
        this.connect();
    }

    close() {
        if (this._reconnectTimer) {
            clearTimeout(this._reconnectTimer);
            this._reconnectTimer = null;
        }
        if (this.conn) {
            try { this.conn.close(); } catch (e) { /* ignore */ }
            this.conn = null;
        }
        this.transition(STATE_IDLE);
    }

    _onOpen() {
        // Subscribe to the workspace topic; include since_id on reconnects
        // so the hub replays events we missed during the outage.
        const topic = 'ws:' + this.workspaceId;
        const msg = { type: 'subscribe', topic: topic };
        if (this.lastEventID) {
            msg.since_id = this.lastEventID;
        }
        if (this.projectId) {
            msg.project_id = this.projectId;
        }
        try {
            this.conn.send(JSON.stringify(msg));
        } catch (e) {
            this._onClose();
            return;
        }
        // We optimistically flip to live on open. Most servers that accept
        // the handshake will accept the subscribe; if they reject, the
        // close handler walks back to reconnecting.
        this.backoffMs = BACKOFF_BASE_MS;
        this.capRetries = 0;
        this.transition(STATE_LIVE);
    }

    _onMessage(ev) {
        let parsed;
        try {
            parsed = JSON.parse(ev.data);
        } catch (e) {
            return;
        }
        if (parsed && parsed.type === 'error') {
            // Server sent an error frame (e.g. not_member). Treat as
            // disconnected — retry would hit the same error.
            this.capRetries = MAX_CAP_RETRIES;
            this.transition(STATE_DISCONNECTED);
            return;
        }
        if (parsed && parsed.ID) {
            this.lastEventID = parsed.ID;
            this._pushRecent(parsed);
            this.onEvent(parsed);
        }
    }

    _onClose() {
        if (this.conn) {
            try { this.conn.close(); } catch (e) { /* ignore */ }
        }
        this.conn = null;
        this._scheduleReconnect();
    }

    _scheduleReconnect() {
        // Zero-flicker: if the connection failed very quickly after the
        // most recent connect attempt and we were previously live, give
        // the first reconnect a ZERO_FLICKER_MS window before showing the
        // reconnecting label.
        const wasLive = (this.status === STATE_LIVE);
        const elapsed = Date.now() - this._connectStartedAt;
        const skipLabel = wasLive && elapsed < ZERO_FLICKER_MS;
        if (!skipLabel) {
            this.transition(STATE_RECONNECTING);
        }
        if (this.backoffMs >= BACKOFF_MAX_MS) {
            this.capRetries += 1;
        }
        const delay = this.backoffMs;
        this.backoffMs = Math.min(this.backoffMs * 2, BACKOFF_MAX_MS);
        if (this.capRetries >= MAX_CAP_RETRIES) {
            this.transition(STATE_DISCONNECTED);
            return;
        }
        if (this._reconnectTimer) {
            clearTimeout(this._reconnectTimer);
        }
        this._reconnectTimer = setTimeout(() => {
            this._reconnectTimer = null;
            this.connect();
        }, delay);
    }

    _pushRecent(ev) {
        if (!this.debug) { return; }
        this.recentEvents.push({
            id: ev.ID,
            kind: ev.Kind || '',
            created_at: ev.CreatedAt || ''
        });
        if (this.recentEvents.length > DEBUG_BUFFER_CAP) {
            this.recentEvents.shift();
        }
    }
}

// Expose constants for tests that grep the source (state-table test).
SatellitesWS.BACKOFF_BASE_MS = BACKOFF_BASE_MS;
SatellitesWS.BACKOFF_MAX_MS = BACKOFF_MAX_MS;
SatellitesWS.ZERO_FLICKER_MS = ZERO_FLICKER_MS;
SatellitesWS.MAX_CAP_RETRIES = MAX_CAP_RETRIES;
SatellitesWS.STATES = [
    STATE_IDLE, STATE_CONNECTING, STATE_LIVE, STATE_RECONNECTING, STATE_DISCONNECTED,
];

window.SatellitesWS = SatellitesWS;

// Alpine.js component factory — bound in nav.html via x-data="wsIndicator".
// Registered through Alpine.data so the bare-name binding works under the
// @alpinejs/csp build (epic:portal-csp-strict). Inline expression directives
// (`:class="'pre-' + s"`, `x-show="status === '...'"`) are absent from
// nav.html — every binding is a property, getter, or method ref.
function wsIndicator() {
    return {
        status: STATE_IDLE,
        debugOpen: false,
        recentEvents: [],
        client: null,
        init() {
            const cfg = window.SATELLITES_WS || {};
            if (!cfg.workspaceId) { return; }
            this.client = new SatellitesWS({
                workspaceId: cfg.workspaceId,
                debug: cfg.debug,
                onStatusChange: (next) => { this.status = next; },
                onEvent: () => {
                    // Keep the UI ring in sync with the client's own buffer
                    // so Alpine sees a fresh reference.
                    if (cfg.debug && this.client) {
                        this.recentEvents = this.client.recentEvents.slice();
                    }
                },
            });
            this.client.connect();
            // Recover immediately when the user returns to the tab or the
            // browser regains network. Without this, after a backend
            // deploy the client sits in DISCONNECTED until refresh — exactly
            // the bug the cap-bump above mitigates over time, but
            // visibility/online events let the recovery happen in seconds.
            const reconnectIfStale = () => {
                if (!this.client) { return; }
                if (this.client.status === STATE_DISCONNECTED || this.client.status === STATE_RECONNECTING) {
                    this.client.retry();
                }
            };
            if (typeof document !== 'undefined' && document.addEventListener) {
                document.addEventListener('visibilitychange', () => {
                    if (document.visibilityState === 'visible') { reconnectIfStale(); }
                });
            }
            if (typeof window !== 'undefined' && window.addEventListener) {
                window.addEventListener('online', reconnectIfStale);
            }
        },
        retry() {
            if (this.client) { this.client.retry(); }
        },
        toggleDebug() {
            if (!window.SATELLITES_WS || !window.SATELLITES_WS.debug) { return; }
            this.debugOpen = !this.debugOpen;
        },
        closeDebug() { this.debugOpen = false; },
        get indicatorClass() {
            return 'ws-indicator-' + this.status;
        },
        get isDisconnected() {
            return this.status === STATE_DISCONNECTED;
        },
        get hasNoRecentEvents() {
            return this.recentEvents.length === 0;
        },
        get hiddenWhenDebugOpen() { return this.debugOpen ? '' : 'is-hidden'; },
        get hiddenWhenIsDisconnected() { return this.isDisconnected ? '' : 'is-hidden'; },
        get hiddenWhenHasNoRecentEvents() { return this.hasNoRecentEvents ? '' : 'is-hidden'; },
        get statusText() {
            switch (this.status) {
                case STATE_LIVE: return 'live';
                case STATE_CONNECTING: return 'connecting';
                case STATE_RECONNECTING: return 'reconnecting';
                case STATE_DISCONNECTED: return 'disconnected';
                default: return 'idle';
            }
        },
    };
}

document.addEventListener('alpine:init', () => {
    Alpine.data('wsIndicator', wsIndicator);
});
