package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type readerHeapWriter struct {
	out        io.Writer
	base, peak uint64
}

func (w *readerHeapWriter) Write(p []byte) (int, error) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if stats.HeapAlloc > w.base {
		w.peak = max(w.peak, stats.HeapAlloc-w.base)
	}
	return w.out.Write(p)
}

func TestReaderRetainedMemoryAndContinuations(t *testing.T) {
	sizes := []int{1, 2, 4}
	if os.Getenv("NOTEBOOK_LARGE_MEMORY_TESTS") == "1" {
		sizes = append(sizes, 8, 32)
	}
	for _, kind := range []string{"row", "block"} {
		for _, size := range sizes {
			t.Run(fmt.Sprintf("%s/%dMiB", kind, size), func(t *testing.T) {
				s, read, db := spillSource(t, 0, "")
				page := spillID(PageKind, 1)
				id := spillID(BlockKind, 1)
				scope := Scope{Page: page}
				item := spillItem(id, Block{Page: page, Position: "A", Content: Content{Kind: Code, Code: ""}})
				if kind == "row" {
					id = spillID(RowKind, 1)
					scope = Scope{Database: db}
					item = spillItem(id, Row{Database: db, Position: "A", Cells: map[ID]CellValue{spillID(PropertyKind, 1): {Type: TitleType, Value: ""}}})
				}
				if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
					if err := putItem(t.Context(), tx, item); err != nil {
						return err
					}
					if kind == "row" {
						_, err := tx.ExecContext(t.Context(), `UPDATE cells SET value=json_set(value,'$.value',?) WHERE row_id=?`, strings.Repeat("x", size<<20), id)
						return err
					}
					_, err := tx.ExecContext(t.Context(), `UPDATE items SET data=json_set(data,'$.content.text',?) WHERE id=?`, strings.Repeat("x", size<<20), id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for _, public := range []bool{false, true} {
					t.Run(fmt.Sprintf("public=%t", public), func(t *testing.T) {
						scope := scope
						scope.Public = public
						m := spillManager(t, 1<<30, newSpillClock())
						var heap readerHeapWriter
						m.wrapWriter = func(out io.Writer) io.Writer {
							runtime.GC()
							var stats runtime.MemStats
							runtime.ReadMemStats(&stats)
							heap = readerHeapWriter{out: out, base: stats.HeapAlloc}
							return &heap
						}
						captured, err := m.Capture(t.Context(), read, scope, nil)
						if err != nil {
							t.Fatal(err)
						}
						t.Logf("peak capture Go heap above baseline: %d bytes", heap.peak)
						if heap.peak > 6<<20 {
							t.Errorf("capture heap grows with retained record: %d bytes", heap.peak)
						}
						text := strings.Repeat("x", size<<20)
						windows := spillWindows(t, captured)
						found := false
						for _, window := range windows {
							for _, raw := range window {
								var got struct {
									ID      ID               `json:"id"`
									Ref     Ref              `json:"ref"`
									Content Content          `json:"content"`
									Cells   map[ID]CellValue `json:"cells"`
								}
								if err := json.Unmarshal(raw, &got); err != nil {
									t.Fatal(err)
								}
								if got.ID != id && got.Ref.ID != id {
									continue
								}
								found = true
								value := got.Content.Code
								if kind == "row" {
									value = got.Cells[spillID(PropertyKind, 1)].Value.(string)
								}
								if value != text {
									t.Fatal("continuations changed retained value")
								}
								if public {
									var fields map[string]any
									if err := json.Unmarshal(raw, &fields); err != nil {
										t.Fatal(err)
									}
									spillAssertPublicFields(t, fields)
								}
							}
						}
						if !found {
							t.Fatal("retained item missing")
						}
						if err := captured.Close(); err != nil {
							t.Fatal(err)
						}
					})
				}
			})
		}
	}
}

func TestReaderEscapedContinuations(t *testing.T) {
	text := strings.Repeat("a\"\\\n<&한😀", 5000)
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprintf("public=%t", public), func(t *testing.T) {
			source, read, db := spillSource(t, 1, text)
			page, block := spillID(PageKind, 1), spillID(BlockKind, 1)
			if err := source.transaction(t.Context(), func(tx *sql.Tx) error {
				return putItem(t.Context(), tx, spillItem(block, Block{Page: page, Position: "A", Content: Content{Kind: Code, Code: text}}))
			}); err != nil {
				t.Fatal(err)
			}
			m := spillManager(t, 1<<30, newSpillClock())
			view := ViewDef{Sort: []Sort{{Property: Target{Ref: &Ref{ID: spillID(PropertyKind, 1), Inc: 7}}, Direction: "asc"}}}
			for _, scope := range []Scope{{Page: page, Public: public}, {Database: db, View: &view, Public: public}} {
				captured, err := m.Capture(t.Context(), read, scope, spillPositionOrder)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, window := range spillWindows(t, captured) {
					for _, raw := range window {
						var got struct {
							Kind    Kind             `json:"kind"`
							Content Content          `json:"content"`
							Cells   map[ID]CellValue `json:"cells"`
						}
						if err := json.Unmarshal(raw, &got); err != nil {
							t.Fatal(err)
						}
						switch got.Kind {
						case BlockKind:
							found = true
							if got.Content.Code != text {
								t.Fatal("block continuation changed escaped text")
							}
						case RowKind:
							found = true
							if got.Cells[spillID(PropertyKind, 1)].Value != text {
								t.Fatal("row continuation changed escaped text")
							}
						}
					}
				}
				if !found {
					t.Fatal("retained item absent")
				}
				if err := captured.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReaderProgressingCaptureHasNoDeadline(t *testing.T) {
	_, read, db := spillSource(t, 12, strings.Repeat("x", 128<<10))
	clock := newSpillClock()
	m := spillManager(t, 1<<30, clock)
	start := clock.now()
	m.wrapWriter = func(out io.Writer) io.Writer {
		return readerProgressWriter{out: out, clock: clock}
	}
	captured, err := m.Capture(t.Context(), func(ctx context.Context, fn func(*sql.Tx, Sequence) error) error {
		if _, ok := ctx.Deadline(); ok {
			t.Error("capture imposed a whole-read deadline")
		}
		return read(ctx, fn)
	}, Scope{Database: db}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	if clock.now().Sub(start) <= spillInterval {
		t.Fatal("capture did not cross a progress window")
	}
}

type readerProgressWriter struct {
	out   io.Writer
	clock *spillClock
}

func (w readerProgressWriter) Write(p []byte) (int, error) {
	w.clock.advance(time.Duration(len(p)) * time.Second / (128 << 10))
	return w.out.Write(p)
}

func TestReaderAdmissionSharesProgressWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, read, db := spillSource(t, 1, "text")
		m := NewSpillManager(t.TempDir(), 1<<30, nil)
		t.Cleanup(func() { m.CancelScope(func(Scope) bool { return true }) })
		entered := make(chan struct{})
		done := make(chan error, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			calls := 0
			_, err := m.Capture(ctx, func(ctx context.Context, fn func(*sql.Tx, Sequence) error) error {
				calls++
				return read(ctx, func(tx *sql.Tx, seq Sequence) error {
					if calls == 1 {
						err := fn(tx, seq)
						time.Sleep(9 * time.Second)
						return err
					}
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				})
			}, Scope{Database: db}, nil)
			done <- err
		}()
		<-entered
		synctest.Sleep(time.Second)
		select {
		case err := <-done:
			if !errors.Is(err, errSpillUnavailable) {
				t.Fatalf("stalled capture after admission: %v", err)
			}
		default:
			cancel()
			<-done
			t.Fatal("admission reset the capture progress window")
		}
	})
}

func TestReaderFirstEstimationStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, read, db := spillSource(t, 1, "text")
		m := NewSpillManager(t.TempDir(), 1<<30, nil)
		t.Cleanup(func() { m.CancelScope(func(Scope) bool { return true }) })
		entered := make(chan struct{})
		done := make(chan error, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			_, err := m.Capture(ctx, func(ctx context.Context, _ func(*sql.Tx, Sequence) error) error {
				return read(ctx, func(tx *sql.Tx, _ Sequence) error {
					var n int
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&n); err != nil {
						return err
					}
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				})
			}, Scope{Database: db}, nil)
			done <- err
		}()
		<-entered
		synctest.Sleep(spillInterval)
		select {
		case err := <-done:
			if !errors.Is(err, errSpillUnavailable) {
				t.Fatalf("stalled estimate: %v", err)
			}
		case <-time.After(time.Second):
			cancel()
			<-done
			t.Fatal("first estimation transaction survived its failing progress window")
		}
		releasedCtx, releaseCancel := context.WithTimeout(t.Context(), time.Second)
		defer releaseCancel()
		if err := read(releasedCtx, func(*sql.Tx, Sequence) error { return nil }); err != nil {
			t.Fatalf("estimate did not release the source transaction: %v", err)
		}
		if s.db.Stats().InUse != 0 {
			t.Fatal("estimate retained its transaction")
		}
	})
}
