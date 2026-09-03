package server

import (
	"log/slog"
	"net"
	"net/http"
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

		switch {
		case isProbePath(r.URL.Path):
			logger.Debug("http request", attrs...)
		case rec.status >= 500:
			logger.Warn("http request", attrs...)
		default:
			logger.Info("http request", attrs...)
		}
	})
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
