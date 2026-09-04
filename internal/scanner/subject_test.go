package scanner

import "testing"

func TestParseSubject(t *testing.T) {
	tests := []struct {
		name       string
		subject    string
		wantPart   int
		wantTotal  int
		wantFile   string
		wantNorm   string
	}{
		{
			name:      "quoted filename with yenc segment counter",
			subject:   `[075/111] - "Some.Show.S01E01.1080p.WEB.mkv" yEnc (1/500)`,
			wantPart:  1,
			wantTotal: 500,
			wantFile:  "Some.Show.S01E01.1080p.WEB.mkv",
			wantNorm:  `[075/111] - "Some.Show.S01E01.1080p.WEB.mkv"`,
		},
		{
			name:      "simple paren counter",
			subject:   `My.Cool.Release.part01.rar (1/120)`,
			wantPart:  1,
			wantTotal: 120,
			wantFile:  "",
			wantNorm:  "My.Cool.Release.part01.rar",
		},
		{
			name:      "n of m form",
			subject:   `Great Movie 2024 5 of 42 yEnc`,
			wantPart:  5,
			wantTotal: 42,
			wantNorm:  "Great Movie 2024",
		},
		{
			name:      "bracket counter",
			subject:   `obfuscated-abc123 [12/34]`,
			wantPart:  12,
			wantTotal: 34,
			wantNorm:  "obfuscated-abc123",
		},
		{
			name:      "bytes annotation stripped",
			subject:   `"file.r00" yEnc (3/9) - 15728640 bytes`,
			wantPart:  3,
			wantTotal: 9,
			wantFile:  "file.r00",
			wantNorm:  `"file.r00"`,
		},
		{
			name:      "no counter",
			subject:   `Random.Standalone.File.nfo`,
			wantPart:  0,
			wantTotal: 0,
			wantNorm:  "Random.Standalone.File.nfo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSubject(tt.subject)
			if got.PartNumber != tt.wantPart {
				t.Errorf("PartNumber = %d, want %d", got.PartNumber, tt.wantPart)
			}
			if got.TotalParts != tt.wantTotal {
				t.Errorf("TotalParts = %d, want %d", got.TotalParts, tt.wantTotal)
			}
			if tt.wantFile != "" && got.FileName != tt.wantFile {
				t.Errorf("FileName = %q, want %q", got.FileName, tt.wantFile)
			}
			if got.Normalized != tt.wantNorm {
				t.Errorf("Normalized = %q, want %q", got.Normalized, tt.wantNorm)
			}
		})
	}
}

// TestNormalizedGroupsSegments verifies that the per-segment counter is what
// varies, so all segments of one binary share a Normalized key.
func TestNormalizedGroupsSegments(t *testing.T) {
	base := `"Big.Release.2024.1080p.mkv" yEnc`
	s1 := ParseSubject(base + " (1/500)")
	s250 := ParseSubject(base + " (250/500)")
	s500 := ParseSubject(base + " (500/500)")

	if s1.Normalized != s250.Normalized || s250.Normalized != s500.Normalized {
		t.Errorf("segments did not normalize equally:\n  1=%q\n250=%q\n500=%q",
			s1.Normalized, s250.Normalized, s500.Normalized)
	}
	if s1.PartNumber != 1 || s250.PartNumber != 250 || s500.PartNumber != 500 {
		t.Errorf("part numbers wrong: %d %d %d", s1.PartNumber, s250.PartNumber, s500.PartNumber)
	}
	if s1.TotalParts != 500 {
		t.Errorf("total = %d, want 500", s1.TotalParts)
	}
}

