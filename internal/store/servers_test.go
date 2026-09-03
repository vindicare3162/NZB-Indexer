package store

import (
	"context"
	"testing"
)

func strptr(s string) *string { return &s }

func TestServerCRUD(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	// Initially none.
	n, err := st.CountServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 servers, got %d", n)
	}
	if _, err := st.GetActiveServer(ctx); err != ErrNotFound {
		t.Errorf("expected ErrNotFound for active server, got %v", err)
	}

	// Create two servers with different priorities.
	primary, err := st.CreateServer(ctx, ServerInput{
		Name: "primary", Host: "news.example.com", Port: 563, TLS: true,
		Username: "u", Password: strptr("secret"), MaxConns: 10, Priority: 0, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create primary: %v", err)
	}
	if primary.Password != "secret" {
		t.Errorf("stored password = %q", primary.Password)
	}
	_, err = st.CreateServer(ctx, ServerInput{
		Name: "block", Host: "block.example.com", Port: 563, TLS: true,
		Username: "u2", Password: strptr("blockpw"), MaxConns: 5, Priority: 10, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create block: %v", err)
	}

	// Active server is the lowest-priority (primary).
	active, err := st.GetActiveServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active.Name != "primary" {
		t.Errorf("active = %q, want primary", active.Name)
	}

	// Update primary WITHOUT a password (nil) preserves the stored password.
	updated, err := st.UpdateServer(ctx, primary.ID, ServerInput{
		Name: "primary", Host: "news2.example.com", Port: 563, TLS: true,
		Username: "u", Password: nil, MaxConns: 20, Priority: 0, Enabled: true,
	})
	if err != nil {
		t.Fatalf("update primary: %v", err)
	}
	if updated.Host != "news2.example.com" {
		t.Errorf("host not updated: %q", updated.Host)
	}
	if updated.Password != "secret" {
		t.Errorf("password should be preserved on nil update, got %q", updated.Password)
	}
	if updated.MaxConns != 20 {
		t.Errorf("max_conns = %d, want 20", updated.MaxConns)
	}

	// Update WITH a new password replaces it.
	updated, err = st.UpdateServer(ctx, primary.ID, ServerInput{
		Name: "primary", Host: "news2.example.com", Port: 563, TLS: true,
		Username: "u", Password: strptr("rotated"), MaxConns: 20, Priority: 0, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Password != "rotated" {
		t.Errorf("password should be replaced, got %q", updated.Password)
	}

	// Disable primary; active becomes block.
	if _, err := st.UpdateServer(ctx, primary.ID, ServerInput{
		Name: "primary", Host: "news2.example.com", Port: 563, TLS: true,
		Username: "u", Password: nil, MaxConns: 20, Priority: 0, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	active, err = st.GetActiveServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active.Name != "block" {
		t.Errorf("active after disabling primary = %q, want block", active.Name)
	}

	// List returns both, ordered by priority.
	list, err := st.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "primary" || list[1].Name != "block" {
		t.Errorf("list order = %+v", list)
	}

	// Delete.
	if err := st.DeleteServer(ctx, primary.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(ctx, primary.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound deleting missing server, got %v", err)
	}
	list, _ = st.ListServers(ctx)
	if len(list) != 1 {
		t.Errorf("after delete, servers = %d, want 1", len(list))
	}
}

func TestUpdateServerNotFound(t *testing.T) {
	st := freshStore(t)
	_, err := st.UpdateServer(context.Background(), 99999, ServerInput{Name: "x", Host: "h"})
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
