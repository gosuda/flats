package design

import (
	"strings"
	"testing"
)

func TestTweaksRoundTrip(t *testing.T) {
	m, err := ParseTweaks([]byte(`{"accent":{"type":"color","label":"Accent","value":"#D97757"},
"gap":{"type":"number","value":24,"unit":"px","min":0,"max":96},
"headline":{"type":"text","value":"Say \"hi\" </style>"},
"density":{"type":"select","options":["compact","roomy"],"value":"roomy"},
"dark":{"type":"boolean","value":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	css := TweakCSS(m)
	want := `:root{--accent:#d97757;--dark:0;--density:roomy;--gap:24px;--headline:"Say \22 hi\22  \3c /style\3e ";}`
	if css != want {
		t.Fatalf("css = %s\nwant  %s", css, want)
	}
	if _, err := SetTweak(m, "gap", 200.0); err == nil {
		t.Fatal("out of range accepted")
	}
	m2, err := SetTweak(m, "accent", "#000")
	if err != nil || m2["accent"].Value != "#000" || m["accent"].Value != "#d97757" {
		t.Fatalf("set = %v %v", m2["accent"], err)
	}
	b, _ := MarshalTweaks(m2)
	if strings.Contains(string(b), "</") {
		t.Fatalf("marshal can close script: %s", b)
	}
	if _, err := ParseTweaks(b); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	for _, bad := range []string{`{"Bad":{"type":"color","value":"#fff"}}`, `{"a":{"type":"color","value":"red"}}`, `{"a":{"type":"select","options":["x"],"value":"y"}}`, `[]`} {
		if _, err := ParseTweaks([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
