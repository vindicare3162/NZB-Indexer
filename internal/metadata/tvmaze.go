package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TVMaze is a keyless TV metadata provider backed by the public TVMaze API
// (https://www.tvmaze.com/api). It matches TV releases by show title. No API
// key is required, which makes it a sensible default provider.
type TVMaze struct {
	client  *http.Client
	baseURL string
}

// NewTVMaze creates a TVMaze provider. A nil client uses a default with a
// sensible timeout.
func NewTVMaze(client *http.Client) *TVMaze {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TVMaze{client: client, baseURL: "https://api.tvmaze.com"}
}

func (t *TVMaze) Name() string { return "tvmaze" }

// tvmazeShow mirrors the subset of the TVMaze show JSON we use.
type tvmazeShow struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Premiered string `json:"premiered"` // "YYYY-MM-DD"
	Summary   string `json:"summary"`   // HTML
	Image     struct {
		Original string `json:"original"`
		Medium   string `json:"medium"`
	} `json:"image"`
}

// Lookup matches a TV query to a show. Movie/non-TV queries are a definitive
// miss for this provider. Season/episode from the query are echoed back on the
// result (TVMaze show search is show-level; per-episode lookup is out of scope
// for the keyless default).
func (t *TVMaze) Lookup(ctx context.Context, q Query) (Result, bool, error) {
	if !q.IsTV || strings.TrimSpace(q.Title) == "" {
		return Result{}, false, nil
	}

	u := t.baseURL + "/singlesearch/shows?q=" + url.QueryEscape(q.Title)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, false, fmt.Errorf("build tvmaze request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return Result{}, false, fmt.Errorf("tvmaze request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		// matched below
	case http.StatusNotFound:
		return Result{}, false, nil // definitive miss
	default:
		return Result{}, false, fmt.Errorf("tvmaze status %d", resp.StatusCode)
	}

	var show tvmazeShow
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return Result{}, false, fmt.Errorf("decode tvmaze response: %w", err)
	}
	if show.ID == 0 || show.Name == "" {
		return Result{}, false, nil
	}

	poster := show.Image.Original
	if poster == "" {
		poster = show.Image.Medium
	}

	return Result{
		Title:      show.Name,
		Year:       yearFromDate(show.Premiered),
		Season:     q.Season,
		Episode:    q.Episode,
		Source:     t.Name(),
		ExternalID: strconv.Itoa(show.ID),
		PosterURL:  poster,
		Overview:   stripHTML(show.Summary),
	}, true, nil
}

func yearFromDate(d string) int {
	if len(d) >= 4 {
		if y, err := strconv.Atoi(d[:4]); err == nil {
			return y
		}
	}
	return 0
}

var reHTMLTag = regexp.MustCompile(`<[^>]*>`)

// stripHTML removes tags from the TVMaze summary and trims whitespace.
func stripHTML(s string) string {
	return strings.TrimSpace(reHTMLTag.ReplaceAllString(s, ""))
}
