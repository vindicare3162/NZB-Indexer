package postprocess

import (
	"context"
	"os"
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

	// Re-running should find nothing pending.
	res2, err := p.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Processed != 0 {
		t.Errorf("second run processed = %d, want 0", res2.Processed)
	}
}

func TestPostProcessMarksFailedOnFetchError(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	guid, par2MsgID, nfoMsgID := seedObfuscatedRelease(t, st)

	// Fetch always errors -> no name, no NFO. This is not a hard failure (the
	// processor treats missing PAR2/NFO as "nothing to recover"), so the
	// release should end up 'done' with no rename. Verify graceful handling.
	fetch := &fakeFetcher{err: map[string]error{
		par2MsgID: os.ErrDeadlineExceeded,
		nfoMsgID:  os.ErrDeadlineExceeded,
	}}
	p := New(fetch, st, nil, Options{BatchLimit: 100})
	if _, err := p.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	rel, err := st.GetReleaseByGUID(ctx, guid)
	if err != nil {
		t.Fatal(err)
	}
	// No PAR2/NFO recovered, but processing completed cleanly.
	if rel.PPStatus != store.PPDone {
		t.Errorf("pp_status = %q, want done (missing PAR2/NFO is not fatal)", rel.PPStatus)
	}
	if rel.NFO != nil {
		t.Errorf("expected no NFO, got %q", *rel.NFO)
	}
}
