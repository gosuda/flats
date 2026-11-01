package notebook

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func vocabID(kind Kind, n byte) ID {
	raw := make([]byte, 16)
	raw[15] = n
	return ID(kindPrefixes[kind] + idEncoding.EncodeToString(raw))
}
func vocabTarget(kind Kind, n byte) Target { return Target{Ref: &Ref{ID: vocabID(kind, n), Inc: 7}} }

func TestIdentityRoundTrip(t *testing.T) {
	t.Parallel()
	for kind := range kindPrefixes {
		t.Run(string(kind), func(t *testing.T) {
			id := vocabID(kind, 255)
			parsed, err := ParseID(string(id))
			if err != nil || parsed != id || parsed.Kind() != kind {
				t.Fatalf("ParseID = %q, %v", parsed, err)
			}
			ref := Ref{ID: id, Inc: 18446744073709551615}
			data, err := json.Marshal(ref)
			if err != nil {
				t.Fatal(err)
			}
			var got Ref
			if err := json.Unmarshal(data, &got); err != nil || got != ref {
				t.Fatalf("reference round trip: %v, %v", got, err)
			}
			for _, target := range []Target{{Ref: &ref}, {Tmp: "a-new-item"}} {
				data, err := json.Marshal(target)
				if err != nil {
					t.Fatal(err)
				}
				var got Target
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				again, _ := json.Marshal(got)
				if !bytes.Equal(data, again) {
					t.Fatalf("target changed: %s -> %s", data, again)
				}
			}
		})
	}
}
func TestIdentityRejectsMalformed(t *testing.T) {
	t.Parallel()
	valid := string(vocabID(PageKind, 0))
	invalid := []string{"", strings.ToUpper(valid), valid + "a", valid[:len(valid)-1], "unknown_" + strings.Repeat("a", 26), "p_" + strings.Repeat("a", 25) + "b", "p_" + strings.Repeat("0", 26), "p_" + strings.Repeat("a", 25) + "8"}
	for _, s := range invalid {
		t.Run(s, func(t *testing.T) {
			if _, err := ParseID(s); err == nil {
				t.Fatalf("accepted %q", s)
			}
		})
	}
	for _, input := range []string{`{}`, `null`, `{"id":"` + valid + `"}`, `{"id":"` + valid + `","inc":1}`, `{"id":"` + valid + `","inc":"1","extra":0}`, `{"id":"` + valid + `","inc":"1","ID":"` + valid + `"}`, `{"id":"` + valid + `","inc":"1","inc":"2"}`} {
		t.Run(input, func(t *testing.T) {
			var r Ref
			if err := json.Unmarshal([]byte(input), &r); err == nil {
				t.Fatalf("accepted %s", input)
			}
		})
	}
	for _, input := range []string{`{}`, `null`, `{"tmp":""}`, `{"tmp":null}`, `{"tmp":"x","inc":"1"}`, `{"tmp":"x","id":"` + valid + `","inc":"1"}`, `{"tmp":"x","extra":true}`, `{"tmp":"line\nbreak"}`} {
		t.Run("target/"+input, func(t *testing.T) {
			var target Target
			if err := json.Unmarshal([]byte(input), &target); err == nil {
				t.Fatalf("accepted %s", input)
			}
		})
	}
	if err := vocabTarget(BlockKind, 0).ValidateKind(PageKind); err == nil {
		t.Fatal("wrong target kind accepted")
	}
	if err := (Target{Tmp: "later"}).ValidateKind(PageKind); err != nil {
		t.Fatal(err)
	}
}
func TestSequenceCodecs(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`"0"`, `"1"`, `"9007199254740993"`, `"18446744073709551615"`} {
		t.Run(input, func(t *testing.T) {
			for _, out := range []any{new(Sequence), new(Version), new(Incarnation)} {
				if err := json.Unmarshal([]byte(input), out); err != nil {
					t.Fatal(err)
				}
				got, err := json.Marshal(out)
				if err != nil || string(got) != input {
					t.Fatalf("round trip %s = %s, %v", input, got, err)
				}
			}
		})
	}
	for _, input := range []string{`0`, `1.0`, `""`, `null`, `"00"`, `"01"`, `"+1"`, `"-1"`, `"1e2"`, `" 1"`, `"18446744073709551616"`} {
		t.Run("invalid/"+input, func(t *testing.T) {
			for _, out := range []any{new(Sequence), new(Version), new(Incarnation)} {
				if err := json.Unmarshal([]byte(input), out); err == nil {
					t.Fatalf("accepted %s", input)
				}
			}
		})
	}
}
func TestStrictJSONBoundary(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`{"x":1,"x":2}`, `{"nested":{"x":1,"x":2}}`, `{"x":1} {}`, `[1,]`, `"\ud800"`, `"\udc00"`, `"\ud800\u0041"`, "\"\xff\"", strings.Repeat("[", 130) + strings.Repeat("]", 130)} {
		t.Run(input, func(t *testing.T) {
			var v any
			if err := strictDecode([]byte(input), &v); err == nil {
				t.Fatalf("accepted %s", input)
			}
		})
	}
	var got string
	if err := strictDecode([]byte(`"\ud83d\ude00"`), &got); err != nil || got != "😀" {
		t.Fatalf("paired surrogates = %q %v", got, err)
	}
	if err := strictDecode([]byte(`"\\ud800"`), &got); err != nil || got != `\ud800` {
		t.Fatalf("literal escape = %q %v", got, err)
	}
	canonical, err := CanonicalJSON(map[string]any{"z": "한글<&>", "a": json.Number("1")})
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != `{"a":1,"z":"한글<&>"}` {
		t.Fatalf("canonical JSON = %s", canonical)
	}
}
