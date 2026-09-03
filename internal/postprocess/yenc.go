// Package postprocess fetches PAR2 and NFO article bodies for releases,
// decodes them, recovers real filenames from PAR2, extracts NFO text, and
// renames/annotates releases accordingly.
package postprocess

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
)

// ErrNoYencData indicates the input contained no yEnc-encoded payload.
var ErrNoYencData = errors.New("postprocess: no yEnc data found")

// DecodeYenc decodes a single-part yEnc payload from an article body. It
// tolerates leading/trailing non-yEnc lines (headers, blank lines) and handles
// the =ybegin/=ypart/=yend control lines. Multi-part yEnc is supported for a
// single segment (the caller concatenates segments in order).
//
// yEnc encoding: each output byte is (input+42) mod 256, with '=' as an escape
// introducing a byte that is (next-64+42) mod 256. Control lines start with
// "=y".
func DecodeYenc(body []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var out bytes.Buffer
	inData := false
	sawBegin := false

	for sc.Scan() {
		line := sc.Bytes()
		trimmed := bytes.TrimRight(line, "\r\n")

		if bytes.HasPrefix(trimmed, []byte("=ybegin")) {
			sawBegin = true
			inData = true
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("=ypart")) {
			// Part header within a multipart post; data follows.
			inData = true
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("=yend")) {
			inData = false
			continue
		}
		if !inData {
			continue
		}
		decodeYencLine(trimmed, &out)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawBegin {
		return nil, ErrNoYencData
	}
	return out.Bytes(), nil
}

// decodeYencLine decodes one line of yEnc data into out.
func decodeYencLine(line []byte, out *bytes.Buffer) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '=' {
			// Escaped byte: next char minus 64, then the usual -42.
			i++
			if i >= len(line) {
				break
			}
			out.WriteByte(byte(int(line[i]) - 64 - 42))
			continue
		}
		out.WriteByte(byte(int(c) - 42))
	}
}

// LooksLikeNFO reports whether a filename or subject suggests an NFO file.
func LooksLikeNFO(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), ".nfo")
}

// LooksLikePAR2 reports whether a filename or subject suggests a PAR2 file.
func LooksLikePAR2(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), ".par2")
}
