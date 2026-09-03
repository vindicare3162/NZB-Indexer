package nntp

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is a scripted in-memory connection for tests.
type fakeConn struct {
	id int

	authErr error
	authed  bool

	// per-call behaviour
	groupInfo    GroupInfo
	groupErr        error
	overviewData    []Overview
	overErr         error
	availableGroups []AvailableGroup
	listErr         error
	bodyData        string
	bodyErr         error

	pingErr error
	closed  bool

	// failN makes the next failN operations return failErr (to exercise
	// retry). Decremented on each qualifying call.
	failN   int
	failErr error
}

func (f *fakeConn) authenticate(user, pass string) error {
	if f.authErr != nil {
		return f.authErr
	}
	f.authed = true
	return nil
}

func (f *fakeConn) selectGroup(name string) (GroupInfo, error) {
	if f.failN > 0 {
		f.failN--
		return GroupInfo{}, f.failErr
	}
	if f.groupErr != nil {
		return GroupInfo{}, f.groupErr
	}
	gi := f.groupInfo
	gi.Name = name
	return gi, nil
}

func (f *fakeConn) overview(begin, end int64) ([]Overview, error) {
	if f.overErr != nil {
		return nil, f.overErr
	}
	return f.overviewData, nil
}

func (f *fakeConn) listActive() ([]AvailableGroup, error) {
	return f.availableGroups, f.listErr
}

func (f *fakeConn) body(_ context.Context, messageID string) (io.ReadCloser, error) {
	if f.bodyErr != nil {
		return nil, f.bodyErr
	}
	return io.NopCloser(strings.NewReader(f.bodyData)), nil
}

func (f *fakeConn) ping() error { return f.pingErr }

func (f *fakeConn) close() error {
	f.closed = true
	return nil
}

// netErr is a fake connection-fatal network error.
type netErr struct{ msg string }

func (e netErr) Error() string   { return e.msg }
func (e netErr) Timeout() bool   { return true }
func (e netErr) Temporary() bool { return true }

func TestPoolReusesConnection(t *testing.T) {
	var dials int32
	conns := []*fakeConn{}
	var mu sync.Mutex

	d := func(cfg Config) (conn, error) {
		atomic.AddInt32(&dials, 1)
		mu.Lock()
		defer mu.Unlock()
		c := &fakeConn{id: len(conns), groupInfo: GroupInfo{High: 100}}
		conns = append(conns, c)
		return c, nil
	}

	p := newWithDialer(Config{MaxConns: 2, Username: "u", Password: "p"}, d)
	defer p.Close()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := p.SelectGroupInfo(ctx, "alt.binaries.test"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// Serial calls should reuse a single connection.
	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Errorf("dials = %d, want 1 (connection should be reused)", got)
	}
	// Auth should have run on the created connection.
	if !conns[0].authed {
		t.Error("expected connection to be authenticated")
	}
	open, idle := p.Stats()
	if open != 1 || idle != 1 {
		t.Errorf("stats = (open=%d idle=%d), want (1,1)", open, idle)
	}
}

func TestPoolRespectsMaxConns(t *testing.T) {
	var dials int32
	release := make(chan struct{})
	started := make(chan struct{}, 3)

	d := func(cfg Config) (conn, error) {
		atomic.AddInt32(&dials, 1)
		return &fakeConn{groupInfo: GroupInfo{High: 1}}, nil
	}
	p := newWithDialer(Config{MaxConns: 2}, d)
	defer p.Close()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.withConn(context.Background(), func(c conn) error {
				started <- struct{}{}
				<-release // hold the connection
				return nil
			})
		}()
	}
	// Wait for both slots to be occupied.
	<-started
	<-started

	// A third acquire must block until a slot frees; enforce via short ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := p.acquire(ctx)
	if !errors.Is(err, ErrNoConns) {
		t.Fatalf("expected ErrNoConns when pool exhausted, got %v", err)
	}

	close(release)
	wg.Wait()
	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Errorf("dials = %d, want 2 (bounded by MaxConns)", got)
	}
}

func TestPoolRetriesTransientError(t *testing.T) {
	var dials int32
	d := func(cfg Config) (conn, error) {
		n := atomic.AddInt32(&dials, 1)
		// First connection fails twice with a network error, then would
		// succeed; but since a fatal error discards the conn, each retry
		// dials fresh. Give the 3rd dial a healthy conn.
		if n <= 2 {
			return &fakeConn{failN: 1, failErr: netErr{"reset"}, groupInfo: GroupInfo{High: 5}}, nil
		}
		return &fakeConn{groupInfo: GroupInfo{High: 42}}, nil
	}
	p := newWithDialer(Config{MaxConns: 3, MaxRetries: 3, RetryBackoff: time.Millisecond}, d)
	defer p.Close()

	info, err := p.SelectGroupInfo(context.Background(), "grp")
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if info.High != 42 {
		t.Errorf("High = %d, want 42", info.High)
	}
	if got := atomic.LoadInt32(&dials); got < 3 {
		t.Errorf("dials = %d, want >=3 (retries dial fresh connections)", got)
	}
}

