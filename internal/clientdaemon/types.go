package clientdaemon

import "time"

// EnqueueRequest is the wire shape of POST /v1/enqueue.
type EnqueueRequest struct {
	TaskID string `json:"task_id"`
}

// EnqueueResponse is the 200 body of POST /v1/enqueue.
type EnqueueResponse struct {
	TaskID        string `json:"task_id"`
	DaemonPID     int    `json:"daemon_pid"`
	QueuePosition int    `json:"queue_position"`
}

// CancelRequest is the wire shape of POST /v1/cancel.
type CancelRequest struct {
	TaskID string `json:"task_id"`
}

// CancelResponse is the 200 body of POST /v1/cancel.
type CancelResponse struct {
	TaskID    string `json:"task_id"`
	PrevState string `json:"prev_state"`
	Action    string `json:"action"`
}

// RunningEntry is one in-flight task in the daemon. ProjectID is
// retained so reconcileBoot can route a `kind:daemon-orphaned-
// subprocess` evidence row to the correct project after a daemon
// crash; the field omitempty's out of /v1/status responses when the
// caller hasn't tagged it.
type RunningEntry struct {
	TaskID    string    `json:"task_id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	ProjectID string    `json:"project_id,omitempty"`
}

// QueuedEntry is one waiting task in the daemon's FIFO.
type QueuedEntry struct {
	TaskID        string    `json:"task_id"`
	EnqueuedAt    time.Time `json:"enqueued_at"`
	QueuePosition int       `json:"queue_position"`
}

// StatusResponse is the 200 body of GET /v1/status (daemon-wide).
type StatusResponse struct {
	Running     []RunningEntry `json:"running"`
	Queued      []QueuedEntry  `json:"queued"`
	Parallelism int            `json:"parallelism"`
	MaxQueue    int            `json:"max_queue"`
	DaemonPID   int            `json:"daemon_pid"`
	StartedAt   time.Time      `json:"started_at"`
}

// TaskStatusResponse is the 200 body of GET /v1/status?task_id=X.
type TaskStatusResponse struct {
	TaskID        string    `json:"task_id"`
	State         string    `json:"state"` // queued | running | done | absent
	QueuePosition int       `json:"queue_position,omitempty"`
	PID           int       `json:"pid,omitempty"`
	EnqueuedAt    time.Time `json:"enqueued_at,omitempty"`
	StartedAt     time.Time `json:"started_at,omitempty"`
}

// ErrorResponse is the body of any non-2xx response.
type ErrorResponse struct {
	TaskID string `json:"task_id,omitempty"`
	Error  string `json:"error,omitempty"`
	State  string `json:"state,omitempty"`
}
