package newznab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// Repo is the subset of the store the Newznab API needs.
type Repo interface {
	ListCategories(ctx context.Context) ([]store.Category, error)
	SearchReleases(ctx context.Context, f store.SearchFilter) ([]store.Release, int, error)
	GetReleaseByGUID(ctx context.Context, guid string) (store.Release, error)
	IncrementGrabs(ctx context.Context, id int64) error
}

// NZBGenerator builds an NZB document for a release GUID.
type NZBGenerator interface {
	ForGUID(ctx context.Context, guid string) (data []byte, filename string, err error)
}

// Handler serves the Newznab-compatible /api endpoint.
type Handler struct {
	repo    Repo
	nzb     NZBGenerator
	baseURL string
	maxLim  int
	defLim  int
}

// Config configures the Handler.
type Config struct {
	// BaseURL is the externally reachable base (e.g. http://host:8080) used to
	// build absolute NZB download links in feed enclosures.
	BaseURL string
	// MaxLimit and DefaultLimit bound result page sizes.
	MaxLimit     int
	DefaultLimit int
}

// NewHandler constructs a Newznab API handler.
func NewHandler(repo Repo, nzb NZBGenerator, cfg Config) *Handler {
	if cfg.MaxLimit <= 0 {
		cfg.MaxLimit = 100
	}
	if cfg.DefaultLimit <= 0 {
		cfg.DefaultLimit = 100
	}
	return &Handler{
		repo:    repo,
		nzb:     nzb,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		maxLim:  cfg.MaxLimit,
		defLim:  cfg.DefaultLimit,
	}
}

// ServeHTTP dispatches on the Newznab t= function parameter.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch strings.ToLower(q.Get("t")) {
	case "caps", "":
		h.handleCaps(w, r)
	case "search", "tvsearch", "movie", "music", "book":
		h.handleSearch(w, r)
	case "get":
		h.handleGet(w, r)
	case "details":
		h.handleDetails(w, r)
	default:
		writeError(w, 202, "No such function")
	}
}

// handleCaps returns the capabilities document.
func (h *Handler) handleCaps(w http.ResponseWriter, r *http.Request) {
	cats, err := h.repo.ListCategories(r.Context())
	if err != nil {
		writeError(w, 900, "failed to load categories")
		return
	}

	c := caps{
		Server: capsServer{
			Version:   "1.0",
			Title:     "goindex",
			Strapline: "self-hosted NZB indexer",
			URL:       h.baseURL,
		},
		Limits: capsLimits{Max: h.maxLim, Default: h.defLim},
		Searching: capsSearching{
			Search:      capsSearch{Available: "yes", SupportedParams: "q,cat,limit,offset"},
			TVSearch:    capsSearch{Available: "yes", SupportedParams: "q,cat,season,ep,rid,tvdbid,limit,offset"},
			MovieSearch: capsSearch{Available: "yes", SupportedParams: "q,cat,imdbid,limit,offset"},
			AudioSearch: capsSearch{Available: "yes", SupportedParams: "q,cat,limit,offset"},
			BookSearch:  capsSearch{Available: "yes", SupportedParams: "q,cat,limit,offset"},
		},
		Categories: buildCapsCategories(cats),
	}
	writeXML(w, http.StatusOK, c)
}

// handleSearch runs a release search and returns an RSS feed.
func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := store.SearchFilter{
		Query:      buildQuery(q),
		Categories: parseCategories(q.Get("cat")),
		Limit:      h.clampLimit(q.Get("limit")),
		Offset:     parseInt(q.Get("offset"), 0),
	}

	releases, total, err := h.repo.SearchReleases(r.Context(), filter)
	if err != nil {
		writeError(w, 900, "search failed")
		return
	}

	feed := rss{
		Version:      "2.0",
		NewznabXMLNS: newznabNS,
		AtomXMLNS:    "http://www.w3.org/2005/Atom",
		Channel: channel{
			Title:       "goindex",
			Description: "goindex search results",
			Link:        h.baseURL,
			Response:    nnResponse{Offset: filter.Offset, Total: total},
		},
	}
	for _, rel := range releases {
		feed.Channel.Items = append(feed.Channel.Items, h.releaseToItem(rel))
	}
	writeXML(w, http.StatusOK, feed)
}

// handleGet streams the NZB for a release. The Newznab convention is
// t=get&id=<guid>.
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	guid := r.URL.Query().Get("id")
	if guid == "" {
		guid = r.URL.Query().Get("guid")
	}
	if guid == "" {
		writeError(w, 200, "Missing parameter (id)")
		return
	}

	data, filename, err := h.nzb.ForGUID(r.Context(), guid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 300, "No such item")
			return
		}
		writeError(w, 900, "failed to generate NZB")
		return
	}

	// Best-effort grab counter increment.
	if rel, err := h.repo.GetReleaseByGUID(r.Context(), guid); err == nil {
		_ = h.repo.IncrementGrabs(r.Context(), rel.ID)
	}

	w.Header().Set("Content-Type", "application/x-nzb")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleDetails returns a single-item feed for one release.
func (h *Handler) handleDetails(w http.ResponseWriter, r *http.Request) {
	guid := r.URL.Query().Get("id")
	if guid == "" {
		guid = r.URL.Query().Get("guid")
	}
	rel, err := h.repo.GetReleaseByGUID(r.Context(), guid)
	if err != nil {
		writeError(w, 300, "No such item")
		return
	}
	feed := rss{
		Version:      "2.0",
		NewznabXMLNS: newznabNS,
		AtomXMLNS:    "http://www.w3.org/2005/Atom",
		Channel: channel{
			Title:    "goindex",
			Link:     h.baseURL,
			Response: nnResponse{Offset: 0, Total: 1},
			Items:    []item{h.releaseToItem(rel)},
		},
	}
	writeXML(w, http.StatusOK, feed)
}

// releaseToItem converts a stored release into a feed item with newznab attrs.
func (h *Handler) releaseToItem(rel store.Release) item {
	dlURL := fmt.Sprintf("%s/api?t=get&id=%s", h.baseURL, rel.GUID)
	pub := rel.CreatedAt
	if rel.PostedAt != nil {
		pub = *rel.PostedAt
	}

	catStr := ""
	if rel.CategoryID != nil {
		catStr = strconv.Itoa(*rel.CategoryID)
	}

	it := item{
		Title:    rel.Name,
		GUID:     rel.GUID,
		Link:     dlURL,
		PubDate:  pub.UTC().Format(time.RFC1123Z),
		Category: catStr,
		Enclosure: enclosure{
			URL:    dlURL,
			Length: rel.SizeBytes,
			Type:   "application/x-nzb",
		},
		Attrs: []nnAttr{
			{Name: "size", Value: strconv.FormatInt(rel.SizeBytes, 10)},
			{Name: "grabs", Value: strconv.FormatInt(rel.Grabs, 10)},
		},
	}
	if catStr != "" {
		it.Attrs = append(it.Attrs, nnAttr{Name: "category", Value: catStr})
	}
	return it
}

// clampLimit parses and bounds the requested page size.
func (h *Handler) clampLimit(s string) int {
	n := parseInt(s, h.defLim)
	if n <= 0 {
		n = h.defLim
	}
	if n > h.maxLim {
		n = h.maxLim
	}
	return n
}
