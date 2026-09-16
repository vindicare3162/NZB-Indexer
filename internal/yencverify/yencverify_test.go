package yencverify

import (
	"context"
	"errors"
	"testing"

	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/store"
)

// fakeFetch serves scripted yEnc control lines (or errors) per message-id.
type fakeFetch struct {
	lines map[string]string
	errs  map[string]error
}

func (f fakeFetch) BodyHeader(_ context.Context, messageID string) (string, error) {
	if err, ok := f.errs[messageID]; ok {
		return "", err
	}
	return f.lines[messageID], nil
}

// fakeRepo records what the verifier decided.
type fakeRepo struct {
	candidates []store.YencCandidate
	applied    []store.YencResult
}

func (r *fakeRepo) ListPartsNeedingYencVerification(_ context.Context, _ int) ([]store.YencCandidate, error) {
	return r.candidates, nil
}
func (r *fakeRepo) ApplyYencVerification(_ context.Context, res []store.YencResult) error {
	r.applied = append(r.applied, res...)
	return nil
}
func (r *fakeRepo) ListReleasesNeedingYencRepair(_ context.Context, _ int) ([]store.YencRepairCandidate, error) {
	return nil, nil
}
func (r *fakeRepo) ApplyYencRepair(_ context.Context, _ []store.YencRepairResult) error { return nil }

// TestRunClassifiesArticles covers the three outcomes that matter: a fragment
// of a larger post, a genuinely standalone post, and an article carrying no
// yEnc payload at all.
func TestRunClassifiesArticles(t *testing.T) {
	repo := &fakeRepo{candidates: []store.YencCandidate{
		{ID: 1, MessageID: "frag@x"},
		{ID: 2, MessageID: "solo@x"},
		{ID: 3, MessageID: "text@x"},
		{ID: 4, MessageID: "gone@x"},
	}}
	fetch := fakeFetch{
		lines: map[string]string{
			"frag@x": "=ybegin part=1871 total=16147 line=128 size=11573468670 name=abc",
			"solo@x": "=ybegin part=1 total=1 line=128 size=700000 name=solo",
		},
		errs: map[string]error{
			// A plain text post: no yEnc payload, and never will have one.
			"text@x": nntp.ErrNoYencHeader,
			// A transport/retention failure: worth retrying later.
			"gone@x": errors.New("430 no such article"),
		},
	}

	v := New(fetch, repo, nil, Options{BatchLimit: 10, Concurrency: 1})
	res, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Fragments != 1 {
		t.Errorf("fragments = %d, want 1", res.Fragments)
	}
	// Both the real single-part post and the non-yEnc text post are complete
	// single articles, so both settle as standalone rather than as failures.
	if res.Standalone != 2 {
		t.Errorf("standalone = %d, want 2 (single-part post + non-yEnc text post)", res.Standalone)
	}
	if res.Failed != 1 {
		t.Errorf("failed = %d, want 1 (only the retryable fetch error)", res.Failed)
	}

	byID := map[int64]store.YencResult{}
	for _, r := range repo.applied {
		byID[r.ID] = r
	}
	if got := byID[1]; got.Total != 16147 || got.Part != 1871 || got.Name != "abc" {
		t.Errorf("fragment recorded as %+v, want part 1871/16147 name abc", got)
	}
	if got := byID[3]; !got.OK || got.Total != 1 {
		t.Errorf("non-yEnc article recorded as %+v, want OK standalone total=1", got)
	}
	if got := byID[4]; got.OK {
		t.Errorf("retryable failure recorded as OK: %+v", got)
	}
}
