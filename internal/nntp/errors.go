package nntp

import (
	"errors"
	"io"
	"net"
	"net/textproto"
)

// isConnFatal reports whether an error means the connection is no longer
// usable and should be discarded (and the operation retried on a fresh
// connection). A well-formed NNTP status response (e.g. "no such group") is
// NOT connection-fatal: the server answered, so the connection is fine and the
// error is returned to the caller.
func isConnFatal(err error) bool {
	if err == nil {
		return false
	}

	// A *textproto.Error is a well-formed status response from the server. The
	// connection is healthy; the command was simply rejected.
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return false
	}
	// A textproto.ProtocolError means the response stream was malformed: the
	// connection is out of sync and must be discarded.
	var streamErr textproto.ProtocolError
	if errors.As(err, &streamErr) {
		return true
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

// isUnrecognized reports whether err is a server response indicating the
// command is not recognized/implemented, so the caller can try an alternative
// (e.g. OVER when XOVER is rejected). NNTP servers signal this with 500
// ("command not recognized") or 501 ("syntax error"); some legacy servers use
// 400.
func isUnrecognized(err error) bool {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		switch protoErr.Code {
		case 400, 500, 501:
			return true
		}
	}
	return false
}
