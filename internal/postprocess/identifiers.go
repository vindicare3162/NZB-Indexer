package postprocess

import (
	"regexp"
	"strings"

	"github.com/vindicare/goindex/internal/store"
)

// Identifier extraction from NFO text (#194).
//
// Sonarr and Radarr search this indexer by external id in preference to free
// text, so a release that carries no identifier is effectively unfindable by
// them. Scene NFOs almost always cite the IMDb title page, and increasingly
// cite TheTVDB/TMDB, so the NFO we already fetch during post-processing is the
// cheapest source of ids available — no external API call, no extra article
// fetch.
//
// Extraction is deliberately a pure function over the NFO text: it is the
// parsing half of the job and is tested without a database, matching how
// subject parsing is separated from storage elsewhere in this codebase.

var (
	// IMDb ids appear bare ("tt0111161") or inside a URL. Requiring the "tt"
	// prefix is what keeps this from matching arbitrary digit runs, of which an
	// NFO has many (bitrates, resolutions, sizes, dates).
	reNFOIMDB = regexp.MustCompile(`(?i)\btt\d{6,9}\b`)

	// TVDB and TMDB ids are bare integers, so they are only safe to take from a
	// URL that names the site. A loose integer match would pick up noise.
	reNFOTVDB = regexp.MustCompile(`(?i)thetvdb\.com[^\s]*?(?:[?&]id=|/series/|/movies/)(\d{1,9})\b`)
	reNFOTMDB = regexp.MustCompile(`(?i)themoviedb\.org/(?:movie|tv)/(\d{1,9})\b`)
)

// maxNFOIdentifiers bounds how many ids are taken from one NFO. A well-formed
// NFO cites a handful; a long list means we are matching noise, and writing it
// would pollute id search for every release that shares those ids.
const maxNFOIdentifiers = 8

// ExtractIdentifiers pulls external identifiers out of NFO text. The result is
// deduplicated and normalised, and is empty when the NFO carries none —
// which is the common case and is not an error.
func ExtractIdentifiers(nfo string) []store.ReleaseIdentifier {
	if strings.TrimSpace(nfo) == "" {
		return nil
	}
	var out []store.ReleaseIdentifier
	seen := make(map[store.ReleaseIdentifier]bool)

	add := func(source, raw string) {
		if len(out) >= maxNFOIdentifiers {
			return
		}
		id, ok := store.NormalizeIdentifier(source, raw)
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}

	for _, m := range reNFOIMDB.FindAllString(nfo, maxNFOIdentifiers+1) {
		add(store.IDSourceIMDB, m)
	}
	for _, m := range reNFOTVDB.FindAllStringSubmatch(nfo, maxNFOIdentifiers+1) {
		add(store.IDSourceTVDB, m[1])
	}
	for _, m := range reNFOTMDB.FindAllStringSubmatch(nfo, maxNFOIdentifiers+1) {
		add(store.IDSourceTMDB, m[1])
	}
	return out
}
