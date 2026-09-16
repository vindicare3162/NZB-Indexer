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
	// As of the fix for obfuscated multi-file collections, these now DO get a
	// collection key keyed on the quoted filename + file count, because all 20
	// files of the same obfuscated post share the same obfuscated name and
	// should be grouped together into one release.
	bare := ParseSubject(`[2/20] "NSV6gyBkS9rHcooOonLqQV89OqtlE" yEnc (1/50)`)
	if bare.CollectionKey == "" {
		t.Fatal("obfuscated multi-file post should now get a collection key")
	}
	if bare.CollectionKey != "NSV6gyBkS9rHcooOonLqQV89OqtlE/20" {
		t.Errorf("collection key = %q, want %q", bare.CollectionKey, "NSV6gyBkS9rHcooOonLqQV89OqtlE/20")
	}
	if bare.FileNumber != 2 || bare.CollectionFiles != 20 {
		t.Errorf("file number = %d/%d, want 2/20", bare.FileNumber, bare.CollectionFiles)
	}
}

// TestParseCollectionCounterPlacement covers file-counter styles that the
// original leading-square-bracket-only pattern missed, each of which caused
// every file of a post to become its own release (#178 follow-up). Subjects
// are real examples taken from the index.
func TestParseCollectionCounterPlacement(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		wantFile int
		wantOf   int
	}{
		{
			name:     "counter after other bracketed segments",
			subject:  `[10764]-[FULL]-[#a.b.teevee@EFNet]-[ Body.Language.S01E10.DVDRip.XviD-aAF ]-[03/25] - "aaf-body.language.s01e10.next.stop.porn.r00" yEnc (1/12)`,
			wantFile: 3, wantOf: 25,
		},
		{
			name:     "parenthesised file counter",
			subject:  `cbc - rmr - s6e07 - (30/36) - "081118.cbc.the.mercer.report.s6e07.640x464.xvid.mp3.vol00+01.PAR2" yEnc (1/12)`,
			wantFile: 30, wantOf: 36,
		},
		{
			name:     "counter after a bracketed presenter tag",
			subject:  `[United-Forums.co.uk Present] The IMDB Top 100 Movies *Repost* [00/90] - "08 Pulp Fiction (1994).par2" yEnc (1/9)`,
			wantFile: 0, wantOf: 90,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSubject(tc.subject)
			if got.CollectionKey == "" {
				t.Fatalf("no collection key; every file of this post would become its own release")
			}
			if got.FileNumber != tc.wantFile || got.CollectionFiles != tc.wantOf {
				t.Errorf("file counter = %d/%d, want %d/%d",
					got.FileNumber, got.CollectionFiles, tc.wantFile, tc.wantOf)
			}
		})
	}
}

// TestParseCollectionIgnoresNonCounters guards the widened search against
// "n/m"-shaped tokens that are not file counters.
func TestParseCollectionIgnoresNonCounters(t *testing.T) {
	// A year range far from the filename must not be read as a file counter:
	// doing so sets an unreachable collection_files and the post never
	// completes.
	got := ParseSubject(`Some.Documentary.Series [2009/2010] Collectors Edition - "disc1.rar" yEnc (1/50)`)
	if got.CollectionFiles == 2010 {
		t.Errorf("year range parsed as a file counter: files=%d", got.CollectionFiles)
	}

	// The yEnc segment counter alone is not a file counter.
	got = ParseSubject(`"single.file.mkv" yEnc (1/50)`)
	if got.CollectionKey != "" {
		t.Errorf("segment counter treated as collection: key=%q", got.CollectionKey)
	}
}

// TestParseCollectionGroupsWholePost is the property that matters: every file
// of one post must produce the same collection key, since that key is what
// folds them into a single binary (and so a single release).
func TestParseCollectionGroupsWholePost(t *testing.T) {
	post := []string{
		`[10764]-[FULL]-[#a.b.teevee@EFNet]-[ Body.Language.S01E10.DVDRip.XviD-aAF ]-[01/25] - "aaf-body.language.s01e10.next.stop.porn.nfo" yEnc (1/1)`,
		`[10764]-[FULL]-[#a.b.teevee@EFNet]-[ Body.Language.S01E10.DVDRip.XviD-aAF ]-[03/25] - "aaf-body.language.s01e10.next.stop.porn.r00" yEnc (1/20)`,
		`[10764]-[FULL]-[#a.b.teevee@EFNet]-[ Body.Language.S01E10.DVDRip.XviD-aAF ]-[20/25] - "aaf-body.language.s01e10.next.stop.porn.sfv" yEnc (1/1)`,
		`[10764]-[FULL]-[#a.b.teevee@EFNet]-[ Body.Language.S01E10.DVDRip.XviD-aAF ]-[24/25] - "aaf-body.language.s01e10.next.stop.porn.vol07+8.par2" yEnc (1/13)`,
	}

	var key string
	for i, subj := range post {
		got := ParseSubject(subj)
		if got.CollectionKey == "" {
			t.Fatalf("file %d produced no collection key", i)
		}
		if i == 0 {
			key = got.CollectionKey
			continue
		}
		if got.CollectionKey != key {
			t.Errorf("file %d key = %q, want %q (post would split into separate releases)",
				i, got.CollectionKey, key)
		}
	}
	t.Logf("shared collection key: %s", key)
}

