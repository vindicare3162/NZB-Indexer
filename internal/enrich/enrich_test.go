package enrich

import (
	"context"
	"errors"
	"testing"

	"github.com/vindicare/goindex/internal/metadata"
	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/store"
)

type mockRepo struct {
	pending []store.ReleaseForEnrichment
	saved   []store.ReleaseMetadataInput
}

func (m *mockRepo) ListReleasesNeedingMetadata(context.Context, int) ([]store.ReleaseForEnrichment, error) {
	return m.pending, nil
}
func (m *mockRepo) UpsertReleaseMetadata(_ context.Context, in store.ReleaseMetadataInput) error {
	m.saved = append(m.saved, in)
	return nil
}

type mockProvider struct {
	match  bool
	err    error
	lastQ  metadata.Query
	result metadata.Result
}

func (p *mockProvider) Name() string { return "mock" }
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
