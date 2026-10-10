package runtime

import (
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
)

// The published browser check script must at least parse as JavaScript. It
// needs a DOM, so it is compiled inside a function that is never called.
func TestPreviewCheckScriptParses(t *testing.T) {
	code := "function previewCheck() {\n" + runtimeref.PreviewCheck + "\n}\n" +
		`export default { fetch() { return new Response(typeof previewCheck); } };`
	f := mustStart(t, newManager(t), "preview-check", map[string]string{"index.js": code}, "index.js", nil)
	if r := f.do(t, "GET", "/", ""); r.status != 200 || r.body != "function" {
		t.Fatalf("preview check script did not compile: %+v", r)
	}
}
