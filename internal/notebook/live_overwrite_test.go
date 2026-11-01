package notebook

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/gosuda/flats/internal/wsconn"
)

type liveOverwriteDelta struct {
	Seq       Sequence          `json:"seq"`
	Changes   []json.RawMessage `json:"changes"`
	Overwrote []struct {
		Block   Ref     `json:"block"`
		Version Version `json:"version"`
		Base    Version `json:"base"`
		Source  struct {
			Session string   `json:"session_id"`
			N       Sequence `json:"n"`
		} `json:"source"`
	} `json:"overwrote"`
}

func liveOverwriteConnect(t *testing.T, f writerFixture, session Session) *wsconn.Conn {
	t.Helper()
	ws, _ := liveDial(t, f.in, "private", nil)
	liveHelloSend(t, ws, session.Token, f.page.ID)
	liveUntil(t, ws, "welcome")
	liveUntil(t, ws, "snapshot")
	return ws
}

func liveOverwriteReceiveDelta(t *testing.T, ws *wsconn.Conn) liveOverwriteDelta {
	t.Helper()
	frame := liveUntil(t, ws, "delta")
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var delta liveOverwriteDelta
	if err := json.Unmarshal(raw, &delta); err != nil {
		t.Fatal(err)
	}
	return delta
}

func liveOverwriteAck(t *testing.T, ws *wsconn.Conn, n, seq Sequence) Ack {
	t.Helper()
	frame := liveUntil(t, ws, "ack")
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var ack Ack
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.N != n || ack.Seq != seq || ack.Summary.Applied != 1 || ack.Summary.Refused != 0 || ack.Summary.Ignored != 0 {
		t.Fatalf("action was not applied: %+v", ack)
	}
	return ack
}

func liveOverwriteCreate(t *testing.T, f writerFixture, ws *wsconn.Conn, text string) Ref {
	t.Helper()
	id, err := NewID(BlockKind)
	if err != nil {
		t.Fatal(err)
	}
	closed := true
	sendLiveAction(t, ws, Action{N: 1, ActionID: "create", Close: &closed, Ops: []Operation{
		CreateBlockOp{CreateFields: CreateFields{Ref: "block", ProposedID: &id}, Place: Placement{Page: editTarget(f.page)}, Content: editText(Paragraph, text)},
	}})
	return Ref{ID: id, Inc: 0}
}

func liveOverwriteSet(block Ref, n Sequence, text string, base Version) Action {
	closed := true
	return Action{N: n, ActionID: fmt.Sprintf("content-%d", n), Close: &closed, Ops: []Operation{
		SetBlockOp{Block: editTarget(block), Content: editText(Paragraph, text), BaseV: &base},
	}}
}

func liveOverwriteContent(t *testing.T, delta liveOverwriteDelta, id ID, text string) Ref {
	t.Helper()
	for _, raw := range delta.Changes {
		item, err := DecodeItem(raw)
		if err != nil {
			t.Fatal(err)
		}
		if item.Ref.ID != id {
			continue
		}
		block, ok := item.Data.(Block)
		if !ok {
			t.Fatalf("expected block: %s", raw)
		}
		got, err := json.Marshal(block.Content)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(editText(Paragraph, text))
		if err != nil {
			t.Fatal(err)
		}
		version, err := item.GroupVersion(ContentGroup)
		if err != nil || string(got) != string(want) || version != Version(delta.Seq) {
			t.Fatalf("delta %d did not save %q: %s (version error: %v)", delta.Seq, text, raw, err)
		}
		return item.Ref
	}
	t.Fatalf("delta %d omitted block %s", delta.Seq, id)
	return Ref{}
}

func liveOverwriteNone(t *testing.T, delta liveOverwriteDelta) {
	t.Helper()
	if len(delta.Overwrote) != 0 {
		t.Errorf("delta %d reported own earlier write as overwritten: %+v", delta.Seq, delta.Overwrote)
	}
}

