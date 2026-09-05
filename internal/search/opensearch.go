package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// OpenSearchBackend is an optional derived release-search index backed by
// OpenSearch/Elasticsearch (#139). It speaks the REST API directly over
// net/http (no client dependency). Documents are denormalized releases; the
// store remains authoritative, so this index is always rebuildable from
// PostgreSQL.
type OpenSearchBackend struct {
	baseURL string // e.g. http://localhost:9200
	index   string // e.g. goindex-releases
	client  *http.Client
}

// releaseDoc is the denormalized release document indexed into OpenSearch. It
// carries the fields advanced searches and facets need. No credentials or
// article contents are included.
type releaseDoc struct {
	GUID       string    `json:"guid"`
	Name       string    `json:"name"`
	SearchName string    `json:"search_name"`
	CategoryID *int      `json:"category_id,omitempty"`
	SizeBytes  int64     `json:"size_bytes"`
	Obfuscated bool      `json:"obfuscated"`
	PostedAt   time.Time `json:"posted_at"`
	// SortAt is the recency key (posted_at, falling back to created_at) used for
	// the default newest-first ordering.
	SortAt time.Time `json:"sort_at"`
}

// NewOpenSearchBackend builds an OpenSearch-backed search index. timeout<=0
// uses 10s.
func NewOpenSearchBackend(baseURL, index string, timeout time.Duration) *OpenSearchBackend {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &OpenSearchBackend{
		baseURL: strings.TrimRight(baseURL, "/"),
		index:   index,
		client:  &http.Client{Timeout: timeout},
	}
}

func (o *OpenSearchBackend) Name() string { return "opensearch" }

// docFromRelease denormalizes a store.Release into a search document.
func docFromRelease(r store.Release) releaseDoc {
	sort := r.CreatedAt
	if r.PostedAt != nil {
		sort = *r.PostedAt
	}
	posted := sort
	if r.PostedAt != nil {
		posted = *r.PostedAt
	}
	return releaseDoc{
		GUID: r.GUID, Name: r.Name, SearchName: r.SearchName,
		CategoryID: r.CategoryID, SizeBytes: r.SizeBytes, Obfuscated: false,
		PostedAt: posted, SortAt: sort,
	}
}

// IndexRelease upserts a release document (idempotent: the guid is the doc id).
func (o *OpenSearchBackend) IndexRelease(ctx context.Context, r store.Release) error {
	body, err := json.Marshal(docFromRelease(r))
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/_doc/%s", o.baseURL, o.index, r.GUID)
	return o.do(ctx, http.MethodPut, url, body, nil)
}

// DeleteRelease removes a release document by guid (idempotent: a missing doc
// is not an error).
func (o *OpenSearchBackend) DeleteRelease(ctx context.Context, guid string) error {
	url := fmt.Sprintf("%s/%s/_doc/%s", o.baseURL, o.index, guid)
	err := o.do(ctx, http.MethodDelete, url, nil, nil)
	if err != nil && strings.Contains(err.Error(), "status 404") {
		return nil
	}
	return err
}

// osSearchResponse is the subset of the _search response we consume.
type osSearchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			Source releaseDoc `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// Search runs a fuzzy/ranked query against the index and maps hits back into
// store.Release results. It supports the query text, category filter, and
// obfuscated exclusion; keyset cursors are not used (OpenSearch paginates by
// from/size), so offset-based pagination is applied.
func (o *OpenSearchBackend) Search(ctx context.Context, f store.SearchFilter) (store.SearchResult, error) {
	query := buildOSQuery(f)
	body, err := json.Marshal(query)
	if err != nil {
		return store.SearchResult{}, err
	}
	var resp osSearchResponse
	url := fmt.Sprintf("%s/%s/_search", o.baseURL, o.index)
	if err := o.do(ctx, http.MethodPost, url, body, &resp); err != nil {
		return store.SearchResult{}, err
	}

	out := store.SearchResult{Total: resp.Hits.Total.Value}
	for _, h := range resp.Hits.Hits {
		d := h.Source
		out.Releases = append(out.Releases, store.Release{
			GUID: d.GUID, Name: d.Name, SearchName: d.SearchName,
			CategoryID: d.CategoryID, SizeBytes: d.SizeBytes,
			PostedAt: nonZeroTime(d.PostedAt),
		})
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	out.HasMore = len(out.Releases) == limit && out.Total > f.Offset+len(out.Releases)
	return out, nil
}

func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// buildOSQuery assembles an OpenSearch query body: a fuzzy multi_match on the
// name for the text query, category and obfuscated filters, newest-first sort,
// and from/size pagination.
func buildOSQuery(f store.SearchFilter) map[string]any {
	var must []any
	if q := strings.TrimSpace(f.Query); q != "" {
		must = append(must, map[string]any{
			"match": map[string]any{
				"search_name": map[string]any{"query": q, "fuzziness": "AUTO", "operator": "and"},
			},
		})
	} else {
		must = append(must, map[string]any{"match_all": map[string]any{}})
	}

	filters := []any{}
	if !f.IncludeObfuscated {
		filters = append(filters, map[string]any{"term": map[string]any{"obfuscated": false}})
	}
	if len(f.Categories) > 0 {
		cats := make([]any, len(f.Categories))
		for i, c := range f.Categories {
			cats[i] = c
		}
		filters = append(filters, map[string]any{"terms": map[string]any{"category_id": cats}})
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	return map[string]any{
		"from": f.Offset,
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{"must": must, "filter": filters},
		},
		"sort": []any{map[string]any{"sort_at": map[string]any{"order": "desc"}}},
	}
}

// do performs one HTTP request against OpenSearch, decoding a JSON response into
// out when non-nil. Non-2xx responses become errors carrying the status.
func (o *OpenSearchBackend) do(ctx context.Context, method, url string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("opensearch %s %s: status %d: %s", method, url, resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
