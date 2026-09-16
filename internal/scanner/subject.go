// Package scanner ingests newsgroup article headers via XOVER, parses their
// subjects into part metadata, and persists them as parts for later assembly.
package scanner

import (
	"regexp"
	"strconv"
	"strings"
)

// ParsedSubject holds the part metadata extracted from an article subject.
type ParsedSubject struct {
	// PartNumber is this article's position within the binary (1-based), or 0
	// when the subject carries no part counter.
	PartNumber int
	// TotalParts is the number of parts in the binary, or 0 when unknown.
	TotalParts int
	// FileName is the embedded quoted filename when present (e.g. from
	// `"movie.part01.rar"`), otherwise empty.
	FileName string
	// Normalized is the subject with the volatile part counter and yEnc
	// suffix stripped, used as a stable grouping key for parts of the same
	// binary.
	Normalized string
	// CollectionKey identifies a multi-file collection (a post spanning many
	// files, e.g. a rar set plus its PAR2). All files of one collection share
	// this key so the assembler can group them into a single binary/release.
	// Empty when the subject is not part of a recognised collection (a plain
	// single-file post), in which case the assembler falls back to Normalized.
	CollectionKey string
	// FileNumber is this file's 1-based position within the collection (from a
	// leading "[n/total]" counter), or 0 when unknown.
	FileNumber int
	// CollectionFiles is the number of files in the collection (the "total" of
	// a leading "[n/total]" counter), or 0 when the subject is not part of a
	// recognised multi-file collection.
	CollectionFiles int
}

// Part-counter patterns, tried in order. Usenet posters use several
// conventions; we normalise them all to (part, total).
var (
	// (1/120) or [1/120] or "1 of 120". The most common yEnc segment counter.
	reParenParts  = regexp.MustCompile(`[\(\[](\d{1,6})\s*/\s*(\d{1,6})[\)\]]`)
	reOfParts     = regexp.MustCompile(`(?i)\b(\d{1,6})\s+of\s+(\d{1,6})\b`)
	reFilePartRAR = regexp.MustCompile(`(?i)\.part(\d{1,4})\.`)

	// A quoted filename, e.g. "Some.Release.mkv".
	reQuotedName = regexp.MustCompile(`"([^"]+)"`)

	// reFileNameish recognises a quoted token that reads as a real filename
	// (a short trailing extension) rather than a release title. Subjects that
	// quote both — `"Show S01E06 Love (1997)" [03/18] - "show.part1.rar"` — put
	// the title first, so taking the first quote picked up the title, left
	// FileName wrong, and anchored the file-counter search ahead of the counter
	// so no counter was found at all (see quotedName).
	reFileNameish = regexp.MustCompile(`\.[A-Za-z0-9]{1,8}$`)

	// yEnc marker and trailing size annotations to strip during normalization.
	reYencMarker = regexp.MustCompile(`(?i)\byenc\b`)
	reWhitespace = regexp.MustCompile(`\s+`)

	// maxCounterToNameGap bounds how far a file counter may sit from the quoted
	// filename it enumerates. Most styles place them adjacent, give or take a
	// separator like " - ", but site-tagged posts wedge a banner between them:
	//
	//	~~ www.example.nl ~~ [38/96] ~~ www.other.nl ~~ post: "file.r36" yEnc (98/131)
	//
	// so the bound has to clear a banner. It is not what keeps an unrelated
	// "n/m"-shaped token (a year range such as "[2009/2010]") from being read
	// as a counter — reYearRange does that, and fileCounter prefers the
	// candidate nearest the filename regardless.
	maxCounterToNameGap = 64

	// reYearRange matches a bracketed span of two adjacent years, the one
	// "n/m"-shaped token that regularly appears in subjects without being a
	// counter.
	reYearRange = regexp.MustCompile(`^[\(\[](19|20)\d{2}\s*/\s*(19|20)\d{2}[\)\]]$`)

	// Trailing archive/volume/parity extensions used to reduce a per-file name
	// to the collection base name (so all volumes of a set share one key).
	reCollectionVolExt = regexp.MustCompile(`(?i)(\.part\d{1,5}|\.vol\d{1,6}\+\d{1,6}|\.r\d{2,4}|\.\d{2,4})?\.(rar|par2|zip|7z|sfv|nfo|nzb|001)$`)
)

