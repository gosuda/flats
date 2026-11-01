package mcpx

import (
	"fmt"
	"strings"
	"testing"
)

func TestPublicInstancePublicationDescriptions(t *testing.T) {
	for _, autopilot := range []bool{false, true} {
		t.Run(fmt.Sprintf("autopilot=%t", autopilot), func(t *testing.T) {
			e := newEnv(t)
			if autopilot {
				setAutopilot(t, e, "")
			}
			cs := connect(t, e.localURL)
			off, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			publicInstanceOn(t, e)
			on, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range on.Tools {
				switch tool.Name {
				case "create_flat", "save_draft", "save_draft_from_dir", "save_page":
					for _, want := range []string{"each save", "created by this mode", "without approval", "before the mode keep approved", "starter is not published"} {
						if !strings.Contains(strings.ToLower(tool.Description), want) {
							t.Errorf("%s lacks %q: %s", tool.Name, want, tool.Description)
						}
					}
					if strings.Contains(tool.Description, "Saving never creates a version or deployment") {
						t.Errorf("%s contradicts publication on save", tool.Name)
					}
				}
			}
			if _, err := e.svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
				t.Fatal(err)
			}
			restored, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(restored.Tools) != len(off.Tools) {
				t.Fatal("mode-off tool count changed")
			}
			for i, tool := range restored.Tools {
				if tool.Name != off.Tools[i].Name || tool.Description != off.Tools[i].Description {
					t.Errorf("mode-off description changed: %s", tool.Name)
				}
			}
		})
	}
}
