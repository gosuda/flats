package store

import (
	"encoding/json"
	"testing"
)

func TestDerivedTypeJSON(t *testing.T) {
	for _, tt := range []struct{ name, raw, want string }{{"legacy", "{}", "flat"}, {"docs", `{"type":"docs"}`, "docs"}} {
		t.Run(tt.name, func(t *testing.T) {
			for _, view := range []any{Version{Manifest: []byte(tt.raw)}, Draft{Manifest: []byte(tt.raw)}} {
				b, err := json.Marshal(view)
				if err != nil {
					t.Fatal(err)
				}
				var out struct {
					Type     string
					Manifest json.RawMessage
				}
				if err := json.Unmarshal(b, &out); err != nil || out.Type != tt.want || string(out.Manifest) != tt.raw {
					t.Fatalf("%s %v", b, err)
				}
			}
		})
	}
}
