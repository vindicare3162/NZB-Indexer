package yenc

import "testing"

func TestParseHeaderMultipart(t *testing.T) {
	// Real header shape observed on an obfuscated post whose Subject carried
	// no counter at all: one 740KB article out of a 10.8GB file.
	body := []byte("=ybegin part=1871 total=16147 line=128 size=11573468670 name=5170b2a08f42e4b5ba86ad76418158773b1f9b5930f0f8c12da23e5e65f4\r\n" +
		"=ypart begin=1340416001 end=1341132800\r\n" +
		"encoded payload here\r\n")

	h, ok := ParseHeader(body)
	if !ok {
		t.Fatal("ParseHeader returned ok=false for a valid yEnc body")
	}
	if h.Part != 1871 {
		t.Errorf("Part = %d, want 1871", h.Part)
	}
	if h.Total != 16147 {
		t.Errorf("Total = %d, want 16147", h.Total)
	}
	if h.Size != 11573468670 {
		t.Errorf("Size = %d, want 11573468670", h.Size)
	}
	want := "5170b2a08f42e4b5ba86ad76418158773b1f9b5930f0f8c12da23e5e65f4"
	if h.Name != want {
		t.Errorf("Name = %q, want %q", h.Name, want)
	}
}

func TestParseHeaderSinglePart(t *testing.T) {
	// A genuinely standalone post declares no part/total.
	body := []byte("=ybegin line=128 size=4096 name=readme.txt\r\npayload\r\n=yend size=4096\r\n")

	h, ok := ParseHeader(body)
	if !ok {
		t.Fatal("ParseHeader returned ok=false for a valid single-part body")
	}
	if h.Total > 1 {
		t.Errorf("Total = %d, want 0 or 1 for a standalone post", h.Total)
	}
	if h.Name != "readme.txt" {
		t.Errorf("Name = %q, want %q", h.Name, "readme.txt")
	}
	if h.Size != 4096 {
		t.Errorf("Size = %d, want 4096", h.Size)
	}
}

func TestParseHeaderNameWithSpaces(t *testing.T) {
	// name= runs to end of line and may contain spaces.
	body := []byte("=ybegin part=1 total=2 line=128 size=100 name=My Great File.mkv\r\n")

	h, ok := ParseHeader(body)
	if !ok {
		t.Fatal("ParseHeader returned ok=false")
	}
	if h.Name != "My Great File.mkv" {
		t.Errorf("Name = %q, want %q", h.Name, "My Great File.mkv")
	}
}

func TestParseHeaderNotYenc(t *testing.T) {
	if _, ok := ParseHeader([]byte("just some text\r\nno yenc here\r\n")); ok {
		t.Error("ParseHeader returned ok=true for a non-yEnc body")
	}
}

func TestParseHeaderSkipsLeadingLines(t *testing.T) {
	// Some servers return headers/blank lines before the control line.
	body := []byte("Subject: whatever\r\n\r\n=ybegin part=3 total=9 line=128 size=500 name=x.bin\r\n")

	h, ok := ParseHeader(body)
	if !ok {
		t.Fatal("ParseHeader returned ok=false")
	}
	if h.Part != 3 || h.Total != 9 {
		t.Errorf("Part/Total = %d/%d, want 3/9", h.Part, h.Total)
	}
}
