// Package yenc parses the yEnc control headers that precede an article's
// encoded payload.
//
// The header is the authoritative source of a post's true shape: multi-part
// posts carry "=ybegin part=N total=M size=S name=..." on the first line and
// "=ypart begin=... end=..." on the second. Crucially this is present even
// when the Subject line carries no "(n/m)" counter — a common anti-indexing
// tactic — which is the only way to tell a genuinely standalone article from
// one segment of a multi-gigabyte file (#178).
package yenc

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// Header is the subset of the yEnc control lines that identifies a post.
type Header struct {
	// Part is this article's 1-based position, 0 when the post is single-part
	// (no part= field).
	Part int
	// Total is the declared segment count, 0 when absent. A single-part post
	// declares no total; callers should treat Total<=1 as standalone.
	Total int
	// Size is the complete file's size in bytes (not this segment's size).
	Size int64
	// Name is the poster's filename for the complete file. Every segment of
	// one file repeats it verbatim, so it is a reliable grouping key even when
	// each segment's Subject is randomised.
	Name string
}

// maxHeaderScan bounds how much of a body is examined before giving up. The
// control lines are always at the very start, so this only guards against
// pathological input.
const maxHeaderScan = 64 * 1024

// ParseHeader extracts the yEnc header from an article body. ok is false when
// the body carries no "=ybegin" line at all (not a yEnc post).
func ParseHeader(body []byte) (h Header, ok bool) {
	if len(body) > maxHeaderScan {
		body = body[:maxHeaderScan]
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 8*1024), maxHeaderScan)

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if !strings.HasPrefix(line, "=ybegin") {
			continue
		}
		h.Part = int(intField(line, "part="))
		h.Total = int(intField(line, "total="))
		h.Size = intField(line, "size=")
		h.Name = nameField(line)
		return h, true
	}
	return Header{}, false
}

// intField reads a numeric "key=value" field from a yEnc control line. Fields
// are space-separated, so the value ends at the next space.
func intField(line, key string) int64 {
	i := strings.Index(line, key)
	if i < 0 {
		return 0
	}
	rest := line[i+len(key):]
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// nameField reads the "name=" field, which is always last on the line and may
// itself contain spaces.
func nameField(line string) string {
	i := strings.Index(line, "name=")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+len("name="):])
}