// TestParseCollectionNoCounter covers multi-file posts that carry no file
// counter at all. Their filenames still reduce to a shared archive base, which
// groups the post; the file count is unknowable from the Subject, so
// CollectionFiles stays 0 and completeness is settled on quiet time instead.
func TestParseCollectionNoCounter(t *testing.T) {
	post := []string{
		`(0133) "212acs5126.par2" - HDTV - EPZ yEnc (1/1)`,
		`(2733) "212acs5126.vol01+01.PAR2" - HDTV - EPZ yEnc (1/4)`,
		`(2733) "212acs5126.part02.rar" - HDTV - EPZ yEnc (1/20)`,
	}
	var key string
	for i, subj := range post {
		got := ParseSubject(subj)
		if got.CollectionKey == "" {
			t.Fatalf("file %d: no collection key, post would split per file", i)
		}
		if got.CollectionFiles != 0 {
			t.Errorf("file %d: CollectionFiles = %d, want 0 (count is unknown)", i, got.CollectionFiles)
		}
		if i == 0 {
			key = got.CollectionKey
			continue
		}
		if got.CollectionKey != key {
			t.Errorf("file %d key = %q, want %q", i, got.CollectionKey, key)
		}
	}
	t.Logf("shared key: %s", key)

	// A plain single file with no archive extension must stay ungrouped rather
	// than collide with unrelated posts on a generic base name.
	if got := ParseSubject(`"holiday.jpg" yEnc (1/1)`); got.CollectionKey != "" {
		t.Errorf("non-archive single file grouped: %q", got.CollectionKey)
	}
}

// TestParseCollectionQuotedTitleBeforeFilename covers subjects that quote a
// release title as well as a filename. The title always comes first, so taking
// the first quoted token picked up the title: FileName was wrong, and because
// the file-counter search is anchored just before the filename, the anchor
// landed ahead of the counter and no counter was found at all — every file of
// the post then became its own binary and its own release.
func TestParseCollectionQuotedTitleBeforeFilename(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		wantName string
		wantFile int
		wantOf   int
	}{
		{
			name:     "quoted title then quoted filename",
			subject:  `"La Femme Nikita S01E06 Love (1997)" [03/18] - "la.femme.nikita.s01e06.part1.rar" yEnc (139/206)`,
			wantName: "la.femme.nikita.s01e06.part1.rar",
			wantFile: 3, wantOf: 18,
		},
		{
			name:     "title itself ends in a dotted token",
			subject:  `Brothers-of-Usenet.org "Two.and.a.Half.Men.S06DVD1.DVDR.German.DL.BoU"[019/141] - "BoU-TAAHM-S6D1.part017.rar" yEnc (94/137)`,
			wantName: "BoU-TAAHM-S6D1.part017.rar",
			wantFile: 19, wantOf: 141,
		},
		{
			name:     "obfuscated name quoted twice",
			subject:  `"82417MSTOXL42"     [071/211] - "82417MSTOXL42.part070.rar" yEnc (0520/1261)`,
			wantName: "82417MSTOXL42.part070.rar",
			wantFile: 71, wantOf: 211,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSubject(tc.subject)
			if got.FileName != tc.wantName {
				t.Errorf("FileName = %q, want %q", got.FileName, tc.wantName)
			}
			if got.CollectionKey == "" {
				t.Fatalf("no collection key; every file of this post would become its own release")
			}
			if got.FileNumber != tc.wantFile || got.CollectionFiles != tc.wantOf {
				t.Errorf("file counter = %d/%d, want %d/%d",
					got.FileNumber, got.CollectionFiles, tc.wantFile, tc.wantOf)
			}
		})
	}
}

// TestParseCollectionCounterBehindBanner covers site-tagged posts, which wedge
// a banner between the file counter and the filename. The counter is still the
// post's file counter, so the gap bound has to clear the banner.
func TestParseCollectionCounterBehindBanner(t *testing.T) {
	got := ParseSubject(`~~ www.example.nl ~~ [38/96] ~~ www.other.nl ~~ nieuwsgroepen~~ post: "nmrcncrmpal.r36" yEnc (098/131)`)
	if got.CollectionKey == "" {
		t.Fatalf("no collection key; every file of this post would become its own release")
	}
	if got.FileNumber != 38 || got.CollectionFiles != 96 {
		t.Errorf("file counter = %d/%d, want 38/96", got.FileNumber, got.CollectionFiles)
	}
}

// TestParseCollectionDuplicateCounter covers the same counter written twice,
// which is one file in many segments rather than many files. Declaring a file
// count the post can never reach leaves the binary permanently incomplete.
func TestParseCollectionDuplicateCounter(t *testing.T) {
	// Obfuscated name, no other evidence of a collection: not a collection.
	got := ParseSubject(`[2256/2574] - "7e808f5e537ffc3c" yEnc (2256/2574) 882223104`)
	if got.CollectionKey != "" {
		t.Errorf("duplicated segment counter read as a collection: key=%q files=%d",
			got.CollectionKey, got.CollectionFiles)
	}

	// An archive extension is independent evidence of a real set, so coinciding
	// counts must not stop it grouping by volume base.
	got = ParseSubject(`[13/62] "gMP0zQ8BE47EzPwVQdPwUl.part12.rar" (13/62)`)
	if got.CollectionKey == "" {
		t.Errorf("archive set left ungrouped because its counts coincide")
	}
}
