package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func spillID(kind Kind, n uint64) ID {
	var b [16]byte
	binary.BigEndian.PutUint64(b[8:], n)
	return ID(kindPrefixes[kind] + idEncoding.EncodeToString(b[:]))
}
func spillItem(id ID, data ItemData) Item {
	return Item{Ref: Ref{ID: id, Inc: 7}, Ver: 7, V: map[GroupKey]Version{}, Data: data}
}

type spillRead func(context.Context, func(*sql.Tx, Sequence) error) error

func spillSource(t *testing.T, count int, text string) (*store, spillRead, ID) {
	t.Helper()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	db := spillID(DatabaseKind, 1)
	prop := spillID(PropertyKind, 1)
	err = s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, item := range []Item{
			spillItem(db, Database{Block: spillID(BlockKind, 1), Title: "Database", PropertyOrder: []ID{prop}, ViewOrder: []ID{}}),
			spillItem(prop, Property{Database: db, Definition: PropertyDef{Name: "Title", Type: TitleType}, OptionOrder: []ID{}}),
			spillItem(spillID(PageKind, 1), Page{Title: "Page", Position: "A"}),
		} {
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		for i := 0; i < count; i++ {
			row := spillItem(spillID(RowKind, uint64(i+1)), Row{Database: db, Position: fmt.Sprintf("%08d", count-i), Cells: map[ID]CellValue{prop: {Type: TitleType, Value: text}}})
			if err := putItem(t.Context(), tx, row); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE meta SET seq='9'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(ctx context.Context, fn func(*sql.Tx, Sequence) error) error {
		return s.transaction(ctx, func(tx *sql.Tx) error { return fn(tx, 9) })
	}
	return s, read, db
}

type spillWirePart struct {
	Seq       Sequence `json:"seq"`
	Window    int      `json:"window"`
	Rows      int      `json:"rows"`
	Final     bool     `json:"final_window"`
	SpillPart int      `json:"spill_part"`
	Part      int      `json:"part"`
	Parts     int      `json:"parts"`
	Data      string   `json:"data"`
}

func spillWindows(t *testing.T, s *Spill) [][]json.RawMessage {
	t.Helper()
	buffers := map[int]*bytes.Buffer{}
	counts := map[int]int{}
	final := false
	for i := 0; i < s.parts; i++ {
		raw, total, err := s.Part(i)
		if err != nil {
			t.Fatal(err)
		}
		if total != s.parts || len(raw) > 512<<10 {
			t.Fatalf("part bound: total=%d bytes=%d", total, len(raw))
		}
		var p spillWirePart
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if p.Seq != 9 || p.SpillPart != i {
			t.Fatalf("part identity: %+v", p)
		}
		if !utf8.ValidString(p.Data) {
			t.Fatal("continuation split a UTF-8 rune")
		}
		if buffers[p.Window] == nil {
			buffers[p.Window] = &bytes.Buffer{}
		}
		if p.Part != counts[p.Window] {
			t.Fatal("window part order")
		}
		counts[p.Window]++
		buffers[p.Window].WriteString(p.Data)
		if p.Part == p.Parts-1 && counts[p.Window] != p.Parts {
			t.Fatal("window part count")
		}
		if p.Final {
			final = true
		}
	}
	if !final {
		t.Fatal("missing scope completion")
	}
	result := make([][]json.RawMessage, len(buffers))
	for window, buffer := range buffers {
		var data struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(buffer.Bytes(), &data); err != nil {
			t.Fatalf("window %d: %v", window, err)
		}
		result[window] = data.Items
	}
	return result
}

func spillPositionOrder(_ Schema, _ ViewDef, rows []RowValues) []ID {
	values := append([]RowValues{}, rows...)
	sort.Slice(values, func(i, j int) bool {
		if values[i].Position != values[j].Position {
			return values[i].Position < values[j].Position
		}
		return values[i].Row < values[j].Row
	})
	ids := make([]ID, len(values))
	for i, r := range values {
		ids[i] = r.Row
	}
	return ids
}

func TestSpillCaptureWindowsAndImmutableParts(t *testing.T) {
	t.Parallel()
	s, read, db := spillSource(t, 1005, "before")
	clock := newSpillClock()
	m := spillManager(t, 1<<30, clock)
	captured, err := m.Capture(t.Context(), read, Scope{Database: db}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Sequence != 9 {
		t.Fatal("capture barrier lost")
	}
	ordered, err := m.Capture(t.Context(), read, Scope{Database: db, View: &ViewDef{}}, spillPositionOrder)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		return putItem(t.Context(), tx, spillItem(spillID(RowKind, 1005), Row{Database: db, Position: "Z", Cells: map[ID]CellValue{spillID(PropertyKind, 1): {Type: TitleType, Value: "after"}}}))
	}); err != nil {
		t.Fatal(err)
	}
	windows := spillWindows(t, captured)
	sortedWindows := spillWindows(t, ordered)
	if len(sortedWindows) != len(windows) {
		t.Fatal("sorted window count differs")
	}
	for i, window := range windows {
		if len(window) != len(sortedWindows[i]) {
			t.Fatal("sorted window size differs")
		}
		for j, raw := range window {
			if !bytes.Equal(raw, sortedWindows[i][j]) {
				t.Fatal("disk rank/prefix index changed sorted content")
			}
		}
	}
	if len(windows) != 3 || len(windows[0]) != 2 || len(windows[1]) != 1000 || len(windows[2]) != 5 {
		t.Fatalf("window sizes %d/%d/%d", len(windows[0]), len(windows[1]), len(windows[2]))
	}
	var positions []string
	for _, window := range windows[1:] {
		for _, raw := range window {
			item, err := DecodeItem(raw)
			if err != nil {
				t.Fatal(err)
			}
			r := item.Data.(Row)
			positions = append(positions, r.Position)
			if r.Cells[spillID(PropertyKind, 1)].Value != "before" {
				t.Fatal("snapshot mixed states")
			}
		}
	}
	if !sort.StringsAreSorted(positions) {
		t.Fatal("manual order not respected")
	}
	first, _, err := captured.Part(0)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := captured.Part(0)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatal("replayed part changed")
	}
}

