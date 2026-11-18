package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

// A sheet reads a revision blob through a reader that keeps the blob
// leased until it is closed, stops with the context of the read, and
// fails at the end of a blob whose bytes no longer match its hash.
func TestSheetBlobReaderHonorsItsContextAndLease(t *testing.T) {
	s, _ := newTestService(t)
	b := sheetBackend{s: s, slug: "budget"}
	data := bytes.Repeat([]byte("row file "), 100_000)
	hash, err := putBlob(s.docsStoreDir("budget"), data)
	if err != nil {
		t.Fatal(err)
	}
	leased := func() int {
		d := s.designs()
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.leases["budget\x00"+hash]
	}

	r, err := b.OpenBlob(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if leased() != 1 {
		t.Fatalf("an open blob has %d leases", leased())
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the blob read %d bytes: %v", len(got), err)
	}
	if err := r.Close(); err != nil || leased() != 0 {
		t.Fatalf("a closed blob has %d leases: %v", leased(), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r, err = b.OpenBlob(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	piece := make([]byte, 4096)
	if _, err := io.ReadFull(r, piece); err != nil {
		t.Fatal(err)
	}
	cancel()
	if n, err := r.Read(piece); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("a read after the cancel got %d bytes: %v", n, err)
	}
	r.Close()

	p, _ := blobPath(s.docsStoreDir("budget"), hash)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.ToUpper(data), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = b.OpenBlob(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("a blob that no longer matches its hash was read to its end")
	}
	if _, err := b.OpenBlob(context.Background(), "ab"+hash[2:]); err == nil {
		t.Fatal("a blob that is not stored was opened")
	}
}
