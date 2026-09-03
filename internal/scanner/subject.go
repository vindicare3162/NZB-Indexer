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

	// yEnc marker and trailing size annotations to strip during normalization.
	reYencMarker = regexp.MustCompile(`(?i)\byenc\b`)
	reWhitespace = regexp.MustCompile(`\s+`)

	// A LEADING file counter like "[002/113]" or "(002/113)" at the very start
	// of the subject. This counts files within a multi-file collection, and is
	// distinct from the trailing yEnc segment counter "(1/464)".
	reLeadingFileCounter = regexp.MustCompile(`^\s*[\[\(](\d{1,6})\s*/\s*(\d{1,6})[\]\)]`)

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

	// Extract a quoted filename if present.
	if m := reQuotedName.FindStringSubmatch(subject); m != nil {
		res.FileName = strings.TrimSpace(m[1])
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

	parseCollection(subject, &res)
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
func parseCollection(subject string, res *ParsedSubject) {
	m := reLeadingFileCounter.FindStringSubmatch(subject)
	if m == nil {
		return
	}
	fileNum := atoi(m[1])
	total := atoi(m[2])
	// A "collection" needs at least two files; a "[1/1]" is a single file.
	if total < 2 {
		return
	}
	base := collectionBase(res.FileName)
	if base == "" {
		// No usable filename to anchor the collection; fall back to grouping
		// by normalized subject rather than risk merging unrelated posts.
		return
	}
	res.FileNumber = fileNum
	res.CollectionFiles = total
	res.CollectionKey = base + "/" + strconv.Itoa(total)
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
