package server

import "testing"

// TestScanConcurrencySizing verifies scan concurrency derives from the given
// (effective) connection ceiling and honours an explicit override, per #104.
func TestScanConcurrencySizing(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		maxConns   int
		want       int
	}{
		{"auto small pool", 0, 2, 1},   // maxConns/2 = 1
		{"auto mid pool", 0, 10, 5},    // 10/2 = 5
		{"auto large pool clamped", 0, 40, 8}, // clamped to 8
		{"explicit honoured", 3, 10, 3},
		{"explicit honoured over pool", 20, 10, 20}, // honoured; pool still caps at runtime
	}
	for _, c := range cases {
		if got := scanConcurrency(c.configured, c.maxConns); got != c.want {
			t.Errorf("%s: scanConcurrency(%d,%d)=%d, want %d", c.name, c.configured, c.maxConns, got, c.want)
		}
	}
}

// TestPPConcurrencySizing verifies post-process concurrency derives from the
// effective connection ceiling (half, clamped [1,4]).
func TestPPConcurrencySizing(t *testing.T) {
	cases := map[int]int{2: 1, 4: 2, 10: 4, 40: 4, 1: 1}
	for maxConns, want := range cases {
		if got := ppConcurrency(maxConns); got != want {
			t.Errorf("ppConcurrency(%d)=%d, want %d", maxConns, got, want)
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
