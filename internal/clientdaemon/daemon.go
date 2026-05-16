package clientdaemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
)

// DefaultParallelism is the slot-cap default when --parallelism is unset.
const DefaultParallelism = 2

// DefaultMaxQueue caps the in-memory FIFO before /v1/enqueue 503s.
const DefaultMaxQueue = 100

// DefaultDrainTimeout is the SIGTERM-to-kill window during shutdown.
const DefaultDrainTimeout = 5 * time.Minute

// DefaultWatchdogInterval is the tick cadence at which the daemon
// walks the head of the FIFO looking for stalled queued tasks.
const DefaultWatchdogInterval = 30 * time.Second

// DefaultWatchdogThreshold is the maximum age the head-of-queue
// task is allowed to wait before the watchdog emits a
// `kind:queue-stalled` ledger evidence row.
const DefaultWatchdogThreshold = 5 * time.Minute

// DefaultHeartbeat overrides dispatchteam.DefaultHeartbeatInterval
// for daemon-spawned tasks; kept on the same 10s cadence as the sync
// CLI path for telemetry-shape parity.
const DefaultHeartbeat = 10 * time.Second

// DispatchFunc is the worker invocation injected by Options. The
// production wiring sets this to worker.RunDispatched; tests inject
// a fake to capture envelope + cfg shape (sty_5aa20f1b AC4).
type DispatchFunc func(ctx context.Context, cfg config.AgentConfig, logger arbor.ILogger, api *cliremote.Client, env worker.TaskEnvelope, stdout, stderr io.Writer) (worker.Outcome, error)

// Options bundles every operator-facing daemon setting plus the
// runtime injections (API client, logger, dispatch closure).
type Options struct {
	Parallelism  int
	MaxQueue     int
	Heartbeat    time.Duration
	DrainTimeout time.Duration

	// WatchdogInterval is how often the head-of-queue watchdog runs.
	// Zero falls back to DefaultWatchdogInterval.
	WatchdogInterval time.Duration

	// WatchdogThreshold is the head-of-queue age past which the
	// watchdog emits a `kind:queue-stalled` ledger evidence row.
	// Zero falls back to DefaultWatchdogThreshold.
	WatchdogThreshold time.Duration

	SocketPath  string
	PidfilePath string
	StatePath   string
	LogfilePath string
	LogsDir     string // per-task subprocess log dir

	AgentConfig config.AgentConfig
	API         *cliremote.Client
	Logger      arbor.ILogger

	// Dispatch is the worker invocation. Production wires
	// worker.RunDispatched here; tests inject a fake.
	Dispatch DispatchFunc

	// Now is the wall-clock source. Tests inject a fixed clock to
	// keep timestamps deterministic. nil falls back to time.Now.
	Now func() time.Time
}

// Daemon is the long-running queue+scheduler+socket process the
// `satellites-client serve run` verb instantiates. The package doc
// (doc.go) bounds the substrate-model refusal that this type owes.
type Daemon struct {
	opts      Options
	startedAt time.Time

	// queue is the FIFO of waiting task IDs. mu guards both queue
	// and running.
	mu          sync.Mutex
	queue       []QueuedEntry
	running     map[string]*runningHandle
	notify      chan struct{} // signal scheduler that queue/running changed
	draining    atomic.Bool
	drainStart  atomic.Pointer[time.Time]
	wg          sync.WaitGroup // counts in-flight runOne goroutines
}

// runningHandle is the per-task state held while runOne executes.
type runningHandle struct {
	TaskID       string
	StartedAt    time.Time
	PID          int            // worker PID (matches dispatchteam start frame)
	processOnce  sync.Once
	process      *os.Process    // claude subprocess handle when adopted (v1: nil; daemon process pid is recorded)
	cancel       context.CancelFunc
	suppressStop atomic.Bool
}