func TestLiveOwnEarlierBlockWritesDoNotOverwrite(t *testing.T) {
	f := newWriterFixture(t)
	ws := liveOverwriteConnect(t, f, f.session)
	alias := liveOverwriteCreate(t, f, ws, "")
	// The browser sends this before creation is acknowledged, using its zero alias and base.
	sendLiveAction(t, ws, liveOverwriteSet(alias, 2, "Line A", 0))
	created := liveOverwriteReceiveDelta(t, ws)
	block := liveOverwriteContent(t, created, alias.ID, "")
	liveOverwriteNone(t, created)
	liveOverwriteAck(t, ws, 1, created.Seq)
	first := liveOverwriteReceiveDelta(t, ws)
	liveOverwriteContent(t, first, block.ID, "Line A")
	liveOverwriteNone(t, first)
	liveOverwriteAck(t, ws, 2, first.Seq)

	sendLiveAction(t, ws, liveOverwriteSet(block, 3, "Line A continued", Version(created.Seq)))
	later := liveOverwriteReceiveDelta(t, ws)
	liveOverwriteContent(t, later, block.ID, "Line A continued")
	liveOverwriteNone(t, later)
	liveOverwriteAck(t, ws, 3, later.Seq)
}

func TestLiveOwnEarlierBlockWritesAfterReconnectAndResend(t *testing.T) {
	f := newWriterFixture(t)
	ws := liveOverwriteConnect(t, f, f.session)
	alias := liveOverwriteCreate(t, f, ws, "")
	created := liveOverwriteReceiveDelta(t, ws)
	block := liveOverwriteContent(t, created, alias.ID, "")
	liveOverwriteAck(t, ws, 1, created.Seq)
	action := liveOverwriteSet(alias, 2, "Before reconnect", 0)
	sendLiveAction(t, ws, action)
	first := liveOverwriteReceiveDelta(t, ws)
	liveOverwriteContent(t, first, block.ID, "Before reconnect")
	liveOverwriteNone(t, first)
	ack := liveOverwriteAck(t, ws, 2, first.Seq)
	ws.Close(1000, "")
	<-ws.Done()

	reconnected, _ := liveDial(t, f.in, "private", nil)
	liveSend(t, reconnected, map[string]any{"type": "hello", "version": 1, "session": f.session.Token, "page": f.page.ID, "pending": map[string]any{"count": 1, "first_n": "2"}})
	liveUntil(t, reconnected, "welcome")
	before := writerCount(t, f.in, "deltas")
	sendLiveAction(t, reconnected, action)
	replayed := liveOverwriteAck(t, reconnected, 2, first.Seq)
	if !reflect.DeepEqual(ack, replayed) || writerCount(t, f.in, "deltas") != before || writerCount(t, f.in, "receipts") != 2 {
		t.Fatalf("resend did not replay the original receipt: %+v / %+v", ack, replayed)
	}

	sendLiveAction(t, reconnected, liveOverwriteSet(alias, 3, "After reconnect", 0))
	later := liveOverwriteReceiveDelta(t, reconnected)
	liveOverwriteContent(t, later, block.ID, "After reconnect")
	liveOverwriteNone(t, later)
	liveOverwriteAck(t, reconnected, 3, later.Seq)
}

