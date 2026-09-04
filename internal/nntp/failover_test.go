package nntp

import (
	"context"
	"net"
	"net/textproto"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// endpointFakeConn behaviour is controlled per-host so tests can make one
// provider fail (connection error) and another succeed.
func failoverTestPoolFactory(t *testing.T) (func(Config) *Pool, map[string]*int32) {
	t.Helper()
	// selectCalls counts SelectGroupInfo dispatches per host.
	selectCalls := map[string]*int32{}
	var mu sync.Mutex

	factory := func(cfg Config) *Pool {
		mu.Lock()
		if selectCalls[cfg.Host] == nil {
			selectCalls[cfg.Host] = new(int32)
		}
		counter := selectCalls[cfg.Host]
		mu.Unlock()

		d := func(c Config) (conn, error) {
			// "down" host: dialing fails (connection-fatal).
			if c.Host == "down" {
				return nil, net.UnknownNetworkError("connection refused")
			}
			return &fakeConn{groupInfo: GroupInfo{High: 42}}, nil
		}
		p := newWithDialer(cfg, d)
		// Wrap SelectGroupInfo counting via the group info high value is not
		// enough; instead we count at the FailoverPool level using the host.
		_ = counter
		return p
	}
	return factory, selectCalls
}

func TestFailoverToHealthyProvider(t *testing.T) {
	factory, _ := failoverTestPoolFactory(t)
	eps := []EndpointConfig{
		{ID: 1, Name: "primary-down", Priority: 0, Config: Config{Host: "down", MaxConns: 2}},
		{ID: 2, Name: "fallback-ok", Priority: 10, Config: Config{Host: "ok", MaxConns: 2}},
	}
	fp := newFailoverWithPool(eps, FailoverOptions{FailureThreshold: 1, Cooldown: time.Minute}, factory)
	defer fp.Close()

	// Primary is down; the call must fail over to the healthy fallback.
	info, err := fp.SelectGroupInfo(context.Background(), "g")
	if err != nil {
		t.Fatalf("expected failover success, got %v", err)
	}
	if info.High != 42 {
		t.Errorf("info.High = %d, want 42 (from fallback)", info.High)
	}

	// The primary's circuit should now be open; active server is the fallback.
	name, _, ok := fp.ActiveServer()
	if !ok || name != "fallback-ok" {
		t.Errorf("active server = %q ok=%v, want fallback-ok", name, ok)
	}
	health := fp.Health()
	if len(health) != 2 {
		t.Fatalf("health entries = %d, want 2", len(health))
	}
	if health[0].Circuit != "open" {
		t.Errorf("primary circuit = %s, want open", health[0].Circuit)
	}
}

func TestFailoverAllProvidersDown(t *testing.T) {
	factory, _ := failoverTestPoolFactory(t)
	eps := []EndpointConfig{
		{ID: 1, Name: "a", Priority: 0, Config: Config{Host: "down", MaxConns: 1}},
		{ID: 2, Name: "b", Priority: 1, Config: Config{Host: "down", MaxConns: 1}},
	}
	fp := newFailoverWithPool(eps, FailoverOptions{FailureThreshold: 1, Cooldown: time.Minute}, factory)
	defer fp.Close()

	// First call trips both circuits (fails over a->b, both fail).
	if _, err := fp.SelectGroupInfo(context.Background(), "g"); err == nil {
		t.Error("expected error when all providers are down")
	}
	// Circuits are open now; a subsequent call short-circuits to ErrNoHealthyServer.
	_, err := fp.SelectGroupInfo(context.Background(), "g")
	if err != ErrNoHealthyServer {
		t.Errorf("err = %v, want ErrNoHealthyServer once all circuits are open", err)
	}
	if _, _, ok := fp.ActiveServer(); ok {
		t.Error("no server should be active when all circuits are open")
	}
}

func TestFailoverRecoveryAfterCooldown(t *testing.T) {
	// A recovering primary: dialing fails while "broken" is set, then succeeds.
	var broken atomic.Bool
	broken.Store(true)
	factory := func(cfg Config) *Pool {
		d := func(c Config) (conn, error) {
			if c.Host == "primary" && broken.Load() {
				return nil, net.UnknownNetworkError("down")
			}
			return &fakeConn{groupInfo: GroupInfo{High: 7}}, nil
		}
		return newWithDialer(cfg, d)
	}
	eps := []EndpointConfig{
		{ID: 1, Name: "primary", Priority: 0, Config: Config{Host: "primary", MaxConns: 1}},
		{ID: 2, Name: "fallback", Priority: 10, Config: Config{Host: "fallback", MaxConns: 1}},
	}
	fp := newFailoverWithPool(eps, FailoverOptions{FailureThreshold: 1, Cooldown: 20 * time.Millisecond}, factory)
	defer fp.Close()

	// Primary down -> fallback serves; primary circuit opens.
	if _, err := fp.SelectGroupInfo(context.Background(), "g"); err != nil {
		t.Fatalf("initial failover: %v", err)
	}
	if fp.Health()[0].Circuit != "open" {
		t.Fatal("primary should be open after failure")
	}

	// Primary recovers; wait out the cooldown, then a call probes it and closes.
	broken.Store(false)
	time.Sleep(30 * time.Millisecond)
	if _, err := fp.SelectGroupInfo(context.Background(), "g"); err != nil {
		t.Fatalf("post-recovery call: %v", err)
	}
	// After a successful probe, primary is active again (highest priority).
	name, _, ok := fp.ActiveServer()
	if !ok || name != "primary" {
		t.Errorf("active server = %q, want primary after recovery", name)
	}
}

func TestFailoverProtocolErrorNoFailover(t *testing.T) {
	// A protocol error (server answered) should NOT trip the breaker or fail
	// over — it's returned to the caller.
	factory := func(cfg Config) *Pool {
		d := func(c Config) (conn, error) {
			return &fakeConn{groupErr: &textproto.Error{Code: 411, Msg: "no such group"}}, nil
		}
		return newWithDialer(cfg, d)
	}
	eps := []EndpointConfig{
		{ID: 1, Name: "only", Priority: 0, Config: Config{Host: "ok", MaxConns: 1}},
	}
	fp := newFailoverWithPool(eps, FailoverOptions{FailureThreshold: 1, Cooldown: time.Minute}, factory)
	defer fp.Close()

	if _, err := fp.SelectGroupInfo(context.Background(), "g"); err == nil {
		t.Error("expected the protocol error to propagate")
	}
	// Breaker must remain closed (server is healthy, just rejected the command).
	if fp.Health()[0].Circuit != "closed" {
		t.Errorf("circuit = %s, want closed (protocol error is not a health problem)", fp.Health()[0].Circuit)
	}
}
