// Package notify delivers pipeline event notifications to configured HTTP
// webhook destinations (#137). It is intentionally decoupled from the pipeline:
// producers call Emit with a typed Event and return immediately; delivery
// happens asynchronously on a bounded worker pool with retry, backoff, HMAC
// signing, and per-destination event filtering, so a slow or failing webhook
// can never block indexing. A bounded in-memory history retains recent delivery
// outcomes (status, attempts, last error) for the admin UI.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// EventType identifies a notifiable pipeline event.
type EventType string

const (
	EventReleaseCreated     EventType = "release.created"
	EventReleaseRenamed     EventType = "release.renamed"
	EventScanFailed         EventType = "scan.failed"
	EventScanLag            EventType = "scan.lag"
	EventPostProcessFailed  EventType = "postprocess.failed"
	EventProviderFailover   EventType = "provider.failover"
	EventStorageWarning     EventType = "storage.warning"
	EventRetentionCompleted EventType = "retention.completed"
	EventBackupOutcome      EventType = "backup.outcome"
	EventJobCompleted       EventType = "job.completed"
	EventJobFailed          EventType = "job.failed"
)

// Event is a single notifiable occurrence. ID is used for delivery
// deduplication and is auto-assigned when empty.
type Event struct {
	ID      string            `json:"id"`
	Type    EventType         `json:"type"`
	Title   string            `json:"title"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
	Time    time.Time         `json:"time"`
}

// Notifier accepts events for asynchronous delivery. Emit never blocks the
// caller for network I/O.
type Notifier interface {
	Emit(e Event)
}

// Destination is a configured webhook target.
type Destination struct {
	// Name is a human label used in delivery history.
	Name string
	// URL is the webhook endpoint. Required.
	URL string
	// Secret, when set, signs each request body with HMAC-SHA256 in the
	// X-Goindex-Signature header (hex, "sha256=" prefixed).
	Secret string
	// Events filters which event types are delivered. Empty = all events.
	Events []EventType
	// Enabled gates delivery; a disabled destination receives nothing.
	Enabled bool
}

func (d Destination) wants(t EventType) bool {
	if !d.Enabled {
		return false
	}
	if len(d.Events) == 0 {
		return true
	}
	for _, e := range d.Events {
		if e == t {
			return true
		}
	}
	return false
}

// Delivery records the outcome of attempting to deliver one event to one
// destination, retained in the history buffer.
type Delivery struct {
	EventID     string    `json:"event_id"`
	Destination string    `json:"destination"`
	Type        EventType `json:"type"`
	Success     bool      `json:"success"`
	Attempts    int       `json:"attempts"`
	StatusCode  int       `json:"status_code,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	At          time.Time `json:"at"`
}

// Options tunes delivery behaviour. Zero values fall back to sane defaults.
type Options struct {
	// Timeout bounds each HTTP attempt (default 10s).
	Timeout time.Duration
	// MaxAttempts is the total number of send attempts per delivery, including
	// the first (default 3).
	MaxAttempts int
	// BaseBackoff is the initial retry backoff, doubled each attempt
	// (default 500ms).
	BaseBackoff time.Duration
	// QueueSize bounds the pending-event queue; when full, new events are
	// dropped (and logged) rather than blocking the producer (default 256).
	QueueSize int
	// Workers is the number of concurrent delivery goroutines (default 2).
	Workers int
	// HistorySize is how many recent deliveries to retain (default 200).
	HistorySize int
	// DedupWindow suppresses re-delivery of an event id already seen within
	// this window (default 5m). Zero disables dedup.
	DedupWindow time.Duration
	// Now is injectable for tests (default time.Now).
	Now func() time.Time
}

