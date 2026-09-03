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
	copy(pkt[40:56], par2FileDescType)
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

func TestIsObfuscated(t *testing.T) {
	obfuscated := []string{
		"b6534bac9d3149e5bcd657b08345c075",
		"4Mg2PERuop6Dzy1Vzu1JupP9fg83J1",
		"abc123xyz-9f8e7d6c5b4a3210fedcba98",
		"",
	}
	for _, n := range obfuscated {
		if !isObfuscated(n) {
			t.Errorf("expected %q to be obfuscated", n)
		}
	}

	readable := []string{
		"Great.Movie.2024.1080p.BluRay.x264-GRP",
		"Some.Show.S01E01.HDTV.x264",
		"Artist - Album (2021) [FLAC]",
		"National Geographic Documentary",
	}
	for _, n := range readable {
		if isObfuscated(n) {
			t.Errorf("expected %q to be treated as readable (not obfuscated)", n)
		}
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
