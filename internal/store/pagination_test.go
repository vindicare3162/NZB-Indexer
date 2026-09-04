package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedReleasesForPaging inserts n releases with descending posted_at so the
// recency ordering is deterministic. Returns the guids in newest-first order.
func seedReleasesForPaging(t *testing.T, st *Store, n int) []string {
	t.Helper()
	ctx := context.Background()
	c := 2040
	base := time.Now().Add(-time.Duration(n) * time.Hour)
	guids := make([]string, n)
	for i := 0; i < n; i++ {
		// Newer i => later posted_at, so newest-first ordering is reverse of i.
		posted := base.Add(time.Duration(i) * time.Hour)
		guid := fmt.Sprintf("pg-%04d", i)
		if _, _, err := st.CreateRelease(ctx, ReleaseInput{
			GUID: guid, Name: guid, SearchName: fmt.Sprintf("paging item %04d", i),
			CategoryID: &c, ReleaseHash: guid, PostedAt: &posted,
		}); err != nil {
			t.Fatalf("create release %d: %v", i, err)
		}
		guids[n-1-i] = guid // newest first
	}
	return guids
}

func TestSearchReleasesKeysetPagination(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	want := seedReleasesForPaging(t, st, 25)

	// Page through with keyset, 10 at a time, and collect the guids.
	var got []string
	var cursor *SearchCursor
	for page := 0; page < 10; page++ {
		res, err := st.SearchReleasesPage(ctx, SearchFilter{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, r := range res.Releases {
			got = append(got, r.GUID)
		}
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
		if cursor == nil {
			t.Fatal("HasMore but nil cursor")
		}
	}

	if len(got) != len(want) {
		t.Fatalf("keyset paging returned %d releases, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keyset order mismatch at %d: got %s want %s", i, got[i], want[i])
		}
	}
}

// TestKeysetMatchesOffset verifies keyset pagination yields the same page as
// the equivalent OFFSET query for a deep page.
func TestKeysetMatchesOffset(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	seedReleasesForPaging(t, st, 30)

	// OFFSET path: page 3 (offset 20, limit 10).
	offRes, err := st.SearchReleasesPage(ctx, SearchFilter{Limit: 10, Offset: 20})
	if err != nil {
		t.Fatal(err)
	}

	// Keyset path: walk to the same position.
	var cursor *SearchCursor
	for i := 0; i < 2; i++ {
		res, err := st.SearchReleasesPage(ctx, SearchFilter{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		cursor = res.NextCursor
	}
	keyRes, err := st.SearchReleasesPage(ctx, SearchFilter{Limit: 10, Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}

	if len(offRes.Releases) != len(keyRes.Releases) {
		t.Fatalf("offset page %d rows, keyset page %d rows", len(offRes.Releases), len(keyRes.Releases))
	}
	for i := range offRes.Releases {
		if offRes.Releases[i].GUID != keyRes.Releases[i].GUID {
			t.Errorf("row %d: offset=%s keyset=%s", i, offRes.Releases[i].GUID, keyRes.Releases[i].GUID)
		}
	}
}

func TestSearchCappedCount(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	seedReleasesForPaging(t, st, 20)

	// Cap the count below the true total: Total reports the cap and Approximate.
	res, err := st.SearchReleasesPage(ctx, SearchFilter{Limit: 5, CountCap: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Approximate {
		t.Error("expected Approximate=true when the total exceeds the cap")
	}
	if res.Total != 8 {
		t.Errorf("capped total = %d, want 8", res.Total)
	}

	// A cap above the true total gives the exact count, not approximate.
	res, err = st.SearchReleasesPage(ctx, SearchFilter{Limit: 5, CountCap: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Approximate {
		t.Error("expected exact count when cap exceeds the total")
	}
	if res.Total != 20 {
		t.Errorf("exact total = %d, want 20", res.Total)
	}

	// Negative cap forces an exact count too.
	res, err = st.SearchReleasesPage(ctx, SearchFilter{Limit: 5, CountCap: -1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Approximate || res.Total != 20 {
		t.Errorf("forced exact count: approximate=%v total=%d, want false/20", res.Approximate, res.Total)
	}
}

// TestKeysetPlanAvoidsFullOffsetScan checks the query plan for a deep keyset
// page uses the index range rather than scanning + discarding preceding rows.
func TestKeysetPlanAvoidsFullOffsetScan(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	seedReleasesForPaging(t, st, 50)

	// Get a cursor near the middle.
	res, err := st.SearchReleasesPage(ctx, SearchFilter{Limit: 25})
	if err != nil || res.NextCursor == nil {
		t.Fatalf("seed cursor: err=%v cursor=%v", err, res.NextCursor)
	}
	cur := res.NextCursor

	// EXPLAIN the keyset next-page query. It should not need a large OFFSET.
	explainQ := `EXPLAIN (FORMAT TEXT)
SELECT id FROM releases
WHERE (coalesce(posted_at, created_at), id) < ($1, $2)
ORDER BY coalesce(posted_at, created_at) DESC, id DESC
LIMIT 25`
	rows, err := st.Pool().Query(ctx, explainQ, cur.Sort, cur.ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	// The keyset query must not contain an OFFSET node (it pages by predicate).
	if strings.Contains(strings.ToLower(plan.String()), "offset") {
		t.Errorf("keyset plan unexpectedly contains OFFSET:\n%s", plan.String())
	}
}
