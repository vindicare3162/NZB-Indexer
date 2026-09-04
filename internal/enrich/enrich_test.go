package enrich

import (
	"context"
	"errors"
	"testing"

	"github.com/vindicare/goindex/internal/metadata"
	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/store"
)

type storedIdent struct {
	releaseID  int64
	source     string
	identifier string
}

type mockRepo struct {
	pending []store.ReleaseForEnrichment
	saved   []store.ReleaseMetadataInput
	idents  []storedIdent
}

func (m *mockRepo) ListReleasesNeedingMetadata(context.Context, int) ([]store.ReleaseForEnrichment, error) {
	return m.pending, nil
}
func (m *mockRepo) UpsertReleaseMetadata(_ context.Context, in store.ReleaseMetadataInput) error {
	m.saved = append(m.saved, in)
	return nil
}
func (m *mockRepo) AddReleaseIdentifier(_ context.Context, releaseID int64, source, identifier string) error {
	m.idents = append(m.idents, storedIdent{releaseID, source, identifier})
	return nil
}

type mockProvider struct {
	name   string
	match  bool
	err    error
	lastQ  metadata.Query
	result metadata.Result
}

func (p *mockProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "mock"
}
func (p *mockProvider) Lookup(_ context.Context, q metadata.Query) (metadata.Result, bool, error) {
	p.lastQ = q
	if p.err != nil {
		return metadata.Result{}, false, p.err
	}
	return p.result, p.match, nil
}

func tvCat() *int { c := release.CatTVHD; return &c }

func TestEnrichMatchStored(t *testing.T) {
	repo := &mockRepo{pending: []store.ReleaseForEnrichment{
		{ID: 1, Name: "The.Expanse.S03E10.1080p.WEB.x264-GRP", CategoryID: tvCat()},
	}}
	prov := &mockProvider{match: true, result: metadata.Result{
		Title: "The Expanse", Year: 2015, Source: "mock", ExternalID: "999",
		PosterURL: "https://example.com/p.jpg", Overview: "space opera",
	}}
	svc := New(repo, prov, nil, Options{})

	res, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Matched != 1 || res.Processed != 1 {
		t.Errorf("result = %+v, want processed=1 matched=1", res)
	}
	// The parser should have extracted title + season/episode + TV flag.
	if !prov.lastQ.IsTV || prov.lastQ.Season != 3 || prov.lastQ.Episode != 10 {
		t.Errorf("query = %+v, want TV s3e10", prov.lastQ)
	}
	if len(repo.saved) != 1 || !repo.saved[0].Matched || repo.saved[0].Title != "The Expanse" {
		t.Errorf("saved = %+v", repo.saved)
	}
	if repo.saved[0].Year == nil || *repo.saved[0].Year != 2015 {
		t.Errorf("year not stored: %+v", repo.saved[0].Year)
	}
}

func TestEnrichMissRecorded(t *testing.T) {
	repo := &mockRepo{pending: []store.ReleaseForEnrichment{
		{ID: 2, Name: "Unknown.Show.S01E01", CategoryID: tvCat()},
	}}
	prov := &mockProvider{match: false}
	svc := New(repo, prov, nil, Options{})

	res, _ := svc.Run(context.Background())
	if res.Misses != 1 {
		t.Errorf("misses = %d, want 1", res.Misses)
	}
	// A miss is still recorded (matched=false) so it is not retried forever.
	if len(repo.saved) != 1 || repo.saved[0].Matched {
		t.Errorf("miss should be recorded as matched=false: %+v", repo.saved)
	}
}

func TestEnrichProviderErrorNotStored(t *testing.T) {
	repo := &mockRepo{pending: []store.ReleaseForEnrichment{
		{ID: 3, Name: "Some.Show.S01E01", CategoryID: tvCat()},
	}}
	prov := &mockProvider{err: errors.New("boom")}
	svc := New(repo, prov, nil, Options{})

	res, _ := svc.Run(context.Background())
	if res.Errors != 1 {
		t.Errorf("errors = %d, want 1", res.Errors)
	}
	// On a transient error nothing is written, so it retries next pass.
	if len(repo.saved) != 0 {
		t.Errorf("nothing should be stored on provider error: %+v", repo.saved)
	}
}