// ParseSubject extracts part metadata and a normalized grouping key from an
// article subject line.
//
// The normalization goal is that every article belonging to one posted binary
// produces the same Normalized string, while the per-article counter (e.g.
// "(1/120)") is removed. This lets the assembler group parts reliably.
func ParseSubject(subject string) ParsedSubject {
	res := ParsedSubject{}

	// Extract a quoted filename if present. Its position also anchors the file
	// counter search below (see fileCounter).
	nameLo := -1
	if m := quotedName(subject); m != nil {
		res.FileName = strings.TrimSpace(subject[m[2]:m[3]])
		nameLo = m[0]
	}

	// The segment counter "(part/total)" is what varies per article; capture
	// the LAST such occurrence, since some subjects also contain a file
	// counter like [1/42] earlier. We treat the parenthesised yEnc counter as
	// authoritative for the segment number when both exist.
	segMatches := reParenParts.FindAllStringSubmatchIndex(subject, -1)
	var segLo, segHi int = -1, -1
	if len(segMatches) > 0 {
		last := segMatches[len(segMatches)-1]
		res.PartNumber = atoi(subject[last[2]:last[3]])
		res.TotalParts = atoi(subject[last[4]:last[5]])
		segLo, segHi = last[0], last[1]
	} else if m := reOfParts.FindStringSubmatch(subject); m != nil {
		res.PartNumber = atoi(m[1])
		res.TotalParts = atoi(m[2])
	} else if m := reFilePartRAR.FindStringSubmatch(subject); m != nil {
		// A .partNN. filename fragment gives a weak part hint.
		res.PartNumber = atoi(m[1])
	}

	res.Normalized = normalizeSubject(subject, segLo, segHi)

	parseCollection(subject, segLo, segHi, nameLo, &res)
	return res
}

// parseCollection detects a multi-file collection from a leading "[n/total]"
// file counter and derives a stable collection key shared by every file of the
// post. The key is the collection base name (the quoted filename with its
// archive/volume/parity extension stripped) combined with the file count, so
// that e.g. "Foo.par2", "Foo.part001.rar" ... "Foo.part112.rar" all map to the
// same key while a different post that happens to share a base name but has a
// different file count does not collide.
//
// When no leading file counter is present (a plain single-file post), the key
// is left empty and the assembler falls back to the normalized subject, so
// single-file behaviour is unchanged.
// fileCounter locates the counter that enumerates files within a collection,
// as opposed to the trailing yEnc segment counter. It returns the submatch
// index slice of the chosen counter, or nil when the subject carries none.
//
// Posting styles vary more than one anchored pattern can express:
//
//	[002/113] "file.rar" yEnc (1/464)                     counter first
//	Release.Name [64/65] - "file.par2" yEnc (1/12)         after a title
//	[10764]-[FULL]-[#grp]-[ Title ]-[03/25] - "file.r00"   after other brackets
//	cbc - rmr - s6e07 - (30/36) - "file.PAR2" yEnc (1/12)  parenthesised
//
// Requiring the counter to be the first bracketed group, in square brackets,
// missed the latter two outright — every file of such a post then became its
// own binary and its own release. What the styles share instead is position:
// the file counter sits just before the quoted filename, while the yEnc
// segment counter follows it. Select on that ordering rather than on bracket
// style or index, and take the counter closest to the filename when several
// qualify.
// quotedName picks the quoted token that is the article's filename. A subject
// may quote a release title as well, always ahead of the filename:
//
//	"La Femme Nikita S01E06 Love (1997)" [03/18] - "la.femme.nikita.s01e06.part1.rar" yEnc (139/206)
//
// so the LAST quoted token that ends in a short extension is the filename, and
// the last quoted token generally is when none of them looks like one. Taking
// the first instead mis-set FileName and, because the file-counter search is
// anchored just before the filename, pushed the anchor ahead of the counter so
// every file of such a post became its own binary and its own release.
func quotedName(subject string) []int {
	all := reQuotedName.FindAllStringSubmatchIndex(subject, -1)
	if len(all) == 0 {
		return nil
	}
	for i := len(all) - 1; i >= 0; i-- {
		if reFileNameish.MatchString(strings.TrimSpace(subject[all[i][2]:all[i][3]])) {
			return all[i]
		}
	}
	return all[len(all)-1]
}

