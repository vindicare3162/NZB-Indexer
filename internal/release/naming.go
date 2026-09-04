// Package release promotes complete binaries into searchable releases: it
// cleans up release names, assigns a Newznab category via a rule engine,
// computes size, and derives a stable hash for deduplication.
package release

import (
	"regexp"
	"strings"
)

var (
	// A leading file/segment counter like "[075/111] - " or "1/50 - ".
	reLeadingCounter = regexp.MustCompile(`^\s*[\[\(]?\d{1,6}\s*/\s*\d{1,6}[\]\)]?\s*[-–]?\s*`)
	// A quoted filename embedded in the subject.
	reQuoted = regexp.MustCompile(`"([^"]+)"`)
	// yEnc marker and trailing size annotations.
	reYenc  = regexp.MustCompile(`(?i)\byenc\b`)
	reBytes = regexp.MustCompile(`(?i)[-\s]+\d[\d,\.]*\s*bytes?\b`)
	// Trailing parenthetical segment counters.
	reTrailCounter = regexp.MustCompile(`[\(\[]\d{1,6}\s*/\s*\d{1,6}[\)\]]`)
	// A common file extension to strip from the display name.
	reFileExt = regexp.MustCompile(`(?i)\.(rar|r\d{2,3}|par2|nfo|sfv|zip|7z|nzb|vol\d+\+\d+\.par2|mkv|mp4|avi|mp3|flac|iso|epub|mobi|pdf)$`)
	// Collapse runs of whitespace.
	reWS = regexp.MustCompile(`\s+`)
)

// CleanName derives a human-readable release name from an article subject or a
// binary's normalized subject. It prefers an embedded quoted filename, strips
// counters, the yEnc marker, byte annotations, and a trailing archive
// extension, then tidies whitespace and separators.
func CleanName(subject string) string {
	name := subject

	// Prefer a quoted filename if present; it is usually the real name.
	if m := reQuoted.FindStringSubmatch(subject); m != nil {
		name = m[1]
	}

	name = reLeadingCounter.ReplaceAllString(name, "")
	name = reTrailCounter.ReplaceAllString(name, " ")
	name = reYenc.ReplaceAllString(name, " ")
	name = reBytes.ReplaceAllString(name, " ")
	name = reFileExt.ReplaceAllString(name, "")

	name = reWS.ReplaceAllString(name, " ")
	name = strings.Trim(name, " -–_.")
	return strings.TrimSpace(name)
}

// collectionBaseName recovers the collection base name from a collection key of
// the form "base/count" (as produced by the scanner). It strips the trailing
// "/<count>" segment, leaving the shared base name of the collection. A
// "t:"-prefixed key (a title-derived key for loose-file collections) has that
// marker removed so the release is named from the title itself.
func collectionBaseName(collectionKey string) string {
	key := strings.TrimPrefix(collectionKey, "t:")
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return key
}

// SearchName produces a normalized form used for text search: lowercased with
// separators collapsed to single spaces.
func SearchName(name string) string {
	s := strings.ToLower(name)
	// Treat common separators as spaces.
	s = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(s)
	s = reWS.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
