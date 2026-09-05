package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until true or the deadline elapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestSuccessfulDelivery(t *testing.T) {
	var got struct {
		mu   sync.Mutex
		body []byte
		sig  string
		n    int32
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.body = b
		got.sig = r.Header.Get("X-Goindex-Signature")
		got.mu.Unlock()
		atomic.AddInt32(&got.n, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New([]Destination{{Name: "wh", URL: srv.URL, Secret: "s3cr3t", Enabled: true}}, Options{}, nil)
	s.Start()
	defer s.Stop()

	s.Emit(Event{Type: EventJobCompleted, Title: "scan done", Message: "ok"})

	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&got.n) == 1 })

	got.mu.Lock()
	body, sig := got.body, got.sig
	got.mu.Unlock()

	// Body is valid JSON carrying the event type.
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if e.Type != EventJobCompleted {
		t.Errorf("event type = %q", e.Type)
	}
	// Signature is present and correct.
	if want := "sha256=" + Sign("s3cr3t", body); sig != want {
		t.Errorf("signature = %q, want %q", sig, want)
	}

	waitFor(t, time.Second, func() bool { return len(s.History(0)) == 1 })
	h := s.History(0)[0]
	if !h.Success || h.Attempts != 1 {
		t.Errorf("delivery = %+v, want success on attempt 1", h)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New([]Destination{{Name: "wh", URL: srv.URL, Enabled: true}},
		Options{MaxAttempts: 3, BaseBackoff: time.Millisecond}, nil)
	s.Start()
	defer s.Stop()

	s.Emit(Event{Type: EventScanFailed})

	waitFor(t, 2*time.Second, func() bool {
		h := s.History(0)
		return len(h) == 1 && h[0].Success
	})
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (2 failures + 1 success)", got)
	}
	if h := s.History(0)[0]; h.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", h.Attempts)
	}
}

func TestRetriesExhaustedRecordsFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s := New([]Destination{{Name: "wh", URL: srv.URL, Enabled: true}},
		Options{MaxAttempts: 3, BaseBackoff: time.Millisecond}, nil)
	s.Start()
	defer s.Stop()

	s.Emit(Event{Type: EventPostProcessFailed})

	waitFor(t, 2*time.Second, func() bool { return len(s.History(0)) == 1 })
	h := s.History(0)[0]
	if h.Success {
		t.Error("expected failure after exhausting retries")
	}
	if h.Attempts != 3 || h.StatusCode != http.StatusBadGateway {
		t.Errorf("delivery = %+v, want 3 attempts / 502", h)
	}
	if h.LastError == "" {
		t.Error("expected a recorded last error")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
}

func TestTimeoutIsRetriedAndRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Respond slower than the client timeout, but bounded so the handler
		// and server always shut down cleanly. Return early if the client
		// already gave up (its request context is cancelled).
		select {
		case <-time.After(500 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	s := New([]Destination{{Name: "slow", URL: srv.URL, Enabled: true}},
		Options{MaxAttempts: 2, BaseBackoff: time.Millisecond, Timeout: 50 * time.Millisecond}, nil)
	s.Start()
	defer s.Stop()

	s.Emit(Event{Type: EventJobFailed})

	waitFor(t, 3*time.Second, func() bool { return len(s.History(0)) == 1 })
	h := s.History(0)[0]
	if h.Success {
		t.Error("expected timeout to be a failed delivery")
	}
	if h.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", h.Attempts)
	}
}

func TestEventFilteringAndDisablement(t *testing.T) {
	var enabledHits, filteredHits int32
	enabled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&enabledHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer enabled.Close()
	filtered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&filteredHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer filtered.Close()

	s := New([]Destination{
		{Name: "all", URL: enabled.URL, Enabled: true},
		{Name: "scan-only", URL: filtered.URL, Enabled: true, Events: []EventType{EventScanFailed}},
		{Name: "off", URL: enabled.URL, Enabled: false},
	}, Options{}, nil)
	s.Start()
	defer s.Stop()

	// A job event reaches only the "all" destination.
	s.Emit(Event{Type: EventJobCompleted})
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&enabledHits) == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&filteredHits); got != 0 {
		t.Errorf("filtered dest received %d, want 0 (event type not subscribed)", got)
	}

	// A scan-failed event reaches both "all" and "scan-only".
	s.Emit(Event{Type: EventScanFailed})
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&filteredHits) == 1 })
}

func TestDeduplication(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New([]Destination{{Name: "wh", URL: srv.URL, Enabled: true}},
		Options{DedupWindow: time.Minute}, nil)
	s.Start()
	defer s.Stop()

	// Same explicit id emitted twice: only the first delivers.
	s.Emit(Event{ID: "evt-1", Type: EventReleaseCreated})
	s.Emit(Event{ID: "evt-1", Type: EventReleaseCreated})

	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&calls) >= 1 })
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("delivered %d times, want 1 (duplicate suppressed)", got)
	}
}

func TestDisabledServiceIsInert(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	s := New([]Destination{{Name: "wh", URL: srv.URL, Enabled: false}}, Options{}, nil)
	if s.Enabled() {
		t.Fatal("service with no enabled destinations should be disabled")
	}
	s.Start()
	defer s.Stop()
	s.Emit(Event{Type: EventJobCompleted})
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("inert service delivered %d, want 0", got)
	}
}

func TestEmitNeverBlocksWhenQueueFull(t *testing.T) {
	// A blocking server plus a queue of size 1 and a single worker: once the
	// worker is busy and the queue holds one item, further Emits must not block.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New([]Destination{{Name: "wh", URL: srv.URL, Enabled: true}},
		Options{QueueSize: 1, Workers: 1, MaxAttempts: 1, Timeout: 100 * time.Millisecond}, nil)
	s.Start()
	// Release the blocked handler and stop the workers before returning, so the
	// single worker is never left stuck in the HTTP call at teardown.
	defer func() { close(block); s.Stop() }()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.Emit(Event{Type: EventScanLag})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked when queue was full")
	}
}
