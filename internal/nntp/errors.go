package nntp

import (
	"errors"
	"io"
	"net"

	lib "github.com/chrisfarms/nntp"
)

// isConnFatal reports whether an error means the connection is no longer
// usable and should be discarded (and the operation retried on a fresh
// connection). Server protocol responses (e.g. "no such group") are NOT
// connection-fatal: they are returned to the caller as-is.
func isConnFatal(err error) bool {
	if err == nil {
		return false
	}

	// A well-formed NNTP status response means the connection is fine; the
	// server simply rejected the request.
	var protoErr lib.ProtocolError
	if errors.As(err, &protoErr) {
		// A malformed/unexpected protocol response means the stream is out of
		// sync: treat as fatal so we reconnect.
		return true
	}
	var srvErr lib.Error
	if errors.As(err, &srvErr) {
		return false
	}

	// EOF / unexpected EOF: the peer closed the connection.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Any network error (timeout, reset, refused) is connection-fatal.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// Default: assume the connection may be bad and retry on a fresh one. This
	// is safe for idempotent read operations (OVER/GROUP/BODY).
	return true
}
