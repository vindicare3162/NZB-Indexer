package postprocess

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildFileDescPacket constructs a valid PAR2 File Description packet for the
// given filename, so the parser can be tested without a real PAR2 fixture.
func buildFileDescPacket(filename string) []byte {
	// Body: 16 fileID + 16 md5 + 16 md5-16k + 8 length + name (NUL padded to
	// a multiple of 4).
	name := []byte(filename)
	pad := (4 - len(name)%4) % 4
	body := make([]byte, 16+16+16+8+len(name)+pad)
	binary.LittleEndian.PutUint64(body[48:56], uint64(1234)) // file length
	copy(body[56:], name)

	pktLen := par2HeaderLen + len(body)
	pkt := make([]byte, pktLen)
	copy(pkt[0:8], par2Magic)
	binary.LittleEndian.PutUint64(pkt[8:16], uint64(pktLen))
	// pkt[16:32] MD5 packet hash (left zero for the test)
	// pkt[32:48] recovery set id (left zero)
	// pkt[48:64] packet type (per the PAR2 spec).
	copy(pkt[48:64], par2FileDescType)
	copy(pkt[par2HeaderLen:], body)
	return pkt
}

func TestParsePar2Filenames(t *testing.T) {
	// Two distinct files plus a duplicate of the first, concatenated with some
	// leading junk to exercise magic-scanning.
	data := []byte("garbage header bytes")
	data = append(data, buildFileDescPacket("Real.Movie.Name.2024.1080p.mkv")...)
	data = append(data, buildFileDescPacket("Real.Movie.Name.2024.1080p.nfo")...)
	data = append(data, buildFileDescPacket("Real.Movie.Name.2024.1080p.mkv")...) // dup

	names, err := ParsePar2Filenames(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v, want 2 unique", names)
	}
	if names[0] != "Real.Movie.Name.2024.1080p.mkv" {
		t.Errorf("names[0] = %q", names[0])
	}
}

func TestParsePar2NoPackets(t *testing.T) {
	if _, err := ParsePar2Filenames([]byte("no par2 packets in here at all")); err == nil {
		t.Error("expected error for data with no File Description packets")
	}
}

func TestParsePar2ToleratesTruncatedTail(t *testing.T) {
	good := buildFileDescPacket("Good.File.mkv")
	// Append a truncated magic with a bogus length.
	truncated := append(good, par2Magic...)
	truncated = append(truncated, 0xFF, 0xFF, 0xFF, 0xFF)

	names, err := ParsePar2Filenames(truncated)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(names) != 1 || names[0] != "Good.File.mkv" {
		t.Errorf("names = %v, want [Good.File.mkv]", names)
	}
}

func TestBestReleaseName(t *testing.T) {
	names := []string{
		"Great.Movie.2024.1080p.par2",
		"Great.Movie.2024.1080p.vol01+02.par2",
		"Great.Movie.2024.1080p.rar",
	}
	got := bestReleaseName(names)
	if got != "Great.Movie.2024.1080p" {
		t.Errorf("bestReleaseName = %q, want Great.Movie.2024.1080p", got)
	}

	// Volume suffixes on the archive parts must be stripped too (#31): all of
	// these reduce to the same base, and the recovered name must not keep a
	// ".partN" / ".rNN" tail.
	volNames := []string{
		"Saving.Grace.S03E10.HDTV.XviD.part1.rar",
		"Saving.Grace.S03E10.HDTV.XviD.part2.rar",
		"Saving.Grace.S03E10.HDTV.XviD.r05",
		"Saving.Grace.S03E10.HDTV.XviD.vol00+01.par2",
	}
	if got := bestReleaseName(volNames); got != "Saving.Grace.S03E10.HDTV.XviD" {
		t.Errorf("bestReleaseName(volNames) = %q, want Saving.Grace.S03E10.HDTV.XviD", got)
	}
}

func TestStripKnownExt(t *testing.T) {
	cases := map[string]string{
		"Show.S03E10.HDTV.XviD.part1.rar":     "Show.S03E10.HDTV.XviD",
		"Movie.2024.1080p.vol01+02.par2":      "Movie.2024.1080p",
		"Movie.2024.1080p.r05":                "Movie.2024.1080p",
		"Movie.2024.1080p.mkv":                "Movie.2024.1080p",
		"Great.Movie.2024.1080p.par2":         "Great.Movie.2024.1080p",
		"plain.name.without.volume":           "plain.name.without.volume",
	}
	for in, want := range cases {
		if got := stripKnownExt(in); got != want {
			t.Errorf("stripKnownExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasPar2Magic(t *testing.T) {
	pkt := buildFileDescPacket("x.mkv")
	if !HasPar2Magic(pkt) {
		t.Error("expected PAR2 magic to be detected")
	}
	// Magic in the middle of surrounding data.
	wrapped := append([]byte("random leading bytes"), pkt...)
	if !HasPar2Magic(wrapped) {
		t.Error("expected PAR2 magic detected with leading bytes")
	}
	if HasPar2Magic([]byte("not par2 data at all")) {
		t.Error("false positive on non-PAR2 data")
	}
}

// TestParsePar2TypeOffsetIsSpecCompliant guards against the packet-type field
// being read at the wrong offset. The PAR2 header is magic(8) + length(8) +
// packet-hash(16) + recovery-set-id(16) + type(16), so the type MUST be at
// byte 48. A regression here (reading at 40, as an earlier bug did) makes
// ParsePar2Filenames silently find nothing. This builds a packet with the type
// placed at offset 48 and distinctive non-type bytes at 40 to catch a misread.
func TestParsePar2TypeOffsetIsSpecCompliant(t *testing.T) {
	name := []byte("Recovered.Name.2024.1080p.mkv")
	pad := (4 - len(name)%4) % 4
	body := make([]byte, 16+16+16+8+len(name)+pad)
	copy(body[56:], name)

	pktLen := par2HeaderLen + len(body)
	pkt := make([]byte, pktLen)
	copy(pkt[0:8], par2Magic)
	binary.LittleEndian.PutUint64(pkt[8:16], uint64(pktLen))
	// Put recognisable garbage across [40:48] (end of the recovery-set-id) so a
	// parser that (wrongly) reads the type at offset 40 would not match.
	for i := 40; i < 48; i++ {
		pkt[i] = 0xAB
	}
	copy(pkt[48:64], par2FileDescType) // correct location
	copy(pkt[par2HeaderLen:], body)

	names, err := ParsePar2Filenames(pkt)
	if err != nil || len(names) != 1 || names[0] != "Recovered.Name.2024.1080p.mkv" {
		t.Fatalf("expected the filename to be recovered from a spec-compliant packet; got %v err=%v", names, err)
	}
}

func TestParsedPacketBoundaries(t *testing.T) {
	// Ensure a single packet round-trips through the offset math cleanly.
	pkt := buildFileDescPacket("x.mkv")
	if !bytes.Equal(pkt[0:8], par2Magic) {
		t.Fatal("magic not at offset 0")
	}
	names, err := ParsePar2Filenames(pkt)
	if err != nil || len(names) != 1 {
		t.Fatalf("single packet parse failed: %v, %v", names, err)
	}
}
