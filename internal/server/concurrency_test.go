package server

import "testing"

// TestScanConcurrencySizing verifies scan concurrency derives from the given
// (effective) NNTP connection ceiling and honours an explicit override, per
// #104. With an ample DB pool the budget (#117) leaves the NNTP heuristic
// intact (no DB clamping).
func TestScanConcurrencySizing(t *testing.T) {
	const ampleDB = 100 // large enough that the DB budget never clamps
	cases := []struct {
		name       string
		configured int
		maxConns   int
		want       int
	}{
		{"auto small pool", 0, 2, 1},          // maxConns/2 = 1
		{"auto mid pool", 0, 10, 5},           // 10/2 = 5
		{"auto large pool clamped", 0, 40, 8}, // clamped to 8
		{"explicit honoured", 3, 10, 3},
		{"explicit honoured over pool", 20, 10, 20}, // honoured; pool still caps at runtime
	}
	for _, c := range cases {
		b := computeBudget(c.maxConns, ampleDB, 0 /*no reserve*/, c.configured, 0)
		if b.ScanWorkers != c.want {
			t.Errorf("%s: scan workers = %d, want %d", c.name, b.ScanWorkers, c.want)
		}
	}
}

// TestPPConcurrencySizing verifies post-process concurrency derives from the
// effective NNTP ceiling (half, clamped [1,4]) when the DB pool is ample.
func TestPPConcurrencySizing(t *testing.T) {
	const ampleDB = 100
	cases := map[int]int{2: 1, 4: 2, 10: 4, 40: 4, 1: 1}
	for maxConns, want := range cases {
		b := computeBudget(maxConns, ampleDB, 0, 0, 0)
		if b.PostProcessWorkers != want {
			t.Errorf("pp workers for nntp=%d = %d, want %d", maxConns, b.PostProcessWorkers, want)
		}
	}
}

// TestSystemProbeCapacity verifies the probe reports the effective sizing it
// was constructed with (the values derived from the active server's limit).
func TestSystemProbeCapacity(t *testing.T) {
	p := systemProbe{nntpMaxConns: 12, scanWorkers: 6, ppWorkers: 4}
	mc, sw, pw := p.Capacity()
	if mc != 12 || sw != 6 || pw != 4 {
		t.Errorf("Capacity() = %d,%d,%d, want 12,6,4", mc, sw, pw)
	}
}
