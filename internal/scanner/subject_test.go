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