func TestPoolAuthFailurePropagates(t *testing.T) {
	d := func(cfg Config) (conn, error) {
		return &fakeConn{authErr: errors.New("auth rejected")}, nil
	}
	p := newWithDialer(Config{MaxConns: 1, MaxRetries: 1, RetryBackoff: time.Millisecond, Username: "u"}, d)
	defer p.Close()

	_, err := p.SelectGroupInfo(context.Background(), "grp")
	if err == nil {
		t.Fatal("expected auth error to propagate")
	}
	if !strings.Contains(err.Error(), "auth rejected") {
		t.Errorf("error = %v, want auth rejected", err)
	}
}

func TestOverviewAndBody(t *testing.T) {
	d := func(cfg Config) (conn, error) {
		return &fakeConn{
			overviewData: []Overview{
				{ArticleNumber: 1, Subject: "a", MessageID: "m1", Bytes: 100},
				{ArticleNumber: 2, Subject: "b", MessageID: "m2", Bytes: 200},
			},
			bodyData: "hello body",
		}, nil
	}
	p := newWithDialer(Config{MaxConns: 1}, d)
	defer p.Close()
	ctx := context.Background()

	ov, err := p.Overview(ctx, "grp", 1, 2)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(ov) != 2 || ov[1].MessageID != "m2" {
		t.Fatalf("unexpected overview: %+v", ov)
	}

	body, err := p.Body(ctx, "m1")
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if string(body) != "hello body" {
		t.Errorf("body = %q, want %q", body, "hello body")
	}
}

func TestPoolClosedRejects(t *testing.T) {
	d := func(cfg Config) (conn, error) { return &fakeConn{}, nil }
	p := newWithDialer(Config{MaxConns: 1}, d)
	p.Close()

	_, err := p.acquire(context.Background())
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("expected ErrPoolClosed, got %v", err)
	}
}

func TestUnhealthyIdleConnDiscarded(t *testing.T) {
	var dials int32
	d := func(cfg Config) (conn, error) {
		n := atomic.AddInt32(&dials, 1)
		if n == 1 {
			// First conn becomes unhealthy after being returned to the pool.
			return &fakeConn{groupInfo: GroupInfo{High: 1}, pingErr: errors.New("dead")}, nil
		}
		return &fakeConn{groupInfo: GroupInfo{High: 2}}, nil
	}
	p := newWithDialer(Config{MaxConns: 2}, d)
	defer p.Close()
	ctx := context.Background()

	// First call creates conn #1 and returns it to idle.
	if _, err := p.SelectGroupInfo(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	// Second call finds conn #1 unhealthy (ping fails), discards it, dials #2.
	info, err := p.SelectGroupInfo(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if info.High != 2 {
		t.Errorf("High = %d, want 2 (should use freshly dialed conn)", info.High)
	}
	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Errorf("dials = %d, want 2", got)
	}
}

func TestReconfigureAppliesNewSettingsAndDropsIdle(t *testing.T) {
	var dialedHosts []string
	var mu sync.Mutex
	d := func(cfg Config) (conn, error) {
		mu.Lock()
		dialedHosts = append(dialedHosts, cfg.Host)
		mu.Unlock()
		return &fakeConn{groupInfo: GroupInfo{High: 1}}, nil
	}
	p := newWithDialer(Config{Host: "old.example.com", MaxConns: 2}, d)
	defer p.Close()
	ctx := context.Background()

	// First call dials the old host and leaves an idle connection.
	if _, err := p.SelectGroupInfo(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	_, idle := p.Stats()
	if idle != 1 {
		t.Fatalf("expected 1 idle conn, got %d", idle)
	}

	// Reconfigure to a new host; idle connections must be dropped.
	p.Reconfigure(Config{Host: "new.example.com", MaxConns: 999})
	if _, idle := p.Stats(); idle != 0 {
		t.Errorf("Reconfigure should drop idle conns, got %d idle", idle)
	}

	// MaxConns ceiling must be preserved (not raised to 999).
	if got := p.config().MaxConns; got != 2 {
		t.Errorf("MaxConns after reconfigure = %d, want 2 (ceiling preserved)", got)
	}

	// Next call dials the new host.
	if _, err := p.SelectGroupInfo(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialedHosts) != 2 || dialedHosts[0] != "old.example.com" || dialedHosts[1] != "new.example.com" {
		t.Errorf("dialed hosts = %v, want [old.example.com new.example.com]", dialedHosts)
	}
}

func TestConnFatalClassification(t *testing.T) {
	if isConnFatal(nil) {
		t.Error("nil should not be fatal")
	}
	if !isConnFatal(io.EOF) {
		t.Error("EOF should be fatal")
	}
	if !isConnFatal(netErr{"x"}) {
		t.Error("net error should be fatal")
	}
}

func TestTrimAndEnsureAngle(t *testing.T) {
	if got := trimAngle("<abc@host>"); got != "abc@host" {
		t.Errorf("trimAngle = %q", got)
	}
	if got := trimAngle("abc@host"); got != "abc@host" {
		t.Errorf("trimAngle no-op = %q", got)
	}
	if got := ensureAngle("abc@host"); got != "<abc@host>" {
		t.Errorf("ensureAngle = %q", got)
	}
	if got := ensureAngle("<abc@host>"); got != "<abc@host>" {
		t.Errorf("ensureAngle no-op = %q", got)
	}
}
