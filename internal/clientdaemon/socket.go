package clientdaemon

import (
	"encoding/json"
	"errors"
	"net/http"
)

// mux returns the http.Handler the unix-socket listener serves. Four
// endpoints, JSON request + response (sty_5aa20f1b plan §3 / anchor §3.3).
func (d *Daemon) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enqueue", d.handleEnqueue)
	mux.HandleFunc("/v1/status", d.handleStatus)
	mux.HandleFunc("/v1/cancel", d.handleCancel)
	mux.HandleFunc("/v1/queued", d.handleQueued)
	return mux
}

func (d *Daemon) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required", "")
		return
	}
	var req EnqueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error(), "")
		return
	}
	resp, err := d.Enqueue(r.Context(), req.TaskID)
	if err != nil {
		writeCodedError(w, err, req.TaskID)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET required", "")
		return
	}
	if id := r.URL.Query().Get("task_id"); id != "" {
		ts := d.TaskStatus(id)
		if ts.State == "absent" {
			writeJSON(w, http.StatusNotFound, ts)
			return
		}
		writeJSON(w, http.StatusOK, ts)
		return
	}
	writeJSON(w, http.StatusOK, d.Status())
}

func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required", "")
		return
	}
	var req CancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error(), "")
		return
	}
	resp, err := d.Cancel(r.Context(), req.TaskID)
	if err != nil {
		writeCodedError(w, err, req.TaskID)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) handleQueued(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET required", "")
		return
	}
	queued := d.Queued()
	if queued == nil {
		queued = []QueuedEntry{}
	}
	writeJSON(w, http.StatusOK, queued)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg, taskID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{TaskID: taskID, Error: msg})
}

func writeCodedError(w http.ResponseWriter, err error, taskID string) {
	var coded errCoded
	if errors.As(err, &coded) {
		writeError(w, coded.HTTPStatus(), err.Error(), taskID)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error(), taskID)
}
