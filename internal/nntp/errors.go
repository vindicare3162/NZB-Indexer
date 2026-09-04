package nntp

import (
	"errors"
	"io"
	"net"
	"net/textproto"
)

// ErrorKind classifies an NNTP failure so health checks and circuit breaking
// (#128) can distinguish connection, authentication, protocol, and retention
// failures.
type ErrorKind int

const (
	// ErrKindNone means no error (success).
	ErrKindNone ErrorKind = iota
	// ErrKindConnection is a network/transport failure (dial, timeout, reset,
	// EOF) or a malformed response stream — the connection is unusable.
	ErrKindConnection
	// ErrKindAuth is an authentication failure (bad credentials): a 480/481/482
	// NNTP status. Retrying won't help until credentials change.
	ErrKindAuth
	// ErrKindProtocol is a well-formed error status from the server that is not
	// auth-related (e.g. command rejected, no such group). The connection is
	// healthy.
	ErrKindProtocol
	// ErrKindRetention is a "no such article" / article-expired response (430),
	// i.e. the provider does not (or no longer) carries the article. The
	// connection and credentials are fine.
	ErrKindRetention
)

func (k ErrorKind) String() string {
	switch k {
	case ErrKindConnection:
		return "connection"
	case ErrKindAuth:
		return "auth"
	case ErrKindProtocol:
		return "protocol"
	case ErrKindRetention:
		return "retention"
	default:
		return "none"
	}
}

// ClassifyError maps an error to an ErrorKind. Exported so other stages (e.g.
// post-processing retry policy, #132) can classify NNTP failures the same way
// the circuit breaker does.
func ClassifyError(err error) ErrorKind { return classifyError(err) }

// IsPermanent reports whether an NNTP error is a permanent failure that will
// not be resolved by retrying the same request (#132): a retention miss (430,
// the article is gone / not carried) or an authentication failure (retrying
// won't help until credentials change). Connection and other protocol errors
// are treated as transient and worth retrying with backoff. A nil error is not
// permanent.
func IsPermanent(err error) bool {
	switch classifyError(err) {
	case ErrKindRetention, ErrKindAuth:
		return true
	default:
		return false
	}
}

// classifyError maps an error to an ErrorKind for health tracking.
func classifyError(err error) ErrorKind {
	if err == nil {
		return ErrKindNone
	}
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		switch protoErr.Code {
		case 480, 481, 482, 502: // auth required / rejected / failed
			return ErrKindAuth
		case 430: // no such article (retention / not carried)
			return ErrKindRetention
		default:
			return ErrKindProtocol
		}
	}
	// Non-status errors are transport/stream problems: connection-fatal.
	if isConnFatal(err) {
		return ErrKindConnection
	}
	return ErrKindProtocol
}

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
