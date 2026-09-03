package nntp

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// conn is the minimal connection surface the pool depends on. The real
// implementation speaks NNTP over net/textproto; tests provide a fake.
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

// netConn is a real NNTP connection implemented directly on net/textproto so
// we control exactly which commands are sent (notably XOVER, which many
// providers require in place of the RFC 3977 OVER command).
type netConn struct {
	raw  net.Conn
	tp   *textproto.Conn
	// overCmd is the header-overview command this server accepts ("XOVER" or
	// "OVER"), resolved lazily on first use.
	overCmd string
}

// dialLib establishes a real connection, optionally over TLS, then switches
// the server into reader mode.
func dialLib(cfg Config) (conn, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}

	var (
		raw net.Conn
		err error
	)
	if cfg.TLS {
		raw, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName:         cfg.Host,
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // operator opt-in for dev/self-signed
		})
	} else {
		raw, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("nntp dial %s: %w", addr, err)
	}

	tp := textproto.NewConn(raw)
	// Read the server greeting (200 posting allowed, or 201 no posting).
	if _, _, err := tp.ReadCodeLine(20); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("nntp greeting %s: %w", addr, err)
	}

	c := &netConn{raw: raw, tp: tp}
	// Reader mode is required by many providers before OVER/BODY work. Ignore
	// errors: some servers don't implement MODE READER but still work.
	_ = c.modeReader()
	return c, nil
}

// modeReader sends MODE READER, accepting the 200/201 response.
func (c *netConn) modeReader() error {
	id, err := c.tp.Cmd("MODE READER")
	if err != nil {
		return err
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)
	_, _, err = c.tp.ReadCodeLine(20)
	return err
}

func (c *netConn) authenticate(user, pass string) error {
	if user == "" {
		return nil
	}
	// AUTHINFO USER expects 381 (more auth required) then AUTHINFO PASS -> 281.
	code, line, err := c.simpleCmd("AUTHINFO USER %s", user)
	if err != nil {
		return err
	}
	if code == 281 {
		return nil // some servers accept user-only auth
	}
	if code != 381 {
		return fmt.Errorf("nntp authinfo user: unexpected %d %s", code, line)
	}
	code, line, err = c.simpleCmd("AUTHINFO PASS %s", pass)
	if err != nil {
		return err
	}
	if code != 281 {
		return fmt.Errorf("nntp authinfo pass: authentication failed (%d %s)", code, line)
	}
	return nil
}

func (c *netConn) selectGroup(name string) (GroupInfo, error) {
	id, err := c.tp.Cmd("GROUP %s", name)
	if err != nil {
		return GroupInfo{}, err
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)

	_, line, err := c.tp.ReadCodeLine(211)
	if err != nil {
		return GroupInfo{}, err
	}
	// Response: "211 count low high group"
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return GroupInfo{}, fmt.Errorf("nntp group: malformed response %q", line)
	}
	count, _ := strconv.ParseInt(fields[0], 10, 64)
	low, _ := strconv.ParseInt(fields[1], 10, 64)
	high, _ := strconv.ParseInt(fields[2], 10, 64)
	return GroupInfo{Name: name, Count: count, Low: low, High: high}, nil
}

func (c *netConn) overview(begin, end int64) ([]Overview, error) {
	if c.overCmd == "" {
		c.overCmd = "XOVER"
	}

	lines, err := c.overviewWith(c.overCmd, begin, end)
	if err != nil {
		// If the chosen command is unrecognized, try the alternative once.
		if isUnrecognized(err) {
			alt := "OVER"
			if c.overCmd == "OVER" {
				alt = "XOVER"
			}
			lines, err = c.overviewWith(alt, begin, end)
			if err != nil {
				return nil, err
			}
			c.overCmd = alt
		} else {
			return nil, err
		}
	}

	out := make([]Overview, 0, len(lines))
	for _, line := range lines {
		ov, ok := parseOverviewLine(line)
		if ok {
			out = append(out, ov)
		}
	}
	return out, nil
}

// overviewWith runs a specific overview command and returns the raw data lines.
func (c *netConn) overviewWith(cmd string, begin, end int64) ([]string, error) {
	id, err := c.tp.Cmd("%s %d-%d", cmd, begin, end)
	if err != nil {
		return nil, err
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)

	// 224 = overview follows (a dot-terminated block).
	if _, _, err := c.tp.ReadCodeLine(224); err != nil {
		return nil, err
	}
	return c.tp.ReadDotLines()
}

func (c *netConn) body(messageID string) (io.ReadCloser, error) {
	id, err := c.tp.Cmd("BODY %s", ensureAngle(messageID))
	if err != nil {
		return nil, err
	}
	c.tp.StartResponse(id)
	// 222 = body follows.
	if _, _, err := c.tp.ReadCodeLine(222); err != nil {
		c.tp.EndResponse(id)
		return nil, err
	}
	lines, err := c.tp.ReadDotLines()
	c.tp.EndResponse(id)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(strings.Join(lines, "\r\n"))), nil
}

func (c *netConn) ping() error {
	// DATE is a lightweight, always-available keepalive.
	_, _, err := c.simpleCmd("DATE")
	return err
}

func (c *netConn) close() error {
	// Best-effort QUIT, then close the socket.
	id, err := c.tp.Cmd("QUIT")
	if err == nil {
		c.tp.StartResponse(id)
		_, _, _ = c.tp.ReadCodeLine(205)
		c.tp.EndResponse(id)
	}
	return c.raw.Close()
}

// simpleCmd sends a command and reads a single status line, returning its code.
func (c *netConn) simpleCmd(format string, args ...any) (int, string, error) {
	id, err := c.tp.Cmd(format, args...)
	if err != nil {
		return 0, "", err
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)
	return c.tp.ReadCodeLine(0) // 0 = accept any code, return it
}

// parseOverviewLine parses a tab-separated XOVER/OVER data line into an
// Overview. The standard overview format is:
//
//	number \t subject \t from \t date \t message-id \t references \t bytes \t lines [\t extra...]
func parseOverviewLine(line string) (Overview, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 8 {
		return Overview{}, false
	}
	num, err := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
	if err != nil {
		return Overview{}, false
	}
	bytes, _ := strconv.ParseInt(strings.TrimSpace(fields[6]), 10, 64)

	ov := Overview{
		ArticleNumber: num,
		Subject:       fields[1],
		From:          fields[2],
		MessageID:     trimAngle(strings.TrimSpace(fields[4])),
		Bytes:         bytes,
	}
	if t, err := parseNNTPDate(fields[3]); err == nil {
		ov.Date = t
	}
	return ov, true
}

// nntpDateLayouts are the date formats seen in overview Date headers.
var nntpDateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 MST",
}

func parseNNTPDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var lastErr error
	for _, layout := range nntpDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
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
