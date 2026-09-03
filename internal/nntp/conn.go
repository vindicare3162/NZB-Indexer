package nntp

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"

	lib "github.com/chrisfarms/nntp"
)

// conn is the minimal connection surface the pool depends on. The real
// implementation wraps github.com/chrisfarms/nntp; tests provide a fake.
type conn interface {
	// authenticate logs in when credentials are configured.
	authenticate(user, pass string) error
	// selectGroup switches the current group and returns its info.
	selectGroup(name string) (GroupInfo, error)
	// overview returns header summaries for [begin,end] in the current group.
	overview(begin, end int64) ([]Overview, error)
	// body returns the decoded body of the article with the given message-id.
	body(messageID string) (io.ReadCloser, error)
	// ping cheaply checks the connection is still usable.
	ping() error
	// close terminates the connection.
	close() error
}

// dialer creates new connections. Swappable in tests.
type dialer func(cfg Config) (conn, error)

// libConn adapts *lib.Conn to the conn interface.
type libConn struct {
	c *lib.Conn
}

// dialLib establishes a real connection, optionally over TLS, and switches the
// server into reader mode. The connect timeout is enforced by racing the dial
// against a timer, since the underlying library's Dial helpers take no
// deadline.
func dialLib(cfg Config) (conn, error) {
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	type dialResult struct {
		c   *lib.Conn
		err error
	}
	done := make(chan dialResult, 1)
	go func() {
		var (
			c   *lib.Conn
			err error
		)
		if cfg.TLS {
			tlsCfg := &tls.Config{
				ServerName:         cfg.Host,
				InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // operator opt-in for dev/self-signed
			}
			c, err = lib.DialTLS("tcp", addr, tlsCfg)
		} else {
			c, err = lib.Dial("tcp", addr)
		}
		done <- dialResult{c: c, err: err}
	}()

	select {
	case <-time.After(timeout):
		return nil, fmt.Errorf("nntp dial %s: timed out after %s", addr, timeout)
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("nntp dial %s: %w", addr, res.err)
		}
		lc := &libConn{c: res.c}
		// Reader mode is required by many providers before OVER/BODY work.
		// Not fatal on all servers, so ignore a mode-switch error.
		_ = res.c.ModeReader()
		return lc, nil
	}
}

func (l *libConn) authenticate(user, pass string) error {
	if user == "" {
		return nil
	}
	return l.c.Authenticate(user, pass)
}

func (l *libConn) selectGroup(name string) (GroupInfo, error) {
	count, low, high, err := l.c.Group(name)
	if err != nil {
		return GroupInfo{}, err
	}
	return GroupInfo{
		Name:  name,
		Count: int64(count),
		Low:   int64(low),
		High:  int64(high),
	}, nil
}

func (l *libConn) overview(begin, end int64) ([]Overview, error) {
	raw, err := l.c.Overview(int(begin), int(end))
	if err != nil {
		return nil, err
	}
	out := make([]Overview, 0, len(raw))
	for _, m := range raw {
		out = append(out, Overview{
			ArticleNumber: int64(m.MessageNumber),
			Subject:       m.Subject,
			From:          m.From,
			Date:          m.Date,
			MessageID:     trimAngle(m.MessageId),
			Bytes:         int64(m.Bytes),
		})
	}
	return out, nil
}

func (l *libConn) body(messageID string) (io.ReadCloser, error) {
	r, err := l.c.Body(ensureAngle(messageID))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(r), nil
}

func (l *libConn) ping() error {
	// Date is a lightweight, always-available command used as a keepalive.
	_, err := l.c.Date()
	return err
}

func (l *libConn) close() error {
	return l.c.Quit()
}

// trimAngle removes surrounding <...> from a message-id if present.
func trimAngle(id string) string {
	if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
		return id[1 : len(id)-1]
	}
	return id
}

// ensureAngle wraps a bare message-id in <...> as required by NNTP commands.
func ensureAngle(id string) string {
	if id == "" {
		return id
	}
	if id[0] == '<' {
		return id
	}
	return "<" + id + ">"
}