func TestLiveOverwriteAttributionKeepsOtherWriters(t *testing.T) {
	for _, writer := range []string{"same_person_other_session", "agent"} {
		t.Run(writer, func(t *testing.T) {
			f := newWriterFixture(t)
			ws := liveOverwriteConnect(t, f, f.session)
			alias := liveOverwriteCreate(t, f, ws, "Initial")
			created := liveOverwriteReceiveDelta(t, ws)
			block := liveOverwriteContent(t, created, alias.ID, "Initial")
			liveOverwriteAck(t, ws, 1, created.Seq)
			base := Version(created.Seq)
			sendLiveAction(t, ws, liveOverwriteSet(block, 2, "My text", base))
			mine := liveOverwriteReceiveDelta(t, ws)
			liveOverwriteContent(t, mine, block.ID, "My text")
			liveOverwriteNone(t, mine)
			liveOverwriteAck(t, ws, 2, mine.Seq)

			var other Session
			if writer == "same_person_other_session" {
				var err error
				other, err = f.in.NewSession(t.Context(), "person", "Person", true)
				if err != nil {
					t.Fatal(err)
				}
				if other.ID == f.session.ID {
					t.Fatal("distinct tab reused the session identity")
				}
				second := liveOverwriteConnect(t, f, other)
				sendLiveAction(t, second, liveOverwriteSet(block, 1, "Their text", base))
				otherDelta := liveOverwriteReceiveDelta(t, second)
				liveOverwriteAck(t, second, 1, otherDelta.Seq)
			} else {
				_, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "agent-content", Ops: []Operation{
					SetBlockOp{Block: editTarget(block), Content: editText(Paragraph, "Their text"), BaseV: &base},
				}}, 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			displaced := liveOverwriteReceiveDelta(t, ws)
			liveOverwriteContent(t, displaced, block.ID, "Their text")
			if len(displaced.Overwrote) != 1 {
				t.Fatalf("other writer lost overwrite attribution: %+v", displaced)
			}
			o := displaced.Overwrote[0]
			if o.Block != block || o.Version != Version(mine.Seq) || o.Base != base || o.Source.Session != f.session.ID || o.Source.N != 2 {
				t.Fatalf("wrong displaced receipt: %+v", o)
			}

			if writer == "same_person_other_session" {
				sendLiveAction(t, ws, liveOverwriteSet(block, 3, "My later text", base))
				later := liveOverwriteReceiveDelta(t, ws)
				liveOverwriteContent(t, later, block.ID, "My later text")
				liveOverwriteAck(t, ws, 3, later.Seq)
				if len(later.Overwrote) != 1 {
					t.Fatalf("returning tab lost overwrite attribution: %+v", later)
				}
				o = later.Overwrote[0]
				if o.Source.Session != other.ID || o.Source.N != 1 || o.Version != Version(displaced.Seq) || o.Block != block || o.Base != base {
					t.Fatalf("wrong other-tab receipt: %+v", o)
				}
			}
		})
	}
}

func TestLiveOwnEarlierBlockWritesAfterUndoRedo(t *testing.T) {
	f := newWriterFixture(t)
	ws := liveOverwriteConnect(t, f, f.session)
	alias := liveOverwriteCreate(t, f, ws, "Initial")
	created := liveOverwriteReceiveDelta(t, ws)
	block := liveOverwriteContent(t, created, alias.ID, "Initial")
	liveOverwriteAck(t, ws, 1, created.Seq)
	sendLiveAction(t, ws, liveOverwriteSet(block, 2, "Edited", Version(created.Seq)))
	edited := liveOverwriteReceiveDelta(t, ws)
	liveOverwriteContent(t, edited, block.ID, "Edited")
	liveOverwriteNone(t, edited)
	liveOverwriteAck(t, ws, 2, edited.Seq)

	for _, control := range []struct {
		n    uint64
		name string
		text string
	}{{3, "undo", "Initial"}, {4, "redo", "Edited"}} {
		sendLiveAction(t, ws, undoBrowserAction(t, control.n, control.name))
		delta := liveOverwriteReceiveDelta(t, ws)
		liveOverwriteContent(t, delta, block.ID, control.text)
		liveOverwriteNone(t, delta)
		ack := liveOverwriteAck(t, ws, Sequence(control.n), delta.Seq)
		undoBrowserSummary(t, ack, 1, 0, 0, 0)
	}
	sendLiveAction(t, ws, liveOverwriteSet(block, 5, "After redo", Version(edited.Seq)))
	later := liveOverwriteReceiveDelta(t, ws)
	liveOverwriteContent(t, later, block.ID, "After redo")
	liveOverwriteNone(t, later)
	liveOverwriteAck(t, ws, 5, later.Seq)
}
