package mcpx

import (
	"context"
	"strings"
	"testing"
)

func TestDesignFlatText(t *testing.T) {
	e := newEnv(t)
	text, failed := call(t, e.local, "save_design", map[string]any{"slug": "kit", "title": "Launch kit"}, nil)
	if failed || !strings.Contains(text, "Created design kit") {
		t.Fatalf("save_design: %s", text)
	}
	text, failed = call(t, e.local, "get_flat", map[string]any{"slug": "kit"}, nil)
	if failed || !strings.Contains(text, "design, running") {
		t.Fatalf("get_flat must describe a running design: %s", text)
	}
	for _, wrong := range []string{"not deployed", "version(s)", "create_version"} {
		if strings.Contains(text, wrong) {
			t.Errorf("get_flat describes a design with website lifecycle text %q: %s", wrong, text)
		}
	}
	text, failed = call(t, e.local, "list_flats", map[string]any{}, nil)
	if failed || !strings.Contains(text, "kit (Launch kit): private, design, running") {
		t.Fatalf("list_flats: %s", text)
	}
	events, err := e.svc.Events(context.Background(), "kit", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		for _, wrong := range []string{"draft revision", "document saved", "health check"} {
			if strings.Contains(ev.Message, wrong) {
				t.Errorf("creating a design logged its internal shell: %q", ev.Message)
			}
		}
	}
}
