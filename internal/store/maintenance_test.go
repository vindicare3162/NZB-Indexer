package store

import (
	"context"
	"testing"
)

// TestAnalyzeStatistics verifies ANALYZE runs over the allow-listed tables
// without error on a fresh (empty) schema.
func TestAnalyzeStatistics(t *testing.T) {
	st := freshStore(t)
	if err := st.AnalyzeStatistics(context.Background()); err != nil {
		t.Fatalf("analyze statistics: %v", err)
	}
}

// TestVerifyBackupReadiness verifies the read-only backup-readiness probe finds
// all key tables reachable and a positive database size.
func TestVerifyBackupReadiness(t *testing.T) {
	st := freshStore(t)
	br, err := st.VerifyBackupReadiness(context.Background())
	if err != nil {
		t.Fatalf("verify backup readiness: %v", err)
	}
	if !br.OK {
		t.Errorf("backup readiness OK = false, want true (tables=%d)", br.Tables)
	}
	if br.Tables != len(capacityTables) {
		t.Errorf("tables reachable = %d, want %d", br.Tables, len(capacityTables))
	}
	if br.DatabaseBytes <= 0 {
		t.Errorf("database bytes = %d, want > 0", br.DatabaseBytes)
	}
}
