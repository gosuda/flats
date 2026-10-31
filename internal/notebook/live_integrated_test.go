package notebook

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestIntegratedNotebookLiveMalformedActionsUseRateBudget(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"action", "action_part"} {
		t.Run(kind, func(t *testing.T) {
			f := newWriterFixture(t)
			ws, _ := liveDial(t, f.in, "private", nil)
			liveHelloSend(t, ws, f.session.Token, f.page.ID)
			liveUntil(t, ws, "welcome")
			for i := 0; i < 65; i++ {
				n := fmt.Sprint(i + 1)
				frame := map[string]any{"type": kind, "n": n, "action_id": "bad", "ops": "invalid"}
				if kind == "action_part" {
					frame = map[string]any{"type": kind, "n": n, "part": -1, "parts": 1, "data": "bad"}
				}
				liveSend(t, ws, frame)
				var response map[string]json.RawMessage
				for {
					response = liveReceive(t, ws)
					if liveType(response) == "reject" {
						break
					}
					if liveType(response) == "fatal" {
						t.Fatalf("unexpected fatal: %s", response)
					}
				}
				var detail struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(response["detail"], &detail); err != nil {
					t.Fatal(err)
				}
				if i < 60 && detail.Reason == "action_rate" {
					t.Fatalf("burst rejected early at %d", i)
				}
				if i >= 60 && (detail.Reason != "action_rate" || string(response["n"]) != fmt.Sprintf("%q", n) || string(response["retry"]) != "true") {
					t.Fatalf("malformed action bypassed rate budget or lost n: %s", response)
				}
			}
			if writerCount(t, f.in, "receipts") != 0 {
				t.Fatal("malformed actions consumed durable action numbers")
			}
			f.clock.Add(1)
			raw, err := EncodeAction(writerAction(f.page, "after refill"))
			if err != nil {
				t.Fatal(err)
			}
			var action map[string]any
			if err := json.Unmarshal(raw, &action); err != nil {
				t.Fatal(err)
			}
			action["type"] = "action"
			liveSend(t, ws, action)
			ack := liveUntil(t, ws, "ack")
			if string(ack["n"]) != `"1"` || writerTitle(t, f.in, f.page) != "after refill" {
				t.Fatalf("rate retry failed: %s", ack)
			}
		})
	}
}
