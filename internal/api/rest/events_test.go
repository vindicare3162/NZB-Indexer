package rest

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/logbuf"
)

// TestEventsStreamsStatusThenLog verifies the SSE endpoint (#121): it emits an
// initial "status" event on connect and a "log" event when a new log entry is
// captured, and it authenticates via the ?token= query parameter (EventSource
// cannot set an Authorization header).
func TestEventsStreamsStatusThenLog(t *testing.T) {
	env := setup(t)
	buf := logbuf.New(50)
	env.api.SetLogStreamer(buf)

	srv := httptest.NewServer(env.api.Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Token in the query string, not a header.
	url := srv.URL + "/api/v1/admin/events?token=" + env.adminTok
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	// Read events line-by-line. Collect the event names we see.
	reader := bufio.NewReader(resp.Body)
	sawStatus := false
	sawLog := false

	readUntil := func(pred func() bool) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: status") {
				sawStatus = true
			}
			if strings.HasPrefix(line, "event: log") {
				sawLog = true
			}
			if pred() {
				return
			}
		}
	}

	// The initial status event should arrive promptly.
	readUntil(func() bool { return sawStatus })
	if !sawStatus {
		t.Fatal("did not receive initial status event")
	}

	// Emit a log entry; it should be streamed as a "log" event.
	slog.New(buf.NewHandler()).Warn("live event log line")
	readUntil(func() bool { return sawLog })
	if !sawLog {
		t.Fatal("did not receive a log event after a new entry was captured")
	}
}

// TestEventsRequiresAdmin verifies the events endpoint rejects unauthenticated
// and non-admin callers.
func TestEventsRequiresAdmin(t *testing.T) {
	env := setup(t)
	srv := httptest.NewServer(env.api.Routes())
	defer srv.Close()

	// No token -> 401.
	resp, err := http.Get(srv.URL + "/api/v1/admin/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", resp.StatusCode)
	}

	// Non-admin user token -> 403.
	resp2, err := http.Get(srv.URL + "/api/v1/admin/events?token=" + env.userTok)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("user-token status = %d, want 403", resp2.StatusCode)
	}
}
