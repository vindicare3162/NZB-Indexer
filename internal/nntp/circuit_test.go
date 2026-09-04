package nntp

import (
	"errors"
	"net"
	"net/textproto"
	"testing"
	"time"
)

func TestCircuitOpensAfterThreshold(t *testing.T) {
	cb := newCircuitBreaker(3, time.Minute)
	connErr := net.UnknownNetworkError("dial fail")

	for i := 0; i < 2; i++ {
		cb.recordFailure(connErr)
		if ok, _ := cb.allow(); !ok {
			t.Fatalf("circuit should stay closed after %d failures (< threshold)", i+1)
		}
	}
	// Third failure opens it.
	cb.recordFailure(connErr)
	if ok, _ := cb.allow(); ok {
		t.Error("circuit should be open after reaching the failure threshold")
	}
	if cb.snapshot().State != "open" {
		t.Errorf("state = %s, want open", cb.snapshot().State)
	}
}

func TestCircuitAuthFailureOpensImmediately(t *testing.T) {
	cb := newCircuitBreaker(5, time.Minute)
	cb.recordFailure(&textproto.Error{Code: 481, Msg: "auth rejected"})
	if ok, _ := cb.allow(); ok {
		t.Error("an auth failure should open the circuit immediately")
	}
	if cb.snapshot().LastErrorKind != "auth" {
		t.Errorf("last error kind = %s, want auth", cb.snapshot().LastErrorKind)
	}
}

func TestCircuitHalfOpenProbeAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	cb := newCircuitBreaker(1, 30*time.Second)
	cb.now = func() time.Time { return now }

	cb.recordFailure(errors.New("boom")) // opens (threshold 1)
	if ok, _ := cb.allow(); ok {
		t.Fatal("circuit should be open")
	}

	// Before cooldown: still open.
	now = now.Add(10 * time.Second)
	if ok, _ := cb.allow(); ok {
		t.Fatal("circuit should still be open before cooldown")
	}

	// After cooldown: half-open, allows exactly one probe.
	now = now.Add(30 * time.Second)
	ok, probe := cb.allow()
	if !ok || !probe {
		t.Fatalf("expected a half-open probe, got ok=%v probe=%v", ok, probe)
	}
	// A concurrent second caller is denied while a probe is in flight.
	if ok2, _ := cb.allow(); ok2 {
		t.Error("second caller should be denied during a probe")
	}
	// Probe succeeds -> closed.
	cb.recordSuccess()
	if cb.snapshot().State != "closed" {
		t.Errorf("state after successful probe = %s, want closed", cb.snapshot().State)
	}
	if ok, _ := cb.allow(); !ok {
		t.Error("circuit should be closed and allow requests after recovery")
	}
}

func TestCircuitHalfOpenProbeFailureReopens(t *testing.T) {
	now := time.Unix(1000, 0)
	cb := newCircuitBreaker(1, 10*time.Second)
	cb.now = func() time.Time { return now }
	cb.recordFailure(errors.New("boom"))
	now = now.Add(10 * time.Second)
	if ok, probe := cb.allow(); !ok || !probe {
		t.Fatal("expected half-open probe")
	}
	cb.recordFailure(errors.New("still down")) // probe fails
	if ok, _ := cb.allow(); ok {
		t.Error("failed probe should re-open the circuit")
	}
}
