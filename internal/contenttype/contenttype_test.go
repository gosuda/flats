package contenttype

import "testing"

func TestFromManifest(t *testing.T) {
	for _, tt := range []struct{ name, raw, want string }{{"legacy", "{}", Flat}, {"missing", "", Flat}, {"flat", `{"type":"flat"}`, Flat}, {"docs", `{"type":"docs"}`, Docs}} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromManifest([]byte(tt.raw)); got != tt.want {
				t.Fatalf("%s", got)
			}
		})
	}
	types := Types()
	if len(types) != 2 || types[0].Type != Flat || types[1].Type != Docs || types[1].Name != "Document" || types[1].Description == "" {
		t.Fatal(types)
	}
}
