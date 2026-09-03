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
}
