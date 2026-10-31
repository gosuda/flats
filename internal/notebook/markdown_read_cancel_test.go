package notebook

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMarkdownCopyStopsAfterOutputError(t *testing.T) {
	for _, flat := range []bool{false, true} {
		source := strings.NewReader(strings.Repeat("\n", 1<<20))
		out := markdownLines{out: &markdownStream{err: context.Canceled}}
		if err := markdownCopy(out, source, false, flat, false); !errors.Is(err, context.Canceled) {
			t.Fatalf("flat=%t: error = %v", flat, err)
		}
		if source.Len() == 0 {
			t.Fatalf("flat=%t: drained the whole source after the output failed", flat)
		}
	}
}