func TestEnrichDisabledNoop(t *testing.T) {
	svc := New(&mockRepo{}, nil, nil, Options{})
	if svc.Enabled() {
		t.Error("service with nil provider should be disabled")
	}
	res, err := svc.Run(context.Background())
	if err != nil || res.Processed != 0 {
		t.Errorf("disabled Run should be a no-op, got %+v err=%v", res, err)
	}
}

// TestEnrichStoresIdentifiers verifies a matched provider's external ids are
// persisted via AddReleaseIdentifier (#134).
func TestEnrichStoresIdentifiers(t *testing.T) {
	repo := &mockRepo{pending: []store.ReleaseForEnrichment{
		{ID: 7, Name: "The.Expanse.S03E10", CategoryID: tvCat()},
	}}
	prov := &mockProvider{match: true, result: metadata.Result{
		Title: "The Expanse", Source: "tvmaze", ExternalID: "999",
		Identifiers: map[string]string{"imdb": "tt3230854", "tvdb": "280619"},
	}}
	svc := New(repo, prov, nil, Options{})

	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.idents) != 2 {
		t.Fatalf("stored identifiers = %d, want 2: %+v", len(repo.idents), repo.idents)
	}
	got := map[string]string{}
	for _, id := range repo.idents {
		if id.releaseID != 7 {
			t.Errorf("identifier release id = %d, want 7", id.releaseID)
		}
		got[id.source] = id.identifier
	}
	if got["imdb"] != "tt3230854" || got["tvdb"] != "280619" {
		t.Errorf("identifiers = %+v", got)
	}
}

// TestEnrichMultiProviderFirstMatchWinsMergedIdentifiers verifies multi-provider
// enrichment (#134): providers are tried in order, the first match supplies the
// metadata row, and identifiers from every matching provider are merged.
func TestEnrichMultiProviderFirstMatchWinsMergedIdentifiers(t *testing.T) {
	repo := &mockRepo{pending: []store.ReleaseForEnrichment{
		{ID: 11, Name: "The.Show.S01E01", CategoryID: tvCat()},
	}}
	// First provider matches with a title + imdb id; second also matches and
	// contributes a tmdb id. The first provider's metadata wins.
	p1 := &mockProvider{name: "alpha", match: true, result: metadata.Result{
		Title: "Alpha Title", Source: "alpha", ExternalID: "1",
		Identifiers: map[string]string{"imdb": "tt1234567"},
	}}
	p2 := &mockProvider{name: "beta", match: true, result: metadata.Result{
		Title: "Beta Title", Source: "beta", ExternalID: "2",
		Identifiers: map[string]string{"tmdb": "42"},
	}}
	svc := NewMulti(repo, []metadata.Provider{p1, p2}, nil, Options{})

	res, err := svc.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 {
		t.Errorf("matched = %d, want 1", res.Matched)
	}
	// First match supplies the metadata row.
	if len(repo.saved) != 1 || repo.saved[0].Title != "Alpha Title" || repo.saved[0].Source != "alpha" {
		t.Errorf("metadata should come from the first matching provider: %+v", repo.saved)
	}
	// Identifiers from both providers are merged.
	got := map[string]string{}
	for _, id := range repo.idents {
		got[id.source] = id.identifier
	}
	if got["imdb"] != "tt1234567" || got["tmdb"] != "42" {
		t.Errorf("merged identifiers = %+v, want imdb+tmdb", got)
	}
}

// TestEnrichSecondProviderMatchesAfterFirstMiss verifies fallthrough: when the
// first provider misses, a later provider's match is used.
func TestEnrichSecondProviderMatchesAfterFirstMiss(t *testing.T) {
	repo := &mockRepo{pending: []store.ReleaseForEnrichment{
		{ID: 12, Name: "The.Show.S01E01", CategoryID: tvCat()},
	}}
	p1 := &mockProvider{name: "alpha", match: false}
	p2 := &mockProvider{name: "beta", match: true, result: metadata.Result{
		Title: "Beta Title", Source: "beta", ExternalID: "2",
	}}
	svc := NewMulti(repo, []metadata.Provider{p1, p2}, nil, Options{})

	res, _ := svc.Run(context.Background())
	if res.Matched != 1 {
		t.Errorf("matched = %d, want 1", res.Matched)
	}
	if len(repo.saved) != 1 || repo.saved[0].Source != "beta" {
		t.Errorf("expected beta match to be stored: %+v", repo.saved)
	}
}
