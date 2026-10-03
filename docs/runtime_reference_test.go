package runtimeref_test

import (
	"fmt"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/runtime"
)

func TestDocumentedRuntimeLimits(t *testing.T) {
	for _, marker := range []string{
		fmt.Sprintf("%d MiB", runtime.MaxFileValue>>20),
		fmt.Sprintf("%d GiB", runtime.MaxFilesTotal>>30),
		fmt.Sprintf("%d MiB", runtime.MaxQueryResult>>20),
		fmt.Sprintf("%d MiB", runtime.MaxRequestBody>>20),
		fmt.Sprintf("%d MiB", runtime.MaxResponseBody>>20),
		fmt.Sprintf("%d MiB", runtime.DefaultMemoryPages*65536>>20),
		fmt.Sprintf("%d-second", int(runtime.DefaultTimeout.Seconds())),
		"20,000 upload files",
	} {
		if !strings.Contains(runtimeref.Markdown, marker) {
			t.Errorf("reference omits runtime limit %s", marker)
		}
	}
	if bundle.MaxFiles != 20000 {
		t.Fatal("upload file count changed: update reference and assertion")
	}
}
