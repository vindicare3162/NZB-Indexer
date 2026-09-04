package rest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sseStatusInterval is how often the events stream pushes a fresh pipeline
// status snapshot. Kept close to the SPA's former 5s poll cadence.
const sseStatusInterval = 3 * time.Second

// sseHeartbeatInterval bounds how long the connection can be idle before a
// comment line is sent to keep proxies/browsers from closing it.
const sseHeartbeatInterval = 20 * time.Second

// handleEvents streams live admin updates over Server-Sent Events (#121):
//   - "status" events carry the worker pipeline status snapshot (the same shape
//     as GET /admin/status), pushed on connect and every few seconds.
//   - "log" events carry each new captured log entry as it happens (when a log
//     streamer is attached).
//
// The SPA connects with EventSource and updates live instead of polling. The
// endpoint is admin-authenticated (EventSource can't set headers, so the
// session token is accepted as the ?token= query parameter by the auth
// middleware). It returns 501 when streaming is unsupported by the writer.
func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusNotImplemented, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable proxy buffering (e.g. nginx) so events flush promptly.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()

	// send writes one SSE event and flushes. Returns false if the client is gone.
	send := func(event string, payload any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return true // skip an unmarshalable payload rather than dropping the stream
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Subscribe to live logs before the initial snapshot so nothing is missed
	// between them. Optional: when no streamer is attached, logCh stays nil and
	// the select simply never fires that case.
	var logCh <-chan any
	if a.logStream != nil {
		ch, cancel := a.logStream.Subscribe()
		defer cancel()
		// Adapt the typed channel to an any-channel via a small forwarding
		// goroutine so the select below is uniform and stops on ctx.Done.
		fwd := make(chan any, 256)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case e, ok := <-ch:
					if !ok {
						return
					}
					select {
					case fwd <- e:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		logCh = fwd
	}

	// Initial status snapshot so the client renders immediately.
	if !send("status", a.currentStatus()) {
		return
	}

	statusTick := time.NewTicker(sseStatusInterval)
	defer statusTick.Stop()
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-statusTick.C:
			if !send("status", a.currentStatus()) {
				return
			}
		case entry := <-logCh:
			if !send("log", entry) {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// currentStatus returns the worker pipeline status snapshot for the events
// stream, mirroring GET /admin/status.
func (a *API) currentStatus() any {
	if a.jobs == nil {
		return map[string]any{"jobs": "unavailable"}
	}
	return a.jobs.Status()
}
