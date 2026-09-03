package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vindicare/goindex/internal/auth"
)

// capture builds a logger writing text records into a strings.Builder at DEBUG
// level so probe records are captured too.
func capture() (*slog.Logger, *strings.Builder) {
	var b strings.Builder
	h := slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &b
}

func TestAccessLogBasicFields(t *testing.T) {
	logger, buf := capture()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/things", nil)
	req.RemoteAddr = "203.0.113.7:44321"
	rec := httptest.NewRecorder()
	accessLog(logger, next).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status passed through = %d, want 201", rec.Code)
	}
	out := buf.String()
	for _, want := range []string{
		`level=INFO`, `method=POST`, `path=/api/v1/things`,
		`status=201`, `bytes=5`, `remote=203.0.113.7`, `duration_ms=`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("access log missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "user=") {
		t.Errorf("anonymous request should not log a user:\n%s", out)
	}
}

func TestAccessLogCapturesPrincipal(t *testing.T) {
	logger, buf := capture()

	// A downstream handler that attaches a principal exactly as the auth
	// middleware does, on a child context.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{UserID: 42, Username: "alice"})
		_ = ctx // the holder is populated as a side effect of WithPrincipal
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rec := httptest.NewRecorder()
	accessLog(logger, next).ServeHTTP(rec, req)

	if got := buf.String(); !strings.Contains(got, "user=alice") {
		t.Errorf("access log should capture the authenticated user:\n%s", got)
	}
}

func TestAccessLogProbeIsDebug(t *testing.T) {
	logger, buf := capture()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	accessLog(logger, next).ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("probe request should log at DEBUG:\n%s", out)
	}
}

func TestAccessLog5xxIsWarn(t *testing.T) {
	logger, buf := capture()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil)
	accessLog(logger, next).ServeHTTP(httptest.NewRecorder(), req)

	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "status=502") {
		t.Errorf("5xx should log at WARN with status:\n%s", out)
	}
}

// ensure the holder correctly reports "not set" when nothing attached one.
func TestPrincipalHolderUnset(t *testing.T) {
	_, holder := auth.WithPrincipalHolder(context.Background())
	if _, ok := holder.Principal(); ok {
		t.Error("holder should report unset before any principal is attached")
	}
}
