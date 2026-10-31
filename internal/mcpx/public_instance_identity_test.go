package mcpx

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestPublicInstanceOffBaseline(t *testing.T) {
	e := newEnv(t)
	for _, ap := range []bool{false, true} {
		t.Run(fmt.Sprintf("autopilot=%v", ap), func(t *testing.T) {
			if ap {
				setAutopilot(t, e, "")
			}
			cs := connect(t, e.localURL)
			texts := map[string]string{"instructions": cs.InitializeResult().Instructions}
			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(tools)
			if err != nil {
				t.Fatal(err)
			}
			texts["tools/list"] = string(b)
			ids := guideItems()
			if ap {
				ids = autopilotGuideItems()
			}
			for _, id := range ids {
				var out GuideOut
				_, failed := call(t, cs, "guide", map[string]any{"items": []string{id}}, &out)
				if failed {
					t.Fatal(id)
				}
				b, err := json.Marshal(out)
				if err != nil {
					t.Fatal(err)
				}
				texts[id] = string(b)
			}
			hashes := map[string]string{}
			for k, v := range texts {
				hashes[k] = fmt.Sprintf("%x", sha256.Sum256([]byte(v)))
			}
			b, err = json.MarshalIndent(hashes, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("testdata/public-instance-off-%v.json", ap)
			if os.Getenv("UPDATE_PUBLIC_INSTANCE_BASELINE") == "1" {
				if err := os.MkdirAll("testdata", 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(b, '\n'), 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(append(b, '\n')) {
				t.Fatalf("off surface changed from unit base 96dca11:\n%s", b)
			}
		})
	}
}
