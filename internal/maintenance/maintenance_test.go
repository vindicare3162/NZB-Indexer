package maintenance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/notify"
)

// fakeJobs records maintenance job lifecycle for assertions.
type fakeJobs struct {
	mu       sync.Mutex
	created  int
	started  int
	finished map[string]int // state -> count
}

func newFakeJobs() *fakeJobs { return &fakeJobs{finished: map[string]int{}} }

func (f *fakeJobs) CreateJob(_ context.Context, _, _, _ string) (any, error) {
	f.mu.Lock()
	f.created++
	f.mu.Unlock()
	return nil, nil
}
func (f *fakeJobs) StartJob(_ context.Context, _ string) error {
	f.mu.Lock()
	f.started++
	f.mu.Unlock()
	return nil
}
func (f *fakeJobs) FinishJob(_ context.Context, _, state, _ string) error {
	f.mu.Lock()
	f.finished[state]++
	f.mu.Unlock()
	return nil
}
func (f *fakeJobs) finishedCount(state string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished[state]
}

// fakeNotifier captures emitted events.
type fakeNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *fakeNotifier) Emit(e notify.Event) {
	n.mu.Lock()
	n.events = append(n.events, e)
	n.mu.Unlock()
}
func (n *fakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.events)
}
func (n *fakeNotifier) types() []notify.EventType {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]notify.EventType, len(n.events))
	for i, e := range n.events {
		out[i] = e.Type
	}
	return out
}

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

// TestSchedulerRunsTaskAndRecordsJob verifies an enabled task runs, is wrapped
// in a job (create/start/finish=completed), and emits a completion event.
func TestSchedulerRunsTaskAndRecordsJob(t *testing.T) {
	jobs := newFakeJobs()
	notifier := &fakeNotifier{}
	var runs int32
	tasks := []Task{{
		Name: "retention", Interval: 10 * time.Millisecond, Enabled: true,
		Run: func(context.Context) (string, error) {
			atomic.AddInt32(&runs, 1)
			return "pruned 5 parts", nil
		},
	}}
	s := New(tasks, jobs, notifier, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&runs) >= 1 })
	cancel()
	<-done

	if jobs.created == 0 || jobs.started == 0 {
		t.Errorf("expected job create/start, got created=%d started=%d", jobs.created, jobs.started)
	}
	if jobs.finishedCount("completed") == 0 {
		t.Error("expected at least one completed job")
	}
	// Retention completion maps to the retention.completed event type.
	var sawRetention bool
	for _, ty := range notifier.types() {
		if ty == notify.EventRetentionCompleted {
			sawRetention = true
		}
	}
	if !sawRetention {
		t.Errorf("expected a retention.completed event, got %v", notifier.types())
	}
}

// TestSchedulerSkipsDisabledAndZeroInterval verifies disabled or zero-interval
// tasks never run.
func TestSchedulerSkipsDisabledAndZeroInterval(t *testing.T) {
	var disabledRuns, zeroRuns int32
	tasks := []Task{
		{Name: "disabled", Interval: time.Millisecond, Enabled: false,
			Run: func(context.Context) (string, error) { atomic.AddInt32(&disabledRuns, 1); return "", nil }},
		{Name: "zero", Interval: 0, Enabled: true,
			Run: func(context.Context) (string, error) { atomic.AddInt32(&zeroRuns, 1); return "", nil }},
	}
	s := New(tasks, nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	if atomic.LoadInt32(&disabledRuns) != 0 {
		t.Errorf("disabled task ran %d times, want 0", disabledRuns)
	}
	if atomic.LoadInt32(&zeroRuns) != 0 {
		t.Errorf("zero-interval task ran %d times, want 0", zeroRuns)
	}
}

// TestSchedulerFailingTaskDoesNotStopOthers verifies a task that errors records
// a failed job + emits a failure event, while an independent task keeps running.
func TestSchedulerFailingTaskDoesNotStopOthers(t *testing.T) {
	jobs := newFakeJobs()
	notifier := &fakeNotifier{}
	var goodRuns int32
	tasks := []Task{
		{Name: "bad", Interval: 10 * time.Millisecond, Enabled: true,
			Run: func(context.Context) (string, error) { return "", errors.New("boom") }},
		{Name: "good", Interval: 10 * time.Millisecond, Enabled: true,
			Run: func(context.Context) (string, error) { atomic.AddInt32(&goodRuns, 1); return "ok", nil }},
	}
	s := New(tasks, jobs, notifier, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor(t, time.Second, func() bool {
		return jobs.finishedCount("failed") >= 1 && atomic.LoadInt32(&goodRuns) >= 1
	})
	cancel()
	<-done

	if jobs.finishedCount("failed") == 0 {
		t.Error("expected a failed job for the erroring task")
	}
	if atomic.LoadInt32(&goodRuns) == 0 {
		t.Error("the good task should keep running despite the bad task failing")
	}
	if notifier.count() == 0 {
		t.Error("expected notifications for both success and failure")
	}
}

// TestSchedulerStopsOnCancel verifies Run returns promptly after cancellation.
func TestSchedulerStopsOnCancel(t *testing.T) {
	tasks := []Task{{
		Name: "slow", Interval: time.Hour, Enabled: true,
		Run: func(ctx context.Context) (string, error) { return "", nil },
	}}
	s := New(tasks, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop promptly after cancel")
	}
}

// TestEventTypeFor maps task names to the right notification event type.
func TestEventTypeFor(t *testing.T) {
	cases := map[string]notify.EventType{
		"retention":     notify.EventRetentionCompleted,
		"backup-verify": notify.EventBackupOutcome,
		"analyze":       notify.EventJobCompleted,
		"job-cleanup":   notify.EventJobCompleted,
	}
	for name, want := range cases {
		if got := eventTypeFor(name); got != want {
			t.Errorf("eventTypeFor(%q) = %q, want %q", name, got, want)
		}
	}
}