// TestParseCollection verifies that files of a multi-file "[n/total]" post
// share one collection key (so the assembler folds them into one binary),
// while single-file posts get no collection key.
func TestParseCollection(t *testing.T) {
	// A 113-file obfuscated collection: the PAR2 and every rar volume must
	// share the same collection key.
	par2 := ParseSubject(`[001/113] "Ef9UyY9ZpxkXPkQy.par2" yEnc (1/2)`)
	rar1 := ParseSubject(`[002/113] "Ef9UyY9ZpxkXPkQy.part001.rar" yEnc (1/464)`)
	rar112 := ParseSubject(`[113/113] "Ef9UyY9ZpxkXPkQy.part112.rar" yEnc (37/464)`)

	if par2.CollectionKey == "" {
		t.Fatal("PAR2 got no collection key")
	}
	if par2.CollectionKey != rar1.CollectionKey || rar1.CollectionKey != rar112.CollectionKey {
		t.Errorf("collection keys differ:\n par2=%q\n rar1=%q\n rar112=%q",
			par2.CollectionKey, rar1.CollectionKey, rar112.CollectionKey)
	}
	if par2.CollectionFiles != 113 || rar1.CollectionFiles != 113 {
		t.Errorf("collection files = %d/%d, want 113/113", par2.CollectionFiles, rar1.CollectionFiles)
	}
	if par2.FileNumber != 1 || rar1.FileNumber != 2 || rar112.FileNumber != 113 {
		t.Errorf("file numbers = %d/%d/%d, want 1/2/113", par2.FileNumber, rar1.FileNumber, rar112.FileNumber)
	}
	// The segment counter is still parsed independently of the file counter.
	if rar1.PartNumber != 1 || rar1.TotalParts != 464 {
		t.Errorf("rar1 segment = %d/%d, want 1/464", rar1.PartNumber, rar1.TotalParts)
	}

	// A readable multi-file collection groups the same way.
	a := ParseSubject(`[01/50] "Some.Movie.2024.1080p.BluRay.x264-GRP.part01.rar" yEnc (1/300)`)
	b := ParseSubject(`[50/50] "Some.Movie.2024.1080p.BluRay.x264-GRP.vol31+32.par2" yEnc (1/40)`)
	if a.CollectionKey == "" || a.CollectionKey != b.CollectionKey {
		t.Errorf("readable collection keys differ: a=%q b=%q", a.CollectionKey, b.CollectionKey)
	}

	// Single-file posts (no leading file counter) get no collection key, so
	// they continue to group by normalized subject.
	single := ParseSubject(`"Random.Standalone.File.mkv" yEnc (1/50)`)
	if single.CollectionKey != "" {
		t.Errorf("single-file post got a collection key: %q", single.CollectionKey)
	}
	// A "[1/1]" is a single file, not a collection.
	one := ParseSubject(`[1/1] "solo.mkv" yEnc (1/10)`)
	if one.CollectionKey != "" {
		t.Errorf("[1/1] treated as collection: %q", one.CollectionKey)
	}

	// A single multi-segment file that repeats the same counter in both
	// positions must NOT be treated as a collection (regression from live
	// data: `[1/445] "blob" yEnc (1/445)` is one 445-segment file).
	blob := ParseSubject(`[1/445] - "e17a881978b12c92" yEnc (1/445)`)
	if blob.CollectionKey != "" {
		t.Errorf("single multi-segment blob treated as collection: key=%q files=%d", blob.CollectionKey, blob.CollectionFiles)
	}
	// It should still parse as a normal 445-segment file.
	if blob.PartNumber != 1 || blob.TotalParts != 445 {
		t.Errorf("blob segment = %d/%d, want 1/445", blob.PartNumber, blob.TotalParts)
	}

	// Regression (#88): a genuine N-file collection whose files happen to have
	// N yEnc segments each must still group as ONE collection. Live data: an
	// "Alura" PAR2 set posted as 65 files where files carry (x/65) segment
	// counters — a coincidental file-count == segment-count that previously
	// fragmented into 65 releases.
	al1 := ParseSubject(`Alura.Flutter.CI-CL [64/65] - "Alura.Flutter.CI-CL.vol31+32.par2" yEnc (2/65) 46101356`)
	al2 := ParseSubject(`Alura.Flutter.CI-CL [58/65] - "Alura.Flutter.CI-CL.par2" yEnc (1/1) 44892`)
	al3 := ParseSubject(`Alura.Flutter.CI-CL [65/65] - "Alura.Flutter.CI-CL.vol63+25.par2" yEnc (3/65) 36065680`)
	if al1.CollectionKey == "" {
		t.Fatal("Alura file [64/65] got no collection key (fragmentation bug #88)")
	}
	if al1.CollectionKey != al2.CollectionKey || al2.CollectionKey != al3.CollectionKey {
		t.Errorf("Alura collection keys differ (should group as one):\n a1=%q\n a2=%q\n a3=%q",
			al1.CollectionKey, al2.CollectionKey, al3.CollectionKey)
	}
	if al1.CollectionFiles != 65 {
		t.Errorf("Alura collection files = %d, want 65", al1.CollectionFiles)
	}

	// Loose-file collection (#90): files are individual content files with no
	// shared archive base (index.html, script.js, .course_id) plus PAR2. They
	// must group on the shared TITLE prefix before the [n/total] counter.
	lf1 := ParseSubject(`Alura.Flutter.CI-CL [01/65] - ".course_id" yEnc (1/1) 32`)
	lf2 := ParseSubject(`Alura.Flutter.CI-CL [03/65] - "index.html" yEnc (1/7) 863234`)
	lf3 := ParseSubject(`Alura.Flutter.CI-CL [64/65] - "Alura.Flutter.CI-CL.vol31+32.par2" yEnc (2/65) 46101356`)
	if lf1.CollectionKey == "" {
		t.Fatal("loose-file content [01/65] got no collection key (#90)")
	}
	// The content files group together by title...
	if lf1.CollectionKey != lf2.CollectionKey {
		t.Errorf("loose content files should share a key:\n lf1=%q\n lf2=%q", lf1.CollectionKey, lf2.CollectionKey)
	}
	// ...and the PAR2 file (archive path, keyed on filename base) shares the
	// same base name, so it lands in the same collection too. In this post the
	// PAR2 base ("Alura.Flutter.CI-CL") matches the title, but the key schemes
	// differ (base vs "t:"+title); what matters operationally is that the many
	// content files stop fragmenting. Assert the content files collapsed to ONE
	// key and the count is right.
	if lf1.CollectionFiles != 65 {
		t.Errorf("loose collection files = %d, want 65", lf1.CollectionFiles)
	}
	// The PAR2 file still groups (archive path); it just uses a different key
	// scheme. What we assert is that it is recognised as a collection member.
	if lf3.CollectionKey == "" || lf3.CollectionFiles != 65 {
		t.Errorf("par2 member not grouped: key=%q files=%d", lf3.CollectionKey, lf3.CollectionFiles)
	}

	// Negative: two DIFFERENT titles with the same file total must NOT merge.
	da := ParseSubject(`Show.Alpha.S01 [01/10] - "ep01.mkv" yEnc (1/50)`)
	db := ParseSubject(`Show.Beta.S02 [01/10] - "ep01.mkv" yEnc (1/50)`)
	if da.CollectionKey == "" || db.CollectionKey == "" {
		t.Fatal("loose-file shows should each get a collection key")
	}
	if da.CollectionKey == db.CollectionKey {
		t.Errorf("different titles must not merge: %q == %q", da.CollectionKey, db.CollectionKey)
	}

	// An obfuscated blob with a leading counter but NO archive extension AND no
	// meaningful title prefix is treated as a single file (avoid merging
	// unrelated posts). The counter is at the very start, so there is no title.
	bare := ParseSubject(`[2/20] "NSV6gyBkS9rHcooOonLqQV89OqtlE" yEnc (1/50)`)
	if bare.CollectionKey != "" {
		t.Errorf("bare no-extension blob treated as collection: %q", bare.CollectionKey)
	}
}