func fileCounter(subject string, segLo, segHi, nameLo int) []int {
	var best []int
	for _, m := range reParenParts.FindAllStringSubmatchIndex(subject, -1) {
		if m[0] == segLo && m[1] == segHi {
			continue // the yEnc segment counter, not a file counter
		}
		if reYearRange.MatchString(subject[m[0]:m[1]]) {
			continue // "[2009/2010]" and friends are not counters
		}
		if nameLo >= 0 {
			// Anything at or past the filename is a segment counter or noise;
			// anything far ahead of it is an unrelated "n/m"-shaped token.
			if m[1] > nameLo || nameLo-m[1] > maxCounterToNameGap {
				continue
			}
		}
		best = m
	}
	return best
}

func parseCollection(subject string, segLo, segHi, nameLo int, res *ParsedSubject) {
	loc := fileCounter(subject, segLo, segHi, nameLo)
	if loc == nil {
		// No file counter anywhere. A multi-file post can still be recognised
		// when its filenames carry archive/parity extensions: every volume of
		// one set reduces to the same base name, so that base groups them.
		// How many files the set holds is unknowable from the Subject, which
		// is why CollectionFiles stays 0 — completeness for these is settled
		// on quiet time instead of a declared count (see SettleQuietCollections).
		if res.FileName != "" && reCollectionVolExt.MatchString(res.FileName) {
			if base := collectionBase(res.FileName); base != "" {
				res.CollectionKey = "b:" + base
			}
		}
		return
	}
	fileNum := atoi(subject[loc[2]:loc[3]])
	total := atoi(subject[loc[4]:loc[5]])
	// A "collection" needs at least two files; a "[1/1]" is a single file.
	if total < 2 {
		return
	}
	// Span of the counter itself, used below to split the title prefix from it
	// and to guard against the counter actually being the yEnc segment counter
	// (e.g. `[1/445] "blob" yEnc (1/445)` — one file, not a collection).
	brLo, brHi := loc[0], loc[1]
	if segLo == brLo && segHi == brHi {
		return
	}
	// dupCounter reports the same counter written twice — `[2256/2574] - "blob"
	// yEnc (2256/2574)`, one file in 2574 segments rather than 2574 files. The
	// span check above only catches a single counter read twice; matching values
	// catch the duplicate. It is consulted only by the obfuscated fallback
	// below: a title or an archive extension is independent evidence of a real
	// collection, and an archive set whose file and segment counts coincide is
	// still an archive set.
	dupCounter := res.TotalParts > 0 && fileNum == res.PartNumber && total == res.TotalParts
	// Derive the collection key. Two cases:
	//
	//  (a) Title-prefixed post: a release title precedes the "[n/total]" file
	//      counter and is identical across every file of the post (both loose
	//      content files like index.html/.course_id AND the PAR2 set). Key on
	//      that title + file count so the WHOLE post — content and parity —
	//      collapses into ONE collection. This is false-merge-safe because two
	//      unrelated posts won't share both an identical title and file total.
	//
	//  (b) Archive set with no title (classic "[n/total] \"Foo.partNN.rar\""):
	//      no title prefix, but the per-file name carries a volume/parity
	//      extension. Key on the filename base so all volumes share one key.
	//
	if title := collectionTitle(subject, brLo); title != "" {
		res.FileNumber = fileNum
		res.CollectionFiles = total
		res.CollectionKey = "t:" + title + "/" + strconv.Itoa(total)
		return
	}
	if res.FileName != "" && reCollectionVolExt.MatchString(res.FileName) {
		if base := collectionBase(res.FileName); base != "" {
			res.FileNumber = fileNum
			res.CollectionFiles = total
			res.CollectionKey = base + "/" + strconv.Itoa(total)
			return
		}
	}

	// Fallback for obfuscated posts: no title prefix and no archive extension,
	// but a leading [n/total] file counter with total >= 2. Use the quoted
	// filename (the obfuscated name) as the collection key. All files of the
	// same obfuscated post share the same quoted filename, so they all group
	// together into one release. This is false-merge-safe because two unrelated
	// obfuscated posts would need to share the same random hex string and file
	// count, which is effectively impossible.
	if res.FileName != "" && total >= 2 && !dupCounter {
		res.FileNumber = fileNum
		res.CollectionFiles = total
		res.CollectionKey = res.FileName + "/" + strconv.Itoa(total)
	}
}

