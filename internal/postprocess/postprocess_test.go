package postprocess

import (
	"context"
	"fmt"
	"net/textproto"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/store"
)

// fakeFetcher returns canned bodies keyed by message-id.
type fakeFetcher struct {
	bodies map[string][]byte
	err    map[string]error
}

func (f *fakeFetcher) Body(_ context.Context, messageID string) ([]byte, error) {
	if e, ok := f.err[messageID]; ok {
		return nil, e
	}
	if b, ok := f.bodies[messageID]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func freshStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping postprocess integration test")
	}
	if err := store.MigrateDown(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// seedObfuscatedRelease creates a release whose parts include an obfuscated
// name plus PAR2 and NFO segments. Returns the release GUID and the PAR2/NFO
// message-ids.
func seedObfuscatedRelease(t *testing.T, st *store.Store) (guid, par2MsgID, nfoMsgID string) {
	t.Helper()
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.pp", true)

	par2MsgID = "par2-seg@example.com"
	nfoMsgID = "nfo-seg@example.com"
	norm := "abc123xyz-obfuscated"

	parts := []store.PartInput{
		{
			GroupID: g.ID, ArticleNumber: 1, MessageID: "data1@x",
			Subject: `"abc123xyz.mkv" yEnc (1/3)`, Poster: "p", Bytes: 1000,
			PartNumber: 1, TotalParts: 3, NormSubject: norm,
		},
		{
			GroupID: g.ID, ArticleNumber: 2, MessageID: par2MsgID,
			Subject: `"abc123xyz.par2" yEnc (2/3)`, Poster: "p", Bytes: 500,
			PartNumber: 2, TotalParts: 3, NormSubject: norm,
		},
		{
			GroupID: g.ID, ArticleNumber: 3, MessageID: nfoMsgID,
			Subject: `"abc123xyz.nfo" yEnc (3/3)`, Poster: "p", Bytes: 300,
			PartNumber: 3, TotalParts: 3, NormSubject: norm,
		},
	}
	if _, err := st.InsertParts(ctx, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}
	b := release.New(st, nil, release.Options{BatchLimit: 100})
	if _, err := b.Build(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().QueryRow(ctx, `SELECT guid FROM releases LIMIT 1`).Scan(&guid); err != nil {
		t.Fatal(err)
	}
	return guid, par2MsgID, nfoMsgID
}

func TestPostProcessRenamesFromPar2AndStoresNFO(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	guid, par2MsgID, nfoMsgID := seedObfuscatedRelease(t, st)

	// PAR2 body (yEnc-encoded) recovering the real name.
	par2Raw := buildFileDescPacket("Actual.Movie.Title.2024.1080p.BluRay.x264-GRP.mkv")
	nfoText := "Actual Movie Title\r\nGroup: GRP\r\nSize: 1.4GB"

	fetch := &fakeFetcher{
		bodies: map[string][]byte{
			par2MsgID: encodeYenc("abc123xyz.par2", par2Raw),
			nfoMsgID:  encodeYenc("abc123xyz.nfo", []byte(nfoText)),
		},
	}

	p := New(fetch, st, nil, Options{BatchLimit: 100, MaxFetchPerRelease: 4})
	res, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Processed != 1 || res.Renamed != 1 || res.NFOFound != 1 {
		t.Errorf("result = %+v, want processed=1 renamed=1 nfo=1", res)
	}

	rel, err := st.GetReleaseByGUID(ctx, guid)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Name != "Actual.Movie.Title.2024.1080p.BluRay.x264-GRP" {
		t.Errorf("renamed to %q, want the PAR2-recovered base name", rel.Name)
	}
	if rel.PPStatus != store.PPDone {
		t.Errorf("pp_status = %q, want done", rel.PPStatus)
	}
	if rel.NFO == nil || *rel.NFO == "" {
		t.Error("expected NFO text to be stored")
	}
	// The recovered name ("...2024.1080p.BluRay.x264...") must re-categorize
	// the release to Movies HD, rather than staying whatever it was built with.
	if rel.CategoryID == nil || *rel.CategoryID != release.CatMoviesHD {
		got := 0
		if rel.CategoryID != nil {
			got = *rel.CategoryID
		}
		t.Errorf("category after recovery = %d, want %d (Movies HD)", got, release.CatMoviesHD)
	}

	// Re-running should find nothing pending.
	res2, err := p.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Processed != 0 {
		t.Errorf("second run processed = %d, want 0", res2.Processed)
	}
}

func TestPostProcessMarksFailedForRetryOnFetchError(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	guid, par2MsgID, nfoMsgID := seedObfuscatedRelease(t, st)

	// Every needed fetch errors and nothing is recovered: this is treated as a
	// transient failure, so the release is marked 'failed' (retryable) rather
	// than 'done' — otherwise a transient blip would permanently lose the name.
	fetch := &fakeFetcher{err: map[string]error{
		par2MsgID: os.ErrDeadlineExceeded,
		nfoMsgID:  os.ErrDeadlineExceeded,
	}}
	p := New(fetch, st, nil, Options{BatchLimit: 100})
	res, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	rel, err := st.GetReleaseByGUID(ctx, guid)
	if err != nil {
		t.Fatal(err)
	}
	if rel.PPStatus != store.PPFailed {
		t.Errorf("pp_status = %q, want failed (retryable)", rel.PPStatus)
	}
}

// TestPostProcessRetriesTransientFailureThenRecovers verifies the retry path:
// a release whose PAR2 fetch fails on the first pass is retried on a later pass
// and, once the fetch succeeds, recovers its name.
func TestPostProcessRetriesTransientFailureThenRecovers(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	guid, par2MsgID, _ := seedObfuscatedRelease(t, st)

	par2Body := encodeYenc("abc.par2", buildFileDescPacket("Recovered.Name.2024.1080p.BluRay.x264-GRP.mkv"))

	// First pass: the PAR2 fetch errors -> release marked failed (retryable).
	failing := &fakeFetcher{err: map[string]error{par2MsgID: os.ErrDeadlineExceeded}}
	p1 := New(failing, st, nil, Options{BatchLimit: 100})
	if _, err := p1.Run(ctx); err != nil {
		t.Fatal(err)
	}
	rel, _ := st.GetReleaseByGUID(ctx, guid)
	if rel.PPStatus != store.PPFailed {
		t.Fatalf("after first pass pp_status = %q, want failed", rel.PPStatus)
	}

	// A transient failure schedules a backoff retry (#132); simulate the delay
	// elapsing so the release is due again.
	clearRetryBackoff(t, st)

	// Second pass: the same PAR2 now fetches successfully. The failed release
	// must be re-queued and recover its name.
	working := &fakeFetcher{bodies: map[string][]byte{par2MsgID: par2Body}}
	p2 := New(working, st, nil, Options{BatchLimit: 100, MaxFetchPerRelease: 5})
	res, err := p2.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Renamed != 1 {
		t.Errorf("second pass Renamed = %d, want 1 (retry should recover)", res.Renamed)
	}
	rel, _ = st.GetReleaseByGUID(ctx, guid)
	if rel.PPStatus != store.PPDone {
		t.Errorf("after retry pp_status = %q, want done", rel.PPStatus)
	}
	if rel.Name != "Recovered.Name.2024.1080p.BluRay.x264-GRP" {
		t.Errorf("recovered name = %q", rel.Name)
	}
}

// TestPostProcessStopsRetryingAfterMaxAttempts verifies retries are bounded: a
// release whose fetch always fails is retried up to MaxPPAttempts and then no
// longer re-queued.
func TestPostProcessStopsRetryingAfterMaxAttempts(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	guid, par2MsgID, nfoMsgID := seedObfuscatedRelease(t, st)

	failing := &fakeFetcher{err: map[string]error{
		par2MsgID: os.ErrDeadlineExceeded,
		nfoMsgID:  os.ErrDeadlineExceeded,
	}}
	p := New(failing, st, nil, Options{BatchLimit: 100})

	// Run enough passes to exhaust the retry budget, clearing the backoff timer
	// between passes to simulate the delay elapsing each time (#132).
	for i := 0; i < store.MaxPPAttempts+2; i++ {
		if _, err := p.Run(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		clearRetryBackoff(t, st)
	}
	// Once attempts reach the cap the release is marked permanently failed and
	// no longer re-queued: a further pass processes nothing.
	res, err := p.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 0 {
		t.Errorf("processed = %d after exhausting retries, want 0 (not re-queued)", res.Processed)
	}
	rel, _ := st.GetReleaseByGUID(ctx, guid)
	if rel.PPStatus != store.PPFailed {
		t.Errorf("pp_status = %q, want failed (exhausted)", rel.PPStatus)
	}
}

// clearRetryBackoff resets next_retry_at on all failed releases so they are due
// immediately, simulating the backoff window elapsing between passes (#132).
func clearRetryBackoff(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE releases SET next_retry_at = NULL WHERE pp_status = 'failed'`); err != nil {
		t.Fatalf("clear retry backoff: %v", err)
	}
}

// TestPostProcessPermanentErrorStopsImmediately verifies a permanent NNTP error
// (a 430 retention miss) marks the release permanently failed without consuming
// the whole retry budget and without ever being re-queued (#132).
func TestPostProcessPermanentErrorStopsImmediately(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	guid, par2MsgID, nfoMsgID := seedObfuscatedRelease(t, st)

	// A 430 (article expired / not carried) is permanent: retrying won't help.
	perm := &textproto.Error{Code: 430, Msg: "no such article"}
	failing := &fakeFetcher{err: map[string]error{par2MsgID: perm, nfoMsgID: perm}}
	p := New(failing, st, nil, Options{BatchLimit: 100})

	res, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	rel, _ := st.GetReleaseByGUID(ctx, guid)
	if rel.PPStatus != store.PPFailed {
		t.Errorf("pp_status = %q, want failed", rel.PPStatus)
	}

	// Even with the backoff cleared, a permanently-failed release is not
	// re-queued: a further pass processes nothing.
	clearRetryBackoff(t, st)
	res2, err := p.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Processed != 0 {
		t.Errorf("processed = %d after a permanent failure, want 0 (not re-queued)", res2.Processed)
	}

	// The last error is recorded, and an operator requeue clears the permanent
	// flag so it can be retried again.
	var permanent bool
	var lastErr string
	if err := st.Pool().QueryRow(ctx,
		`SELECT pp_permanent, last_error FROM releases WHERE guid = $1`, guid).Scan(&permanent, &lastErr); err != nil {
		t.Fatal(err)
	}
	if !permanent {
		t.Error("expected pp_permanent = true after a permanent error")
	}
	if lastErr == "" {
		t.Error("expected last_error to be recorded")
	}
	if _, err := st.RequeueFailedReleases(ctx); err != nil {
		t.Fatal(err)
	}
	res3, err := p.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Processed != 1 {
		t.Errorf("processed = %d after operator requeue, want 1 (permanent flag cleared)", res3.Processed)
	}
}

// TestPostProcessBackoffDefersRetry verifies a transiently-failed release is NOT
// re-selected on an immediate second pass because its next_retry_at is in the
// future (#132).
func TestPostProcessBackoffDefersRetry(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	guid, par2MsgID, nfoMsgID := seedObfuscatedRelease(t, st)

	failing := &fakeFetcher{err: map[string]error{
		par2MsgID: os.ErrDeadlineExceeded,
		nfoMsgID:  os.ErrDeadlineExceeded,
	}}
	p := New(failing, st, nil, Options{BatchLimit: 100})

	if _, err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// A next_retry_at in the future must be set.
	var hasRetry bool
	if err := st.Pool().QueryRow(ctx,
		`SELECT next_retry_at IS NOT NULL AND next_retry_at > now() FROM releases WHERE guid = $1`, guid).Scan(&hasRetry); err != nil {
		t.Fatal(err)
	}
	if !hasRetry {
		t.Fatal("expected next_retry_at to be scheduled in the future after a transient failure")
	}

	// Immediate second pass: the release is not yet due, so nothing is processed.
	res, err := p.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 0 {
		t.Errorf("processed = %d before backoff elapsed, want 0", res.Processed)
	}
}

// blockingFetcher blocks in Body until the fetch context is cancelled, then
// returns its error. It records how long the longest fetch actually took.
type blockingFetcher struct{}

func (blockingFetcher) Body(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestPostProcessBoundsHungFetch verifies that a stalled body fetch is bounded
// by FetchTimeout so a single hung article cannot block the whole pass (#22).
func TestPostProcessBoundsHungFetch(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	guid, _, _ := seedObfuscatedRelease(t, st)

	// A fetcher that never returns until its per-fetch context is cancelled.
	p := New(blockingFetcher{}, st, nil, Options{
		BatchLimit:         100,
		MaxFetchPerRelease: 4,
		FetchTimeout:       200 * time.Millisecond,
	})

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := p.Run(ctx); done <- err }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("post-processing hung despite FetchTimeout (a stalled fetch was not bounded)")
	}
	// Each of up to MaxFetchPerRelease fetches is bounded by FetchTimeout, so
	// the whole release resolves quickly relative to an unbounded hang.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("processing took %v, expected it to be bounded by FetchTimeout", elapsed)
	}

	rel, err := st.GetReleaseByGUID(ctx, guid)
	if err != nil {
		t.Fatal(err)
	}
	// The hung fetch times out (an error), so nothing is recovered and the
	// release is marked failed (retryable) rather than done. The point of this
	// test is that the pass completed promptly despite the hang.
	if rel.PPStatus != store.PPFailed {
		t.Errorf("pp_status = %q, want failed (retryable after a timed-out fetch)", rel.PPStatus)
	}
}

// seedFullyObfuscatedRelease creates a release whose segment subjects are pure
// random hex (NO .par2/.nfo filename hints at all) but one segment's body is a
// PAR2 file. Returns the release GUID and the message-id of the PAR2 segment.
func seedFullyObfuscatedRelease(t *testing.T, st *store.Store) (guid, par2MsgID string) {
	t.Helper()
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.obf", true)

	par2MsgID = "obf-par2@example.com"
	norm := "e5bcd657b08345c075b6534bac9d3149"

	parts := []store.PartInput{
		{
			GroupID: g.ID, ArticleNumber: 1, MessageID: "obf-data1@x",
			Subject: "e5bcd657b08345c075b6534bac9d3149 (1/3)", Poster: "p", Bytes: 500000,
			PartNumber: 1, TotalParts: 3, NormSubject: norm,
		},
		{
			// The PAR2 segment — smaller, and its subject gives no hint.
			GroupID: g.ID, ArticleNumber: 2, MessageID: par2MsgID,
			Subject: "e5bcd657b08345c075b6534bac9d3149 (2/3)", Poster: "p", Bytes: 800,
			PartNumber: 2, TotalParts: 3, NormSubject: norm,
		},
		{
			GroupID: g.ID, ArticleNumber: 3, MessageID: "obf-data3@x",
			Subject: "e5bcd657b08345c075b6534bac9d3149 (3/3)", Poster: "p", Bytes: 500000,
			PartNumber: 3, TotalParts: 3, NormSubject: norm,
		},
	}
	if _, err := st.InsertParts(ctx, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}
	b := release.New(st, nil, release.Options{BatchLimit: 100})
	if _, err := b.Build(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().QueryRow(ctx, `SELECT guid FROM releases LIMIT 1`).Scan(&guid); err != nil {
		t.Fatal(err)
	}
	return guid, par2MsgID
}

func TestPostProcessRenamesObfuscatedViaContentProbe(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	guid, par2MsgID := seedFullyObfuscatedRelease(t, st)

	// The PAR2 body recovers the real name; note the subject had no hint.
	par2Raw := buildFileDescPacket("Real.Movie.Title.2024.1080p.BluRay.x264-GRP.mkv")
	fetch := &fakeFetcher{
		bodies: map[string][]byte{
			par2MsgID:     encodeYenc("blob", par2Raw),
			"obf-data1@x": encodeYenc("blob", []byte("not par2, just media bytes")),
			"obf-data3@x": encodeYenc("blob", []byte("more media bytes")),
		},
	}

	// Budget large enough to probe past the non-PAR2 data segments.
	p := New(fetch, st, nil, Options{BatchLimit: 100, MaxFetchPerRelease: 5})
	res, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Renamed != 1 {
		t.Errorf("Renamed = %d, want 1 (content-probe should recover the name)", res.Renamed)
	}

	rel, err := st.GetReleaseByGUID(ctx, guid)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Name != "Real.Movie.Title.2024.1080p.BluRay.x264-GRP" {
		t.Errorf("renamed to %q, want the PAR2-recovered base name", rel.Name)
	}
	if rel.PPStatus != store.PPDone {
		t.Errorf("pp_status = %q, want done", rel.PPStatus)
	}
}

// TestPostProcessRejectsObfuscatedRecoveredName verifies that when the PAR2's
// own internal filename is itself obfuscated (random hex), post-processing does
// NOT rename the release to that junk and does NOT clear the obfuscated flag —
// otherwise the release would leak into default (non-obfuscated) search.
func TestPostProcessRejectsObfuscatedRecoveredName(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	guid, par2MsgID := seedFullyObfuscatedRelease(t, st)

	// PAR2 recovers a name that is itself obfuscated hex — not a real name.
	par2Raw := buildFileDescPacket("9c914cea0eb54d4892abd3a5b681032a.par2")
	fetch := &fakeFetcher{
		bodies: map[string][]byte{
			par2MsgID:     encodeYenc("blob", par2Raw),
			"obf-data1@x": encodeYenc("blob", []byte("media bytes")),
			"obf-data3@x": encodeYenc("blob", []byte("more media bytes")),
		},
	}

	p := New(fetch, st, nil, Options{BatchLimit: 100, MaxFetchPerRelease: 5})
	if _, err := p.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	rel, err := st.GetReleaseByGUID(ctx, guid)
	if err != nil {
		t.Fatal(err)
	}
	// The obfuscated recovered name must be rejected: the release keeps its
	// original obfuscated name and stays flagged, so default search hides it.
	if !release.IsObfuscated(rel.Name) {
		t.Errorf("release name %q should still be obfuscated (junk PAR2 name rejected)", rel.Name)
	}
	// The obfuscated flag must remain set (queried directly; not on the model).
	var obf bool
	if err := st.Pool().QueryRow(ctx,
		`SELECT obfuscated FROM releases WHERE guid = $1`, guid).Scan(&obf); err != nil {
		t.Fatal(err)
	}
	if !obf {
		t.Error("obfuscated flag must NOT be cleared when the recovered name is itself obfuscated")
	}
}

func TestPostProcessSkipsContentProbeForReadableNames(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	// A readable release name: content-probing should NOT run, so even though a
	// PAR2 body is available on a non-hinted segment, no rename occurs (we trust
	// the already-good name and save bandwidth).
	g, _ := st.UpsertGroup(ctx, "alt.binaries.readable", true)
	norm := `"Good.Release.Name.2024.1080p.mkv"`
	parts := []store.PartInput{
		{GroupID: g.ID, ArticleNumber: 1, MessageID: "rd1@x", Subject: `"Good.Release.Name.2024.1080p.mkv" yEnc (1/2)`, Poster: "p", Bytes: 1000, PartNumber: 1, TotalParts: 2, NormSubject: norm},
		{GroupID: g.ID, ArticleNumber: 2, MessageID: "rd2@x", Subject: `"Good.Release.Name.2024.1080p.mkv" yEnc (2/2)`, Poster: "p", Bytes: 800, PartNumber: 2, TotalParts: 2, NormSubject: norm},
	}
	if _, err := st.InsertParts(ctx, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}
	b := release.New(st, nil, release.Options{BatchLimit: 100})
	if _, err := b.Build(ctx); err != nil {
		t.Fatal(err)
	}
	var guid string
	st.Pool().QueryRow(ctx, `SELECT guid FROM releases LIMIT 1`).Scan(&guid)

	var fetchCount int
	fetch := &countingFetcher{onFetch: func() { fetchCount++ }}
	p := New(fetch, st, nil, Options{BatchLimit: 100, MaxFetchPerRelease: 5})
	if _, err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// No .par2/.nfo-hinted segments and a readable name => no fetches at all.
	if fetchCount != 0 {
		t.Errorf("fetches = %d, want 0 (readable name should skip content probe)", fetchCount)
	}
	// The release builder cleans the name (strips the .mkv extension); the
	// point is that post-processing did NOT further change it via a probe.
	rel, _ := st.GetReleaseByGUID(ctx, guid)
	if rel.Name != "Good.Release.Name.2024.1080p" {
		t.Errorf("name = %q, should be the builder-cleaned name (unchanged by pp)", rel.Name)
	}
}

// countingFetcher counts Body calls; it always returns not-found so nothing is
// recovered, letting us assert the probe was (or was not) attempted.
type countingFetcher struct{ onFetch func() }

func (c *countingFetcher) Body(_ context.Context, _ string) ([]byte, error) {
	c.onFetch()
	return nil, os.ErrNotExist
}

// slowFetcher delays each fetch by delay (respecting ctx) to simulate a slow
// provider, and returns a canned body per message-id. It records the peak
// number of concurrently in-flight fetches, which is a deterministic proof of
// parallelism (independent of wall-clock timing under CI/DB load).
type slowFetcher struct {
	delay  time.Duration
	bodies map[string][]byte

	mu      sync.Mutex
	inFlt   int
	peakFlt int
}

func (f *slowFetcher) Body(ctx context.Context, messageID string) ([]byte, error) {
	f.mu.Lock()
	f.inFlt++
	if f.inFlt > f.peakFlt {
		f.peakFlt = f.inFlt
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlt--
		f.mu.Unlock()
	}()

	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if b, ok := f.bodies[messageID]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func (f *slowFetcher) peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peakFlt
}

// TestPostProcessRunsReleasesConcurrently verifies that releases are processed
// in parallel (a slow fetch on one release does not serialise the others) and
// that the aggregated counts remain correct (#33).
func TestPostProcessRunsReleasesConcurrently(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.conc", true)

	// Seed N independent single-file releases, each with a subject-hinted PAR2
	// segment whose body recovers a distinct real name.
	const n = 8
	bodies := map[string][]byte{}
	for i := 0; i < n; i++ {
		base := fmt.Sprintf("obfN%d", i)
		par2Msg := fmt.Sprintf("conc-par2-%d@x", i)
		parts := []store.PartInput{
			{GroupID: g.ID, ArticleNumber: int64(1000 + i*2), MessageID: fmt.Sprintf("conc-data-%d@x", i),
				Subject: fmt.Sprintf(`"%s.par2" yEnc (1/2)`, base), Poster: "p", Bytes: 500,
				PartNumber: 1, TotalParts: 2, NormSubject: base},
			{GroupID: g.ID, ArticleNumber: int64(1001 + i*2), MessageID: par2Msg,
				Subject: fmt.Sprintf(`"%s.par2" yEnc (2/2)`, base), Poster: "p", Bytes: 500,
				PartNumber: 2, TotalParts: 2, NormSubject: base},
		}
		if _, err := st.InsertParts(ctx, parts); err != nil {
			t.Fatal(err)
		}
		bodies[par2Msg] = encodeYenc(base+".par2",
			buildFileDescPacket(fmt.Sprintf("Recovered.Show.S01E%02d.1080p.WEB.mkv", i)))
		bodies[fmt.Sprintf("conc-data-%d@x", i)] = encodeYenc(base, []byte("media"))
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := release.New(st, nil, release.Options{BatchLimit: 100}).Build(ctx); err != nil {
		t.Fatal(err)
	}

	const delay = 200 * time.Millisecond
	fetch := &slowFetcher{delay: delay, bodies: bodies}
	// 4 workers over 8 releases -> ~2 sequential waves; each release does ~2
	// fetches. Sequential would be ~n*2*delay = 3.2s; concurrent should be far
	// less.
	p := New(fetch, st, nil, Options{BatchLimit: 100, MaxFetchPerRelease: 3, Concurrency: 4})

	start := time.Now()
	res, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	elapsed := time.Since(start)

	if res.Processed != n {
		t.Errorf("processed = %d, want %d", res.Processed, n)
	}
	if res.Renamed != n {
		t.Errorf("renamed = %d, want %d (each release recovers a name)", res.Renamed, n)
	}
	// Deterministic proof of parallelism: at some point more than one fetch was
	// in flight at once, which can only happen if releases ran concurrently.
	// This does not depend on wall-clock timing (robust under CI/DB load).
	if peak := fetch.peak(); peak < 2 {
		t.Errorf("peak concurrent fetches = %d, want >= 2 (releases were not processed concurrently)", peak)
	}
	// Loose wall-clock sanity check against the fully-sequential time
	// (n releases * 2 fetches * delay = 3.2s); generous margin for slow CI/DB.
	if seq := time.Duration(n) * 2 * delay; elapsed >= seq {
		t.Errorf("elapsed %v >= sequential time %v: no speedup from concurrency", elapsed, seq)
	}
}
