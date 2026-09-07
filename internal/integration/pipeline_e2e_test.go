package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/assembler"
	"github.com/vindicare/goindex/internal/nzb"
	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/store"
)

func TestPipelineEndToEnd(t *testing.T) {
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping pipeline E2E test")
	}
	if err := store.MigrateDown(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	g, err := st.UpsertGroup(ctx, "alt.binaries.e2e.test", true)
	if err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	name := "Test.Release.2025.1080p.WEB-DL.x264-GROUP"
	norm := strings.ToLower(strings.ReplaceAll(name, " ", "."))
	var parts []store.PartInput
	for i := 1; i <= 3; i++ {
		parts = append(parts, store.PartInput{GroupID: g.ID, ArticleNumber: int64(1000 + i), MessageID: fmt.Sprintf("<%s-%d@e2e>", norm, i), Subject: fmt.Sprintf("\"%s\" yEnc (%d/3)", norm, i), Poster: "p@e2e.test", Bytes: 1000, PartNumber: i, TotalParts: 3, NormSubject: norm})
	}
	inserted, err := st.InsertParts(ctx, parts)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if inserted != 3 {
		t.Errorf("inserted %d parts, want 3", inserted)
	}
	asm := assembler.New(st, nil, assembler.Options{BatchLimit: 100})
	r, _ := asm.Assemble(ctx)
	if r.BinariesTouched < 1 {
		t.Errorf("touched %d, want >= 1", r.BinariesTouched)
	}
	var assigned int
	st.Pool().QueryRow(ctx, "SELECT count(*) FROM parts WHERE binary_id IS NOT NULL").Scan(&assigned)
	if assigned < 3 {
		t.Errorf("assigned %d parts, want %d", assigned, 3)
	}
	builder := release.New(st, nil, release.Options{BatchLimit: 100})
	build, _ := builder.Build(ctx)
	if build.Created != 1 {
		t.Errorf("created %d releases, want 1", build.Created)
	}
	res, total, _ := st.SearchReleases(ctx, store.SearchFilter{Query: "Test Release 1080p", Limit: 10})
	if total < 1 {
		t.Error("no search results")
	}
	if res[0].Name != name {
		t.Errorf("name = %q, want %q", res[0].Name, name)
	}
	nzbData, nzbName, err := nzb.NewGenerator(st).ForGUID(ctx, res[0].GUID)
	if err != nil {
		t.Fatalf("generate NZB: %v", err)
	}
	if !strings.HasSuffix(nzbName, ".nzb") {
		t.Errorf("nzb filename = %q, want .nzb suffix", nzbName)
	}
	if len(nzbData) < 200 {
		t.Errorf("nzb data too short: %d bytes", len(nzbData))
	}
	if !strings.Contains(string(nzbData), "<file") {
		t.Error("nzb missing <file> element")
	}
}