func TestSpillViewOrderAndPublicContinuations(t *testing.T) {
	_, read, db := spillSource(t, 7, strings.Repeat("한😀\"\n", 3000))
	m := spillManager(t, 1<<30, newSpillClock())
	view := ViewDef{}
	calls := 0
	order := func(_ Schema, _ ViewDef, rows []RowValues) []ID {
		calls++
		filtered := append([]RowValues{}, rows...)
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].Position > filtered[j].Position })
		var ids []ID
		for _, r := range filtered {
			if r.Position != "00000004" {
				ids = append(ids, r.Row)
			}
		}
		return ids
	}
	captured, err := m.Capture(t.Context(), read, Scope{Database: db, View: &view, Public: true}, order)
	if err != nil {
		t.Fatal(err)
	}
	windows := spillWindows(t, captured)
	if calls < 7 || len(windows) != 2 || len(windows[1]) != 6 {
		t.Fatalf("query not applied: calls=%d windows=%d", calls, len(windows))
	}
	previous := "Z"
	for _, raw := range windows[1] {
		var row struct {
			Position string `json:"position"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if row.Position >= previous {
			t.Fatal("view order not respected")
		}
		previous = row.Position
	}
	for _, name := range []string{"data", "records", "parts"} {
		raw, err := os.ReadFile(filepath.Join(captured.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{`"inc":`, `"ver":`, `"v":`, `"authors":`, `"store":`} {
			if bytes.Contains(raw, []byte(private)) {
				t.Fatalf("public %s leaked %s", name, private)
			}
		}
	}
}

func TestSpillViewKeysAndStableTies(t *testing.T) {
	source, read, db := spillSource(t, 8, "")
	property := spillID(PropertyKind, 1)
	if err := source.transaction(t.Context(), func(tx *sql.Tx) error {
		for i := 1; i <= 8; i++ {
			value := CellValue{Type: TitleType, Value: fmt.Sprint(i % 2)}
			raw, err := EncodeCellValue(value)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `UPDATE cells SET value=? WHERE row_id=?`, string(raw), spillID(RowKind, uint64(i))); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view := ViewDef{Sort: []Sort{{Property: Target{Ref: &Ref{ID: property, Inc: 7}}, Direction: "asc"}}}
	order := func(_ Schema, _ ViewDef, rows []RowValues) []ID {
		values := append([]RowValues{}, rows...)
		sort.SliceStable(values, func(i, j int) bool {
			return values[i].Cells[property].Value.(string) < values[j].Cells[property].Value.(string)
		})
		ids := make([]ID, len(values))
		for i, r := range values {
			ids[i] = r.Row
		}
		return ids
	}
	m := spillManager(t, 1<<30, newSpillClock())
	captured, err := m.Capture(t.Context(), read, Scope{Database: db, View: &view}, order)
	if err != nil {
		t.Fatal(err)
	}
	windows := spillWindows(t, captured)
	var last, lastPosition string
	for _, raw := range windows[1] {
		item, err := DecodeItem(raw)
		if err != nil {
			t.Fatal(err)
		}
		row := item.Data.(Row)
		key := row.Cells[property].Value.(string)
		if key < last || key == last && row.Position < lastPosition {
			t.Fatal("sort keys or stable manual ties lost")
		}
		last = key
		lastPosition = row.Position
	}
}

func TestSpillForwardProgressAndReplayExpiry(t *testing.T) {
	for _, tc := range []struct {
		name               string
		advancing, fitting bool
	}{{"replaying", false, false}, {"slow_forward", true, false}, {"fitting_replay", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, read, db := spillSource(t, 1, strings.Repeat("x", 600000))
			clock := newSpillClock()
			budget := int64(1)
			if tc.fitting {
				budget = 1 << 30
			}
			m := spillManager(t, budget, clock)
			s, err := m.Capture(t.Context(), read, Scope{Database: db}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Part(0); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if !tc.fitting {
				go func() { _, err := m.acquire(ctx, 2, Scope{Tree: true}); done <- err }()
				spillPending(t, m, 1)
			}
			i := 0
			for step := 0; step < 8; step++ {
				stepTime := 9 * time.Second
				if !tc.advancing && !tc.fitting && step > 0 {
					stepTime = time.Second
				}
				clock.advance(stepTime)
				if tc.advancing {
					i++
				}
				_, _, err = s.Part(i)
				if err != nil {
					break
				}
			}
			if tc.fitting || tc.advancing {
				if err != nil {
					t.Fatalf("reader with progress expired: %v", err)
				}
			} else {
				if !errors.Is(err, errSpillExpired) {
					t.Fatalf("replay retained slot: %v", err)
				}
				select {
				case e := <-done:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("waiter not admitted")
				}
			}
			if !tc.fitting && tc.advancing {
				for i++; i < s.parts; i++ {
					if _, _, err := s.Part(i); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case e := <-done:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("completion did not release slot")
				}
			}
			if tc.fitting {
				clock.advance(60 * time.Second)
				if _, ok := m.Lookup(s.Cursor()); ok {
					t.Fatal("idle fitting spill survived")
				}
			}
		})
	}
}

func TestSpillCompletedOverRetention(t *testing.T) {
	for _, waiter := range []bool{false, true} {
		t.Run(fmt.Sprint("waiter_", waiter), func(t *testing.T) {
			_, read, _ := spillSource(t, 0, "")
			clock := newSpillClock()
			m := spillManager(t, 1, clock)
			s, err := m.Capture(t.Context(), read, Scope{Tree: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Part(0); err != nil {
				t.Fatal(err)
			}
			if waiter {
				done := make(chan error, 1)
				go func() { _, err := m.acquire(t.Context(), 2, Scope{Database: spillID(DatabaseKind, 1)}); done <- err }()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("completed oversize spill did not release immediately")
				}
				if _, ok := m.Lookup(s.Cursor()); ok {
					t.Fatal("completed contended spill retained")
				}
			} else {
				clock.advance(50 * time.Second)
				if _, _, err := s.Part(0); err != nil {
					t.Fatal(err)
				}
				clock.advance(10 * time.Second)
				if _, ok := m.Lookup(s.Cursor()); ok {
					t.Fatal("completed oversize replay renewed idle clock")
				}
			}
		})
	}
}

func TestSpillCaptureBelowFloorClosesRead(t *testing.T) {
	_, read, db := spillSource(t, 1, strings.Repeat("x", 400000))
	clock := newSpillClock()
	m := spillManager(t, 1, clock)
	m.wrapWriter = func(out io.Writer) io.Writer { return spillSlowWriter{out: out, clock: clock} }
	if _, err := m.Capture(t.Context(), read, Scope{Database: db}, nil); !errors.Is(err, errSpillUnavailable) {
		t.Fatalf("below-floor capture: %v", err)
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch after floor cancellation: %v %v", entries, err)
	}
	if err := read(t.Context(), func(*sql.Tx, Sequence) error { return nil }); err != nil {
		t.Fatalf("cancelled read retained transaction: %v", err)
	}
}

type spillSlowWriter struct {
	out   io.Writer
	clock *spillClock
}

func (w spillSlowWriter) Write(p []byte) (int, error) {
	w.clock.advance(time.Duration(len(p)) * time.Second / (32 << 10))
	return w.out.Write(p)
}

func TestSpillCancelBeforeAdmission(t *testing.T) {
	m := spillManager(t, 1, newSpillClock())
	entered := make(chan struct{})
	done := make(chan error, 1)
	read := func(ctx context.Context, _ func(*sql.Tx, Sequence) error) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	go func() { _, err := m.Capture(t.Context(), read, Scope{Tree: true, Public: true}, nil); done <- err }()
	<-entered
	m.CancelScope(func(s Scope) bool { return s.Public })
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("preflight cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("preflight not cancelled")
	}
}

func TestSpillCancelledCaptureClosesTransaction(t *testing.T) {
	_, read, db := spillSource(t, 2, "text")
	m := spillManager(t, 1, newSpillClock())
	entered := make(chan struct{})
	released := make(chan struct{})
	calls := 0
	blocked := func(ctx context.Context, fn func(*sql.Tx, Sequence) error) error {
		calls++
		if calls == 1 {
			return read(ctx, fn)
		}
		return read(ctx, func(tx *sql.Tx, seq Sequence) error { close(entered); <-ctx.Done(); close(released); return ctx.Err() })
	}
	done := make(chan error, 1)
	go func() { _, err := m.Capture(t.Context(), blocked, Scope{Database: db, Public: true}, nil); done <- err }()
	<-entered
	m.CancelScope(func(s Scope) bool { return s.Public })
	select {
	case err := <-done:
		if !errors.Is(err, errSpillUnavailable) {
			t.Fatalf("cancelled capture: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capture not cancelled")
	}
	<-released
	entries, err := os.ReadDir(m.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled capture files: %v %v", entries, err)
	}
	if err := read(t.Context(), func(*sql.Tx, Sequence) error { return nil }); err != nil {
		t.Fatalf("transaction not closed: %v", err)
	}
}
