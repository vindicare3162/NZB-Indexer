package postprocess

import (
	"bytes"
	"fmt"
	"testing"
)

// encodeYenc produces a minimal single-part yEnc stream for the given payload,
// used to exercise the decoder.
func encodeYenc(name string, data []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "=ybegin line=128 size=%d name=%s\r\n", len(data), name)
	col := 0
	for _, c := range data {
		e := byte((int(c) + 42) % 256)
		switch e {
		case '\x00', '\r', '\n', '=':
			b.WriteByte('=')
			b.WriteByte(byte((int(e) + 64) % 256))
			col += 2
		default:
			b.WriteByte(e)
			col++
		}
		if col >= 128 {
			b.WriteString("\r\n")
			col = 0
		}
	}
	fmt.Fprintf(&b, "\r\n=yend size=%d\r\n", len(data))
	return b.Bytes()
}

func TestDecodeYencRoundTrip(t *testing.T) {
	payloads := [][]byte{
		[]byte("hello world"),
		{0, 1, 2, 42, 214, 255, '=', '\r', '\n'}, // includes chars needing escaping
		bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 100),
	}
	for i, p := range payloads {
		enc := encodeYenc("test.bin", p)
		dec, err := DecodeYenc(enc)
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		if !bytes.Equal(dec, p) {
			t.Errorf("case %d: round-trip mismatch\n got %v\nwant %v", i, dec, p)
		}
	}
}

func TestDecodeYencToleratesSurroundingLines(t *testing.T) {
	payload := []byte("payload bytes here")
	enc := encodeYenc("f.bin", payload)
	wrapped := append([]byte("From: someone\r\nSubject: x\r\n\r\n"), enc...)
	wrapped = append(wrapped, []byte("\r\n. trailing junk\r\n")...)

	dec, err := DecodeYenc(wrapped)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(dec, payload) {
		t.Errorf("mismatch: got %q want %q", dec, payload)
	}
}

func TestDecodeYencNoData(t *testing.T) {
	if _, err := DecodeYenc([]byte("just some plain text\r\nno yenc here")); err != ErrNoYencData {
		t.Errorf("expected ErrNoYencData, got %v", err)
	}
}

func TestLooksLikeHelpers(t *testing.T) {
	if !LooksLikeNFO("release.NFO") {
		t.Error("NFO detection failed")
	}
	if !LooksLikePAR2("archive.vol01+02.PAR2") {
		t.Error("PAR2 detection failed")
	}
	if LooksLikeNFO("movie.mkv") || LooksLikePAR2("movie.mkv") {
		t.Error("false positive on .mkv")
	}
}