// New constructs a Daemon. It does NOT start any goroutines; the
// caller must invoke Run to drive the foreground loop.
func New(opts Options) (*Daemon, error) {
	if opts.API == nil {
		return nil, errors.New("clientdaemon: Options.API is required")
	}
	if opts.Dispatch == nil {
		return nil, errors.New("clientdaemon: Options.Dispatch is required")
	}
	if opts.Parallelism <= 0 {
		opts.Parallelism = DefaultParallelism
	}
	if opts.MaxQueue <= 0 {
		opts.MaxQueue = DefaultMaxQueue
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = DefaultHeartbeat
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	if opts.WatchdogInterval <= 0 {
		opts.WatchdogInterval = DefaultWatchdogInterval
	}
	if opts.WatchdogThreshold <= 0 {
		opts.WatchdogThreshold = DefaultWatchdogThreshold
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.SocketPath == "" {
		opts.SocketPath = DefaultSocketPath()
	}
	if opts.PidfilePath == "" {
		opts.PidfilePath = DefaultPidfilePath()
	}
	if opts.StatePath == "" {
		opts.StatePath = DefaultStatePath()
	}
	if opts.LogsDir == "" {
		opts.LogsDir = DefaultLogsDir()
	}
	d := &Daemon{
		opts:      opts,
		startedAt: opts.Now(),
		queue:     []QueuedEntry{},
		running:   map[string]*runningHandle{},
		notify:    make(chan struct{}, 1),
	}
	return d, nil
}

// DefaultSocketPath returns ~/.satellites/daemon/satellites-client.sock.
func DefaultSocketPath() string { return filepath.Join(daemonHome(), "satellites-client.sock") }

// DefaultPidfilePath returns ~/.satellites/daemon/satellites-client.pid.
func DefaultPidfilePath() string { return filepath.Join(daemonHome(), "satellites-client.pid") }

// DefaultStatePath returns ~/.satellites/daemon/state.json.
func DefaultStatePath() string { return filepath.Join(daemonHome(), "state.json") }

// DefaultLogfilePath returns ~/.satellites/daemon/daemon.log.
func DefaultLogfilePath() string { return filepath.Join(daemonHome(), "daemon.log") }

// DefaultLogsDir returns ~/.satellites/daemon/logs.
func DefaultLogsDir() string { return filepath.Join(daemonHome(), "logs") }

// daemonHome returns ~/.satellites/daemon — the operator-side
// per-machine directory holding the socket, pidfile, state.json,
// daemon.log, and per-task subprocess logs.
func daemonHome() string {
	if v := os.Getenv("SATELLITES_DAEMON_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "satellites-daemon")
	}
	return filepath.Join(home, ".satellites", "daemon")
}

// Now returns the daemon's monotonic-ish UTC clock (test-injectable
// via Options.Now).
func (d *Daemon) Now() time.Time { return d.opts.Now() }

// WaitInflight blocks until every in-flight runOne goroutine exits.
// Tests use this to drain spawned dispatch closures before tearing
// down the temp directory.
func (d *Daemon) WaitInflight() { d.wg.Wait() }

// StartedAt is the wall-clock the daemon's main loop began.
func (d *Daemon) StartedAt() time.Time { return d.startedAt }

// Parallelism returns the configured slot cap.
func (d *Daemon) Parallelism() int { return d.opts.Parallelism }

// MaxQueue returns the configured FIFO cap.
func (d *Daemon) MaxQueue() int { return d.opts.MaxQueue }

// Status returns a snapshot of the daemon's running + queued state.
func (d *Daemon) Status() StatusResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	running := make([]RunningEntry, 0, len(d.running))
	for _, h := range d.running {
		running = append(running, RunningEntry{TaskID: h.TaskID, PID: h.PID, StartedAt: h.StartedAt})
	}
	queued := make([]QueuedEntry, len(d.queue))
	for i, q := range d.queue {
		queued[i] = q
		queued[i].QueuePosition = i + 1
	}
	return StatusResponse{
		Running:     running,
		Queued:      queued,
		Parallelism: d.opts.Parallelism,
		MaxQueue:    d.opts.MaxQueue,
		DaemonPID:   os.Getpid(),
		StartedAt:   d.startedAt,
	}
}

// TaskStatus returns the per-task state for the given task_id. When
// the task is unknown the response State is "absent".
func (d *Daemon) TaskStatus(taskID string) TaskStatusResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.running[taskID]; ok {
		return TaskStatusResponse{TaskID: taskID, State: "running", PID: h.PID, StartedAt: h.StartedAt}
	}
	for i, q := range d.queue {
		if q.TaskID == taskID {
			return TaskStatusResponse{TaskID: taskID, State: "queued", QueuePosition: i + 1, EnqueuedAt: q.EnqueuedAt}
		}
	}
	return TaskStatusResponse{TaskID: taskID, State: "absent"}
}

// Queued returns a snapshot of the FIFO with positions.
func (d *Daemon) Queued() []QueuedEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]QueuedEntry, len(d.queue))
	for i, q := range d.queue {
		out[i] = q
		out[i].QueuePosition = i + 1
	}
	return out
}

