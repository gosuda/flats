package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// ReserveConsoleName holds the name against flats while the domain is added,
// and refuses names a flat, preview or redirect holds.
func TestReserveConsoleName(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	var mu sync.Mutex
	listed := []string{}
	s.cfg.ReservedNames = func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), listed...) }
	if _, err := s.CreateFlat(ctx, "blog", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveConsoleName(ctx, "blog", func() error { return nil }); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "flat") {
		t.Fatalf("a flat's name: %v", err)
	}
	// While commit runs, flat creation of the same name waits, then is refused.
	done := make(chan error, 1)
	err := s.ReserveConsoleName(ctx, "admin", func() error {
		go func() { _, err := s.CreateFlat(ctx, "admin", "", ViaAPI); done <- err }()
		select {
		case err := <-done:
			t.Errorf("CreateFlat during the addition returned early: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		mu.Lock()
		listed = append(listed, "admin")
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "reserved for the Flats console") {
		t.Fatalf("the waiting CreateFlat: %v", err)
	}
	if _, err := s.CreateFlat(ctx, "admin", "", ViaAPI); err == nil || !strings.Contains(err.Error(), "reserved for the Flats console") {
		t.Fatalf("a flat took the console domain: %v", err)
	}
	if err := s.ReserveConsoleName(ctx, "admin", func() error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("a listed name: %v", err)
	}
	// A failed commit frees the name again.
	boom := errors.New("boom")
	if err := s.ReserveConsoleName(ctx, "spare", func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("commit error: %v", err)
	}
	if _, err := s.CreateFlat(ctx, "spare", "", ViaAPI); err != nil {
		t.Fatalf("name not released: %v", err)
	}
	// Fixed reserved names stay free for console domains.
	s.cfg.Reserved = []string{"flats"}
	if err := s.ReserveConsoleName(ctx, "flats", func() error { return nil }); err != nil {
		t.Fatalf("fixed reserved name: %v", err)
	}
	if h, err := s.ConsoleNameHolder(ctx, "nobody"); err != nil || h != "" {
		t.Fatalf("holder of a free name: %q %v", h, err)
	}
}

// Preview hosts never take a console name: a chosen host is reserved until
// its preview is registered, and console additions of it are refused.
func TestPreviewHostAvoidsConsoleNames(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	calls := 0
	s.cfg.ReservedNames = func() []string {
		calls++
		return nil
	}
	host, release := s.previewHost("site")
	if !strings.HasPrefix(host, "site-") || calls == 0 {
		t.Fatalf("preview host %q (checks %d)", host, calls)
	}
	if err := s.ReserveConsoleName(ctx, host, func() error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("console addition of a preview host being opened: %v", err)
	}
	release()
	if err := s.ReserveConsoleName(ctx, host, func() error { return nil }); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestApprovalURLFollowsRequest(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.ConsoleURL = func() string { return "http://127.0.0.1:7878" }
	if got := s.ApprovalURL(context.Background(), "apr-1"); got != "http://127.0.0.1:7878/approvals/apr-1" {
		t.Fatalf("without ConsoleURLFor: %q", got)
	}
	s.cfg.ConsoleURLFor = func(context.Context) string { return "https://two.ts.net/" }
	if got := s.ApprovalURL(context.Background(), "apr-1"); got != "https://two.ts.net/approvals/apr-1" {
		t.Fatalf("with ConsoleURLFor: %q", got)
	}
}

func TestHostKeySealing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const key = "tskey-auth-k123-abcdef"
	if err := PutHostKeyValue(ctx, dir, st, HostKeyTailscale, key, KeySourceCLI, time.Now()); err != nil {
		t.Fatal(err)
	}
	k, err := st.GetHostKey(ctx, HostKeyTailscale)
	if err != nil || strings.Contains(string(k.Ciphertext), "tskey") {
		t.Fatalf("stored %+v %v", k, err)
	}
	if v, _, err := StoredHostKey(ctx, dir, st, HostKeyTailscale); err != nil || v != key {
		t.Fatalf("StoredHostKey = %q %v", v, err)
	}
	// A value cannot be moved to another id.
	k.ID = HostKeyZrok
	if _, err := OpenHostKey(dir, k); err == nil {
		t.Fatal("a sealed value opened under another id")
	}
	// Another secret.key cannot open it.
	if _, err := OpenHostKey(t.TempDir(), k); err == nil {
		t.Fatal("opened without secret.key")
	}
	if v, _, err := StoredHostKey(ctx, dir, st, HostKeyNetbird); err != nil || v != "" {
		t.Fatalf("missing key = %q %v", v, err)
	}
	// secret.key gone with a stored key: refuse, as for flat secrets.
	if err := CheckSecretKey(ctx, t.TempDir(), st); !errors.Is(err, ErrSecretKeyMissing) || !strings.Contains(err.Error(), "flats key rm") {
		t.Fatalf("CheckSecretKey = %v", err)
	}
	// A connection record without a value does not count.
	if err := st.DeleteHostKey(ctx, HostKeyTailscale); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHostKey(ctx, store.HostKey{ID: HostKeyZrok, Source: KeySourceConsole, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := CheckSecretKey(ctx, t.TempDir(), st); err != nil {
		t.Fatalf("connection record: %v", err)
	}
}

func TestCheckHostKeyValue(t *testing.T) {
	for v, ok := range map[string]bool{
		"tskey-auth-abc-def":   true,
		"tskey-client-abc-def": false,
		"tskey-api-abc":        false,
		"something":            false,
		"":                     false,
		"tskey-auth-a b":       false,
	} {
		if err := CheckHostKeyValue(HostKeyTailscale, v); (err == nil) != ok {
			t.Errorf("%q: %v", v, err)
		}
	}
	if err := CheckHostKeyValue(HostKeyZrok, "any-token"); err != nil {
		t.Errorf("zrok token: %v", err)
	}
	if err := CheckHostKeyValue(HostKeyNetbird, strings.Repeat("x", maxHostKeyLen+1)); err == nil {
		t.Error("an oversized key was accepted")
	}
}
