package server

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vindicare/goindex/internal/auth"
)

// statusRecorder wraps http.ResponseWriter to capture the status code and the
// number of bytes written, for access logging. It defaults to 200 because a
// handler that writes a body without calling WriteHeader implies 200.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// accessLog wraps a handler and emits one structured log record per request:
// method, path, status, duration, response size, remote host, and the
// authenticated username when present. 5xx responses are logged at WARN so they
// stand out; everything else at INFO. The liveness/readiness probes are logged
// at DEBUG to avoid flooding logs when an orchestrator polls them frequently.
func accessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Install a holder so downstream auth middleware, which attaches the
		// principal on a child context, becomes observable here after the
		// handler returns.
		ctx, holder := auth.WithPrincipalHolder(r.Context())
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r.WithContext(ctx))

		dur := time.Since(start)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
			"bytes", rec.bytes,
			"remote", remoteHost(r.RemoteAddr),
		}
		if p, ok := holder.Principal(); ok {
			attrs = append(attrs, "user", p.Username)
		}

		msg := requestLabel(r.Method, r.URL.Path)
		switch {
		case isProbePath(r.URL.Path):
			logger.Debug(msg, attrs...)
		case rec.status >= 500:
			logger.Warn(msg, attrs...)
		default:
			logger.Info(msg, attrs...)
		}
	})
}

// requestLabel maps a request to a concise, human-readable action name used as
// the log message, so the admin Logs view reads "backfill", "postprocess",
// "stats", etc. rather than a uniform "http request". Unmapped paths fall back
// to "http request". Path parameters (ids/guids) are tolerated by matching on
// prefixes/suffixes.
func requestLabel(method, path string) string {
	switch path {
	case "/api/v1/login":
		return "login"
	case "/api/v1/me":
		return "whoami"
	case "/api/v1/health":
		return "health probe"
	case "/api/v1/ready":
		return "readiness probe"
	case "/metrics":
		return "metrics scrape"
	case "/api/v1/releases":
		return "search"
	case "/api/v1/categories":
		return "categories"
	case "/api/v1/admin/scan":
		return "scan"
	case "/api/v1/admin/backfill":
		return "backfill"
	case "/api/v1/admin/postprocess":
		return "postprocess"
	case "/api/v1/admin/postprocess/retry-failed":
		return "retry failed postprocess"
	case "/api/v1/admin/stats":
		return "stats"
	case "/api/v1/admin/status":
		return "status"
	case "/api/v1/admin/health":
		return "health report"
	case "/api/v1/admin/logs":
		return "logs"
	case "/api/v1/admin/schedule":
		return "schedule"
	case "/api/v1/admin/discover":
		return "discover groups"
	case "/api/v1/admin/groups":
		return "groups"
	case "/api/v1/admin/groups/bulk":
		return "bulk add groups"
	case "/api/v1/admin/servers":
		return "servers"
	case "/api/v1/admin/users":
		return "users"
	case "/api/v1/setup", "/api/v1/setup/status":
		return "setup"
	case "/api/v1/apikeys":
		return "api keys"
	}

	// Path-parameter routes.
	switch {
	case strings.HasPrefix(path, "/api/v1/releases/") && strings.HasSuffix(path, "/nzb"):
		return "nzb download"
	case strings.HasPrefix(path, "/api/v1/releases/"):
		return "release detail"
	case strings.HasPrefix(path, "/api/v1/apikeys/"):
		return "api key"
	case strings.HasPrefix(path, "/api/v1/admin/groups/"):
		return "group update"
	case strings.HasPrefix(path, "/api/v1/admin/servers/"):
		return "server update"
	case strings.HasPrefix(path, "/api/v1/admin/users/"):
		return "user update"
	case path == "/api" || (strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/api/v1/")):
		// The Newznab endpoint is mounted at /api and /api/; the REST API lives
		// under /api/v1/ and is handled by the exact cases above.
		return "newznab"
	}
	return "http request"
}

// remoteHost strips the port from a RemoteAddr, returning just the host/IP.
func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// isProbePath reports whether a path is a health/readiness probe, which are
// polled frequently and logged at DEBUG to avoid drowning out real traffic.
func isProbePath(p string) bool {
	return p == "/api/v1/health" || p == "/api/v1/ready"
}