// collectionTitle extracts and normalises the release title that precedes the
// leading "[n/total]" file counter (whose "[" is at index brLo). It is used as
// the collection key for loose-file posts that lack a shared archive base. An
// empty or too-short result signals "no reliable title", so the caller leaves
// the post ungrouped rather than risk merging unrelated single files.
func collectionTitle(subject string, brLo int) string {
	if brLo <= 0 || brLo > len(subject) {
		return ""
	}
	// Preserve original case: all files of one post share a byte-identical
	// prefix, so grouping does not need lowercasing, and keeping the case lets
	// the release be named nicely from the key.
	title := strings.TrimSpace(subject[:brLo])
	title = reWhitespace.ReplaceAllString(title, " ")
	title = strings.Trim(title, " -–_.")
	// Require a reasonably specific title so we do not group on a stray token.
	if len(title) < 8 {
		return ""
	}
	return title
}

// collectionBase reduces a per-file name to the shared collection base by
// stripping a trailing archive/volume/parity extension (e.g. ".part001.rar",
// ".vol01+02.par2", ".r03", ".par2"). Returns "" when there is no filename.
func collectionBase(fileName string) string {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		return ""
	}
	if loc := reCollectionVolExt.FindStringIndex(fileName); loc != nil {
		fileName = fileName[:loc[0]]
	}
	return strings.TrimSpace(fileName)
}

// normalizeSubject builds the stable grouping key. It removes the identified
// segment counter span (segLo:segHi, when >=0), strips any remaining
// parenthesised/bracketed counters, drops the yEnc marker, and collapses
// whitespace.
func normalizeSubject(subject string, segLo, segHi int) string {
	s := subject
	if segLo >= 0 && segHi <= len(subject) && segLo < segHi {
		// Strip only the identified segment counter span. File-level counters
		// like [075/111] are deliberately retained: they distinguish
		// different files within the same posted collection and are part of
		// the grouping key.
		s = subject[:segLo] + " " + subject[segHi:]
	}
	// When segLo was set we already removed the segment counter; any
	// remaining paren/bracket counters are file-level and intentionally kept.
	// Remove "n of m" segment forms.
	s = reOfParts.ReplaceAllString(s, " ")
	// Drop the yEnc marker word.
	s = reYencMarker.ReplaceAllString(s, " ")
	// Remove standalone byte-size annotations like "- 12345678 bytes".
	s = stripBytesAnnotation(s)
	// Collapse whitespace and trim punctuation debris.
	s = reWhitespace.ReplaceAllString(s, " ")
	s = strings.Trim(s, " -_.")
	return strings.TrimSpace(s)
}

var reBytesAnnotation = regexp.MustCompile(`(?i)[-\s]+\d[\d,\.]*\s*bytes?\b`)

func stripBytesAnnotation(s string) string {
	return reBytesAnnotation.ReplaceAllString(s, " ")
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
