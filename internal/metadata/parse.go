package metadata

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// SxxExx (with optional separators) — the primary TV episode marker.
	reSxxExx = regexp.MustCompile(`(?i)\bs(\d{1,2})\s*e(\d{1,3})\b`)
	// NxNN form, e.g. "2x05".
	reNxNN = regexp.MustCompile(`\b(\d{1,2})x(\d{2,3})\b`)
	// "Season N" (episode unknown).
	reSeasonWord = regexp.MustCompile(`(?i)\bseason\s*(\d{1,2})\b`)
	// A 4-digit year, 1900–2099.
	reYear = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
)

// ParseName extracts a lookup Query from a (human-readable) release name. It
// derives the title by taking the text before the first season/episode/year
// marker, then normalising separators to spaces. isTV comes from the caller's
// category classification; it controls whether season/episode are meaningful.
func ParseName(name string, isTV bool) Query {
	q := Query{IsTV: isTV}

	// Normalise separators to spaces for a stable working copy.
	work := strings.NewReplacer(".", " ", "_", " ").Replace(name)

	// Find the earliest boundary marker so the title is everything before it.
	boundary := len(work)
	consider := func(loc []int) {
		if loc != nil && loc[0] < boundary {
			boundary = loc[0]
		}
	}

	if m := reSxxExx.FindStringSubmatchIndex(work); m != nil {
		q.Season = atoi(work[m[2]:m[3]])
		q.Episode = atoi(work[m[4]:m[5]])
		consider(m)
	} else if m := reNxNN.FindStringSubmatchIndex(work); m != nil {
		q.Season = atoi(work[m[2]:m[3]])
		q.Episode = atoi(work[m[4]:m[5]])
		consider(m)
	} else if m := reSeasonWord.FindStringSubmatchIndex(work); m != nil {
		q.Season = atoi(work[m[2]:m[3]])
		consider(m)
	}

	// Year (used as a boundary and to disambiguate). Prefer the first year that
	// appears; for movies it is usually right after the title.
	if m := reYear.FindStringIndex(work); m != nil {
		if y := atoi(work[m[0]:m[1]]); y > 0 {
			q.Year = y
		}
		consider(m)
	}

	title := work
	if boundary < len(work) {
		title = work[:boundary]
	}
	q.Title = cleanTitle(title)
	return q
}

// cleanTitle tidies a candidate title: collapse whitespace, trim separator
// punctuation and a trailing release-group/dash, and lower-case for matching.
func cleanTitle(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.Trim(s, " -–_.")
	return strings.TrimSpace(s)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
