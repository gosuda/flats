package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHostKeys(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.GetHostKey(ctx, "tailscale"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if has, err := s.HasSealedHostKeys(ctx); err != nil || has {
		t.Fatalf("HasSealedHostKeys = %v, %v", has, err)
	}
	at := time.UnixMilli(1_700_000_000_000).UTC()
	k := HostKey{ID: "tailscale", Nonce: []byte("n"), Ciphertext: []byte("c"), Source: "console", UpdatedAt: at}
	if err := s.PutHostKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetHostKey(ctx, "tailscale")
	if err != nil || !got.HasValue() || string(got.Ciphertext) != "c" || got.Source != "console" || !got.UpdatedAt.Equal(at) {
		t.Fatalf("GetHostKey = %+v, %v", got, err)
	}
	if has, _ := s.HasSealedHostKeys(ctx); !has {
		t.Fatal("sealed key not counted")
	}
	// A connection record without a value.
	if err := s.PutHostKey(ctx, HostKey{ID: "zrok", Source: "env", UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHostKeyError(ctx, "zrok", "refused"); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListHostKeys(ctx)
	if err != nil || len(all) != 2 || all[0].ID != "tailscale" || all[1].HasValue() || all[1].LastError != "refused" {
		t.Fatalf("ListHostKeys = %+v, %v", all, err)
	}
	for name, bad := range map[string]HostKey{
		"unknown id":     {ID: "portal", Source: "cli", UpdatedAt: at},
		"unknown source": {ID: "netbird", Source: "agent", UpdatedAt: at},
		"nonce only":     {ID: "netbird", Nonce: []byte("n"), Source: "cli", UpdatedAt: at},
	} {
		if err := s.PutHostKey(ctx, bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := s.DeleteHostKey(ctx, "tailscale"); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.HasSealedHostKeys(ctx); has {
		t.Fatal("deleted key still counted")
	}
	if err := s.DeleteHostKey(ctx, "tailscale"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
}

func TestConsoleNodes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, n := range []ConsoleNode{{Name: "flats"}, {Name: "admin", Retiring: true, LastError: "logout failed"}} {
		if err := s.PutConsoleNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutConsoleNode(ctx, ConsoleNode{Name: "flats", Retiring: true}); err != nil {
		t.Fatal(err)
	}
	// The same name may be a NetBird console node too.
	if err := s.PutConsoleNode(ctx, ConsoleNode{Provider: "netbird", Name: "flats"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListConsoleNodes(ctx)
	if err != nil || len(got) != 3 || got[0] != (ConsoleNode{Provider: "netbird", Name: "flats"}) ||
		got[1] != (ConsoleNode{Provider: "tailscale", Name: "admin", Retiring: true, LastError: "logout failed"}) ||
		got[2] != (ConsoleNode{Provider: "tailscale", Name: "flats", Retiring: true}) {
		t.Fatalf("ListConsoleNodes = %+v, %v", got, err)
	}
	if err := s.DeleteConsoleNode(ctx, "", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConsoleNode(ctx, "netbird", "flats"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListConsoleNodes(ctx); len(got) != 1 || got[0].Provider != "tailscale" {
		t.Fatalf("after delete: %+v", got)
	}
}
