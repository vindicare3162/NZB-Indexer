// Package nntp provides a pooled, retrying client for an upstream Usenet
// (NNTP) provider. It wraps a low-level connection library behind a small
// interface so the pool and its operations can be unit tested without a live
// server.
package nntp

import (
	"errors"
	"time"
)

// GroupInfo describes the state of a newsgroup on the server.
type GroupInfo struct {
	// Name is the newsgroup name.
	Name string
	// Count is the estimated number of articles in the group.
	Count int64
	// Low is the lowest available article number.
	Low int64
	// High is the highest available article number (the water mark).
	High int64
}

// Overview is one article's header summary, as returned by the XOVER command.
// It mirrors the fields goindex needs from the underlying library's richer
// type, keeping the rest of the codebase decoupled from that dependency.
type Overview struct {
	// ArticleNumber is the message number within the current group.
	ArticleNumber int64
	// Subject is the article Subject header (may be empty).
	Subject string
	// From is the article From header (the poster).
	From string
	// Date is the parsed posting time (zero when missing/unparseable).
	Date time.Time
	// MessageID is the global Message-Id, without angle brackets.
	MessageID string
	// Bytes is the reported article size in bytes.
	Bytes int64
}

// Config configures the connection pool.
type Config struct {
	// Host is the NNTP server hostname.
	Host string
	// Port is the NNTP server port.
	Port int
	// TLS enables an implicit TLS connection.
	TLS bool
	// Username and Password authenticate to the provider (optional).
	Username string
	Password string
	// MaxConns bounds the number of pooled connections.
	MaxConns int
	// ConnectTimeout bounds dialing and the initial handshake.
	ConnectTimeout time.Duration
	// MaxRetries is the number of additional attempts for a transient
	// failure before giving up (0 means try once).
	MaxRetries int
	// RetryBackoff is the base delay between retries; it grows linearly with
	// the attempt number.
	RetryBackoff time.Duration
	// InsecureSkipVerify disables TLS certificate verification (test/dev only).
	InsecureSkipVerify bool
}

// ErrPoolClosed is returned when operations are attempted on a closed pool.
var ErrPoolClosed = errors.New("nntp: pool is closed")

// ErrNoConns is returned when a connection cannot be obtained before the
// caller's context is cancelled.
var ErrNoConns = errors.New("nntp: no connection available")
