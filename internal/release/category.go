package release

import "regexp"

// Newznab category IDs used by the classifier. These mirror the seeded
// categories in the database.
const (
	CatConsole   = 1000
	CatMovies    = 2000
	CatMoviesSD  = 2030
	CatMoviesHD  = 2040
	CatMoviesUHD = 2045
	CatAudio     = 3000
	CatAudioMP3  = 3010
	CatAudioLoss = 3040
	CatPC        = 4000
	CatPCGames   = 4050
	CatTV        = 5000
	CatTVSD      = 5030
	CatTVHD      = 5040
	CatTVUHD     = 5045
	CatTVAnime   = 5070
	CatXXX       = 6000
	CatBooks     = 7000
	CatBooksEbook = 7020
	CatBooksMags = 7010
	CatOther     = 8000
)

// rule maps a compiled pattern to a category. Rules are evaluated in order;
// the first match wins, so more specific rules must precede general ones.
type rule struct {
	re  *regexp.Regexp
	cat int
}

// mustRule compiles a case-insensitive rule.
func mustRule(pattern string, cat int) rule {
	return rule{re: regexp.MustCompile(`(?i)` + pattern), cat: cat}
}

// rules is the ordered classification table. It runs against the SearchName
// form (lowercased, separators normalized to spaces).
var rules = []rule{
	// XXX first: adult tags are unambiguous and should not be miscategorized.
	mustRule(`\b(xxx|porn|brazzers|naughtyamerica|blacked|onlyfans)\b`, CatXXX),

	// TV: SxxExx / season-episode patterns, or explicit resolution combined
	// with episode markers.
	mustRule(`\bs\d{1,2}\s*e\d{1,3}\b`, CatTV),
	mustRule(`\b\d{1,2}x\d{2,3}\b`, CatTV),
	mustRule(`\bseason\s*\d{1,2}\b`, CatTV),
	mustRule(`\b(hdtv|pdtv|web[- ]?dl|webrip)\b.*\b(s\d{1,2}|episode)\b`, CatTV),
	mustRule(`\banime\b`, CatTVAnime),

	// Movies: a 4-digit year with common movie source/quality tags.
	mustRule(`\b(19|20)\d{2}\b.*\b(bluray|blu[- ]?ray|bdrip|brrip|dvdrip|web[- ]?dl|webrip|hdrip|remux|x264|x265|h264|h265|hevc)\b`, CatMovies),
	mustRule(`\b(1080p|2160p|720p|480p)\b.*\b(19|20)\d{2}\b`, CatMovies),

	// Audio.
	mustRule(`\b(flac|ape|wavpack|dsd)\b`, CatAudioLoss),
	mustRule(`\b(mp3|320kbps|v0|cbr|vbr|discography|album|single)\b`, CatAudioMP3),

	// Books.
	mustRule(`\b(epub|mobi|azw3|azw|kindle)\b`, CatBooksEbook),
	mustRule(`\b(magazine|comic|cbr|cbz)\b`, CatBooksMags),
	mustRule(`\b(ebook|retail\s*ebook)\b`, CatBooks),

	// PC / games / software.
	mustRule(`\b(pc\s*game|repack|codex|plaza|skidrow|fitgirl|gog|steamrip)\b`, CatPCGames),
	mustRule(`\b(x86|x64|win(dows)?\s*(7|8|10|11)?|keygen|iso|installer)\b`, CatPC),

	// Consoles.
	mustRule(`\b(ps[3-5]|playstation|xbox|xbox360|nintendo|switch|wii|nds|psp)\b`, CatConsole),
}

// Resolution tiers used to refine video categories.
const (
	resSD = iota
	resHD
	resUHD
)

var (
	reResUHD = regexp.MustCompile(`(?i)\b(2160p|4320p|4k|8k|uhd)\b`)
	reResHD  = regexp.MustCompile(`(?i)\b(1080p|1080i|720p|hd)\b`)
	reResSD  = regexp.MustCompile(`(?i)\b(480p|576p|360p|sd|dvdrip|dvd|vhs|xvid|divx)\b`)
)

// resolutionTier classifies the resolution implied by a normalized name. When
// no explicit resolution is present it defaults to HD, which is the most common
// modern posting and the least surprising default for clients.
func resolutionTier(s string) int {
	switch {
	case reResUHD.MatchString(s):
		return resUHD
	case reResHD.MatchString(s):
		return resHD
	case reResSD.MatchString(s):
		return resSD
	default:
		return resHD
	}
}

// refineVideoCategory maps a parent video category (Movies/TV) to its SD/HD/UHD
// child based on the resolution tier. Non-video and already-specific categories
// are returned unchanged.
func refineVideoCategory(parent int, s string) int {
	switch parent {
	case CatMovies:
		switch resolutionTier(s) {
		case resUHD:
			return CatMoviesUHD
		case resSD:
			return CatMoviesSD
		default:
			return CatMoviesHD
		}
	case CatTV:
		switch resolutionTier(s) {
		case resUHD:
			return CatTVUHD
		case resSD:
			return CatTVSD
		default:
			return CatTVHD
		}
	default:
		return parent
	}
}

// Categorize returns the best-matching Newznab category ID for a release name.
// The name is normalized via SearchName before matching. A Movies or TV match
// is refined to its SD/HD/UHD subcategory using resolution tags. When nothing
// matches it returns CatOther.
func Categorize(name string) int {
	s := SearchName(name)
	for _, r := range rules {
		if r.re.MatchString(s) {
			return refineVideoCategory(r.cat, s)
		}
	}
	return CatOther
}
