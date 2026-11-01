package mcpx

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublishedSaveAfterRequestModeChanges(t *testing.T) {
	for _, name := range []string{"save_draft", "save_draft_from_dir", "save_page"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			publicInstanceOn(t, e)
			if text, failed := call(t, e.local, name, publishedSaveArgs(t, name, "<title>First</title>"), nil); failed {
				t.Fatal(text)
			}
			if _, err := e.svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
				t.Fatal(err)
			}
			p := newPausedClient(t, e)
			done := p.call(name, publishedSaveArgs(t, name, "<title>Second</title>"))
			_, err := e.svc.SetPublicInstance(t.Context(), true, "portal", "test")
			close(p.resume)
			res := <-done
			if err != nil {
				t.Fatal(err)
			}
			text := resultText(res)
			if res.IsError {
				t.Fatalf("publication returned an error: %s", text)
			}
			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var out SaveOut
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if out.Version != 2 || out.Draft.Revision != 2 || out.Flat == nil || out.Flat.LiveVersion != 2 || out.Flat.PublicURL == "" || out.Flat.PublicNotice == "" {
				t.Fatalf("missing publication receipt: %s", raw)
			}
			for _, want := range []string{"published as version 2", out.Flat.PublicURL, out.Flat.PublicNotice} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q: %s", want, text)
				}
			}
			f, err := e.svc.GetFlat(t.Context(), "published")
			if err != nil || f.LiveVersion != 2 || f.Versions != 2 || f.Draft.Dirty {
				t.Fatalf("publication state: %+v %v", f, err)
			}
		})
	}
}