// Enqueue accepts a task_id and pushes it onto the FIFO. Idempotent:
// re-enqueueing a task already running or queued returns the existing
// position/pid (sty_5aa20f1b plan §3 / anchor §5 open question 3).
func (d *Daemon) Enqueue(_ context.Context, taskID string) (EnqueueResponse, error) {
	taskID = trimSpace(taskID)
	if taskID == "" {
		return EnqueueResponse{}, errBadRequest("task_id required")
	}
	if d.draining.Load() {
		return EnqueueResponse{}, errDraining("daemon draining")
	}
	d.mu.Lock()
	if h, ok := d.running[taskID]; ok {
		_ = h
		resp := EnqueueResponse{TaskID: taskID, DaemonPID: os.Getpid(), QueuePosition: 0}
		d.mu.Unlock()
		d.info("task already running", "task_id", taskID)
		return resp, nil
	}
	for i, q := range d.queue {
		if q.TaskID == taskID {
			resp := EnqueueResponse{TaskID: taskID, DaemonPID: os.Getpid(), QueuePosition: i + 1}
			d.mu.Unlock()
			d.info("task already queued", "task_id", taskID, "queue_position", i+1)
			return resp, nil
		}
	}
	if len(d.queue) >= d.opts.MaxQueue {
		d.mu.Unlock()
		return EnqueueResponse{}, errQueueFull(fmt.Sprintf("queue full (max=%d)", d.opts.MaxQueue))
	}
	entry := QueuedEntry{TaskID: taskID, EnqueuedAt: d.opts.Now()}
	d.queue = append(d.queue, entry)
	pos := len(d.queue)
	queueDepth := len(d.queue)
	runningCount := len(d.running)
	d.mu.Unlock()
	d.info("task enqueued", "task_id", taskID, "queue_position", pos, "queue_depth", queueDepth, "running_count", runningCount)
	d.signalScheduler()
	if err := d.persistState(); err != nil {
		d.warn("persistState (enqueue)", err)
	}
	return EnqueueResponse{TaskID: taskID, DaemonPID: os.Getpid(), QueuePosition: pos}, nil
}

// Cancel removes a queued task or sends SIGTERM to a running task's
// claude subprocess (v1 records the running cancel as an evidence
// row but does not adopt subprocess handles, so SIGTERM coverage is
// limited to drain — Cancel of a running task returns prev_state
// without taking action; sty_5aa20f1b anchor §3.7 + §5 open question 1).
func (d *Daemon) Cancel(_ context.Context, taskID string) (CancelResponse, error) {
	taskID = trimSpace(taskID)
	if taskID == "" {
		return CancelResponse{}, errBadRequest("task_id required")
	}
	d.mu.Lock()
	for i, q := range d.queue {
		if q.TaskID == taskID {
			d.queue = append(d.queue[:i], d.queue[i+1:]...)
			d.mu.Unlock()
			if err := d.persistState(); err != nil {
				d.warn("persistState (cancel)", err)
			}
			return CancelResponse{TaskID: taskID, PrevState: "queued", Action: "removed_from_queue"}, nil
		}
	}
	if h, ok := d.running[taskID]; ok {
		// Signal the per-task context to cancel; the worker loop
		// observes the cancel and exits via the dispatchteam stop
		// path. Stop emission falls back to the natural Run finalisation
		// since SuppressStop stays unset for an operator-initiated
		// cancel.
		if h.cancel != nil {
			h.cancel()
		}
		d.mu.Unlock()
		return CancelResponse{TaskID: taskID, PrevState: "running", Action: "context_cancelled"}, nil
	}
	d.mu.Unlock()
	return CancelResponse{}, errNotFound("not found")
}

func (d *Daemon) signalScheduler() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

func (d *Daemon) warn(label string, err error) {
	if d.opts.Logger != nil {
		d.opts.Logger.Warn().Str("scope", "clientdaemon").Str("label", label).Str("error", err.Error()).Msg("daemon warn")
	}
}

func (d *Daemon) info(label string, kv ...any) {
	if d.opts.Logger == nil {
		return
	}
	ev := d.opts.Logger.Info().Str("scope", "clientdaemon").Str("label", label)
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		v := kv[i+1]
		ev = ev.Str(k, fmt.Sprint(v))
	}
	ev.Msg("daemon")
}

func (d *Daemon) debug(label string, kv ...any) {
	if d.opts.Logger == nil {
		return
	}
	ev := d.opts.Logger.Debug().Str("scope", "clientdaemon").Str("label", label)
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		v := kv[i+1]
		ev = ev.Str(k, fmt.Sprint(v))
	}
	ev.Msg("daemon")
}

func trimSpace(s string) string {
	out := []byte{}
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	out = append(out, s[start:end]...)
	return string(out)
}
