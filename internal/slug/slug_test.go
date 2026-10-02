package slug

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := []string{"abc", "blog", "my-site", "a1b2", "x" + strings.Repeat("y", 53)}
	for _, s := range valid {
		if err := Validate(s); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"", "ab", "1abc", "-abc", "abc-", "a--b", "Abc", "ab_c", "ab.c", "x" + strings.Repeat("y", 54), "한글이다"}
	for _, s := range invalid {
		if err := Validate(s); err == nil {
			t.Errorf("Validate(%q) = nil, want error", s)
		}
	}
}

func TestPreviewHostFitsDNSLabel(t *testing.T) {
	s := "a" + strings.Repeat("b", MaxLen-1)
	h := PreviewHost(s)
	if len(h) > 63 {
		t.Fatalf("preview host %q is %d bytes, want <= 63", h, len(h))
	}
	if !strings.HasPrefix(h, s+"-") || len(h) != len(s)+1+RandomLen {
		t.Fatalf("unexpected preview host %q", h)
	}
	if err := Validate(h); err != nil && len(h) <= MaxLen {
		t.Fatalf("preview host should be a valid label: %v", err)
	}
}