func (o *Options) applyDefaults() {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 500 * time.Millisecond
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.HistorySize <= 0 {
		o.HistorySize = 200
	}
	if o.DedupWindow == 0 {
		o.DedupWindow = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Service is the concurrent webhook notifier.
type Service struct {
	dests  []Destination
	opts   Options
	client *http.Client
	log    *slog.Logger

	queue chan Event
	wg    sync.WaitGroup
	stop  chan struct{}
	once  sync.Once

	mu      sync.Mutex
	history []Delivery // ring buffer, newest last
	seen    map[string]time.Time
}

// New builds a notifier for the given destinations. When no destination is
// enabled the returned service is inert (Emit is a no-op) but safe to use.
func New(dests []Destination, opts Options, log *slog.Logger) *Service {
	opts.applyDefaults()
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		dests:  dests,
		opts:   opts,
		client: &http.Client{Timeout: opts.Timeout},
		log:    log,
		queue:  make(chan Event, opts.QueueSize),
		stop:   make(chan struct{}),
		seen:   map[string]time.Time{},
	}
	return s
}

// Enabled reports whether any destination is enabled.
func (s *Service) Enabled() bool {
	for _, d := range s.dests {
		if d.Enabled {
			return true
		}
	}
	return false
}

// Start launches the delivery workers. Call Stop (or cancel via Run) to drain.
func (s *Service) Start() {
	for i := 0; i < s.opts.Workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
}

// Run starts the workers and blocks until ctx is cancelled, then stops.
func (s *Service) Run(ctx context.Context) {
	s.Start()
	<-ctx.Done()
	s.Stop()
}

// Stop signals the workers to finish and waits for them.
func (s *Service) Stop() {
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Emit queues an event for delivery. It never blocks: if the queue is full the
// event is dropped and logged, so notification pressure cannot stall the
// pipeline.
func (s *Service) Emit(e Event) {
	if !s.Enabled() {
		return
	}
	if e.Time.IsZero() {
		e.Time = s.opts.Now()
	}
	if e.ID == "" {
		e.ID = fmt.Sprintf("%s-%d", e.Type, e.Time.UnixNano())
	}
	select {
	case s.queue <- e:
	default:
		s.log.Warn("notification queue full, dropping event", "type", e.Type, "id", e.ID)
	}
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case e := <-s.queue:
			s.dispatch(e)
		}
	}
}

// dispatch delivers one event to every interested destination.
func (s *Service) dispatch(e Event) {
	if s.deduped(e.ID) {
		s.log.Debug("suppressing duplicate notification", "id", e.ID)
		return
	}
	for _, d := range s.dests {
		if d.wants(e.Type) {
			s.deliver(d, e)
		}
	}
}

// deduped reports whether the event id was already seen within the dedup
// window, and records it otherwise.
func (s *Service) deduped(id string) bool {
	if s.opts.DedupWindow <= 0 {
		return false
	}
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Opportunistic GC of stale entries.
	for k, t := range s.seen {
		if now.Sub(t) > s.opts.DedupWindow {
			delete(s.seen, k)
		}
	}
	if t, ok := s.seen[id]; ok && now.Sub(t) <= s.opts.DedupWindow {
		return true
	}
	s.seen[id] = now
	return false
}

// deliver attempts delivery with retries and records the outcome.
func (s *Service) deliver(d Destination, e Event) {
	body, err := json.Marshal(e)
	if err != nil {
		s.record(Delivery{EventID: e.ID, Destination: d.Name, Type: e.Type, LastError: "marshal: " + err.Error(), At: s.opts.Now()})
		return
	}

	var (
		lastErr    string
		lastStatus int
	)
	for attempt := 1; attempt <= s.opts.MaxAttempts; attempt++ {
		status, err := s.send(d, body)
		lastStatus = status
		if err == nil && status >= 200 && status < 300 {
			s.record(Delivery{EventID: e.ID, Destination: d.Name, Type: e.Type, Success: true, Attempts: attempt, StatusCode: status, At: s.opts.Now()})
			return
		}
		if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = fmt.Sprintf("unexpected status %d", status)
		}
		if attempt < s.opts.MaxAttempts {
			backoff := s.opts.BaseBackoff << (attempt - 1)
			select {
			case <-s.stop:
				lastErr = "shutting down"
				goto done
			case <-time.After(backoff):
			}
		}
	}
done:
	s.log.Warn("notification delivery failed", "dest", d.Name, "type", e.Type, "attempts", s.opts.MaxAttempts, "err", lastErr)
	s.record(Delivery{EventID: e.ID, Destination: d.Name, Type: e.Type, Success: false, Attempts: s.opts.MaxAttempts, StatusCode: lastStatus, LastError: lastErr, At: s.opts.Now()})
}

// send performs one HTTP POST attempt, signing the body when a secret is set.
func (s *Service) send(d Destination, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "goindex-notify/1")
	if d.Secret != "" {
		req.Header.Set("X-Goindex-Signature", "sha256="+Sign(d.Secret, body))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Sign returns the hex HMAC-SHA256 of body keyed by secret.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// record appends a delivery to the bounded history buffer.
func (s *Service) record(d Delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, d)
	if len(s.history) > s.opts.HistorySize {
		s.history = s.history[len(s.history)-s.opts.HistorySize:]
	}
}

// History returns up to limit most-recent deliveries, newest first. limit <= 0
// returns all retained deliveries.
func (s *Service) History(limit int) []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.history)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Delivery, 0, n)
	for i := len(s.history) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.history[i])
	}
	return out
}
