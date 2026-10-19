package contenttype

import "testing"

func TestFromManifest(t *testing.T) {
	for _, tt := range []struct{ name, raw, want string }{{"legacy", "{}", Flat}, {"missing", "", Flat}, {"flat", `{"type":"flat"}`, Flat}, {"docs", `{"type":"docs"}`, Docs}, {"page", `{"type":"page"}`, Page}, {"design", `{"type":"design"}`, Design}, {"board", `{"type":"board"}`, Board}} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromManifest([]byte(tt.raw)); got != tt.want {
				t.Fatalf("%s", got)
			}
		})
	}
	types := Types()
	if len(types) != 5 || types[3].Type != Design || types[3].Name != "Design" || types[4].Type != Board || types[4].Name != "Board" || !RevisionModel(Design) || !RevisionModel(Docs) || !RevisionModel(Board) || RevisionModel(Page) || types[0].Type != Flat || types[1].Type != Docs || types[1].Name != "Document" || types[1].Description == "" ||
		types[2].Type != Page || types[2].Name != "Page" || types[2].Description == "" {
		t.Fatal(types)
	}
}
