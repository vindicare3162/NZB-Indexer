package store

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// BenchmarkInsertParts measures COPY-based bulk ingestion throughput at
// representative batch sizes. It requires GOINDEX_TEST_DSN (skipped otherwise).
// Run e.g.:
//
//	GOINDEX_TEST_DSN=... go test ./internal/store -run x -bench BenchmarkInsertParts -benchtime 5x
func BenchmarkInsertParts(b *testing.B) {
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		b.Skip("GOINDEX_TEST_DSN not set")
	}
	if err := MigrateDown(dsn); err != nil {
		b.Fatal(err)
	}
	if err := Migrate(dsn); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn, 5)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()

	var gid int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO groups (name, active) VALUES ('bench.copy', true) RETURNING id`).Scan(&gid); err != nil {
		b.Fatal(err)
	}

	for _, size := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("batch-%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				// Unique article numbers per iteration so each batch is a fresh insert.
				base := int64(i)*int64(size)*2 + int64(b.N)
				parts := make([]PartInput, size)
				for j := 0; j < size; j++ {
					an := base + int64(j)
					parts[j] = mkPart(gid, an, fmt.Sprintf("m%d@x", an), "bench.subject")
				}
				if _, err := st.InsertParts(ctx, parts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
